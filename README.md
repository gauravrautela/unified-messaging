# unified-messaging

A Unipile-style unified messaging API.

One Go service that connects end-user accounts over OAuth, keeps a local mirror
in sync, exposes a provider-neutral REST API over it, and pushes normalized
webhooks when messages arrive.

**Outlook / Microsoft 365 is the first provider; WhatsApp is the second.** Both
are implementations of the contracts in
[`internal/provider`](internal/provider/provider.go), not the architecture.
Nothing outside `internal/provider/outlook` knows Microsoft Graph exists, and
nothing outside `internal/provider/whatsapp` knows whatsmeow exists. Outlook is
a **mail** provider (`kind: "mail"`, OAuth); WhatsApp is a **chat** provider
(`kind: "chat"`, QR-linked) — see "Providers" below for what that split means
for the API and for `internal/chatsync`, the chat counterpart of
`internal/syncer`.

**Status: proof of concept.** It proves the mechanisms that carry real risk.
Multi-tenancy, API-key management and a developer dashboard are in; billing
and a hosted auth UI for your own end users (as opposed to you, the
developer) are still deliberately absent.

## Layout

```
internal/
  model/        provider-neutral types — Account, Email, Chat, ChatMessage, Event
  provider/     the contracts every backend implements, plus a registry
    outlook/    Microsoft Graph implementation (mail)
    whatsapp/   whatsmeow linked-device implementation (chat)
  accounts/     identity and token/device custody, provider-agnostic
  syncer/       mail: backfill, incremental sync, push subscription upkeep
  chatsync/     chat: one persistent socket per linked account, reconnect/backoff
  store/        SQLite: accounts, sealed tokens, cursors, the mail + chat mirror
  events/       normalized outbound webhooks
  api/          the HTTP surface
```

---

## What it does

| Area | Included |
|---|---|
| **Providers** | Contract-driven registry; a mail backend supplies an `Authenticator` and a `Mailbox`, optionally a `Pusher`; a chat backend supplies a `Linker` and a `Chatter` |
| **Connect (mail)** | Auth-code + PKCE flow, single-use connect links, `notify_url` callback, silent token refresh, reconnect detection |
| **Connect (chat)** | Disclosure + explicit consent, QR pairing (whatsmeow), single-use connect links, same `notify_url` callback shape as mail |
| **Read** | List/get emails, threads, folders (well-known roles), attachments, search, paging; list/get chats, messages, attendees — all served from the local store |
| **Write** | Mail: send, **reply and reply-all in-thread**, forward, drafts, attachments, mark read/flagged. Chat: send, start a chat, edit, delete, react, mark read — with idempotent retries via `Idempotency-Key` |
| **Sync** | Mail: bounded backfill, per-scope incremental cursors, push subscriptions with auto-renewal, polling as the safety net. Chat: one persistent socket per linked account with reconnect/backoff, capped account count |
| **Events** | Mail: `mail_received`, `mail_sent`, `mail_updated`, `mail_deleted`. Chat: `chat_received`, `chat_sent`, `chat_updated`, `chat_reaction`, `chat_deleted`. Both: `account_status` — HMAC-signed, retried |

Not included: calendar, contacts, open/click tracking, folder management, and
any provider other than Outlook and WhatsApp. WhatsApp is text-only (no
media) with no history sync.

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

Open **`http://localhost:8080/signup`**, create your developer account, then
on the dashboard create an API key (shown once — copy it somewhere) and click
**Connect account**. It walks the same flow below but does the `hosted-auth`
call and redirect for you, and lists what's connected afterward — status, last
synced, resync, disconnect.

To do it by hand instead, export the key you created on the dashboard as
`API_KEY` and:

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

### 6. Optional: WhatsApp

WhatsApp is integrated through the **linked-device model** — the same
mechanism as `web.whatsapp.com` — not the official WhatsApp Business API. The
end user links this service as an additional device on their own phone by
scanning a QR code; nothing is registered as a business number, and there is
no Meta app registration to do.

> **Read this before turning it on.** Meta can ban a phone number it judges to
> be automating WhatsApp (bulk sends, non-human pacing, spam reports). This is
> exactly the risk every unofficial WhatsApp client carries. The connect flow
> shows the end user a disclosure and requires an explicit consent click before
> a QR code is ever generated — don't build a UI on top of it that skips or
> hides that step. Send at a human pace; this is not a marketing/broadcast
> channel.
>
> **Device keys are stored unsealed.** Unlike OAuth refresh tokens (sealed
> with AES-256-GCM before touching disk — see "Token custody" below),
> whatsmeow writes its own device-credential tables straight into this
> service's SQLite file, in the clear: anyone who can read the database file
> can impersonate a linked device. If you turn this on for anything beyond
> local testing, put the database on encrypted disk (or run the whole host
> encrypted-at-rest) — there is no application-level mitigation for this.

Enable it:

```bash
WHATSAPP_ENABLED=true
```

Then connect the same way as a mailbox, naming the provider explicitly (an
unnamed `hosted-auth` call only ever defaults to the sole *mail* provider —
pairing a phone number is always something a caller must ask for by name):

```bash
curl -s -X POST localhost:8080/api/v1/hosted-auth \
  -H "Authorization: Bearer $API_KEY" -H 'Content-Type: application/json' \
  -d '{"provider": "WHATSAPP"}' | jq -r .url
```

Opening that URL shows the disclosure above with a consent checkbox; ticking
it and clicking through starts a pairing session and displays a QR code
(`GET /connect/{state}/qr`, polled by the page). Scan it in WhatsApp on your
phone: **Settings → Linked devices → Link a device**. On success the page
redirects and `notify_url` (if set) receives
`{"status":"CREATED","account_id":"acc_…","identifier":"+15551234567","provider":"WHATSAPP"}`.
Unlinking the device from the phone, or 30 consecutive reconnect failures,
flips the account to `status: "CREDENTIALS"` and emits `account_status` —
exactly like a revoked Outlook token. Mint a fresh connect link to pair
again: relinking the same number reuses the account id (the old device
keys are forgotten first). See
[`docs/whatsapp-manual-checklist.md`](docs/whatsapp-manual-checklist.md) for
the full manual walkthrough with a real phone.

**Known limits:** text messages only (media arrives as `kind: "unsupported"`,
none can be sent); no history sync (a newly linked device only sees messages
sent after pairing, same as WhatsApp Web); QR linking only, no phone-number
pairing codes; one live socket per account, capped process-wide by
`WHATSAPP_MAX_ACCOUNTS`; reconnect backoff rises to 5 minutes between
attempts.

---

## UI

There is no mail client — everything is one API plus a handful of small
screens, all served by the same binary with no build step:

| Screen | Route | Audience | Auth |
|---|---|---|---|
| Signup / login | `GET`/`POST /signup`, `GET`/`POST /login` | You / the integrating developer | None (that's the point) |
| Connect landing page | `GET /connect/{state}` | The end user being connected (Priya) | The single-use state token in the URL |
| Account dashboard | `GET /dashboard` | You / the integrating developer | Signed-in browser session (`um_session` cookie) |

The landing page is what stands between "clicked a link from some app" and
"typing a Microsoft password" — a bare redirect there looks like phishing. With
one provider it's a single confirmation screen; with a second provider this is
where a picker (à la Unipile's hosted auth wizard) would go.

The dashboard requires signing in at `/signup` or `/login` first; it redirects
there otherwise. It is connection *lifecycle* management plus API key
issuance — list accounts, see `OK` vs `CREDENTIALS` status, connect a new one,
force a resync, disconnect, create/revoke API keys. It does not browse mail or
send anything; that's `/api/v1/emails*`, for a real caller's own app to build
on. The dashboard's own fetches ride the session cookie; API keys it mints are
for external callers (scripts, your own backend) to use as a bearer token.

## API

All `/api/v1/*` routes require `Authorization: Bearer $API_KEY`, where
`$API_KEY` is a key you created on the dashboard (see "Developers, sessions
and API keys" below) — it is not an environment variable. The `um_session`
cookie from `/login` works too (a `POST`/`PUT`/`PATCH` riding the cookie must
send `Content-Type: application/json`), except for managing API keys
themselves, which is session-only — see below.
All mail routes require `?account_id=…`.

### Connection

| Method | Path | Notes |
|---|---|---|
| `GET` | `/api/v1/providers` | `{items: [{name, kind: "mail"\|"chat", auth: "oauth"\|"link", push_notifications}]}` — WhatsApp reports `kind: "chat", auth: "link"` |
| `POST` | `/api/v1/hosted-auth` | Mint a connect link. Body (all optional unless several providers of one kind are registered): `provider`, `success_redirect_url`, `failure_redirect_url`, `notify_url`, `expires_in_minutes`, `force_consent`. `provider` is **required** to connect WhatsApp — the unnamed-call default only ever resolves to a lone mail provider |
| `GET` | `/api/v1/accounts` | |
| `GET` | `/api/v1/accounts/{id}` | Chat accounts add `connection: {state, since, reconnects, last_error?}` |
| `DELETE` | `/api/v1/accounts/{id}` | Mail: also removes the upstream push subscription. Chat: also logs the linked device out |
| `POST` | `/api/v1/accounts/{id}/resync` | Mail only. Force a sync now; `400 unsupported_for_kind` on a chat account |
| `POST` | `/api/v1/accounts/{id}/reconnect` | Chat only. Force the socket to reconnect now; `400 unsupported_for_kind` on a mail account; `503 capacity` at `WHATSAPP_MAX_ACCOUNTS` |

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

### Chat (WhatsApp)

| Method | Path | Notes |
|---|---|---|
| `GET` | `/api/v1/chats` | `kind=direct\|group`, `unread=true`, `q=`, `limit`, `offset` |
| `POST` | `/api/v1/chats` | `{"account_id","phone" or "attendee_id","text"}` → `201 {"chat","message"}`. Starts a direct chat and sends the first message |
| `GET` | `/api/v1/chats/{id}` | |
| `PATCH` | `/api/v1/chats/{id}` | `{"read":true}` and/or `{"archived":true}`/`{"muted":true}` |
| `GET` | `/api/v1/chats/{id}/messages` | `before=`, `limit` — `{"items","next_before"}`, newest first |
| `POST` | `/api/v1/chats/{id}/messages` | `{"text","quoted_message_id"}`. Supports `Idempotency-Key` |
| `GET` | `/api/v1/chats/{id}/messages/{mid}` | |
| `PATCH` | `/api/v1/chats/{id}/messages/{mid}` | `{"text"}`. `403 not_own_message` if you didn't send it |
| `DELETE` | `/api/v1/chats/{id}/messages/{mid}` | Revokes for everyone. `403 not_own_message` if you didn't send it |
| `PUT` | `/api/v1/chats/{id}/messages/{mid}/reaction` | `{"emoji"}` (`""` removes it). `403 not_own_message` doesn't apply — you can react to anyone's message |
| `GET` | `/api/v1/attendees` | `q=` searches name/phone |
| `GET` | `/api/v1/attendees/{id}` | |

All chat routes require `?account_id=…` (or in the body for `POST /api/v1/chats`)
and return `400 unsupported_for_kind` against a mail account. `Idempotency-Key`
on a write is scoped per developer and per operation (method, path, account,
body): the same key+body replays the first response; the same key with a
different body is `409 idempotency_conflict` — use it on any chat write you
might retry, since a retried send must never double-send a message.

### Webhooks

Webhooks are configured **per account**: each connected mailbox belongs to a
different end user, so their mail can go to a different endpoint. A hook with
no account is developer-wide and receives every one of that developer's
accounts' events.

| Method | Path |
|---|---|
| `POST` | `/api/v1/accounts/{id}/webhooks` — `{"url": "...", "name": "...", "secret": "...", "events": ["mail_received"]}` (events default to `mail_received`) |
| `GET` | `/api/v1/accounts/{id}/webhooks` |
| `DELETE` | `/api/v1/accounts/{id}/webhooks/{wid}` |
| `POST` | `/api/v1/webhooks` — developer-wide; empty `events` means everything |
| `GET` | `/api/v1/webhooks` |
| `DELETE` | `/api/v1/webhooks/{id}` |
| `GET` | `/api/v1/webhooks/{id}/deliveries` — failed deliveries still queued, and dead ones |

The easiest way to set one is at connect time: pass `"webhook": {"url": ...,
"secret": ...}` to `POST /api/v1/hosted-auth` and it is bound to the account
the moment the user finishes signing in — before the first sync, so nothing is
missed. The dashboard's account card has a small **Set webhook** form for the
same thing.

Each delivery is `{"type", "account_id", "timestamp", "webhook": {"id", "name"},
...}` where the rest of the payload depends on `type`. For mail, `"email"` is
the full normalized message: `body`, `body_plain` (markup stripped),
`from`/`to`/`cc`/`bcc`/`reply_to`, `role` (well-known folder: `inbox`,
`sentitems`, …), `internet_message_id`, `thread_id`, `has_attachments`, and for
new mail the `attachments` list (id, name, mime_type, size) so no follow-up
call is needed. For chat, `"message"` (a `ChatMessage`) and `"chat"` (a
`Chat`) carry the equivalent; `chat_reaction` additionally carries
`"reaction"`, and `"message_ids"` names the affected messages for
`chat_updated`/`chat_deleted`. `webhook.name` is whatever you passed as `name`
when registering the hook.

Chat event names: `chat_received` (inbound), `chat_sent` (sent via the API or
echoed back from the phone), `chat_updated` (edit, mark-read, or a chat's
`archived`/`muted` flags changed), `chat_reaction`, `chat_deleted`.
`account_status` covers both providers — connection lost/regained, or the
terminal `CREDENTIALS` state — and, for a chat account, can arrive with no
request of yours in flight at all: the socket can drop or a device can be
unlinked from the phone at any time.

Deliveries carry `X-Outlook-Event`, `X-Outlook-Delivery` (attempt number) and,
when a secret is set, `X-Outlook-Signature: sha256=<hex hmac of the raw body>`.
Delivery is **at-least-once** — dedupe on `(type, email.id)`.

**Retries.** Anything other than a 2xx is a failure. The delivery is written to
SQLite and retried on this schedule after the immediate first attempt:
30s, 2m, 10m, 30m, 2h, 6h, 12h. After the last one it is marked `dead` and
kept, visible via the `deliveries` endpoint. The queue survives restarts.

### Developers, sessions and API keys

| Method | Path | Notes |
|---|---|---|
| `GET`/`POST` | `/signup` | HTML form (`application/x-www-form-urlencoded`). `303` + `um_session` cookie on success; `409` on a duplicate email |
| `GET`/`POST` | `/login` | HTML form. `303` + `um_session` cookie on success |
| `POST` | `/logout` | Clears the session |
| `GET` | `/api/v1/me` | The signed-in developer plus `"auth": "session"` or `"auth": "api_key"` |
| `GET` | `/api/v1/api-keys` | |
| `POST` | `/api/v1/api-keys` | `{"name": "..."}` → `201` with the full key under `"key"` — shown once, never again |
| `DELETE` | `/api/v1/api-keys/{id}` | |

Everything a developer owns — accounts, webhooks, mail — is scoped to that
developer; a request that names another developer's resource gets a plain
`404`, not a `403`, so existence isn't leaked across tenants. **Managing API
keys is session-only**: `POST`/`DELETE /api/v1/api-keys*` reject a bearer API
key with `403 session_required`, so a leaked key alone can never mint or
revoke keys — you have to be signed into the dashboard.

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

**The provider seam.** `internal/provider` defines the contracts a mail
backend implements — an `Authenticator` attaches accounts and keeps grants
alive, a `Mailbox` reads and writes messages, a `Pusher` is optional (a
provider without one simply does not implement it, and the core polls
instead) — and the chat counterparts a linked-device backend implements
instead: a `Linker` runs the QR pairing session, a `Chatter` reads and writes
chats/messages. `Provider.Kind()` (`"mail"` or `"chat"`) is what the API and
`internal/chatsync` branch on; nothing needs a provider's name to know which
set of routes and events apply to its accounts. The registry resolves an
account's provider from the `provider` field stored on it, so adding a
backend touches no code above that line.

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

**A mail provider:**

1. Create `internal/provider/<name>/`.
2. Implement `provider.Authenticator`, `provider.Mailbox`, and — only if the
   backend can push — `provider.Pusher`.
3. Map the backend's failures onto the shared sentinels: `ErrReauthRequired`,
   `ErrCursorExpired`, `ErrNotFound`, `ErrSubscriptionExists`. The core changes
   behaviour based on these, so a provider that never returns `ErrCursorExpired`
   will stall permanently the first time a cursor goes stale.
4. Add it to the registry in [`cmd/server/main.go`](cmd/server/main.go).

**A chat provider** (see `internal/provider/whatsapp` for a worked example):

1. Create `internal/provider/<name>/`.
2. Implement `provider.Linker` (runs a pairing session and yields a stream of
   `LinkCode`s and a terminal `LinkResult`) and `provider.Chatter` (chats,
   messages, send/edit/delete/react, `StartDirect`, `MarkRead`, `Logout`).
3. `internal/chatsync` owns reconnect/backoff, capacity, and turning inbound
   provider events into the store + dispatcher; the adapter itself holds no
   policy — see the doc comment atop `internal/provider/whatsapp/whatsapp.go`.
4. Wire it conditionally in `cmd/server/main.go` (WhatsApp only constructs and
   registers its provider `if cfg.WhatsAppEnabled`), following whatever
   config gate makes sense for the backend.

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

- **WhatsApp device keys are stored unsealed** in whatsmeow's own SQLite
  tables, unlike OAuth refresh tokens (AES-256-GCM sealed). Run behind
  disk-level encryption if you enable it for anything beyond local testing —
  see "Setup" §6.
- **WhatsApp is text-only, with no media and no history sync.** A media
  message arrives as `kind: "unsupported"`; none can be sent. A newly linked
  device only sees messages sent after pairing.
- **WhatsApp pairing is QR-only** — there is no phone-number pairing code
  flow.
- **WhatsApp accounts cap at `WHATSAPP_MAX_ACCOUNTS`** live sockets
  process-wide (default 200); beyond that, connecting or reconnecting an
  account gets `503 capacity`.
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
- **The event queue is bounded.** When it is full, `Emit` pushes back on the
  producer for up to 5 s and then drops the event; the running total is
  exposed as `dropped_events` on `/healthz` and logged as `dropped_total`.
- **Scope listing is not incremental** for Outlook — the folder tree is relisted
  each round. Cheap at mailbox scale, but it is a full listing, not a delta.
- **Open signup, no password reset, no login rate limiting.** Anyone can
  create a developer account; a forgotten password has no recovery path, and
  repeated bad logins aren't throttled.
- **Pre-tenancy databases are refused; there is no migration.** A database
  created before multi-tenancy shipped fails to open with `database <path>
  predates multi-tenancy; delete it (and its -wal/-shm files) and reconnect
  your mailboxes` — set up a fresh `DB_PATH` and reconnect mailboxes under it.
- **DNS-rebinding/hostname SSRF is not prevented.** Webhook, `notify_url`, and
  redirect URLs are checked as written: literal private, loopback, link-local,
  and multicast addresses and `localhost` are rejected, but a hostname that
  resolves to an internal address is not, and nothing re-checks the address the
  request actually dials.
- **Login-CSRF on `POST /login`.** The login form carries no CSRF token, so a
  third-party page can sign a visitor into an attacker-controlled developer
  account. Session-authenticated writes are defended separately (SameSite=Lax
  plus a JSON content-type requirement); this is the sign-in step itself.

---

## Logging

Every request gets an `X-Request-Id`: the incoming header value if the caller
sent one (capped at 64 characters), otherwise a generated one — either way
it's echoed back on the response and attached to every log line the request
produces, so a caller-supplied trace ID ties their logs to the server's.

Set `DEBUG=1` (or any non-empty value) for exhaustive debug logging: request
bodies, auth resolution (`auth: bearer present, resolving api key`,
`auth: resolved`, `auth: api key rejected`, `auth: session rejected`, etc.),
sync scope and message decisions (`decision=new`, `decision=suppressed`, …),
webhook delivery decisions (`decision=delivered`, `decision=scheduled retry`,
`decision=dead`), and token refresh decisions. Without it the server logs at
`INFO` only.

**Secrets are always redacted, even at DEBUG.** Request bodies logged at
DEBUG are passed through a redactor first: any JSON field whose key looks like
`password`, `secret`, `token`, `key`, `code`, `verifier`, `cookie`,
`authorization`, `client_state` or `session` (substring match, so it
over-redacts on purpose) is replaced with `[redacted]`. Elsewhere the code
simply never logs the sensitive value in the first place — a minted API key
is logged by its safe 12-character prefix, never in full; a session token is
never logged at all. The net effect: passwords, full API keys, session cookie
values, and `Authorization` header values do not appear in the log at any
level.

The same discipline applies to chat: a QR pairing code is only ever held
in-memory on the connect page's poll response, never logged (`GET
/connect/{state}/qr` carries no request body for `logBody` to touch in the
first place). `logx.Redact`'s content-key rule (`"text"`, `"body"`) reduces a
chat message's text to a byte count rather than logging it, and
`internal/chatsync`/`internal/provider/whatsapp` identify a chat by its
account id or a one-way digest (`logx.Digest`) rather than logging a phone
number or chat id in the clear.

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

`scripts/smoke.sh` exercises the running server end to end against a real
mailbox: it signs up (or logs in) as `SMOKE_EMAIL`/`SMOKE_PASSWORD`, mints an
API key, and drives the mail API, including one send. It needs a mailbox
already connected under that developer — log in as `SMOKE_EMAIL` on the
dashboard and click **Connect account** first — so it isn't run in CI; it's a
manual, credentialed check you run locally before shipping a change that
touches the mail path.
