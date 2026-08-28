# WhatsApp provider — design

Date: 2026-08-25
Status: approved design, awaiting implementation plan
Depends on: multi-tenancy (`docs/superpowers/specs/2026-08-25-multi-tenancy-design.md`, shipped on `feature/multi-tenancy`)

## Goal

Add WhatsApp as a second provider so a developer's end users can link their own
WhatsApp number (personal or Business app) and the developer can read and send
1:1 and group messages through the same API, with real-time webhooks — the
"session-based" model Unipile uses, implemented on whatsmeow.

Decisions already made with the user:

| Decision | Choice |
|---|---|
| Approach | **A** — capability-based provider, new `chats` resource family; mail code untouched except the connect branch |
| v1 scope | 1:1 **and group** text messages both ways; reactions; edit/delete of own messages; read receipts. **No media** (arrives as `unsupported`), **no history import**, **QR linking only** (no pairing code) |
| Identity | Attendees carry a stable id plus `phone` (E.164) when resolvable and push `name` |
| Disclosure | Connect page shows a brief disclosure and a consent checkbox before the QR |
| Protocol library | `go.mau.fi/whatsmeow` (pure Go); proven by the spike (QR link, history sync, receive, send, receipts, 1 s round-trip) |
| Stack | Unchanged: stdlib `net/http`, raw SQL over `modernc.org/sqlite`, `log/slog` |

Non-goals (follow-ups): media pipeline, history import, pairing-code linking,
presence/typing, address-book contacts, broadcast lists, Business catalog,
per-account outbound rate limits, running the chat runtime as its own process.

---

## 1. Data model

All chat tables hang off `accounts` (which carries `developer_id`), so tenancy
inherits through `account_id`; no new ownership columns.

```sql
CREATE TABLE chats (
  account_id      TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  id              TEXT NOT NULL,            -- provider chat id (JID: 9188…@s.whatsapp.net | 1203…@g.us)
  kind            TEXT NOT NULL,            -- 'direct' | 'group'
  name            TEXT NOT NULL DEFAULT '', -- group subject or contact push-name
  unread_count    INTEGER NOT NULL DEFAULT 0,
  last_message_at INTEGER,
  archived        INTEGER NOT NULL DEFAULT 0,
  muted           INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (account_id, id)
);
CREATE INDEX chats_by_activity ON chats(account_id, last_message_at DESC);

CREATE TABLE attendees (
  account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  id         TEXT NOT NULL,                 -- stable id: phone JID when known, else LID
  lid        TEXT NOT NULL DEFAULT '',      -- privacy id when both are known (internal, never in API)
  phone      TEXT NOT NULL DEFAULT '',      -- E.164 when resolvable
  name       TEXT NOT NULL DEFAULT '',      -- push name
  is_self    INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (account_id, id)
);

CREATE TABLE chat_members (
  account_id  TEXT NOT NULL,
  chat_id     TEXT NOT NULL,
  attendee_id TEXT NOT NULL,
  role        TEXT NOT NULL DEFAULT '',     -- 'admin' | ''
  PRIMARY KEY (account_id, chat_id, attendee_id),
  FOREIGN KEY (account_id, chat_id) REFERENCES chats(account_id, id) ON DELETE CASCADE
);

CREATE TABLE chat_messages (
  account_id     TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  id             TEXT NOT NULL,             -- provider message id
  chat_id        TEXT NOT NULL,
  sender_id      TEXT NOT NULL,             -- attendee id
  is_from_me     INTEGER NOT NULL DEFAULT 0,
  kind           TEXT NOT NULL,             -- 'text' | 'unsupported'
  text           TEXT NOT NULL DEFAULT '',
  quoted_id      TEXT NOT NULL DEFAULT '',
  sent_at        INTEGER NOT NULL,
  edited_at      INTEGER,
  deleted        INTEGER NOT NULL DEFAULT 0,
  status         TEXT NOT NULL DEFAULT '',  -- own messages: 'sending'|'sent'|'delivered'|'read'
  reactions_json TEXT NOT NULL DEFAULT '[]',-- [{attendee_id, emoji, at}]
  PRIMARY KEY (account_id, id)
);
CREATE INDEX chat_messages_by_chat ON chat_messages(account_id, chat_id, sent_at DESC, id);

CREATE TABLE chat_sessions (                -- maps an account to its linked device; holds no secrets
  account_id TEXT PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
  provider   TEXT NOT NULL,
  device_jid TEXT NOT NULL,                 -- whatsmeow device JID (e.g. 9188…:22@s.whatsapp.net)
  updated_at INTEGER NOT NULL
);
-- Device credentials (identity keys, Signal sessions, pre-keys, app-state keys)
-- live in whatsmeow's own `whatsmeow_*` tables in the same SQLite file, one
-- device per account keyed by device_jid, schema managed by whatsmeow's
-- Container.Upgrade at startup. See "Custody" in §2.

CREATE TABLE idempotency_keys (             -- POST …/messages replay protection
  developer_id TEXT NOT NULL REFERENCES developers(id) ON DELETE CASCADE,
  key          TEXT NOT NULL,
  response     BLOB NOT NULL,
  created_at   INTEGER NOT NULL,
  PRIMARY KEY (developer_id, key)
);
```

`accounts` gains, additively: `kind TEXT NOT NULL DEFAULT 'mail'` (`'mail'|'chat'`).
For chat accounts the existing `email` column holds the E.164 phone; the API
exposes it as `identifier` on every account and keeps `email` on mail accounts.
`UNIQUE(developer_id, email)` therefore already gives "reconnecting the same
number keeps the same `account_id`". Status values unchanged: `OK`,
`CREDENTIALS` (device unlinked / logged out), `DISCONNECTED`.

Media in v1: the adapter stores `kind='unsupported'` with `text` set to a
label (`[image]`, `[document]`, `[audio]`, `[sticker]`, `[location]`, …).

Edits/deletes update in place (`edited_at`; `deleted=1` with `text=''`).
Reactions are merged into `reactions_json`, one entry per attendee; an empty
emoji removes.

Migration: a pre-tenancy database is still refused; this feature adds tables
and one `ALTER TABLE accounts ADD COLUMN kind …` migration guarded by the
existing "duplicate column" rule (the multi-tenancy DB is current production).

---

## 2. Architecture by pattern

| Pattern | Where | Purpose |
|---|---|---|
| Plugin / Strategy | `provider.Registry` — providers by name; per-account selection | Second strategy without branching in the API |
| Interface segregation (capabilities) | `Provider` exposes optional `Auth()`/`Linker()`, `Mailbox()`/`Chat()`, `Push()` — each may be nil | Callers test capabilities, never provider names |
| Adapter / anti-corruption | `internal/provider/whatsapp` — sole importer of whatsmeow; translates JIDs, protobufs, receipts to `model.*` | Protocol churn is contained like Graph is in `outlook` |
| Factory | `whatsapp.New(sessions, log)`; wiring in `main.go` | Construction in one place, fakes in tests |
| Template method | `/connect/{state}` — common flow with a provider-specific consent/auth step | One connect flow, one pending state, one `notify_url` |
| State machine | `LinkSession` (`idle → showing_qr → paired\|expired\|cancelled`); account status (`OK ⇄ CREDENTIALS → DISCONNECTED`) | Table-testable transitions |
| Observer / pub-sub | `EventSink` — adapter publishes, runtime subscribes, dispatcher fans out | Adapter never touches store or webhooks |
| Supervisor (actor per connection) | `chatsync.Runtime` — one goroutine + conn per chat account, backoff, health | Isolates the long-lived-socket risk; splittable into its own process later |
| Command | Outbound `SendText/React/Edit/Delete/MarkRead` | Uniform validation, logging, idempotency |
| Repository | `store` chat methods; `SessionStore` | Raw SQL behind intent-revealing methods |
| Custodian | `accounts.Manager` owns the account ↔ device mapping and deletion; device secrets are held by whatsmeow's store | One owner of the *lifecycle* of credentials; see Custody below |
| Facade | `Runtime.Start/Attach/Detach/Health/Wait` | Internals hidden from API and `main` |

Deliberately not: a generic message abstraction across mail and chat; an
in-process event bus beyond `EventSink → Runtime → Dispatcher`; an ORM.

Package map (new in bold):

```
internal/provider/             contracts (+ Linker, Chatter, EventSink, ChatConn; Provider.Kind/Linker/Chat)
internal/provider/outlook/     unchanged
**internal/provider/whatsapp/** adapter over whatsmeow: Linker + Chatter
**internal/provider/providertest/** FakeChat used by every test layer
**internal/chatsync/**         Runtime (supervisor/facade), per-account actor, EventSink → store + dispatcher
internal/accounts/             + ConnectLinked, DeleteLinked, DeviceJID
internal/store/                + chats/attendees/members/messages/sessions/idempotency repositories
internal/api/                  + connect QR step, /api/v1/chats…, /api/v1/attendees…, chat viewer, docs
internal/events/               unchanged; new event names in model.KnownEvent
cmd/server/                    wiring + shutdown order
```

### Contracts (`internal/provider`)

```go
type Provider interface {
    Name() string
    Kind() string          // "mail" | "chat"
    Auth() Authenticator   // nil for chat providers
    Linker() Linker        // nil for OAuth providers
    Mailbox() Mailbox      // nil for chat providers
    Chat() Chatter         // nil for mail providers
    Push() Pusher          // nil for chat providers
}

type Identity struct{ Identifier, Email, Name string } // Identifier: email or E.164

type Linker interface {
    StartLink(ctx context.Context) (LinkSession, error)
}
type LinkCode struct{ Code string; ExpiresAt time.Time }
type LinkResult struct{ Identity Identity; DeviceJID string; Err error }
type LinkSession interface {
    Codes() <-chan LinkCode
    Result() <-chan LinkResult // resolves exactly once
    Close()
}

type Chatter interface {
    Connect(ctx context.Context, accountID, deviceJID string, sink EventSink) (ChatConn, error)
    Forget(ctx context.Context, deviceJID string) error // delete stored device credentials
    Chats(ctx context.Context, accountID string) ([]model.Chat, []model.Attendee, []model.ChatMember, error)
    SendText(ctx context.Context, accountID, chatID, text, quotedID string) (SendResult, error)
    StartDirect(ctx context.Context, accountID, phoneE164 string) (chatID string, err error)
    React(ctx context.Context, accountID, chatID, messageID, emoji string) error
    Edit(ctx context.Context, accountID, chatID, messageID, text string) error
    Delete(ctx context.Context, accountID, chatID, messageID string) error
    MarkRead(ctx context.Context, accountID, chatID string, messageIDs []string) error
    Logout(ctx context.Context, accountID string) error // unlink the device on the phone
}
type ChatConn interface {
    Close() error
}
type EventSink interface {
    Message(accountID string, m model.ChatMessage, chat model.Chat, sender model.Attendee)
    Receipt(accountID, chatID string, messageIDs []string, status string) // delivered | read
    Reaction(accountID, chatID, messageID string, r model.Reaction)
    Edited(accountID, chatID, messageID, text string, at time.Time)
    Deleted(accountID, chatID, messageID string)
    Disconnected(accountID, reason string, loggedOut bool)
}
```

The syncer and subscription loops skip accounts whose provider `Kind() != "mail"`.

### Linking flow

1. `POST /api/v1/hosted-auth {"provider":"WHATSAPP", …}` — same pending state
   (developer id, redirects, `notify_url`, connect-time webhook, expiry), same
   single-use `/connect/{state}` link. `provider` is required once two
   providers are registered (existing rule). Returns `503 capacity` when the
   runtime is at `WHATSAPP_MAX_ACCOUNTS`.
2. `GET /connect/{state}` for a `Linker` provider renders the disclosure and a
   consent checkbox; "Show QR code" is enabled only after consent. Consent is
   recorded on the pending state (`consented_at`) via `POST /connect/{state}/consent`.
3. The page polls `GET /connect/{state}/qr` every 2 s →
   `{status: "waiting"|"paired"|"expired"|"failed", png_base64?, expires_in?}`.
   The link session starts lazily on the first poll after consent, lives in
   memory keyed by state, is bounded to 3 minutes, and is swept when done.
   Without consent the endpoint returns `409 consent_required`. The QR is
   never exposed by any `/api/v1` route.
4. On `Result`: `accounts.ConnectLinked(ctx, developerID, provider, identity, device)`
   creates/reuses the account under the minting developer (`kind='chat'`,
   `email`=E.164), seals and stores the device, marks `OK`, fires `notify_url`
   `{status:"CREATED", account_id, identifier, provider}`, binds the
   connect-time webhook, calls `Runtime.Attach`. The page redirects to
   `success_redirect_url?account_id=…` or shows the confirmation.
5. Timeout/cancel: `notify_url` `{status:"FAILED", error:"link_timeout"|"link_cancelled"}`;
   redirect to `failure_redirect_url` when set.

### Custody and revocation

- **Decision (user):** whatsmeow keeps device credentials in its own
  `whatsmeow_*` tables inside our SQLite database (`sqlstore.NewWithDB` on the
  same `*sql.DB`, `Container.Upgrade` at startup, `GetDevice(jid)` per
  account). They are **not** row-sealed with `TOKEN_ENCRYPTION_KEY` — unlike
  mail refresh tokens — because whatsmeow mutates Signal session state
  continuously and exposes no serialize-to-bytes API. At-rest protection is
  DB/disk level (Cloud SQL / disk encryption in deployment). This is written
  into README "Known limits" and the ops section. `chat_sessions` therefore
  stores only `device_jid`; the `Linker`/`Chatter` contracts drop the `[]byte`
  device parameters: `LinkResult{Identity, DeviceJID}`,
  `Connect(ctx, accountID, deviceJID, sink)`, no `ChatConn.Device()` and no
  `EventSink.DeviceChanged`.
- `accounts.Manager` remains the custodian of the *mapping and lifecycle*:
  `ConnectLinked` records `device_jid`, `DeleteLinked` removes the row and
  calls `Chatter.Forget(deviceJID)` (→ `Container.DeleteDevice`) so no
  orphaned device keys remain after an account is deleted or logged out.
- `LoggedOut` → status `CREDENTIALS`, `account_status` event, session row
  deleted, actor stopped. Re-link = new connect link (same `account_id`).
- `DELETE /api/v1/accounts/{id}` on a chat account: `Runtime.Detach`,
  `Chatter.Logout` (best effort), then the existing cascade.

---

## 3. Chat runtime and events (`internal/chatsync`)

```
Runtime.Start(ctx)        boot: Attach every account with kind='chat' and status OK
Runtime.Attach(accountID) idempotent; spawns an actor
Runtime.Detach(accountID) close conn, stop actor
Runtime.Health() []AccountHealth{account_id, state, since, last_event_at, reconnects, last_error}
Runtime.Wait()            drain on shutdown
```

Actor states: `connecting → connected → (disconnected → backoff → connecting)* → stopped`.

- Connect: `accounts.DeviceJID(accountID)` → `Chatter.Connect(…, deviceJID, sink)` → `Chatter.Chats`
  roster upsert (chats, attendees, members; no messages).
- Reconnect on `Disconnected{loggedOut:false}` with jittered exponential backoff
  1s, 2s, 4s … capped at 5 min; counter resets after 10 min connected. After
  30 consecutive failures the account is marked `CREDENTIALS` with
  `reason: "unreachable"` and an `account_status` event, and the actor stops
  (an operator/integrator re-links).
- `Disconnected{loggedOut:true}` is terminal as in §2 (`DeleteLinked` forgets the device).
- Health feeds the dashboard card and `GET /api/v1/accounts/{id}.connection`.

Event mapping — every handler is idempotent on `(account_id, message id)`:

| Sink call | Store | Webhook event |
|---|---|---|
| `Message` inbound | upsert message, sender attendee, chat (`last_message_at`, `unread_count++`) | `chat_received` (first-seen only) |
| `Message` own (from phone or our echo) | upsert `status='sent'` | `chat_sent` — **only** for phone-originated sends; API sends emit at send time and the echo is deduped by id |
| `Receipt` | own messages' `status`; inbound `read` → `unread_count=0` | `chat_updated` `{message_ids, status}` |
| `Reaction` | merge/remove in `reactions_json` | `chat_reaction` `{message_id, reaction}` |
| `Edited` | `text`, `edited_at` | `chat_updated` `{message_id, change:"edited"}` |
| `Deleted` | `deleted=1`, `text=''` | `chat_deleted` `{message_id}` |
| `Disconnected` | status/health | `account_status` on logout or backoff exhaustion |

Outbound `POST …/messages`: insert row `status='sending'` with a temporary id,
call `SendText`, then rewrite the id to the provider id and `status='sent'`;
on failure delete the row and return `502 provider_error`. `chat_sent` fires
at send time. `React/Edit/Delete/MarkRead` mutate the store only after the
provider acknowledges.

Ordering: one goroutine per actor processes events in arrival order and blocks
on the socket if the dispatcher's bounded queue is full (whatsmeow replays on
reconnect; handlers are idempotent).

Logging (spec §5 of the tenancy design applies): every actor line carries
`component=chatsync`, `account_id`, `developer_id`, `conn_id`; DEBUG logs each
inbound event (`kind`, `chat_id`, `message_id`, `text_bytes` — never text),
each decision (`new|replay|own-echo`), each command and result; INFO only for
connect/disconnect/backoff transitions. Never logged: message text, phone
numbers of attendees, device keys, QR codes.

---

## 4. API surface and UI

All routes under the developer middleware; `?account_id` required; cross-tenant → 404.

| Method | Path | Body / query | Response |
|---|---|---|---|
| `GET` | `/api/v1/chats` | `account_id`, `kind=direct\|group`, `unread=true`, `q`, `limit≤200`, `offset` | `{items:[Chat], limit, offset}` by `last_message_at` desc |
| `POST` | `/api/v1/chats` | `{account_id, phone \| attendee_id, text}` | `201 {chat, message}` |
| `GET` | `/api/v1/chats/{id}` | `account_id` | `Chat` + `members` |
| `PATCH` | `/api/v1/chats/{id}` | `{read?: true, archived?, muted?}` | `Chat` |
| `GET` | `/api/v1/chats/{id}/messages` | `account_id`, `before` (message id), `limit≤100` | `{items:[ChatMessage], next_before}` (keyset on `sent_at,id`) |
| `POST` | `/api/v1/chats/{id}/messages` | `{text, quoted_message_id?}` + optional `Idempotency-Key` header | `201 ChatMessage` |
| `GET` | `/api/v1/chats/{id}/messages/{mid}` | `account_id` | `ChatMessage` |
| `PATCH` | `/api/v1/chats/{id}/messages/{mid}` | `{text}` | `ChatMessage` |
| `DELETE` | `/api/v1/chats/{id}/messages/{mid}` | `account_id` | `204` |
| `PUT` | `/api/v1/chats/{id}/messages/{mid}/reaction` | `{emoji}` (`""` removes) | `204` |
| `GET` | `/api/v1/attendees` | `account_id`, `q`, `limit`, `offset` | `{items:[Attendee]}` |
| `GET` | `/api/v1/attendees/{id}` | `account_id` | `Attendee` |

Shapes:

```
Chat        {id, account_id, kind, name, unread_count, last_message_at?, archived, muted, members?:[Attendee]}
Attendee    {id, phone?, name, is_self}
ChatMessage {id, account_id, chat_id, sender: Attendee, is_from_me, kind, text, quoted_message_id?,
             sent_at, edited_at?, deleted, status?, reactions:[{attendee_id, emoji, at}]}
Account     + kind, identifier, connection?: {state, since, reconnects, last_error?}
```

`Idempotency-Key`: scoped to the developer, stored 24 h, replays return the
stored `201` body; a different body under the same key → `409 idempotency_conflict`.

Events: `chat_received`, `chat_sent`, `chat_updated`, `chat_reaction`,
`chat_deleted` (+ existing `account_status`), same envelope, HMAC, retries,
`deliveries` endpoint. Payload fields: `message?`, `chat?`, `message_ids?`,
`status?`, `change?`, `reaction?`. Event filters and `"*"` include them.

Errors (new codes): `consent_required` (409), `capacity` (503),
`idempotency_conflict` (409), `unsupported_for_kind` (400 — e.g. `resync` on a
chat account, `chats` on a mail account), `not_own_message` (403 for edit/delete
of others' messages — the one legitimate 403, since ownership within a chat is
not a tenancy boundary).

Pages:
- `/connect/{state}` Linker variant: disclosure, consent checkbox, self-refreshing QR, status, redirect/confirmation, expired state.
- Dashboard: provider picker on Connect when >1 provider; account cards show `kind`, identifier (phone masked as `+91 88••• •855`), connection state; **Reconnect** replaces Resync for chat accounts.
- `/chat?account_id=` viewer: chat list + message pane + send box, session-gated, same style as `/mail`.
- `/docs` and `/llms.txt`: Chat section; endpoint tables regenerate from `apiRoutes`.

---

## 5. Operations and risk

- One socket per connected chat account inside the API process. Shutdown order: HTTP → `Runtime.Wait` (≤10 s) → dispatcher drain. Cloud Run Phase 1 (single always-on instance) remains valid.
- Config: `WHATSAPP_ENABLED` (default `true`), `WHATSAPP_MAX_ACCOUNTS` (default 200), `WHATSAPP_DEVICE_NAME` (default "Unified Messaging"). No provider credentials.
- Failure modes: transient drops → backoff + replay dedup; device removed → `CREDENTIALS`; protocol break → all chat accounts in `error` state, mail unaffected, fix = library bump; banned number → `loggedOut` reason surfaced in `account_status`; restart → auto-reattach from `chat_sessions`.
- README "Known limits" also states that WhatsApp device keys are stored unsealed in the database (DB-level encryption required in deployment).
- Disclosure, in README, `/docs` and `/llms.txt`: session-based linked-device access, not an official Meta API; may break on protocol changes; linked numbers carry a ban risk under WhatsApp's terms; the developer owns their users' consent. The connect page carries the user-facing version.

---

## 6. Testing

TDD per task; real SQLite; `Routes()`-level API tests; the cross-tenant
isolation table must cover every new route.

| Layer | Method | Key cases |
|---|---|---|
| `providertest.FakeChat` | scripted `Linker`+`Chatter` | shared by all layers |
| store | real DB | idempotent message upsert; reaction merge/remove; keyset paging stable under inserts; unread maintenance; cascades; idempotency key round-trip |
| accounts (custodian) | real store + fake Chatter | `ConnectLinked` creates under the right developer and reuses on same number and records `device_jid`; `DeleteLinked` removes the mapping and calls `Forget` |
| link session | `Routes()` + fake | consent gate (409); codes rotate; pairing → account under the minting developer, `notify_url` CREATED, connect-time webhook bound; timeout → FAILED; consumed state → 404 |
| runtime | fake + fake clock | attach only OK chat accounts; backoff sequence; logout terminal path forgets the device; replay/own-echo dedup; ordering; backoff exhaustion → CREDENTIALS |
| event mapping | fake + real dispatcher + httptest receiver | each sink call → exactly one mapped event with documented payload; no `lid`/foreign phone in payload; HMAC intact |
| API | `Routes()` + fake | happy paths, 400s, isolation table 404s, send `sending→sent`, idempotency replay/conflict, provider errors → 502, `not_own_message` 403 |
| pages/docs | `Routes()` | provider picker; card connection state; `/chat` gated; `/docs` + `/llms.txt` cover every route |
| logging | `logx.Capture` | `component=chatsync`, `conn_id=`; no text/phone/device keys in any line |

Manual checklist before merge (real number): link, receive, send, react, edit,
delete, unlink on phone → `CREDENTIALS` + webhook, restart → auto-reconnect.

---

## 7. Follow-ups

Media (encrypted download/upload + attachments endpoint), history import,
pairing-code linking, presence/typing, phone address-book contacts, outbound
rate limits, splitting `chatsync` into its own process (Approach C), Teams /
Telegram on the same `Chatter` contract.
