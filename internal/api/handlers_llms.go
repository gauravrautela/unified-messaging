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

> One REST API to connect end users' mailboxes (Outlook / Microsoft 365), read and send mail in a provider-neutral shape, and receive signed webhooks when mail arrives. Multi-tenant: every resource belongs to exactly one developer.

Base URL: {{.Base}}
Human guide: {{.Base}}/docs
Spec version: v1

## Rules for agents

- Authenticate every /api/v1 call with ` + "`Authorization: Bearer <api_key>`" + ` (or ` + "`X-API-Key`" + `). Keys start with ` + "`um_`" + ` and are 43 chars.
- Never print, log, or echo an API key, a webhook secret, or a session cookie.
- Mail routes need ` + "`?account_id=acc_…`" + ` in the query, except ` + "`POST /api/v1/emails`" + ` and ` + "`POST /api/v1/drafts`" + ` which take ` + "`account_id`" + ` in the JSON body.
- A 404 for an id you did not create means it belongs to another developer; do not retry.
- ` + "`POST`" + `/` + "`PATCH`" + ` bodies are JSON with ` + "`Content-Type: application/json`" + `.
- Webhook delivery is at-least-once: dedupe on ` + "`(type, email.id)`" + `. Always verify ` + "`X-Outlook-Signature`" + ` when a secret was set.
- Send ` + "`X-Request-Id`" + ` on requests you may need to debug; every response echoes one.

## Authentication

- Developers sign up at ` + "`/signup`" + ` and create keys on ` + "`/dashboard`" + `. Keys are shown once.
- ` + "`GET /api/v1/me`" + ` -> ` + "`{id, email, name, created_at, auth: \"api_key\"|\"session\"}`" + `
- Key management (` + "`POST`" + `/` + "`DELETE /api/v1/api-keys`" + `) is session-only: an API key gets ` + "`403 session_required`" + `.

## Objects

Account:
` + "```" + `
{id, provider: "OUTLOOK", email, name, status: "OK"|"CREDENTIALS", created_at, updated_at, last_synced_at?}
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
{id, account_id?: "" (developer-wide) | "acc_…", name?, url, secret? (only at creation), events: [..], created_at}
` + "```" + `

Event (webhook payload):
` + "```" + `
{type: "mail_received"|"mail_sent"|"mail_updated"|"mail_deleted"|"account_status",
 account_id, timestamp, webhook: {id, name?},
 email?: Email, email_id?: string (mail_deleted), account?: Account (account_status)}
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
- Per account: ` + "`POST /api/v1/accounts/{id}/webhooks`" + ` body ` + "`{url, secret?, name?, events?}`" + ` (events default ` + "`[\"mail_received\"]`" + `) -> 201 Webhook incl. secret once.
- Developer-wide: ` + "`POST /api/v1/webhooks`" + ` same body; empty ` + "`events`" + ` means all. ` + "`\"*\"`" + ` also means all.
- List/delete: ` + "`GET|DELETE /api/v1/webhooks[/{id}]`" + `, ` + "`GET|DELETE /api/v1/accounts/{id}/webhooks[/{wid}]`" + `.
- URLs must be public http(s); localhost, loopback, link-local and private IPs are rejected (400 invalid_url / invalid_webhook).
- Delivery: POST JSON Event with headers ` + "`X-Outlook-Event`" + ` (type), ` + "`X-Outlook-Delivery`" + ` (attempt number), ` + "`X-Outlook-Signature: sha256=<hex HMAC-SHA256 of raw body with secret>`" + `. Respond 2xx within 15 s.
- Retries after a non-2xx: 30s, 2m, 10m, 30m, 2h, 6h, 12h; then marked dead. ` + "`GET /api/v1/webhooks/{id}/deliveries`" + ` -> ` + "`{items: [{id, webhook_id, account_id, event_type, attempts, next_attempt_at, last_error, dead, created_at}]}`" + `.

### Account lifecycle
- ` + "`GET /api/v1/accounts`" + ` -> ` + "`{items: [Account]}`" + `; ` + "`GET /api/v1/accounts/{id}`" + `.
- ` + "`status: \"CREDENTIALS\"`" + ` means the provider rejected the refresh token: mint a new connect link for the same user (same account_id). An ` + "`account_status`" + ` event is emitted.
- ` + "`POST /api/v1/accounts/{id}/resync`" + ` -> ` + "`202 {status: \"queued\"}`" + `; 409 account_not_ok if status is not OK.
- ` + "`DELETE /api/v1/accounts/{id}`" + ` -> 204; removes tokens, mirror, subscriptions and per-account webhooks.
- ` + "`GET /api/v1/providers`" + ` -> ` + "`{items: [{name, push_notifications}]}`" + `.

## Error codes

| status | code |
|---|---|
| 400 | invalid_body, missing_account_id, missing_recipients, invalid_webhook, invalid_url, unknown_folder_role, missing_name |
| 401 | unauthorized |
| 403 | session_required |
| 404 | account_not_found, not_found (also for resources owned by another developer) |
| 409 | account_not_ok, reconnect_required |
| 415 | json_required (dashboard-session writes only) |
| 502 | provider_error (message carries the provider's code) |

## Endpoints (generated from the server's route table)

{{.Routes}}
## Limits

- Provider: Outlook / Microsoft 365 only. No calendar or contacts.
- Backfill window 30 days; older messages fetched on demand by id.
- Offset paging; items may shift while a mailbox changes.
- First webhook attempt is in-memory; a crash before the first POST loses that event (the mirror re-converges on next sync).
- Webhook URL check is literal-IP only; hostnames are not resolved.
`
