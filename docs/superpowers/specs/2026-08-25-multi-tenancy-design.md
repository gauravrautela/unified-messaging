# Multi-tenancy with developer login — design

Date: 2026-08-25
Status: approved design, awaiting implementation plan

## Goal

Turn the single-tenant POC into a multi-tenant service where a **developer**
(integrator) signs up, logs in to the dashboard, mints revocable API keys, and
connects end-user mailboxes that only they can see. Alongside, make the whole
codebase exhaustively debuggable through structured logs.

Decisions already made with the user:

| Decision | Choice |
|---|---|
| Tenant | Developer / integrator (Unipile model). End users only see the hosted connect page. |
| Login | Email + password (bcrypt). |
| API keys | Multiple named keys per developer, revocable, shown once. |
| Existing data | Fresh start. Pre-tenancy databases are refused at startup with a clear message. |
| Signup | Open. |
| Scoping approach | `developer_id` column on the owning tables; one middleware resolves the caller. |
| Stack | Unchanged: `net/http` ServeMux + raw SQL over `modernc.org/sqlite`. Gin/ORM is a possible later step, not part of this. |
| Logging | Exhaustive structured logging of inputs, decisions, and outcomes; secrets never logged. |

## Non-goals

Password reset, email verification, login rate limiting, admin views,
per-key scopes, org/teams, billing. Each is called out in the README's
limitations list.

---

## 1. Data model

New tables:

```sql
CREATE TABLE developers (
  id            TEXT PRIMARY KEY,            -- dev_<hex>
  email         TEXT NOT NULL UNIQUE,        -- stored lower-cased, trimmed
  password_hash TEXT NOT NULL,               -- bcrypt, cost 12
  name          TEXT NOT NULL DEFAULT '',
  created_at    INTEGER NOT NULL
);

CREATE TABLE api_keys (
  id           TEXT PRIMARY KEY,             -- key_<hex>
  developer_id TEXT NOT NULL REFERENCES developers(id) ON DELETE CASCADE,
  name         TEXT NOT NULL,
  prefix       TEXT NOT NULL,                -- first 12 chars of the key, for display
  hash         TEXT NOT NULL UNIQUE,         -- hex(sha256(full key))
  created_at   INTEGER NOT NULL,
  last_used_at INTEGER,
  revoked_at   INTEGER                       -- soft revoke; row kept
);
CREATE INDEX api_keys_by_developer ON api_keys(developer_id);

CREATE TABLE sessions (
  id           TEXT PRIMARY KEY,             -- 32 random bytes, base64url; the cookie value
  developer_id TEXT NOT NULL REFERENCES developers(id) ON DELETE CASCADE,
  created_at   INTEGER NOT NULL,
  expires_at   INTEGER NOT NULL
);
CREATE INDEX sessions_by_developer ON sessions(developer_id);
```

Ownership on existing tables:

| Table | Change |
|---|---|
| `accounts` | `developer_id TEXT NOT NULL REFERENCES developers(id) ON DELETE CASCADE`. Unique index `accounts_email` becomes `UNIQUE(developer_id, email)`: the same mailbox may be connected by two developers as two distinct accounts with separate tokens. |
| `webhooks` | `developer_id` (same FK). `account_id = ''` now means "all of this developer's accounts". |
| `oauth_states` | `developer_id` (same FK). A connect link is minted by a developer; the callback creates the account under them. |

Everything below `accounts` (`tokens`, `folders`, `sync_state`, `emails`,
`subscriptions`, `webhook_deliveries`) is unchanged and inherits ownership
through `account_id`.

`store.AccountIDByEmail(email)` becomes `AccountIDByEmail(devID, email)`;
`accounts.Manager.Connect` takes the developer id.

### API key format

`um_` + 40 characters from `[A-Za-z0-9]` (≈238 bits). Stored only as
SHA-256: keys are high-entropy random strings, so a slow hash buys nothing and
would cost ~250 ms per API request. `prefix` is the first 12 characters
(`um_` + 9) and is what the dashboard lists alongside the name.

### Fresh start

The schema is written clean (no `ALTER` migrations for the new columns).
`store.Open` inspects `PRAGMA table_info(accounts)`; if the table exists
without `developer_id` it returns

```
database <path> predates multi-tenancy; delete it (and its -wal/-shm files) and reconnect your mailboxes
```

and the process exits non-zero. It never deletes on its own. The existing
additive migrations for `webhooks.account_id`, `webhooks.name`, and
`oauth_states.webhook_json` are removed as dead code (any DB old enough to
need them is refused anyway).

---

## 2. Authentication and request resolution

### Resolver

Middleware `withDeveloper` wraps every `/api/v1/*` route. It tries, in order:

1. `Authorization: Bearer um_…` or `X-API-Key` → `sha256` → `api_keys` where
   `revoked_at IS NULL` → developer. `last_used_at` is updated at most once
   per minute per key.
2. Cookie `um_session` → `sessions` where `expires_at > now` → developer. If
   more than 24 h of the TTL has elapsed, `expires_at` is pushed forward
   (sliding expiry).
3. Neither → `401 {"error":{"code":"unauthorized"}}`.

The resolved `model.Developer` plus the credential kind (`api_key` /
`session`) are placed in the request context; handlers call
`developerFrom(ctx)`. Handlers never see raw keys or cookies.

### Session cookie

`um_session`; `HttpOnly`; `SameSite=Lax`; `Path=/`; `Secure` when the request
is TLS or `PUBLIC_BASE_URL` starts with `https://`. TTL `SESSION_TTL_DAYS`
(default 30). CSRF: `SameSite=Lax` blocks cross-site form POSTs from carrying
the cookie, and every state-changing JSON endpoint requires
`Content-Type: application/json`, which a cross-origin HTML form cannot send.
The HTML form endpoints (`/login`, `/signup`, `/logout`) are not
cookie-authenticated actions on data, so they need no token.

### Passwords

bcrypt cost 12. Signup: email must contain `@` and a dot after it, password
≥ 10 characters. Login and signup errors are uniform: "invalid email or
password" / "could not create account" — no user enumeration through error
text or timing beyond what bcrypt itself leaks (a dummy hash is compared when
the email is unknown so both paths cost the same).

### Ownership enforcement

Every path that resolves an account, webhook, or delivery does so **with the
developer id in the SQL** (`WHERE developer_id = ? AND id = ?`). A miss is a
**404**, never 403, so ids are not enumerable across tenants.

Callers that legitimately have no developer keep explicitly named unscoped
store methods: `ListAllAccounts()` (syncer poll loop), `ListWebhooksFor(accountID)`
(dispatcher), `GetSubscription(id)` (push handler, via the syncer). Unscoped access is a
visible choice in the name, never a default.

### Credential-free routes (unchanged)

`/healthz`, `/connect/{state}`, `/oauth/callback`,
`/notifications/{provider}[/lifecycle]`, and the login/signup pages.

---

## 3. API surface and pages

### New endpoints

| Method | Path | Credential | Behaviour |
|---|---|---|---|
| `GET` | `/api/v1/me` | key or session | `{id, email, name, created_at, auth: "api_key"\|"session"}` |
| `GET` | `/api/v1/api-keys` | key or session | `{items: [{id, name, prefix, created_at, last_used_at, revoked_at}]}` — never the hash |
| `POST` | `/api/v1/api-keys` | **session only** | `{"name":"prod"}` → `201 {id, name, prefix, key, created_at}`; `key` is returned exactly once. Using an API key → `403 session_required`. |
| `DELETE` | `/api/v1/api-keys/{id}` | **session only** | soft-revoke → 204; unknown/other-developer → 404 |

### Existing endpoints — scope change only

- `GET /api/v1/accounts` → this developer's accounts.
- `POST /api/v1/hosted-auth` → pending state carries `developer_id`; callback
  creates the account under them, and binds the connect-time webhook to the
  same developer.
- Every `/accounts/{id}…`, `/emails…`, `/folders`, `/threads`, `/drafts…`
  route → 404 for an account the caller does not own.
- `/api/v1/webhooks…` → developer-wide hooks; `/webhooks/{id}/deliveries`
  scoped through the hook's owner.
- Response shapes are unchanged. `developer_id` is never serialised.

### Pages

| Route | Behaviour |
|---|---|
| `GET /signup`, `POST /signup` | form (email, password, name); creates developer, starts session, 303 → `/dashboard` |
| `GET /login`, `POST /login` | form; on success 303 → `?next=` (same-origin path only) or `/dashboard` |
| `POST /logout` | deletes session, clears cookie, 303 → `/login` |
| `GET /dashboard`, `GET /mail` | require session; otherwise 302 → `/login?next=<path>`. The `localStorage` API-key gate is removed; page JS calls `/api/v1/*` with `credentials: "same-origin"`. |

Dashboard additions: header shows developer email + Log out; an **API keys**
panel (name → Create; new key shown once in a copy box; table of keys with
prefix / created / last used / Revoke).

### Config

`API_KEY` is removed from `config.Config`, `.env.example`, and the README.
New optional `SESSION_TTL_DAYS` (default 30). `DEBUG` (existing) now
controls the exhaustive logging level described in §5.

### Tooling

`scripts/smoke.sh` gains a bootstrap phase: `POST /signup` (or `/login` if
the email exists) with a cookie jar, `POST /api/v1/api-keys` via the cookie,
then runs the existing steps with the returned key, and finally proves
isolation by signing up a second developer and asserting 404 on the first
developer's account id.

---

## 4. Package layout

| Package | Change |
|---|---|
| `internal/model` | `Developer{ID, Email, Name, CreatedAt}`, `APIKey{ID, Name, Prefix, CreatedAt, LastUsedAt, RevokedAt}`. `Account` and `Webhook` gain `DeveloperID string \`json:"-"\``. |
| `internal/auth` (new) | `Service` with `Signup`, `Login`, `NewSession`, `SessionDeveloper`, `TouchSession`, `DeleteSession`, `NewAPIKey`, `KeyDeveloper`, `RevokeKey`. Sole importer of `bcrypt`; owns key format and hashing. |
| `internal/store` | `developers.go` (new): developers, sessions, api_keys. `store.go`/`aux.go`: `developer_id` threaded through account, webhook, and oauth-state queries; unscoped variants renamed as in §2. Pre-tenancy check in `Open`. |
| `internal/accounts` | `Connect(ctx, devID, provider, code, verifier)`. |
| `internal/api` | `handlers_auth.go` (new): pages, `/me`, api-keys. `withDeveloper` replaces `requireAPIKey`; `developerFrom(ctx)`; `resolveID(w, dev, id)`. `handleDashboard`/`handleMailPage` gate on session. Request-id + logging middleware (§5). |
| `internal/config` | drop `APIKey`; add `SessionTTL`. |
| `cmd/server` | wire `auth.Service`; log the effective config at startup (secrets redacted). |
| `README.md`, `.env.example` | setup flow becomes: run → `/signup` → create key → use as bearer. |

Dependencies: `golang.org/x/crypto/bcrypt` is the only addition.

---

## 5. Logging

### Principle

At `DEBUG` level a reader can reconstruct, for any request or background
step, **what came in, what was decided and why, and what went out** — from
the log alone. At `INFO` the log stays quiet enough for production.

### Mechanics

- `log/slog` throughout (already in use). `DEBUG=1` selects `slog.LevelDebug`.
- **Request id.** Middleware generates `req_…` (or honours an inbound
  `X-Request-Id`), stores a request-scoped logger in the context, and echoes
  the id in the `X-Request-Id` response header. All handler and store logs
  for that request carry `request_id`. Background work (sync runs, webhook
  deliveries) mints its own `run_id` / `delivery_id` the same way.
- Every log line has: `request_id`/`run_id`, `developer_id` when known,
  `account_id` when known, and a `component` (`api`, `auth`, `store`,
  `syncer`, `events`, `outlook`).

### What gets logged, per component

| Component | INFO | DEBUG |
|---|---|---|
| `api` | one line per request: method, path, status, duration, developer_id, auth kind | request headers of interest (content-type, length, auth *kind* only), decoded request body (see redaction), the resolution steps ("bearer present → key lookup → hit key_…", "no bearer → cookie lookup → miss → 401"), each ownership check and its outcome, the response status and a body summary (type, item count, byte length) |
| `auth` | signup, login success, logout, key created, key revoked | login failure reason (unknown email vs bad password — DEBUG only, never in the response), session touch/slide, key lookup hit/miss/revoked, bcrypt timing |
| `store` | — | every query: operation name, bound params (redacted), rows affected / rows returned, duration |
| `syncer` | run start/end per account with counts (scopes, changed, removed, events emitted), cursor expiry, errors | each scope: cursor in → cursor out, page count, per-message decision (`new → mail_received`, `existed → mail_updated`, `quiet → suppressed`), attachment fetch for events |
| `events` | delivery abandoned (dead), queue-full drops | every attempt: webhook_id, event, attempt no., URL, payload bytes, status/error, decision (`delivered` / `scheduled retry in 2m` / `dead`), retry-loop ticks with due count |
| `outlook` | provider errors with Graph error code | every Graph request: method, URL (query included), status, duration, response bytes; token refresh decisions (`cached, expires in 41m` / `expired → refreshing`); delta page walks with counts |
| `accounts` | connect success (email, account_id, new vs reconnect), status changes | token custody steps (sealed/unsealed, never the token) |

### Redaction — hard rule

Never logged, at any level: passwords, password hashes, session ids, full API
keys, key hashes, OAuth `code`/`state` verifiers, access/refresh tokens,
webhook secrets, `Authorization`/`Cookie` header values, Graph
`clientState`. A single `redact` helper in `internal/logx` masks known field
names in request bodies (`password`, `secret`, `token`, `key`, `code`) and is
applied before any body is logged. API keys appear only as their `prefix`.
Email bodies are logged as byte length, not content.

### Enforcement

- `internal/logx` (new, tiny): `FromContext`, `WithRequestID`, `Redact`, and
  a `NewTestHandler` that captures records for tests.
- Tests assert that the auth and connect flows produce **no** record
  containing the plaintext password, key, session id, or token — the
  redaction rule is itself under test.

---

## 6. Testing

TDD throughout; each change starts with a failing test.

| Layer | Tests |
|---|---|
| `auth` | signup→login round trip; wrong password and unknown email yield the same error; expired session rejected; sliding expiry extends; key resolves, tampered key fails, revoked key fails; stored hash ≠ key and prefix matches. |
| `store` | `GetAccount(devA, acctOfDevB)` → `ErrNotFound`; per-developer `ListAccounts`; same email under two developers; deleting a developer cascades keys, sessions, accounts and their children; pre-tenancy DB refused with the expected message. |
| `api` | 401 without credential; 200 with key; 200 with cookie; revoked key 401; `POST /api-keys` with key → 403; created key returned once and absent from list; hosted-auth pending state carries developer and callback lands the account under them; `/dashboard` without session → 302 `/login`; signup sets `HttpOnly` cookie; **table-driven isolation test**: developer B calls every `/api/v1` route with developer A's ids and gets only 404s — a new route fails the test until its scoping is decided. |
| `logx` / logging | request id present on every record within a request; redaction test as in §5. |
| `syncer`, `events` | existing tests updated to seed a developer; behaviour unchanged. |

End-to-end (verification skill, read-only DB): delete DB, start, sign up in a
browser, connect the mailbox, create a key, run `scripts/smoke.sh`; sign up a
second developer and confirm 404 on the first developer's account.

---

## 7. Out of scope / follow-ups

- Gin and/or an ORM migration (user's option 2–4), to be done after tenancy
  lands with these tests as the safety net.
- Password reset / email verification / login rate limiting.
- Per-key scopes, teams, billing.
- Durable first-attempt for webhook deliveries (unchanged from the current
  limitation).
