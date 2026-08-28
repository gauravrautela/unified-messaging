package api

import (
	"net/http"
	"strings"
	"text/template"
)

// handleLLMsTxt serves the machine-oriented twin of /docs, following the
// llms.txt convention: plain Markdown, exact shapes, no prose an agent has
// to interpret. Public for the same reason /docs is. The route list comes
// from apiRoutes so it cannot drift from the server.
func (s *Server) handleLLMsTxt(w http.ResponseWriter, r *http.Request) {
	var b strings.Builder
	for _, p := range apiRoutes {
		b.WriteString("- `" + p + "`\n")
	}
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.WriteHeader(http.StatusOK)
	_ = llmsTmpl.Execute(w, struct{ Base, Routes string }{s.baseURL(r), b.String()})
}

var llmsTmpl = template.Must(template.New("llms").Parse(llmsTxt))

const llmsTxt = `# Unified Messaging API

> One REST API to connect end users' mailboxes (Outlook / Microsoft 365) and WhatsApp numbers, read and send mail or chat messages in a provider-neutral shape, and receive signed webhooks when something arrives. Multi-tenant: every resource belongs to exactly one developer.

Base URL: {{.Base}}
Human guide: {{.Base}}/docs
Spec version: v1

## Rules for agents

- Authenticate every /api/v1 call with ` + "`Authorization: Bearer <api_key>`" + ` (or ` + "`X-API-Key`" + `). Keys start with ` + "`um_`" + ` and are 43 chars.
- Never print, log, or echo an API key, a webhook secret, a session cookie, a WhatsApp QR payload, or a phone number/message text you did not already know.
- Mail and chat routes need ` + "`?account_id=acc_…`" + ` in the query, except ` + "`POST /api/v1/emails`" + `, ` + "`POST /api/v1/drafts`" + ` and ` + "`POST /api/v1/chats`" + ` which take ` + "`account_id`" + ` in the JSON body.
- A 404 for an id you did not create means it belongs to another developer; do not retry.
- ` + "`POST`" + `/` + "`PATCH`" + ` bodies are JSON with ` + "`Content-Type: application/json`" + `.
- Webhook delivery is at-least-once: dedupe on ` + "`(type, email.id)`" + ` for mail, ` + "`(type, message.id)`" + ` for chat. Always verify ` + "`X-Outlook-Signature`" + ` when a secret was set.
- Send an ` + "`Idempotency-Key`" + ` (any unique string) on chat writes you may need to retry (send/edit/delete/react/start-chat) — the same key with the same request body replays the first response instead of repeating the side effect; the same key with a different body is ` + "`409 idempotency_conflict`" + `.
- ` + "`account_status`" + ` may arrive at any time, unprompted by anything you did — always handle it, don't assume it only follows a request.
- Editing, deleting or reacting to a chat message you did not send is ` + "`403 not_own_message`" + `, not retryable.
- Calling a mail-only route on a chat account, or a chat-only route on a mail account, is ` + "`400 unsupported_for_kind`" + `, not retryable.
- Send ` + "`X-Request-Id`" + ` on requests you may need to debug; every response echoes one.

## Authentication

- Developers sign up at ` + "`/signup`" + ` and create keys on ` + "`/dashboard`" + `. Keys are shown once.
- ` + "`GET /api/v1/me`" + ` -> ` + "`{id, email, name, created_at, redirect_domains: [...], auth: \"api_key\"|\"session\"}`" + `
- Key management (` + "`POST`" + `/` + "`DELETE /api/v1/api-keys`" + `) is session-only: an API key gets ` + "`403 session_required`" + `.
- ` + "`POST /api/v1/me/password`" + ` (session-only) body ` + "`{current_password, new_password}`" + ` -> 204; changes the password and signs out every other session. ` + "`400 invalid_credentials`" + ` on a wrong current password, ` + "`400 invalid_body`" + ` if the new one is under 10 characters.
- ` + "`PUT /api/v1/me/redirect-domains`" + ` (session-only) body ` + "`{domains: [\"app.example.com\", \"*.example.com\"]}`" + ` (max 20, ` + "`*.`" + ` covers subdomains) -> ` + "`{redirect_domains: [...]}`" + `. ` + "`success_redirect_url`" + `/` + "`failure_redirect_url`" + ` on ` + "`hosted-auth`" + ` must be this server's own origin or on this list, else ` + "`400 invalid_url`" + `.

## Objects

Account:
` + "```" + `
{id, provider: "OUTLOOK"|"WHATSAPP", kind: "mail"|"chat", email (the address for mail, the E.164 phone for chat), identifier (same value), name,
 status: "OK"|"CREDENTIALS", created_at, updated_at, last_synced_at?,
 connection?: {state: "connecting"|"connected"|"backoff"|"stopped"|"error", since, reconnects, last_error?}}  // connection is chat-only
` + "```" + `

Chat:
` + "```" + `
{id, account_id, kind: "direct"|"group", name, unread_count, last_message_at?, archived, muted, members?: [Attendee]}
` + "```" + `

Attendee:
` + "```" + `
{id (stable provider id, phone JID when known), phone? (E.164), name, is_self}
` + "```" + `

ChatMessage:
` + "```" + `
{id, account_id, chat_id, sender: Attendee, is_from_me, kind: "text"|"unsupported", text,
 quoted_message_id?, sent_at, edited_at?, deleted, status?: "sending"|"sent"|"delivered"|"read", reactions: [Reaction]}
` + "```" + `

Reaction:
` + "```" + `
{attendee_id, emoji, at}
` + "```" + `

Email (complete form, returned by GET /api/v1/emails/{id} and inside events; list responses omit body/body_plain/attachments):
` + "```" + `
{id, account_id, thread_id, folder_id, role?: "inbox"|"sentitems"|"drafts"|...,
 subject, from: {name?, email}, to: [{name?, email}], cc?: [...], bcc?: [...], reply_to?: [...],
 date (RFC3339), snippet, body?, body_type?: "html"|"text", body_plain?,
 read, flagged, draft, has_attachments,
 attachments?: [{id, name, mime_type, size, is_inline, content_id?}],
 internet_message_id?}
` + "```" + `

Webhook:
` + "```" + `
{id, account_id?: "" | "acc_…", name?, kind: "webhook"|"discord"|"telegram", url? (webhook/discord), secret? (webhook, creation only), telegram?: {chat_id}, events: [..], created_at}
` + "```" + `

Event (webhook payload):
` + "```" + `
{type: "mail_received"|"mail_sent"|"mail_updated"|"mail_deleted"
     |"chat_received"|"chat_sent"|"chat_updated"|"chat_reaction"|"chat_deleted"|"account_status",
 account_id, timestamp, webhook: {id, name?},
 email?: Email, email_id?: string (mail_deleted),
 message?: ChatMessage, chat?: Chat, message_ids?: [string], status?: string, change?: string,
 reaction?: Reaction (chat_reaction), account?: Account (account_status)}
` + "```" + `

Error:
` + "```" + `
{error: {code, message}}
` + "```" + `

## Flows

### Connect a mailbox
1. ` + "`POST /api/v1/hosted-auth`" + ` body ` + "`{success_redirect_url?, failure_redirect_url?, notify_url?, webhook?: {url, secret?, name?, events?}, expires_in_minutes?: 30, provider?, force_consent?}`" + ` -> ` + "`{url, state, provider, expires_at}`" + `.
2. Open ` + "`url`" + ` in the end user's browser. Single-use, expires.
3. On completion ` + "`notify_url`" + ` receives ` + "`{status: \"CREATED\", account_id, email, provider}`" + ` or ` + "`{status: \"FAILED\", error, message}`" + `; the browser is redirected to ` + "`success_redirect_url?account_id=…`" + ` or ` + "`failure_redirect_url?error=…`" + `.
4. Backfill (30 days) runs in the background; ` + "`GET /api/v1/accounts/{id}.last_synced_at`" + ` is set when done.
5. Reconnecting the same mailbox keeps the same ` + "`account_id`" + `.
6. ` + "`success_redirect_url`" + `/` + "`failure_redirect_url`" + ` must be this server's own origin or a host on your redirect-domain allowlist (see ` + "`PUT /api/v1/me/redirect-domains`" + ` above) — otherwise step 1 answers ` + "`400 invalid_url`" + `.

### Read mail
- ` + "`GET /api/v1/emails?account_id&folder_role=inbox|sentitems|drafts|deleteditems|junkemail|archive&folder_id&thread_id&unread=true&q=<substring>&limit<=200&offset`" + ` -> ` + "`{items: [Email without body], limit, offset}`" + `, newest first.
- ` + "`GET /api/v1/emails/{id}?account_id`" + ` -> complete Email (falls back to the provider if not yet synced).
- ` + "`GET /api/v1/emails/{id}/attachments?account_id`" + ` -> ` + "`{items: [Attachment]}`" + `; ` + "`GET /api/v1/emails/{id}/attachments/{aid}?account_id`" + ` -> raw bytes with Content-Type and Content-Disposition.
- ` + "`GET /api/v1/threads?account_id&limit&offset`" + ` -> ` + "`{items: [{id, account_id, subject, count, last_date, unread}]}`" + `.
- ` + "`GET /api/v1/folders?account_id`" + ` -> ` + "`{items: [{id, account_id, name, parent_id?, role?, total_count, unread_count}]}`" + `.

### Send and update
- ` + "`POST /api/v1/emails`" + ` body ` + "`{account_id, to: [{email, name?}], cc?, bcc?, subject, body, body_type?: \"html\", attachments?: [{name, mime_type, content: base64}], reply_to_email_id?, reply_all?}`" + ` -> ` + "`202 {status: \"sent\"}`" + `. Inline attachments ≤ ~3 MB.
- ` + "`POST /api/v1/emails/{id}/reply?account_id`" + ` body ` + "`{body, body_type?, reply_all?, attachments?}`" + ` -> 202.
- ` + "`POST /api/v1/emails/{id}/forward?account_id`" + ` body ` + "`{to: [...], body?, attachments?}`" + ` -> 202.
- ` + "`POST /api/v1/drafts`" + ` body as send -> Email (draft); ` + "`POST /api/v1/drafts/{id}/send?account_id`" + ` -> 202.
- ` + "`PATCH /api/v1/emails/{id}?account_id`" + ` body ` + "`{read?: bool, flagged?: bool}`" + ` -> Email.

### Webhooks
- Per account: ` + "`POST /api/v1/accounts/{id}/webhooks`" + ` body ` + "`{kind?: \"webhook\"|\"discord\"|\"telegram\", url?, secret?, bot_token?, chat_id?, name?, events?}`" + ` (events default ` + "`[\"mail_received\"]`" + ` for a mailbox, ` + "`[\"chat_received\"]`" + ` for a chat account) -> 201 Webhook incl. secret once. ` + "`kind`" + ` defaults to ` + "`webhook`" + `; see the per-kind bodies below.
- Developer-wide: ` + "`POST /api/v1/webhooks`" + ` same body; empty ` + "`events`" + ` means all. ` + "`\"*\"`" + ` also means all.
- List/delete: ` + "`GET /api/v1/webhooks`" + `, ` + "`DELETE /api/v1/webhooks/{id}`" + `, ` + "`GET /api/v1/accounts/{id}/webhooks`" + `, ` + "`DELETE /api/v1/accounts/{id}/webhooks/{wid}`" + ` (there is no single-hook GET).
- URLs must be public http(s); localhost, loopback, link-local and private IPs are rejected as written (400 invalid_url / invalid_webhook); the actual delivery additionally re-checks the resolved address at dial time and never follows a redirect.
- Delivery: POST JSON Event with headers ` + "`X-Outlook-Event`" + ` (type), ` + "`X-Outlook-Delivery`" + ` (attempt number), ` + "`X-Outlook-Signature: sha256=<hex HMAC-SHA256 of raw body with secret>`" + `. Respond 2xx within 15 s — a 3xx (never followed) or any other non-2xx is a failure, ` + "`last_error`" + ` reads ` + "`status <code>`" + `.
- Retries after a non-2xx: 30s, 2m, 10m, 30m, 2h, 6h, 12h; then marked dead and kept ` + "`DELIVERY_RETENTION_DAYS`" + ` (default 7) before an hourly purge. ` + "`GET /api/v1/webhooks/{id}/deliveries?limit&offset`" + ` (limit default 50, clamps at 200) -> ` + "`{items: [{id, webhook_id, account_id, event_type, attempts, next_attempt_at, last_error, dead, created_at}], limit, offset}`" + `.
- kind=discord: body ` + "`{kind:\"discord\", url:\"https://discord.com/api/webhooks/…\", name?, events?}`" + `; receives a formatted text message, no signature. The URL must be a plain ` + "`discord.com`" + `/` + "`discordapp.com`" + ` host with no port and no userinfo.
- kind=telegram: body ` + "`{kind:\"telegram\", bot_token, chat_id, name?, events?}`" + ` (create the bot with ` + "`@BotFather`" + `, then ` + "`getUpdates`" + ` to find ` + "`chat_id`" + `); bot_token is never returned; a Telegram rejection at creation is 400 invalid_webhook, Telegram unreachable is 502 provider_error.
- Notifications: one message per event, mail snippet ≤200 chars, chat ≤300, phones masked. The whole message is capped at the transport's limit — 2,000 runes for Discord, 4,096 for Telegram — and cut with an ellipsis. Same retry schedule and deliveries log as kind=webhook; a Discord 429 (rate limit) is an ordinary retryable failure, not a permanent one.

### Account lifecycle
- ` + "`GET /api/v1/accounts`" + ` -> ` + "`{items: [Account]}`" + `; ` + "`GET /api/v1/accounts/{id}`" + `.
- ` + "`status: \"CREDENTIALS\"`" + ` means the provider rejected the refresh token (mail) or the linked device was logged out / 30 consecutive reconnects failed (WhatsApp): mint a new connect link for the same user (mail keeps its account_id; WhatsApp always needs a fresh connect link, since a phone that logged out has no token to refresh, but relinking the **same number** reuses the same account_id — a different number makes a new account). An ` + "`account_status`" + ` event is emitted either way, and it can arrive with no request of yours in flight.
- ` + "`POST /api/v1/accounts/{id}/resync`" + ` (mail only) -> ` + "`202 {status: \"queued\"}`" + `; 409 account_not_ok if status is not OK; 400 unsupported_for_kind on a chat account.
- ` + "`POST /api/v1/accounts/{id}/reconnect`" + ` (chat only) -> forces the socket to reconnect now; 400 unsupported_for_kind on a mail account; 503 capacity at ` + "`WHATSAPP_MAX_ACCOUNTS`" + `.
- ` + "`DELETE /api/v1/accounts/{id}`" + ` -> 204; removes tokens/device credentials, mirror, subscriptions and per-account webhooks (and, for WhatsApp, logs the device out).
- ` + "`GET /api/v1/providers`" + ` -> ` + "`{items: [{name, kind: \"mail\"|\"chat\", auth: \"oauth\"|\"link\", push_notifications}]}`" + `. WhatsApp is ` + "`{name: \"WHATSAPP\", kind: \"chat\", auth: \"link\", push_notifications: false}`" + `.

### Chat (WhatsApp)
- Linked-device model, like WhatsApp Web/Desktop — not the official Business API. The end user's phone shows this as another entry under Settings > **Linked devices**. Meta can ban a number for automation; get explicit consent before pairing (the connect page enforces this — see below).
- Connect: ` + "`POST /api/v1/hosted-auth {\"provider\": \"WHATSAPP\", ...}`" + ` (naming the provider is required — the unnamed-call default only ever resolves to a lone *mail* provider) -> ` + "`{url, state, provider, expires_at}`" + `. Open ` + "`url`" + `: it renders a disclosure, not a third-party sign-in screen.
- ` + "`POST /connect/{state}/consent`" + ` (no body) -> 204; must happen before ` + "`/qr`" + ` will do anything. 410 if the connect link expired first.
- ` + "`GET /connect/{state}/qr`" + `, poll every ~2s -> 409 ` + "`consent_required`" + ` before consent; then ` + "`{status: \"waiting\"}`" + `, then ` + "`{status: \"waiting\", png_base64, expires_in}`" + ` once whatsmeow has a code, then ` + "`{status: \"paired\", account_id}`" + ` or ` + "`{status: \"expired\"|\"failed\"}`" + ` (pairing window ~3 minutes).
- On success, ` + "`notify_url`" + ` gets ` + "`{status: \"CREATED\", account_id, identifier (E.164 phone), provider: \"WHATSAPP\"}`" + ` or ` + "`{status: \"FAILED\", error, message}`" + `, exactly like mail.
- ` + "`GET /api/v1/chats?account_id&kind=direct|group&unread=true&q=&limit&offset`" + ` -> ` + "`{items: [Chat], limit, offset}`" + `.
- ` + "`POST /api/v1/chats`" + ` body ` + "`{account_id, phone? or attendee_id?, text}`" + ` -> 201 ` + "`{chat: Chat, message: ChatMessage}`" + ` (starts a direct chat and sends the first message in one call).
- ` + "`PATCH /api/v1/chats/{id}?account_id`" + ` body ` + "`{read?: true, archived?: bool, muted?: bool}`" + ` -> Chat. The upstream read receipt covers the 50 most recent messages; the local unread count is cleared in full.
- ` + "`GET /api/v1/chats/{id}/messages?account_id&before=&limit`" + ` -> ` + "`{items: [ChatMessage], next_before?}`" + `, newest first; page with ` + "`?before=<next_before>`" + `.
- ` + "`POST /api/v1/chats/{id}/messages?account_id`" + ` body ` + "`{text, quoted_message_id?}`" + ` -> 201 ChatMessage. Send an ` + "`Idempotency-Key`" + ` header to make a retried send safe.
- ` + "`PATCH /api/v1/chats/{id}/messages/{mid}?account_id`" + ` body ` + "`{text}`" + ` -> ChatMessage. ` + "`DELETE .../messages/{mid}?account_id`" + ` -> 204 (revokes for everyone). ` + "`PUT .../messages/{mid}/reaction?account_id`" + ` body ` + "`{emoji}`" + ` (` + "`\"\"`" + ` removes it) -> 204. All three: ` + "`403 not_own_message`" + ` on a message ` + "`is_from_me: false`" + `.
- ` + "`GET /api/v1/attendees?account_id&q=`" + ` -> ` + "`{items: [Attendee]}`" + `; ` + "`GET /api/v1/attendees/{id}?account_id`" + `.
- Events: ` + "`chat_received`" + ` (inbound message), ` + "`chat_sent`" + ` (sent via API or echoed from the phone), ` + "`chat_updated`" + ` (edit, mark-read, or chat flags changed), ` + "`chat_reaction`" + `, ` + "`chat_deleted`" + `. ` + "`account_status`" + ` covers connection loss/regain and the terminal ` + "`CREDENTIALS`" + ` (unreachable) state — it can arrive at any time.
- Device keys live unsealed in whatsmeow's own tables in this service's SQLite file (unlike OAuth refresh tokens, which are AES-256-GCM sealed) — run this behind disk-level encryption for anything beyond local testing.

## Error codes

| status | code |
|---|---|
| 400 | invalid_body, missing_account_id, missing_recipients, invalid_webhook, invalid_url, unknown_folder_role, missing_name, unknown_provider ("provider is required"), unsupported_for_kind, missing_text, missing_recipient, missing_emoji, empty_patch, attachment_too_large (attachments exceed 3 MB decoded, in total), invalid_credentials (wrong current password on ` + "`POST /api/v1/me/password`" + `) |
| 401 | unauthorized |
| 403 | session_required (key management and account-settings changes — password, redirect domains — need the dashboard session), not_own_message (editing/deleting/reacting to a chat message you did not send), link_browser_mismatch (a WhatsApp consent/` + "`/qr`" + ` call from a different browser than the one that started it) |
| 404 | account_not_found, not_found (also for resources owned by another developer) |
| 409 | account_not_ok, reconnect_required (dead grant, or a chat account with no live socket right now — e.g. connection state "backoff"), consent_required (WhatsApp ` + "`/qr`" + ` polled before consent), idempotency_conflict (` + "`Idempotency-Key`" + ` reused with a different request) |
| 410 | expired (connect link, or the ~3-minute WhatsApp pairing window) |
| 413 | body_too_large (request body over this route's limit — 64 KB default, 8 MB on send/reply/forward/drafts) |
| 415 | json_required (dashboard-session writes only) |
| 502 | provider_error (message carries the provider's code; also: Telegram unreachable when creating a telegram hook) |
| 503 | capacity (chat runtime at ` + "`WHATSAPP_MAX_ACCOUNTS`" + `, or disabled) |

## Endpoints (generated from the server's route table)

{{.Routes}}
## Limits

- Providers: Outlook / Microsoft 365 (mail) and WhatsApp (chat, linked-device model — not the official Business API). No calendar or contacts on either.
- Backfill window 30 days; older messages fetched on demand by id. (Mail only — WhatsApp has no history sync; a newly linked device only sees messages sent after pairing.)
- Request bodies are capped per route: 64 KB by default, 8 MB on the mail-send family (emails, reply, forward, drafts) since those carry base64 attachment bytes. Over the cap is 413 body_too_large.
- Inline attachments are additionally capped at 3 MB total, decoded, by this service — over that is 400 attachment_too_large.
- Hosted-auth redirect URLs must be this server's own origin or on the developer's redirect-domain allowlist (` + "`PUT /api/v1/me/redirect-domains`" + `, max 20 entries) — otherwise 400 invalid_url.
- Offset paging on mail; items may shift while a mailbox changes. Chat message paging is cursor-based (` + "`before`" + `/` + "`next_before`" + `) and stable.
- First webhook attempt is in-memory; a crash before the first POST loses that event (the mirror re-converges on next sync). A dead delivery (retries exhausted) is kept ` + "`DELIVERY_RETENTION_DAYS`" + ` (default 7) before an hourly purge removes it.
- The event queue is bounded: a subscriber slow enough to fill it pushes back on the producer for 5 s, and only then is an event dropped. Drops are counted and reported as ` + "`dropped_events`" + ` on ` + "`GET /healthz`" + `; a dropped event is never persisted and cannot be replayed.
- Webhook/notify_url checks are literal-IP only at registration; hostnames are not resolved there. Delivery and notify fetches re-check the resolved address at dial time on every connection attempt (so a hostname resolving to a private/loopback/link-local address still fails, just later — last_error reads "not a public address") and never follow a redirect. Redirect URLs get no SSRF check: this service never fetches one, only the end user's browser follows it. They are constrained by the redirect-domain allowlist instead.
- This service does not rate-limit callers or cap how many keys/webhooks/connect links one developer can create; that is a gateway responsibility (see the README's "Security" section).
- With ` + "`DB_DRIVER=sqlite`" + ` (the default), storage is a single-connection file sized for one process and one disk — fine for local dev and tests, not for a public deployment. Set ` + "`DB_DRIVER=postgres`" + ` for a real connection pool (see the README's "Setup" §7).
- WhatsApp: text only — media arrives as ` + "`kind: \"unsupported\"`" + ` and cannot be sent. QR linking only, no phone-number pairing codes. One socket per account, capped process-wide at ` + "`WHATSAPP_MAX_ACCOUNTS`" + ` (default 200). Reconnect backoff up to 5 minutes; 30 consecutive failures -> ` + "`CREDENTIALS`" + ` (unreachable).
- Discord/Telegram targets are one-way and text-only.
`
