# Security hardening before public exposure — design

**Status:** approved in conversation, 2026-08-27
**Source:** the launch security audit (four lenses; isolation verdict: holds; perimeter findings C1–C6, I1–I12, minors)
**Out of scope (handled by the Kong gateway / ops):** rate limiting and brute-force lockout, per-developer caps on keys/webhooks/links, send-rate limits, signup email verification, `govulncheck` in CI, `/healthz` exposure, disk encryption, key rotation. The single-connection SQLite is a documented limitation: the public deployment moves to Postgres.

## 1. Outbound HTTP hardening (C1)

New package `internal/safehttp`:

```go
// Client returns an *http.Client that never follows redirects and refuses to
// dial private, loopback, link-local, multicast or unspecified addresses —
// checked on the resolved IP at connect time, so DNS tricks cannot get past
// the literal check in publicHTTPURL.
func Client(timeout time.Duration) *http.Client
// Dial guard used by Client; exported for tests.
func PublicOnlyControl(network, address string, c syscall.RawConn) error
var ErrPrivateAddress = errors.New("safehttp: destination is not a public address")
```

- `CheckRedirect` returns `http.ErrUseLastResponse` (a 3xx counts as a failed delivery: `status 3xx`).
- The `net.Dialer.Control` parses the address, unmaps 4-in-6, and rejects `IsLoopback/IsPrivate/IsLinkLocalUnicast/IsLinkLocalMulticast/IsMulticast/IsUnspecified/IsInterfaceLocalMulticast`, plus the CGNAT block `100.64.0.0/10` and `169.254.0.0/16` (metadata). Tests may install an allow-loopback override (`safehttp.AllowLoopbackForTests(t)`) because `httptest` servers are loopback.
- Used by: `notify.NewRegistry` (default client), `events.NewDispatcher` (default registry client), `handlers_connect.go` `notify()` (replaces `http.DefaultClient`). The Outlook Graph clients keep their own clients (fixed Microsoft hosts).
- `publicHTTPURL` stays as the cheap registration-time check; its comment loses the "known gap" paragraph.

## 2. HTTP server timeouts (C4)

`cmd/server/main.go`: `ReadHeaderTimeout: 10s`, `ReadTimeout: 30s`, `WriteTimeout: 60s`, `IdleTimeout: 120s`, `MaxHeaderBytes: 64 << 10`.

## 3. Security headers and cache control (C5, I11)

One middleware `secureHeaders` wrapping the whole mux in `Routes()`:

| Header | Value | Where |
|---|---|---|
| `X-Frame-Options` | `DENY` | all |
| `Content-Security-Policy` | `default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; form-action 'self'; base-uri 'self'; frame-src 'self'` | all HTML pages (the mail viewer's sandboxed iframe is `srcdoc`, covered by `frame-src 'self'`) |
| `X-Content-Type-Options` | `nosniff` | all |
| `Referrer-Policy` | `no-referrer` | all |
| `Strict-Transport-Security` | `max-age=31536000; includeSubDomains` | when the request is HTTPS (see §5 proxy trust) |
| `Cache-Control` / `Vary` | `no-store` / `Cookie, Authorization` | `/api/v1/*`, `/dashboard`, `/mail`, `/chat`, `/connect/*`, `/oauth/callback`, `/login`, `/signup` (`/docs`, `/llms.txt`, `/healthz` keep their own) |

## 4. Repository and file hygiene (C6)

- `store.Open` creates the DB file with mode `0600` (`os.OpenFile(..., O_CREATE, 0600)` before `sql.Open`) and chmods an existing file to `0600` when it is looser.
- `.gitignore` adds `unified-messaging.db.pre-tenancy*`; new `.dockerignore` = `.gitignore` contents + `.git`, `docs/superpowers`, `.superpowers`.
- The stray `unified-messaging.db.pre-tenancy*` files are deleted (operator confirmed).

## 5. Sessions, cookies, login (C3, I1, I3, I10)

- **Hashed at rest:** `sessions.id` stores `sha256(token)` (hex), exactly like `api_keys.hash`. `auth.HashKey` is reused. Existing plaintext sessions become invalid on deploy (users log in again).
- **Absolute lifetime:** `sessions.created_at` already exists; `SessionDeveloper` also rejects sessions older than `SESSION_MAX_AGE_DAYS` (default 90) regardless of sliding expiry.
- **Rotation:** login always issues a new session; logout deletes the current one.
- **CSRF on the auth forms:** a double-submit token — cookie `um_csrf` (random 32 bytes, `SameSite=Strict`, `HttpOnly=false` is unnecessary because the server renders the form: keep it `HttpOnly`) and a hidden `<input name="csrf">` rendered from the same value; `POST /login`, `/signup`, `/logout` require the field to equal the cookie (constant-time), else `403 csrf`. Additionally, when an `Origin` (or `Referer`) header is present it must match the request host (scheme-aware via §5 proxy trust), else `403 csrf`. The dashboard's logout form gets the hidden field too.
- **Secure cookies:** `secureCookies(r)` is true when `r.TLS != nil`, or `PUBLIC_BASE_URL` is https, or `TRUST_PROXY=1` and `X-Forwarded-Proto: https`. New config `TRUST_PROXY` (bool, default false). `requestIsHTTPS(r)` is shared with the HSTS decision.
- **Uniform signup error:** an existing email answers the same `400 invalid_signup` ("could not create the account — check the details or sign in") as invalid input; the log line keeps the real reason at DEBUG with the email digested.
- **Password change:** `POST /api/v1/me/password` (session-only) body `{"current_password","new_password"}` → verifies current, re-hashes, deletes every other session of the developer (`DeleteSessionsExcept(developerID, currentHash)`), 204. Wrong current → `400 invalid_credentials`. Dashboard gets a small "Change password" form calling it.
- Login/signup/logout do not add rate limiting (gateway).

## 6. Redirect allowlist (I2)

- `developers.redirect_domains_json` (TEXT, default `[]`): lower-case hostnames; a leading `*.` allows subdomains.
- `GET /api/v1/me` returns `redirect_domains`; `PUT /api/v1/me/redirect-domains` (session-only) body `{"domains":[...]}` validates (≤ 20 entries, hostname syntax, no IPs) and replaces the list.
- `POST /api/v1/hosted-auth`: each of `success_redirect_url`/`failure_redirect_url` must be absolute http(s) **and** its hostname must match the developer's list; otherwise `400 invalid_url` "redirect host is not on your allowlist — add it under Settings → Redirect domains". The dashboard's own connect flow uses the server's origin (`PUBLIC_BASE_URL` host or `r.Host`), which is always allowed.
- Dashboard: a "Redirect domains" textarea under a new Settings block.

## 7. Delivery pipeline (I4, I8)

- **Worker pool:** `deliver` fans each event's hooks to a bounded pool (`DeliveryWorkers`, default 8, settable before `Start`) and waits for the event's own sends to finish before taking the next event; the retry loop uses the same pool. A slow target costs its own worker, not the dispatcher. Drain-on-shutdown waits for in-flight sends (bounded as today).
- **Deliveries listing:** `GET /api/v1/webhooks/{id}/deliveries?limit=<≤200, default 50>&offset=` → `{items, limit, offset}`; `ListDeliveries(webhookID, limit, offset)`.
- **Purge:** `PurgeDeadDeliveries(olderThan)` deletes `dead=1` rows older than `DELIVERY_RETENTION_DAYS` (default 7); `PurgeExpiredOAuthStates` and this purge run hourly on a ticker in `main.go` (first run at boot as today).
- **Cascade:** `webhook_deliveries.account_id` rows for a deleted account are removed in `DeleteAccount` (same transaction).
- `LIKE` inputs (`ListEmails`, `ListChats`, `ListAttendees`) escape `\`, `%`, `_` and use `ESCAPE '\'`.

## 8. Input limits and amplification (I5, I7, minors)

- `decodeJSON(r, v)` keeps a per-call limit: default `64 << 10`; `decodeJSONLarge` with `8 << 20` for `POST /emails`, `/reply`, `/forward`, `/drafts`. `readRawBody` (chat send) caps at `64 << 10`. Over-limit → `413 body_too_large`.
- Attachment cap: total decoded attachment bytes per send ≤ `3 << 20` → `400 attachment_too_large` (docs already say ~3 MB).
- `GET /api/v1/emails/{id}` mirror miss: a negative cache (`sync.Map` of `(account, id) → until`, TTL 60 s, capped at 10 000 entries by opportunistic sweep) short-circuits repeated misses with `404 not_found`; on a provider 404 the miss is cached; on other provider errors it is not. Tests stub the mailbox so no test reaches Graph.
- `X-Request-Id`: accepted only if `^[A-Za-z0-9_.:-]{1,64}$`, else replaced.
- `GET /notifications/{provider}`: a semaphore of 32 concurrent handlers; beyond that the request still answers 202 but the payload is processed inline with a 10 s cap (never dropped, never unbounded).

## 9. Provider push and chat runtime (I6, minor)

- `adoptExisting` no longer stores an unknown `clientState`; it deletes the remote subscription and creates a fresh one (`pusher.Delete` then `create`).
- `HandleNotifications` compares `clientState` with `subtle.ConstantTimeCompare` and always requires it (an empty stored state is treated as a mismatch).
- `chatsync` sink asserts `accountID == a.acct.ID` on every callback; a mismatch is logged at ERROR with both ids digested and the event dropped.

## 10. QR link binding (I11)

- `GET /connect/{state}` for a linker provider sets cookie `um_link=<random 32 bytes>` (`HttpOnly`, `SameSite=Strict`, `Secure` per §5, `Max-Age` = link TTL) and stores its hash in the link registry entry when it is created. `POST /consent` and `GET /qr` require the cookie to match the entry (403 `link_browser_mismatch` "open the link in the browser where you started"). The first browser to open the page claims the state; a second browser gets the same 403.

## 11. Logging PII (I9)

- `accounts.Connect`/`ConnectLinked` and `handlers_connect.go` log the identity as `email_digest` (`logx.Digest`) instead of the address.
- `logx.Redact` masks map values under keys containing `email` (keep the domain: `j•••@example.com`) and `phone`/`identifier` (`notify.MaskPhone` shape, implemented locally to avoid an import cycle).

## 12. Tests and docs

- Isolation test gains a session-authenticated pass over the same table and a second table for browser routes (`/dashboard`, `/mail`, `/chat`, `/connect/{state}[/consent|/qr]`, `/oauth/callback`, `/login`, `/signup`, `/logout`) asserting the auth/CSRF/header behaviour.
- Header middleware, CSRF, hashed sessions, absolute lifetime, redirect allowlist, worker pool, purge, pagination, body limits, negative cache, safehttp dial guard and redirect refusal, link cookie binding, PII masking each get unit/API tests.
- `/docs`, `/llms.txt`, README: redirect-domain requirement, `PUT /api/v1/me/redirect-domains`, `POST /api/v1/me/password`, deliveries pagination, body limits, new env vars (`TRUST_PROXY`, `SESSION_MAX_AGE_DAYS`, `DELIVERY_RETENTION_DAYS`), a "Security" section stating what is enforced in code and what the gateway must do (rate limits, caps, TLS termination with `X-Forwarded-Proto`).
- End-to-end verification on a DB copy: headers present, redirect refused (`3xx` recorded as failure), private dial refused, CSRF-less login rejected, hashed session works, allowlist enforced.
