# unified-messaging

A Unipile-style unified messaging API.

One Go service that connects end-user accounts over OAuth, keeps a local mirror
in sync, exposes a provider-neutral REST API over it, and pushes normalized
webhooks when messages arrive.

**Outlook / Microsoft 365 is the first provider.** It is an implementation of
the contracts in [`internal/provider`](internal/provider/provider.go), not the
architecture. Nothing outside `internal/provider/outlook` knows Microsoft Graph
exists.

**Status: proof of concept.** It proves the mechanisms that carry real risk.
Multi-tenancy, API-key management, billing, a developer dashboard and a hosted
auth UI are deliberately absent.

## Layout

```
internal/
  model/        provider-neutral types — Account, Email, Folder, Event
  provider/     the contracts every backend implements, plus a registry
    outlook/    Microsoft Graph implementation
  accounts/     identity and token custody, provider-agnostic
  syncer/       backfill, incremental sync, push subscription upkeep
  store/        SQLite: accounts, sealed tokens, cursors, the mail mirror
  events/       normalized outbound webhooks
  api/          the HTTP surface
```

---

## What it does

| Area | Included |
|---|---|
| **Providers** | Contract-driven registry; a backend supplies an `Authenticator`, a `Mailbox`, and optionally a `Pusher` |
| **Connect** | Auth-code + PKCE flow, single-use connect links, `notify_url` callback, silent token refresh, reconnect detection |
| **Read** | List/get emails, threads, folders (with well-known roles), attachments, search, paging — served from the local store |
| **Write** | Send, **reply and reply-all in-thread**, forward, drafts, attachments, mark read/flagged |
| **Sync** | Bounded backfill, per-scope incremental cursors, push subscriptions with auto-renewal, lifecycle handling, polling as the safety net |
| **Events** | `mail_received`, `mail_sent`, `mail_updated`, `mail_deleted`, `account_status` — HMAC-signed, retried |

Not included: calendar, contacts, open/click tracking, folder management, and
any provider other than Outlook.

---

## Setup

All configuration below is Outlook-specific. A second provider would bring its
own settings and leave these untouched.

### 1. Register an application with Microsoft

You must do this yourself — it requires signing in to your own Microsoft account.

1. Go to <https://entra.microsoft.com> → **App registrations** → **New registration**.
2. **Name:** anything, e.g. `outlook-api-poc`.
3. **Supported account types:** *Personal Microsoft accounts only*.
   (Choose *…any organizational directory and personal Microsoft accounts* if you
   also want work/school mailboxes — then set `MS_TENANT=common`.)
4. **Redirect URI:** platform **Web**, value `http://localhost:8080/oauth/callback`.
5. Copy the **Application (client) ID** → `MS_CLIENT_ID`.
6. **Certificates & secrets** → **New client secret** → copy the *Value*
   (not the ID) → `MS_CLIENT_SECRET`.
7. **API permissions** → **Add a permission** → **Microsoft Graph** →
   **Delegated permissions** → add:
   `offline_access`, `User.Read`, `Mail.Read`, `Mail.ReadWrite`, `Mail.Send`.
   Personal accounts consent individually; no admin consent step.

> Registering under **Personal Microsoft accounts only** means the authority must
> be `consumers`. Using `common` against such a registration fails at sign-in.

> Prefer no client secret? Register the redirect under **Mobile and desktop
> applications** instead and leave `MS_CLIENT_SECRET` empty — PKCE alone is then
> sufficient. The Web + secret path above is the more production-shaped one.

### 2. Configure

```bash
cp .env.example .env
openssl rand -base64 32   # paste into TOKEN_ENCRYPTION_KEY
$EDITOR .env
```

### 3. Run

```bash
set -a && source .env && set +a && go run ./cmd/server
```

### 4. Connect a mailbox

The easiest path is the dashboard: open **`http://localhost:8080/dashboard`**,
paste in `API_KEY`, and click **Connect account**. It walks the same flow below
but does the `hosted-auth` call and redirect for you, and lists what's connected
afterward — status, last synced, resync, disconnect.

To do it by hand instead:

```bash
curl -s -X POST localhost:8080/api/v1/hosted-auth \
  -H "Authorization: Bearer $API_KEY" -H 'Content-Type: application/json' \
  -d '{}' | jq -r .url
```

Open that URL in a browser. It shows a branded "Connect your Outlook account"
page first — this is what a caller's own users would see mid-flow — then sign
in and consent on Microsoft's real screen. The confirmation page reports an
`account_id`. Backfill starts immediately in the background.

### 5. Optional: real-time push

Graph will only create a subscription if it can reach your notification endpoint
over public HTTPS and complete a validation handshake against it. Without this
the service still works, falling back to incremental polling every
`POLL_INTERVAL_SECONDS`.

```bash
cloudflared tunnel --url http://localhost:8080     # or: ngrok http 8080
```

Then set `PUBLIC_BASE_URL` to the HTTPS origin it prints, **add
`https://<that-origin>/oauth/callback` as a second Redirect URI** on the app
registration, point `MS_REDIRECT_URI` at it, and restart. Startup logs
`push_notifications=true` when subscriptions are active.

---

## UI

There is no mail client and no operator login system — everything is one API
plus two small screens, both served by the same binary with no build step:

| Screen | Route | Audience | Auth |
|---|---|---|---|
| Connect landing page | `GET /connect/{state}` | The end user being connected (Priya) | The single-use state token in the URL |
| Account dashboard | `GET /dashboard` | You / the integrating developer | API key pasted client-side, kept in `localStorage` |

The landing page is what stands between "clicked a link from some app" and
"typing a Microsoft password" — a bare redirect there looks like phishing. With
one provider it's a single confirmation screen; with a second provider this is
where a picker (à la Unipile's hosted auth wizard) would go.

The dashboard is connection *lifecycle* management only — list accounts, see
`OK` vs `CREDENTIALS` status, connect a new one, force a resync, disconnect.
It does not browse mail or send anything; that's `/api/v1/emails*`, for a real
caller's own app to build on. The page itself carries no secret — the API key
lives only in the browser, exactly like any other API consumer.

## API

All `/api/v1/*` routes require `Authorization: Bearer $API_KEY`.
All mail routes require `?account_id=…`.

### Connection

| Method | Path | Notes |
|---|---|---|
| `GET` | `/api/v1/providers` | Which backends are configured, and whether each supports push |
| `POST` | `/api/v1/hosted-auth` | Mint a connect link. Body (all optional): `provider`, `success_redirect_url`, `failure_redirect_url`, `notify_url`, `expires_in_minutes`, `force_consent` |
| `GET` | `/api/v1/accounts` | |
| `GET` | `/api/v1/accounts/{id}` | |
| `DELETE` | `/api/v1/accounts/{id}` | Also removes the upstream push subscription |
| `POST` | `/api/v1/accounts/{id}/resync` | Force a sync now |

### Mail

| Method | Path | Notes |
|---|---|---|
| `GET` | `/api/v1/folders` | |
| `GET` | `/api/v1/threads` | |
| `GET` | `/api/v1/emails` | `folder_id`, `folder_role=inbox`, `thread_id`, `unread=true`, `q=`, `limit`, `offset`. Bodies omitted |
| `GET` | `/api/v1/emails/{id}` | The complete message: full `body` + `body_plain`, folder `role`, and `attachments` metadata (fetched once from the provider, then cached). Falls back to the provider on a local miss |
| `PATCH` | `/api/v1/emails/{id}` | `{"read":true}` / `{"flagged":true}` |
| `POST` | `/api/v1/emails` | Send. `reply_to_email_id` routes it through the reply path |
| `POST` | `/api/v1/emails/{id}/reply` | `{"reply_all":true}` supported |
| `POST` | `/api/v1/emails/{id}/forward` | `to` required |
| `GET` | `/api/v1/emails/{id}/attachments` | |
| `GET` | `/api/v1/emails/{id}/attachments/{aid}` | Raw bytes |
| `POST` | `/api/v1/drafts` | |
| `POST` | `/api/v1/drafts/{id}/send` | |

### Webhooks

Webhooks are configured **per account**: each connected mailbox belongs to a
different end user, so their mail can go to a different endpoint. A hook with
no account is global and receives every account's events.

| Method | Path |
|---|---|
| `POST` | `/api/v1/accounts/{id}/webhooks` — `{"url": "...", "name": "...", "secret": "...", "events": ["mail_received"]}` (events default to `mail_received`) |
| `GET` | `/api/v1/accounts/{id}/webhooks` |
| `DELETE` | `/api/v1/accounts/{id}/webhooks/{wid}` |
| `POST` | `/api/v1/webhooks` — global; empty `events` means everything |
| `GET` | `/api/v1/webhooks` |
| `DELETE` | `/api/v1/webhooks/{id}` |
| `GET` | `/api/v1/webhooks/{id}/deliveries` — failed deliveries still queued, and dead ones |

The easiest way to set one is at connect time: pass `"webhook": {"url": ...,
"secret": ...}` to `POST /api/v1/hosted-auth` and it is bound to the account
the moment the user finishes signing in — before the first sync, so nothing is
missed. The dashboard's account card has a small **Set webhook** form for the
same thing.

Each delivery is `{"type", "account_id", "timestamp", "webhook": {"id", "name"},
"email": {...}}` where `email` is the full normalized message: `body`,
`body_plain` (markup stripped), `from`/`to`/`cc`/`bcc`/`reply_to`, `role`
(well-known folder: `inbox`, `sentitems`, …), `internet_message_id`,
`thread_id`, `has_attachments`, and for new mail the `attachments` list
(id, name, mime_type, size) so no follow-up call is needed. `webhook.name` is
whatever you passed as `name` when registering the hook.

Deliveries carry `X-Outlook-Event`, `X-Outlook-Delivery` (attempt number) and,
when a secret is set, `X-Outlook-Signature: sha256=<hex hmac of the raw body>`.
Delivery is **at-least-once** — dedupe on `(type, email.id)`.

**Retries.** Anything other than a 2xx is a failure. The delivery is written to
SQLite and retried on this schedule after the immediate first attempt:
30s, 2m, 10m, 30m, 2h, 6h, 12h. After the last one it is marked `dead` and
kept, visible via the `deliveries` endpoint. The queue survives restarts.

### Examples

```bash
# newest 10 in the inbox
curl -s -H "Authorization: Bearer $API_KEY" \
  "localhost:8080/api/v1/emails?account_id=$ACC&folder_role=inbox&limit=10" | jq

# send
curl -s -X POST localhost:8080/api/v1/emails \
  -H "Authorization: Bearer $API_KEY" -H 'Content-Type: application/json' \
  -d "{\"account_id\":\"$ACC\",\"to\":[{\"email\":\"someone@example.com\"}],
       \"subject\":\"Hello\",\"body\":\"<p>Hi there</p>\"}"

# reply in-thread
curl -s -X POST "localhost:8080/api/v1/emails/$MSG_ID/reply" \
  -H "Authorization: Bearer $API_KEY" -H 'Content-Type: application/json' \
  -d "{\"account_id\":\"$ACC\",\"body\":\"<p>Thanks!</p>\",\"reply_all\":false}"
```

---

## How it works

```
  end user ──consent──▶ provider
      ▲                    │
      │ /connect/{state}   │ code
      │                    ▼
  caller ──hosted-auth──▶ this service ──sync + subscriptions──▶ provider
      ▲                    │
      └──normalized────────┘
         webhooks / REST
```

**The provider seam.** `internal/provider` defines three contracts. An
`Authenticator` attaches accounts and keeps grants alive. A `Mailbox` reads and
writes messages. A `Pusher` is optional — a provider without one simply does not
implement it, and the core polls instead. The registry resolves an account's
provider from the `provider` field stored on it, so adding a backend touches no
code above that line.

**Scopes are the key abstraction.** A `Scope` is one unit of incremental sync.
Microsoft Graph exposes message delta only per mail folder, so the Outlook
provider returns one scope per folder. A provider with a single mailbox-wide
cursor — Gmail's `historyId`, an IMAP `MODSEQ` — returns exactly one scope. The
sync loop never learns which it is dealing with; `internal/syncer` contains the
word "folder" only where it is persisting a provider's folder list.

**Token custody.** Refresh tokens are sealed with AES-256-GCM before touching
SQLite. Refresh is serialized per account: providers rotate refresh tokens, so
concurrent redemptions would leave all but one caller holding a dead token. A
terminal auth failure surfaces as `provider.ErrReauthRequired`, which flips the
account to `CREDENTIALS` and emits `account_status` — only the end user can fix
that, by reconnecting.

**Sync.** Push and poll converge on the same idempotent walk. A notification is
treated purely as "something changed" and triggers that walk rather than being
trusted as data, which keeps one set of dedupe rules for both paths.

**Threading.** Providers that generate threading headers themselves should be
allowed to. Outlook's `Reply` goes through Graph's `createReply` → `PATCH` →
`send`, so `conversationId`, `In-Reply-To` and `References` come from Microsoft.
Composing a fresh message with hand-written headers does not thread reliably.

---

## Adding a provider

1. Create `internal/provider/<name>/`.
2. Implement `provider.Authenticator`, `provider.Mailbox`, and — only if the
   backend can push — `provider.Pusher`.
3. Map the backend's failures onto the shared sentinels: `ErrReauthRequired`,
   `ErrCursorExpired`, `ErrNotFound`, `ErrSubscriptionExists`. The core changes
   behaviour based on these, so a provider that never returns `ErrCursorExpired`
   will stall permanently the first time a cursor goes stale.
4. Add it to the registry in [`cmd/server/main.go`](cmd/server/main.go).

Push endpoints are namespaced per provider (`/notifications/{provider}`), so
each backend's validation scheme and payload format stay independent.

`internal/syncer/provider_contract_test.go` exercises the contracts with a
deliberately un-Outlook-like provider — one mailbox-wide cursor, no folders, no
push — which is what stops Graph's shape leaking back into the core.

---

## Graph constraints worth knowing

These are Outlook-specific and confined to `internal/provider/outlook`.

| Constraint | Consequence |
|---|---|
| Message `delta` is folder-scoped — there is no `/me/messages/delta` | Scopes are folders for this provider |
| The only `$filter` delta accepts is `receivedDateTime ge/gt`, and only on the first call | `BACKFILL_DAYS` is baked into the cursor; changing it needs a resync |
| Delta **replays** items and returns unrelated read/unread churn | Every write is idempotent |
| Delta tokens can be evicted (`syncStateNotFound`, `410 Gone`) | Mapped to `ErrCursorExpired`; the core resyncs the scope |
| Outlook subscriptions last ≤ 10,080 min (~7 days) | Renewed hourly, well before expiry |
| Duplicate `(changeType, resource)` subscriptions → `409` | Mapped to `ErrSubscriptionExists`; the existing one is adopted |
| Graph cannot send custom headers | Notification auth is the per-subscription `clientState` |
| Personal accounts: delegated permissions only, no shared mailboxes | One mailbox per connection |
| `$search` cannot combine with `$filter`, and is unavailable in delta | Search runs against the local mirror |

---

## Known gaps

- **Attachments >3 MB on send** need Graph upload sessions; not implemented.
- **Search** is SQL `LIKE` over locally synced mail, not server-side search.
- **`/sendMail` returns no ID**, so a plain Outlook send has no message ID until
  the next sync surfaces it in Sent Items. Replies and forwards do return one.
- **Adopted subscriptions** (after a `409`) have no known `clientState`, so
  their notifications are verified by subscription ID alone.
- **First delivery attempt is in-memory.** A crash between an event being
  emitted and its first POST loses it; once it has failed once it is in SQLite
  and survives restarts. The poll re-converges the data but those events are
  not replayed.
- **Scope listing is not incremental** for Outlook — the folder tree is relisted
  each round. Cheap at mailbox scale, but it is a full listing, not a delta.
- **Single tenant.** One API key, one shared account pool.

---

## Development

```bash
make test    # full suite, no credentials needed
make build
make key     # generate TOKEN_ENCRYPTION_KEY
```

Tests run entirely in-process, covering delta pagination, removal handling,
event suppression on first sync, dedupe, cursor persistence, token sealing, the
push validation handshake, the PKCE connect flow, and the provider contracts
against a non-Outlook-shaped backend.
