# Delivery targets: Discord and Telegram — design

**Status:** approved in conversation, 2026-08-26
**Depends on:** the webhook system (`internal/events`), the WhatsApp provider spec (`2026-08-25-whatsapp-provider-design.md`)

## 1. Goal

Let a developer forward events not only to a raw JSON webhook but also to a
Discord channel or a Telegram chat as **human-readable notifications**. The
developer supplies every credential (Discord incoming-webhook URL; Telegram
bot token + chat id). No inbound listeners, no two-way replies, no media.

Decisions taken during brainstorming:

| Question | Decision |
|---|---|
| What Discord/Telegram receive | Formatted notification text, one message per event (not the JSON dump, not two-way). |
| How it is configured | The existing **webhook resource gains a `kind`**; no separate `destinations` resource. |
| Telegram identity | Developer's own bot (`bot_token` + `chat_id`); the service runs no shared bot. |

## 2. Data model

`model.Webhook` gains:

```go
Kind     string          `json:"kind"`               // "webhook" (default) | "discord" | "telegram"
Telegram *TelegramTarget `json:"telegram,omitempty"` // kind=telegram only; ChatID exposed, token never
```

```go
type TelegramTarget struct {
    ChatID   string `json:"chat_id"`
    BotToken string `json:"-"`
}
```

Per kind:

| kind | `url` | `secret` | `telegram` |
|---|---|---|---|
| `webhook` | developer endpoint (SSRF-checked as today) | optional HMAC secret, returned once | — |
| `discord` | Discord incoming-webhook URL; host must be `discord.com` or `discordapp.com`, path must start with `/api/webhooks/` | — (rejected with 400 if given) | — |
| `telegram` | — (omitted in responses; rejected with 400 if given) | — | required |

### Storage

Additive migration on `webhooks`:

- `kind TEXT NOT NULL DEFAULT 'webhook'`
- `config TEXT NOT NULL DEFAULT ''` — sealed JSON (`secretbox.Seal` with `TOKEN_ENCRYPTION_KEY`, the same primitive OAuth tokens use) holding `{"bot_token":"…","chat_id":"…"}` for Telegram; empty otherwise.

Existing rows read back as `kind='webhook'` with no behavioural change. `Store.SaveWebhook`/`queryWebhooks` seal/unseal `config`; a row whose `config` fails to open is returned with an empty target and logged once (`WARN webhook config unreadable`), and deliveries to it fail with `last_error: "telegram: config unreadable"` rather than crashing the dispatcher.

## 3. API

Routes are unchanged: `POST|GET /api/v1/webhooks`, `GET|DELETE /api/v1/webhooks/{id}`, `GET /api/v1/webhooks/{id}/deliveries`, `POST|GET /api/v1/accounts/{id}/webhooks`, `DELETE /api/v1/accounts/{id}/webhooks/{wid}`, and the connect-time `webhook` field on `POST /api/v1/hosted-auth`.

`webhookRequest` gains `kind`, `bot_token`, `chat_id`:

```json
{"kind":"webhook","url":"https://…","secret":"…","name":"…","events":["…"]}
{"kind":"discord","url":"https://discord.com/api/webhooks/123/abc","name":"…","events":["…"]}
{"kind":"telegram","bot_token":"123456:ABC…","chat_id":"-1001234567890","name":"…","events":["…"]}
```

- `kind` defaults to `"webhook"`. Unknown kind → `400 invalid_webhook` ("kind must be webhook, discord or telegram").
- Field presence per kind is validated (missing `url` / `bot_token` / `chat_id`, or a field that does not belong to the kind) → `400 invalid_webhook` with a field-specific message.
- Telegram creation calls `getChat` once (`POST https://api.telegram.org/bot<token>/getChat {"chat_id"}`): `ok:false` → `400 invalid_webhook` with Telegram's `description`; transport failure or non-JSON → `502 provider_error`. The check has a 10 s timeout. It is skipped in tests via an injectable base URL on the sender (see §4).
- Events filter and the kind-aware default (`mail_received` for a mailbox, `chat_received` for a chat account, everything for developer-wide) are unchanged.
- Responses: `kind` always present; `secret` only at creation and only for `webhook`; `url` absent for `telegram`; `telegram.chat_id` present for `telegram`; `bot_token` never.
- The Discord `url` is returned as stored (the developer's own value).

### Dashboard

The "Set webhook" form gets a `<select name="kind">` (Webhook / Discord / Telegram) that switches the visible fields: URL + secret; Discord URL; bot token + chat id. Cards show a kind badge next to each hook and, for Telegram, the chat id. Session-authenticated writes keep sending `Content-Type: application/json`.

## 4. Delivery

### Senders

New package `internal/notify`:

```go
type Sender interface {
    // Send delivers ev to h. A nil error means the target accepted it.
    Send(ctx context.Context, h model.Webhook, ev model.Event, attempt int) error
}
func ForKind(kind string) (Sender, bool)
```

- `webhookSender` — today's `Dispatcher.post` moved verbatim: JSON body, `X-Outlook-Event`, `X-Outlook-Delivery`, `X-Outlook-Signature` (HMAC-SHA256 when a secret is set), 15 s timeout, 2xx = success.
- `discordSender` — `POST h.URL` with `{"content": text, "allowed_mentions": {"parse": []}}`; 2xx = success (Discord answers 204). Any other status is an error; `429` is an ordinary failure so the standard retry schedule applies. `content` is truncated to 2,000 runes with a trailing `…`.
- `telegramSender` — `POST <base>/bot<token>/sendMessage` with `{"chat_id", "text", "parse_mode":"HTML", "disable_web_page_preview": true}`; success iff HTTP 2xx and body `ok:true`; otherwise the error carries Telegram's `description`. `text` truncated to 4,096 runes. `base` defaults to `https://api.telegram.org` and is injectable for tests.

`Dispatcher.deliver` selects the sender by `hook.Kind` and otherwise keeps its flow: first attempt in-memory, failure → `schedule` → `webhook_deliveries` with the existing schedule (30s, 2m, 10m, 30m, 2h, 6h, 12h, then dead), `GET /webhooks/{id}/deliveries` unchanged. `Emit` back-pressure and the dropped counter are untouched.

### Error scrubbing

Errors and log lines never contain a Telegram bot token or the token segment of a Discord URL:

- Telegram: any URL in an error is rewritten to `https://api.telegram.org/bot•••/<method>`.
- Discord: `/api/webhooks/<id>/<token>` is logged and stored as `/api/webhooks/<id>/•••`.

`last_error` passes through the same scrubber before `SaveDelivery`. `logx.secretKeys` gains `bot_token`.

## 5. Formatting

`internal/notify/format.go`:

```go
type Flavour int // Markdown (Discord) | HTML (Telegram)
func Format(ev model.Event, f Flavour) string
```

One notification per event; the same content in two escapers (Discord Markdown special characters escaped: `* _ ~ ` | > #`; Telegram HTML: `< > &`). Phone numbers are masked (`+91 88••• •855`, same rule as the dashboard). Snippets are cut at a rune boundary with `…`.

| Event | Text (Markdown flavour shown) |
|---|---|
| `mail_received` | `📧 **New mail** · <account email>` / `From: Name <addr>` / `**Subject**` / first 200 chars of `body_plain` (fallback `snippet`) |
| `mail_sent` | `📤 **Mail sent** · <account>` / `To: …` / `**Subject**` |
| `mail_updated` | `✏️ **Mail updated** · <account> — **Subject** (read/flagged state)` |
| `mail_deleted` | `🗑 **Mail deleted** · <account> — id` |
| `chat_received` / `chat_sent` | `💬 **WhatsApp** · <chat name or masked phone>` / `**Sender name or masked phone**: first 300 chars` (`[image]`-style placeholders pass through) |
| `chat_updated` | `✏️ **Message edited** · <chat>` / new text (300 chars) — or `📬 delivered/read` for receipt updates |
| `chat_reaction` | `👍 **Reaction** · <chat> — <emoji> by <sender>` (emoji removed → `reaction removed`) |
| `chat_deleted` | `🗑 **Message deleted** · <chat>` |
| `account_status` | `⚠️ **Account needs attention** · <account> → CREDENTIALS — relink from the dashboard` |

Unknown event types render as `<type> · <account_id>` so a future event never fails to deliver.

## 6. Security

- Bot tokens: sealed at rest, never in responses, never in logs, scrubbed from errors.
- Fixed outbound hosts: Telegram → `api.telegram.org` only; Discord → `discord.com`/`discordapp.com` only; the generic webhook keeps the literal-IP SSRF check.
- Content is escaped per flavour; Discord mentions disabled via `allowed_mentions: {parse: []}` so a message body cannot ping `@everyone`.
- Tenancy unchanged: a hook belongs to one developer; account-scoped hooks fire only for that developer's account; cross-tenant ids are 404.

## 7. Configuration

None new. `TOKEN_ENCRYPTION_KEY` (already required) seals Telegram config. No Telegram/Discord environment variables — every credential comes from the developer.

## 8. Testing

- **Formatter**: table test per event type × flavour; escaping of `*`, `<`, `&`; truncation; phone masking; unknown event fallback.
- **Senders** (`httptest` fakes): webhook headers/HMAC unchanged; Discord 204 → ok, 429/400/500 → error with status; Telegram `ok:true` → ok, `ok:false` → error with description; token never present in `err.Error()` or captured logs.
- **API**: create/list/get/delete for each kind; validation matrix (bad kind, missing fields, foreign fields, bad Discord host); `bot_token` absent from every response; `secret` only at creation for `webhook`; Telegram `getChat` failure → 400; existing isolation test rows stay green; kind-aware default filter still applies.
- **Store**: round-trip seal/unseal of Telegram config; legacy row without `kind` reads as `webhook`.
- **Dispatcher**: one event, three hooks (webhook/discord/telegram) → JSON body, Markdown text, HTML text respectively; Discord 429 → scheduled retry; retry schedule and dead-lettering unchanged.
- **Manual** (`docs/delivery-targets-manual-checklist.md`): create a Discord hook against a real channel, send a WhatsApp message and mail, see both; same for Telegram; delete a hook; confirm `deliveries` shows a failed attempt after pointing a hook at a dead channel.

## 9. Docs

- `/docs` webhooks section: "Delivery targets" subsection with the three request bodies, the Telegram setup walk-through (BotFather → add the bot to the group/channel → find the chat id via `getUpdates`), Discord's "Integrations → Webhooks → New webhook → copy URL", and what each kind receives.
- `/llms.txt`: Webhook object gains `kind` and `telegram.chat_id`; rules bullet "kind=discord|telegram receive formatted text, not JSON; no signature header"; error notes.
- README: short "Forward to Discord / Telegram" subsection under webhooks.

## 10. Out of scope

Two-way replies, media forwarding, per-hook templates, Slack/other targets (the `Sender` seam makes them additive later), a shared service-run Telegram bot.
