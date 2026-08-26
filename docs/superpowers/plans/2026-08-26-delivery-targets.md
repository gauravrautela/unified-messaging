# Delivery Targets (Discord / Telegram) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let a developer forward events as human-readable notifications to a Discord channel or a Telegram chat, using the existing webhook resource with a new `kind`.

**Architecture:** `model.Webhook` gains `Kind` (`webhook` | `discord` | `telegram`) and a sealed Telegram target; a new `internal/notify` package owns a `Sender` per kind plus the shared formatter; `events.Dispatcher` picks the sender by kind and keeps its retry/dead-letter machinery unchanged. The API validates per kind, the dashboard form gets a kind selector, docs describe the three bodies.

**Tech Stack:** Go 1.26 stdlib only (`net/http`, `encoding/json`, `html`, `crypto/*`), existing `internal/secretbox` (AES-GCM) for the Telegram config, `modernc.org/sqlite`.

**Spec:** `docs/superpowers/specs/2026-08-26-delivery-targets-design.md`

## Global Constraints

- No new dependencies. `gofmt -l internal cmd` empty; `go vet ./...` clean; `go test ./...` green after every task.
- TDD: failing test first, show RED, then GREEN.
- Kind values are exactly `"webhook"`, `"discord"`, `"telegram"`; default `"webhook"`.
- Telegram `bot_token` is never returned by the API, never logged, scrubbed from every error string; stored sealed with `TOKEN_ENCRYPTION_KEY` via `secretbox`.
- Discord URL host must be `discord.com` or `discordapp.com` with path prefix `/api/webhooks/`; in logs and `last_error` the token segment is masked as `/api/webhooks/<id>/•••`.
- Telegram requests only go to `api.telegram.org` (base injectable for tests). Telegram URLs in errors/logs are rewritten to `https://api.telegram.org/bot•••/<method>`.
- Discord content ≤ 2,000 runes, Telegram text ≤ 4,096 runes, truncated with `…`.
- Discord body always includes `"allowed_mentions": {"parse": []}`.
- Phone numbers in notifications are masked: keep `+CC` and the first two digits, mask the middle, keep the last three (`+91 88••• •855`).
- Existing behaviour for `kind=webhook` (headers, HMAC, retry schedule, deliveries endpoint, tenancy) must not change.
- Commit trailers on every commit:
  `Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>` and
  `Claude-Session: https://claude.ai/code/session_01RwMaDW9KNcu6BjtbMkU8mo`

---

## File structure

| File | Responsibility |
|---|---|
| `internal/model/model.go` | `Webhook.Kind`, `Webhook.Telegram`, `TelegramTarget`, kind constants |
| `internal/store/schema.go`, `internal/store/aux.go`, `internal/store/store.go` | migrations (`kind`, `config`), sealed config round-trip, `SetSealKey`, `PendingWebhook` kind fields |
| `internal/notify/scrub.go` | `ScrubErr`, `MaskDiscordURL`, `MaskPhone` |
| `internal/notify/format.go` | `Format(ev, flavour)` |
| `internal/notify/sender.go` | `Sender`, `Registry`, webhook/discord/telegram senders, `ValidateTelegram` |
| `internal/events/events.go` | dispatcher uses `Registry.For(kind)` |
| `internal/api/handlers_misc.go`, `handlers_connect.go` | request fields, per-kind validation, Telegram check, response shaping |
| `internal/api/handlers_ui.go` | kind selector in the Set-webhook form, kind badge |
| `internal/api/handlers_docs.go`, `handlers_llms.go`, `README.md`, `docs/delivery-targets-manual-checklist.md` | docs |
| `cmd/server/main.go` | `db.SetSealKey(cfg.TokenKey)`, `events.NewDispatcher(db, notify.NewRegistry(...), log)` |

---

### Task 1: Model and store — `kind`, sealed Telegram config, pending webhook fields

**Files:**
- Modify: `internal/model/model.go` (Webhook struct, ~line 134)
- Modify: `internal/store/schema.go` (migrations var, ~line 238), `internal/store/store.go` (Store struct ~line 22), `internal/store/aux.go` (webhooks ~lines 91–175, PendingWebhook ~line 284)
- Test: `internal/store/webhooks_test.go` (create)

**Interfaces:**
- Produces: `model.WebhookKindWebhook = "webhook"`, `model.WebhookKindDiscord = "discord"`, `model.WebhookKindTelegram = "telegram"`; `model.Webhook{Kind string; Telegram *TelegramTarget}`; `model.TelegramTarget{ChatID string; BotToken string}`; `(*store.Store).SetSealKey(key []byte)`; `store.PendingWebhook{Kind, BotToken, ChatID string}`.
- `URL` JSON tag becomes `json:"url,omitempty"` (spec: absent for telegram).

- [ ] **Step 1: Failing store test**

```go
package store_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/gauravrautela/unified-messaging/internal/model"
	"github.com/gauravrautela/unified-messaging/internal/store"
)

var testKey = []byte("0123456789abcdef0123456789abcdef")

func openWithKey(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	s.SetSealKey(testKey)
	t.Cleanup(func() { s.Close() })
	if err := s.CreateDeveloper(model.Developer{ID: "dev_1", Email: "d@x.com"}, "h"); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestWebhookTelegramConfigRoundTripsSealed(t *testing.T) {
	s := openWithKey(t)
	in := model.Webhook{ID: "wh_1", DeveloperID: "dev_1", Kind: model.WebhookKindTelegram,
		Telegram: &model.TelegramTarget{ChatID: "-100123", BotToken: "123:ABC"},
		Events: []string{"chat_received"}, CreatedAt: time.Now().UTC()}
	if err := s.SaveWebhook(in); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetWebhook("dev_1", "wh_1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != "telegram" || got.Telegram == nil || got.Telegram.ChatID != "-100123" || got.Telegram.BotToken != "123:ABC" {
		t.Fatalf("round trip = %+v (%+v)", got, got.Telegram)
	}
	// The raw column must not contain the token in clear.
	var raw string
	if err := s.DB().QueryRow(`SELECT config FROM webhooks WHERE id = 'wh_1'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if raw == "" || contains(raw, "123:ABC") {
		t.Fatalf("config stored unsealed: %q", raw)
	}
}

func TestWebhookDefaultsToKindWebhookForLegacyRows(t *testing.T) {
	s := openWithKey(t)
	// Simulate a row written before the columns existed: only the old columns.
	if _, err := s.DB().Exec(`INSERT INTO webhooks (id, developer_id, account_id, name, url, secret, events_json, created_at)
		VALUES ('wh_old','dev_1','','','https://h.example.com','','[]',1)`); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetWebhook("dev_1", "wh_old")
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != model.WebhookKindWebhook || got.Telegram != nil {
		t.Fatalf("legacy row = %+v", got)
	}
}

func TestSaveTelegramWebhookWithoutSealKeyFails(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_ = s.CreateDeveloper(model.Developer{ID: "dev_1", Email: "d@x.com"}, "h")
	err = s.SaveWebhook(model.Webhook{ID: "wh_1", DeveloperID: "dev_1", Kind: model.WebhookKindTelegram,
		Telegram: &model.TelegramTarget{ChatID: "1", BotToken: "t"}, CreatedAt: time.Now()})
	if err == nil {
		t.Fatal("expected an error without a seal key")
	}
}

func contains(s, sub string) bool { return len(sub) > 0 && len(s) >= len(sub) && indexOf(s, sub) >= 0 }
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/store/ -run 'TestWebhook|TestSaveTelegram' -v`
Expected: compile errors — `model.WebhookKindTelegram`, `model.TelegramTarget`, `SetSealKey` undefined.

- [ ] **Step 3: Implement**

`internal/model/model.go` — replace the `Webhook` struct:

```go
// Webhook kinds. A "webhook" receives the JSON event; "discord" and
// "telegram" receive a formatted notification (see internal/notify).
const (
	WebhookKindWebhook  = "webhook"
	WebhookKindDiscord  = "discord"
	WebhookKindTelegram = "telegram"
)

// KnownWebhookKind reports whether k is one of the three delivery kinds.
func KnownWebhookKind(k string) bool {
	return k == WebhookKindWebhook || k == WebhookKindDiscord || k == WebhookKindTelegram
}

// TelegramTarget is where a kind=telegram hook posts. The bot token is the
// developer's own credential: sealed at rest, never serialised.
type TelegramTarget struct {
	ChatID   string `json:"chat_id"`
	BotToken string `json:"-"`
}

type Webhook struct {
	ID          string `json:"id"`
	DeveloperID string `json:"-"`
	// Name is a caller-chosen label echoed in every delivery, so one endpoint
	// fed by several hooks can tell them apart.
	Name string `json:"name,omitempty"`
	// AccountID scopes the hook to one connected account. Empty means global:
	// the hook receives events from every account.
	AccountID string `json:"account_id,omitempty"`
	// Kind selects the transport and payload shape; see WebhookKind*.
	Kind string `json:"kind"`
	// URL is the developer endpoint (webhook) or the Discord incoming-webhook
	// URL (discord); unused for telegram.
	URL       string          `json:"url,omitempty"`
	Secret    string          `json:"secret,omitempty"`
	Telegram  *TelegramTarget `json:"telegram,omitempty"`
	Events    []string        `json:"events"`
	CreatedAt time.Time       `json:"created_at"`
}
```

`internal/store/schema.go` — append to `migrations`:

```go
	`ALTER TABLE webhooks ADD COLUMN kind TEXT NOT NULL DEFAULT 'webhook'`,
	`ALTER TABLE webhooks ADD COLUMN config TEXT NOT NULL DEFAULT ''`,
```

and add the two columns to the `CREATE TABLE webhooks` block (after `events_json`): `kind TEXT NOT NULL DEFAULT 'webhook',` and `config TEXT NOT NULL DEFAULT '',`.

`internal/store/store.go` — add a field and setter:

```go
type Store struct {
	db  *sql.DB
	log *slog.Logger
	// sealKey seals per-hook credentials (a Telegram bot token). Nil until
	// SetSealKey; saving a hook that needs sealing without it is an error.
	sealKey []byte
}

// SetSealKey installs the key used to seal per-hook credentials at rest. It
// is the same TOKEN_ENCRYPTION_KEY the account manager uses for OAuth tokens.
func (s *Store) SetSealKey(key []byte) { s.sealKey = key }
```

`internal/store/aux.go` — webhooks section:

```go
const webhookSelect = `SELECT id, developer_id, account_id, name, url, secret, events_json, created_at, kind, config FROM webhooks`

// webhookConfig is the sealed part of a hook: credentials that must not sit
// in the row in clear. Only telegram hooks have one today.
type webhookConfig struct {
	BotToken string `json:"bot_token,omitempty"`
	ChatID   string `json:"chat_id,omitempty"`
}

var errNoSealKey = errors.New("store: seal key not set")

func (s *Store) SaveWebhook(w model.Webhook) error {
	if w.Kind == "" {
		w.Kind = model.WebhookKindWebhook
	}
	ev, _ := json.Marshal(w.Events)
	config := ""
	if w.Kind == model.WebhookKindTelegram && w.Telegram != nil {
		if s.sealKey == nil {
			return errNoSealKey
		}
		raw, _ := json.Marshal(webhookConfig{BotToken: w.Telegram.BotToken, ChatID: w.Telegram.ChatID})
		sealed, err := secretbox.Seal(s.sealKey, string(raw))
		if err != nil {
			return err
		}
		config = sealed
	}
	_, err := s.db.Exec(`
		INSERT INTO webhooks (id, developer_id, account_id, name, url, secret, events_json, created_at, kind, config)
		VALUES (?,?,?,?,?,?,?,?,?,?)`,
		w.ID, w.DeveloperID, w.AccountID, w.Name, w.URL, w.Secret, string(ev), w.CreatedAt.Unix(), w.Kind, config)
	return err
}
```

and in `queryWebhooks` scan the two extra columns and unseal:

```go
		var ev, kind, config string
		var created int64
		if err := rows.Scan(&w.ID, &w.DeveloperID, &w.AccountID, &w.Name, &w.URL, &w.Secret, &ev, &created, &kind, &config); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(ev), &w.Events)
		w.CreatedAt = time.Unix(created, 0).UTC()
		w.Kind = kind
		if w.Kind == "" {
			w.Kind = model.WebhookKindWebhook
		}
		if w.Kind == model.WebhookKindTelegram {
			w.Telegram = &model.TelegramTarget{}
			if config != "" && s.sealKey != nil {
				if raw, err := secretbox.Open(s.sealKey, config); err == nil {
					var c webhookConfig
					_ = json.Unmarshal([]byte(raw), &c)
					w.Telegram.BotToken, w.Telegram.ChatID = c.BotToken, c.ChatID
				} else if s.log != nil {
					// Wrong key or corrupt row: keep the hook listable, deliveries
					// to it fail with a clear error (see notify.telegramSender).
					s.log.Warn("webhook config unreadable", "webhook_id", w.ID)
				}
			}
		}
```

Add the imports `errors` and `github.com/gauravrautela/unified-messaging/internal/secretbox` to `aux.go`. Extend `PendingWebhook`:

```go
type PendingWebhook struct {
	Name     string   `json:"name,omitempty"`
	Kind     string   `json:"kind,omitempty"`
	URL      string   `json:"url,omitempty"`
	Secret   string   `json:"secret,omitempty"`
	BotToken string   `json:"bot_token,omitempty"`
	ChatID   string   `json:"chat_id,omitempty"`
	Events   []string `json:"events,omitempty"`
}
```

The pending webhook is stored inside `oauth_states.webhook_json` (a 30-minute row) — encode it with `secretbox.Seal` when `BotToken != ""` and the seal key is set: change `encodePendingWebhook`/`decodePendingWebhook` into store methods `s.encodePendingWebhook`/`s.decodePendingWebhook` that seal the whole JSON when it carries a bot token (prefix the stored string with `sealed:`) and open it on read; leave unsealed JSON as-is for the other kinds. Update their two call sites in `aux.go`.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/store/ ./internal/api/ ./internal/events/`
Expected: PASS (existing tests unaffected: `Kind` is filled on read; JSON `url` omitted only when empty).

- [ ] **Step 5: Commit**

```bash
git add internal/model/model.go internal/store
git commit -m "feat(store): webhook kind and sealed telegram config"
```

---

### Task 2: `internal/notify` — scrubbing, masking and the formatter

**Files:**
- Create: `internal/notify/scrub.go`, `internal/notify/format.go`
- Test: `internal/notify/scrub_test.go`, `internal/notify/format_test.go`

**Interfaces:**
- Produces: `notify.Flavour` (`notify.Markdown`, `notify.HTML`); `notify.Format(ev model.Event, f Flavour) string`; `notify.MaskPhone(s string) string`; `notify.MaskDiscordURL(u string) string`; `notify.ScrubErr(err error) error` (also `notify.Scrub(s string) string`).
- Consumes: `model.Event` fields `Type, AccountID, Email, EmailID, Account, Message, Chat, Status, Change, Reaction`; `model.Email{Subject, From, To, Snippet, BodyPlain, Read, Flagged}`; `model.ChatMessage{Sender, Text, Kind}`; `model.Chat{Name, Kind}`; `model.Attendee{Name, Phone}`; `model.Reaction{Emoji}`; `model.Account{Email, Status}`.

- [ ] **Step 1: Failing tests**

`internal/notify/scrub_test.go`:

```go
package notify

import (
	"errors"
	"strings"
	"testing"
)

func TestMaskPhone(t *testing.T) {
	cases := map[string]string{
		"+919888000855": "+91 98••• •855",
		"+15551234567":  "+1 55••• •567",
		"12345":         "12•••",
		"":              "",
	}
	for in, want := range cases {
		if got := MaskPhone(in); got != want {
			t.Errorf("MaskPhone(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMaskDiscordURL(t *testing.T) {
	got := MaskDiscordURL("https://discord.com/api/webhooks/1234567890/AbCdEf-ghIJ_kl")
	if got != "https://discord.com/api/webhooks/1234567890/•••" {
		t.Fatalf("got %q", got)
	}
	if MaskDiscordURL("https://x.example.com/hook") != "https://x.example.com/hook" {
		t.Fatal("non-discord URL must pass through")
	}
}

func TestScrubErrHidesTelegramTokenAndDiscordToken(t *testing.T) {
	err := ScrubErr(errors.New(`Post "https://api.telegram.org/bot123456:ABC-def_GHI/sendMessage": dial tcp: timeout`))
	if strings.Contains(err.Error(), "123456:ABC") || !strings.Contains(err.Error(), "bot•••/sendMessage") {
		t.Fatalf("telegram not scrubbed: %v", err)
	}
	err = ScrubErr(errors.New("status 404 from https://discord.com/api/webhooks/42/s3cr3t-token"))
	if strings.Contains(err.Error(), "s3cr3t") || !strings.Contains(err.Error(), "/api/webhooks/42/•••") {
		t.Fatalf("discord not scrubbed: %v", err)
	}
	if ScrubErr(nil) != nil {
		t.Fatal("nil stays nil")
	}
}
```

`internal/notify/format_test.go`:

```go
package notify

import (
	"strings"
	"testing"
	"time"

	"github.com/gauravrautela/unified-messaging/internal/model"
)

func TestFormatMailReceivedMarkdownAndHTML(t *testing.T) {
	ev := model.Event{Type: model.EventMailReceived, AccountID: "acc_1", Account: &model.Account{Email: "me@x.com"},
		Email: &model.Email{Subject: "Q3 *plan*", From: model.Recipient{Name: "Bob <b>", Email: "bob@x.com"},
			BodyPlain: strings.Repeat("a", 250)}}
	md := Format(ev, Markdown)
	if !strings.Contains(md, "📧 **New mail** · me@x.com") || !strings.Contains(md, `Q3 \*plan\*`) ||
		!strings.Contains(md, "Bob <b> <bob@x.com>") || !strings.HasSuffix(md, strings.Repeat("a", 200)+"…") {
		t.Fatalf("markdown:\n%s", md)
	}
	h := Format(ev, HTML)
	if !strings.Contains(h, "<b>New mail</b>") || !strings.Contains(h, "Bob &lt;b&gt; &lt;bob@x.com&gt;") || strings.Contains(h, "<b>Q3") {
		t.Fatalf("html:\n%s", h)
	}
}

func TestFormatChatReceivedMasksPhonesAndEscapes(t *testing.T) {
	ev := model.Event{Type: model.EventChatReceived, AccountID: "acc_1",
		Chat:    &model.Chat{Name: "", Kind: "direct"},
		Message: &model.ChatMessage{Sender: model.Attendee{Phone: "+919888000855"}, Text: "hi _there_ <3"}}
	md := Format(ev, Markdown)
	if strings.Contains(md, "9888000855") || !strings.Contains(md, "+91 98••• •855") || !strings.Contains(md, `hi \_there\_ <3`) {
		t.Fatalf("markdown:\n%s", md)
	}
	h := Format(ev, HTML)
	if !strings.Contains(h, "hi _there_ &lt;3") {
		t.Fatalf("html:\n%s", h)
	}
}

func TestFormatOtherEvents(t *testing.T) {
	at := time.Now()
	cases := []struct {
		ev   model.Event
		want string
	}{
		{model.Event{Type: model.EventMailSent, Email: &model.Email{Subject: "S", To: []model.Recipient{{Email: "a@x.com"}}}}, "📤 **Mail sent**"},
		{model.Event{Type: model.EventMailUpdated, Email: &model.Email{Subject: "S", Read: true}}, "✏️ **Mail updated**"},
		{model.Event{Type: model.EventMailDeleted, EmailID: "m1"}, "🗑 **Mail deleted**"},
		{model.Event{Type: model.EventChatUpdated, Change: "edited", Chat: &model.Chat{Name: "Team"}, Message: &model.ChatMessage{Text: "new"}}, "✏️ **Message edited**"},
		{model.Event{Type: model.EventChatUpdated, Change: "receipt", Status: "read", Chat: &model.Chat{Name: "Team"}}, "📬"},
		{model.Event{Type: model.EventChatReaction, Chat: &model.Chat{Name: "Team"}, Reaction: &model.Reaction{Emoji: "👍", At: at}, Message: &model.ChatMessage{Sender: model.Attendee{Name: "Ada"}}}, "👍 **Reaction**"},
		{model.Event{Type: model.EventChatReaction, Chat: &model.Chat{Name: "Team"}, Reaction: &model.Reaction{Emoji: "", At: at}}, "reaction removed"},
		{model.Event{Type: model.EventChatDeleted, Chat: &model.Chat{Name: "Team"}}, "🗑 **Message deleted**"},
		{model.Event{Type: model.EventAccountStatus, Account: &model.Account{Email: "me@x.com", Status: model.AccountCredentials}}, "⚠️ **Account needs attention**"},
		{model.Event{Type: "something_new", AccountID: "acc_9"}, "something_new · acc_9"},
	}
	for _, c := range cases {
		if got := Format(c.ev, Markdown); !strings.Contains(got, c.want) {
			t.Errorf("%s: got %q, want it to contain %q", c.ev.Type, got, c.want)
		}
	}
}
```

Check the exact constant names (`model.EventMailSent`, `EventMailUpdated`, `EventMailDeleted`, `EventAccountStatus`, `EventChatUpdated`, `EventChatReaction`, `EventChatDeleted`, `model.AccountCredentials`) in `internal/model/model.go` and `chat.go` before running; use the real names.

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/notify/`
Expected: FAIL to compile (package has no source files yet).

- [ ] **Step 3: Implement**

`internal/notify/scrub.go`:

```go
// Package notify turns events into notifications for chat targets (Discord,
// Telegram) and delivers them, next to the raw JSON webhook.
package notify

import (
	"fmt"
	"regexp"
	"strings"
)

var (
	telegramToken = regexp.MustCompile(`/bot[^/\s"]+/`)
	discordToken  = regexp.MustCompile(`(/api/webhooks/\d+)/[^/\s"?]+`)
)

// Scrub removes credentials that transports embed in URLs: the Telegram bot
// token path segment and the Discord webhook token.
func Scrub(s string) string {
	s = telegramToken.ReplaceAllString(s, "/bot•••/")
	return discordToken.ReplaceAllString(s, "$1/•••")
}

// ScrubErr is Scrub for errors; nil stays nil.
func ScrubErr(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s", Scrub(err.Error()))
}

// MaskDiscordURL hides the token segment of a Discord incoming-webhook URL
// for logs; other URLs pass through unchanged.
func MaskDiscordURL(u string) string { return discordToken.ReplaceAllString(u, "$1/•••") }

// MaskPhone keeps the country code and first two digits plus the last three:
// +919888000855 -> "+91 98••• •855". Short or odd values keep their first
// two characters only.
func MaskPhone(p string) string {
	if p == "" {
		return ""
	}
	digits := strings.TrimPrefix(p, "+")
	if len(digits) < 8 {
		if len(digits) <= 2 {
			return p
		}
		return p[:len(p)-len(digits)+2] + "•••"
	}
	cc := ""
	rest := digits
	// Country codes are 1–3 digits; take 1 for +1, 2 otherwise (good enough
	// for a notification — this is display, not parsing).
	if strings.HasPrefix(p, "+") {
		n := 2
		if strings.HasPrefix(digits, "1") {
			n = 1
		}
		cc, rest = "+"+digits[:n], digits[n:]
	}
	if len(rest) < 5 {
		return cc + " " + rest[:1] + "•••"
	}
	return strings.TrimSpace(cc + " " + rest[:2] + "••• •" + rest[len(rest)-3:])
}
```

`internal/notify/format.go`:

```go
package notify

import (
	"fmt"
	"html"
	"strings"
	"unicode/utf8"

	"github.com/gauravrautela/unified-messaging/internal/model"
)

// Flavour is the markup a target understands.
type Flavour int

const (
	Markdown Flavour = iota // Discord
	HTML                    // Telegram (parse_mode=HTML)
)

const (
	mailSnippet = 200
	chatSnippet = 300
)

// Format renders one event as a short notification. Every value that came
// from a user (subject, names, message text) is escaped for the flavour;
// phone numbers are masked. Unknown event types still render, so a new
// event never blocks delivery.
func Format(ev model.Event, f Flavour) string {
	b := &builder{f: f}
	acct := ev.AccountID
	if ev.Account != nil && ev.Account.Email != "" {
		acct = ev.Account.Email
	}
	switch ev.Type {
	case model.EventMailReceived:
		b.head("📧", "New mail", acct)
		if ev.Email != nil {
			b.line("From: " + recipient(ev.Email.From))
			b.bold(ev.Email.Subject)
			body := ev.Email.BodyPlain
			if body == "" {
				body = ev.Email.Snippet
			}
			b.snippet(body, mailSnippet)
		}
	case model.EventMailSent:
		b.head("📤", "Mail sent", acct)
		if ev.Email != nil {
			b.line("To: " + recipients(ev.Email.To))
			b.bold(ev.Email.Subject)
		}
	case model.EventMailUpdated:
		b.head("✏️", "Mail updated", acct)
		if ev.Email != nil {
			b.bold(ev.Email.Subject)
			b.line(fmt.Sprintf("read: %v · flagged: %v", ev.Email.Read, ev.Email.Flagged))
		}
	case model.EventMailDeleted:
		b.head("🗑", "Mail deleted", acct)
		b.line("id " + ev.EmailID)
	case model.EventChatReceived, model.EventChatSent:
		b.head("💬", "WhatsApp", chatName(ev))
		if ev.Message != nil {
			b.boldInline(attendee(ev.Message.Sender), ": ")
			b.snippetInline(ev.Message.Text, chatSnippet)
		}
	case model.EventChatUpdated:
		switch ev.Change {
		case "receipt":
			b.head("📬", "Message "+ev.Status, chatName(ev))
		default:
			b.head("✏️", "Message edited", chatName(ev))
			if ev.Message != nil {
				b.snippet(ev.Message.Text, chatSnippet)
			}
		}
	case model.EventChatReaction:
		b.head("👍", "Reaction", chatName(ev))
		who := ""
		if ev.Message != nil {
			who = attendee(ev.Message.Sender)
		}
		if ev.Reaction == nil || ev.Reaction.Emoji == "" {
			b.line("reaction removed" + by(who))
		} else {
			b.line(ev.Reaction.Emoji + by(who))
		}
	case model.EventChatDeleted:
		b.head("🗑", "Message deleted", chatName(ev))
	case model.EventAccountStatus:
		status := ""
		if ev.Account != nil {
			status = ev.Account.Status
		}
		b.head("⚠️", "Account needs attention", acct)
		b.line("→ " + status + " — relink from the dashboard")
	default:
		b.line(ev.Type + " · " + ev.AccountID)
	}
	return strings.TrimRight(b.sb.String(), "\n")
}

func by(who string) string {
	if who == "" {
		return ""
	}
	return " by " + who
}

func chatName(ev model.Event) string {
	if ev.Chat != nil && ev.Chat.Name != "" {
		return ev.Chat.Name
	}
	if ev.Message != nil {
		return attendee(ev.Message.Sender)
	}
	return ev.AccountID
}

func attendee(a model.Attendee) string {
	if a.Name != "" {
		return a.Name
	}
	if a.Phone != "" {
		return MaskPhone(a.Phone)
	}
	return a.ID
}

func recipient(r model.Recipient) string {
	if r.Name != "" {
		return r.Name + " <" + r.Email + ">"
	}
	return r.Email
}

func recipients(rs []model.Recipient) string {
	parts := make([]string, 0, len(rs))
	for _, r := range rs {
		parts = append(parts, recipient(r))
	}
	return strings.Join(parts, ", ")
}

// builder writes lines in one flavour; every user value passes through esc.
type builder struct {
	f  Flavour
	sb strings.Builder
}

func (b *builder) esc(s string) string {
	switch b.f {
	case HTML:
		return html.EscapeString(s)
	default:
		return escapeMarkdown(s)
	}
}

func (b *builder) strong(s string) string {
	if b.f == HTML {
		return "<b>" + b.esc(s) + "</b>"
	}
	return "**" + b.esc(s) + "**"
}

func (b *builder) head(icon, title, where string) {
	b.sb.WriteString(icon + " " + b.strong(title) + " · " + b.esc(where) + "\n")
}
func (b *builder) line(s string)          { b.sb.WriteString(b.esc(s) + "\n") }
func (b *builder) bold(s string)          { b.sb.WriteString(b.strong(s) + "\n") }
func (b *builder) boldInline(s, sep string) { b.sb.WriteString(b.strong(s) + sep) }
func (b *builder) snippet(s string, n int) { b.sb.WriteString(b.esc(truncate(s, n)) + "\n") }
func (b *builder) snippetInline(s string, n int) {
	b.sb.WriteString(b.esc(truncate(s, n)) + "\n")
}

// truncate cuts at n runes, appending an ellipsis when it cut.
func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n]) + "…"
}

var mdReplacer = strings.NewReplacer(`\`, `\\`, "*", `\*`, "_", `\_`, "~", `\~`, "`", "\\`", "|", `\|`, ">", `\>`, "#", `\#`)

func escapeMarkdown(s string) string { return mdReplacer.Replace(s) }
```

Adjust the test expectations only if a real constant name differs; never weaken the assertions.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/notify/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/notify
git commit -m "feat(notify): notification formatter, phone masking and credential scrubbing"
```

---

### Task 3: `internal/notify` senders — webhook (moved), Discord, Telegram, Telegram validation

**Files:**
- Create: `internal/notify/sender.go`
- Test: `internal/notify/sender_test.go`

**Interfaces:**
- Produces:
  ```go
  type Sender interface {
      Send(ctx context.Context, h model.Webhook, ev model.Event, payload []byte, attempt int) error
  }
  type Registry struct { /* client, telegramBase */ }
  func NewRegistry(client *http.Client) *Registry
  func (r *Registry) SetTelegramBase(base string)         // tests
  func (r *Registry) For(kind string) (Sender, bool)
  func (r *Registry) ValidateTelegram(ctx context.Context, botToken, chatID string) error
  var ErrTelegramRejected = errors.New("telegram rejected the target")   // wraps description; API maps to 400
  ```
  `payload` is the JSON event the dispatcher already marshals (used verbatim by the webhook sender; ignored by the others, which call `Format`).
- Consumes: `Format`, `Scrub`, `MaskDiscordURL` from Task 2.

- [ ] **Step 1: Failing tests**

```go
package notify

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gauravrautela/unified-messaging/internal/model"
)

func capture(t *testing.T, code int, body string) (*httptest.Server, *[]map[string]any, *http.Header) {
	t.Helper()
	var got []map[string]any
	var hdr http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		m["_path"] = r.URL.Path
		got = append(got, m)
		hdr = r.Header.Clone()
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &got, &hdr
}

var chatEv = model.Event{Type: model.EventChatReceived, AccountID: "acc_1",
	Chat: &model.Chat{Name: "Team"}, Message: &model.ChatMessage{Sender: model.Attendee{Name: "Ada"}, Text: "hello *world*"}}

func TestWebhookSenderKeepsHeadersAndSignature(t *testing.T) {
	srv, got, hdr := capture(t, 200, "")
	s, _ := NewRegistry(srv.Client()).For(model.WebhookKindWebhook)
	h := model.Webhook{Kind: model.WebhookKindWebhook, URL: srv.URL, Secret: "s3"}
	if err := s.Send(context.Background(), h, chatEv, []byte(`{"type":"chat_received"}`), 2); err != nil {
		t.Fatal(err)
	}
	if (*got)[0]["type"] != "chat_received" || hdr.Get("X-Outlook-Event") != "chat_received" ||
		hdr.Get("X-Outlook-Delivery") != "2" || !strings.HasPrefix(hdr.Get("X-Outlook-Signature"), "sha256=") {
		t.Fatalf("got %v headers %v", *got, *hdr)
	}
}

func TestDiscordSenderPostsFormattedContentWithoutMentions(t *testing.T) {
	srv, got, _ := capture(t, 204, "")
	reg := NewRegistry(srv.Client())
	s, _ := reg.For(model.WebhookKindDiscord)
	err := s.Send(context.Background(), model.Webhook{Kind: model.WebhookKindDiscord, URL: srv.URL + "/api/webhooks/1/tok"}, chatEv, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	m := (*got)[0]
	content, _ := m["content"].(string)
	if !strings.Contains(content, `hello \*world\*`) || !strings.Contains(content, "**Ada**") {
		t.Fatalf("content = %q", content)
	}
	am, _ := m["allowed_mentions"].(map[string]any)
	if parse, ok := am["parse"].([]any); !ok || len(parse) != 0 {
		t.Fatalf("allowed_mentions = %v", m["allowed_mentions"])
	}
}

func TestDiscordSenderTreats429AsFailureAndScrubsURL(t *testing.T) {
	srv, _, _ := capture(t, 429, `{"retry_after":1.2}`)
	s, _ := NewRegistry(srv.Client()).For(model.WebhookKindDiscord)
	err := s.Send(context.Background(), model.Webhook{Kind: model.WebhookKindDiscord, URL: srv.URL + "/api/webhooks/1/s3cr3t"}, chatEv, nil, 1)
	if err == nil || !strings.Contains(err.Error(), "429") || strings.Contains(err.Error(), "s3cr3t") {
		t.Fatalf("err = %v", err)
	}
}

func TestTelegramSenderUsesBotTokenPathAndHTML(t *testing.T) {
	srv, got, _ := capture(t, 200, `{"ok":true}`)
	reg := NewRegistry(srv.Client())
	reg.SetTelegramBase(srv.URL)
	s, _ := reg.For(model.WebhookKindTelegram)
	h := model.Webhook{Kind: model.WebhookKindTelegram, Telegram: &model.TelegramTarget{BotToken: "123:ABC", ChatID: "-100"}}
	if err := s.Send(context.Background(), h, chatEv, nil, 1); err != nil {
		t.Fatal(err)
	}
	m := (*got)[0]
	if m["_path"] != "/bot123:ABC/sendMessage" || m["chat_id"] != "-100" || m["parse_mode"] != "HTML" ||
		m["disable_web_page_preview"] != true || !strings.Contains(m["text"].(string), "<b>Ada</b>") {
		t.Fatalf("request = %v", m)
	}
}

func TestTelegramSenderErrorsCarryDescriptionNotToken(t *testing.T) {
	srv, _, _ := capture(t, 400, `{"ok":false,"description":"Bad Request: chat not found"}`)
	reg := NewRegistry(srv.Client())
	reg.SetTelegramBase(srv.URL)
	s, _ := reg.For(model.WebhookKindTelegram)
	h := model.Webhook{Kind: model.WebhookKindTelegram, Telegram: &model.TelegramTarget{BotToken: "123:ABC", ChatID: "-100"}}
	err := s.Send(context.Background(), h, chatEv, nil, 1)
	if err == nil || !strings.Contains(err.Error(), "chat not found") || strings.Contains(err.Error(), "123:ABC") {
		t.Fatalf("err = %v", err)
	}
	// A hook whose config could not be unsealed fails clearly, not with a panic.
	err = s.Send(context.Background(), model.Webhook{Kind: model.WebhookKindTelegram, Telegram: &model.TelegramTarget{}}, chatEv, nil, 1)
	if err == nil || !strings.Contains(err.Error(), "config unreadable") {
		t.Fatalf("empty config err = %v", err)
	}
}

func TestValidateTelegram(t *testing.T) {
	ok, _, _ := capture(t, 200, `{"ok":true,"result":{"id":-100}}`)
	reg := NewRegistry(ok.Client())
	reg.SetTelegramBase(ok.URL)
	if err := reg.ValidateTelegram(context.Background(), "1:A", "-100"); err != nil {
		t.Fatal(err)
	}
	bad, _, _ := capture(t, 400, `{"ok":false,"description":"chat not found"}`)
	reg = NewRegistry(bad.Client())
	reg.SetTelegramBase(bad.URL)
	err := reg.ValidateTelegram(context.Background(), "1:A", "-100")
	if !errors.Is(err, ErrTelegramRejected) || !strings.Contains(err.Error(), "chat not found") {
		t.Fatalf("err = %v", err)
	}
	reg = NewRegistry(&http.Client{})
	reg.SetTelegramBase("http://127.0.0.1:1")
	if err := reg.ValidateTelegram(context.Background(), "1:A", "-100"); err == nil || errors.Is(err, ErrTelegramRejected) {
		t.Fatalf("unreachable must be a transport error, got %v", err)
	}
}

func TestUnknownKind(t *testing.T) {
	if _, ok := NewRegistry(&http.Client{}).For("slack"); ok {
		t.Fatal("slack is not a kind")
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/notify/ -run 'Sender|Validate|UnknownKind'`
Expected: compile failure — `NewRegistry`, `Sender` undefined.

- [ ] **Step 3: Implement `internal/notify/sender.go`**

```go
package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gauravrautela/unified-messaging/internal/model"
)

// Sender delivers one event to one hook. payload is the JSON event as the
// dispatcher encoded it; only the raw webhook uses it, the chat targets
// render their own text with Format. A nil error means the target accepted
// the notification; any error is retried on the dispatcher's schedule and
// must already be scrubbed of credentials.
type Sender interface {
	Send(ctx context.Context, h model.Webhook, ev model.Event, payload []byte, attempt int) error
}

// ErrTelegramRejected marks a Telegram "ok": false answer; the wrapped
// message is Telegram's description. Callers map it to a 400.
var ErrTelegramRejected = errors.New("telegram rejected the target")

const (
	discordMax  = 2000
	telegramMax = 4096
	telegramAPI = "https://api.telegram.org"
)

// Registry hands out the Sender for a hook kind. One per process.
type Registry struct {
	client       *http.Client
	telegramBase string
}

func NewRegistry(client *http.Client) *Registry {
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	return &Registry{client: client, telegramBase: telegramAPI}
}

// SetTelegramBase points Telegram calls at a test server.
func (r *Registry) SetTelegramBase(base string) { r.telegramBase = strings.TrimRight(base, "/") }

func (r *Registry) For(kind string) (Sender, bool) {
	switch kind {
	case model.WebhookKindWebhook, "":
		return webhookSender{client: r.client}, true
	case model.WebhookKindDiscord:
		return discordSender{client: r.client}, true
	case model.WebhookKindTelegram:
		return telegramSender{client: r.client, base: r.telegramBase}, true
	}
	return nil, false
}

// ValidateTelegram checks the token/chat pair with getChat. A Telegram
// rejection returns ErrTelegramRejected (wrapped); a transport problem
// returns a plain error.
func (r *Registry) ValidateTelegram(ctx context.Context, botToken, chatID string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err := telegramCall(ctx, r.client, r.telegramBase, botToken, "getChat", map[string]any{"chat_id": chatID})
	return err
}

// ---- webhook: the JSON event, signed ----

type webhookSender struct{ client *http.Client }

func (s webhookSender) Send(ctx context.Context, h model.Webhook, _ model.Event, payload []byte, attempt int) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.URL, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Outlook-Event", eventType(payload))
	req.Header.Set("X-Outlook-Delivery", fmt.Sprintf("%d", attempt))
	if h.Secret != "" {
		mac := hmac.New(sha256.New, []byte(h.Secret))
		mac.Write(payload)
		req.Header.Set("X-Outlook-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}

// eventType reads "type" back out of the encoded payload so the header and
// body can never disagree.
func eventType(payload []byte) string {
	var v struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(payload, &v)
	return v.Type
}

// ---- discord: incoming webhook, Markdown ----

type discordSender struct{ client *http.Client }

func (s discordSender) Send(ctx context.Context, h model.Webhook, ev model.Event, _ []byte, _ int) error {
	body, _ := json.Marshal(map[string]any{
		"content":          truncate(Format(ev, Markdown), discordMax),
		"allowed_mentions": map[string]any{"parse": []string{}},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.URL, bytes.NewReader(body))
	if err != nil {
		return ScrubErr(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return ScrubErr(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return fmt.Errorf("discord: status %d %s", resp.StatusCode, Scrub(strings.TrimSpace(string(snippet))))
	}
	return nil
}

// ---- telegram: Bot API sendMessage, HTML ----

type telegramSender struct {
	client *http.Client
	base   string
}

func (s telegramSender) Send(ctx context.Context, h model.Webhook, ev model.Event, _ []byte, _ int) error {
	if h.Telegram == nil || h.Telegram.BotToken == "" || h.Telegram.ChatID == "" {
		return errors.New("telegram: config unreadable")
	}
	_, err := telegramCall(ctx, s.client, s.base, h.Telegram.BotToken, "sendMessage", map[string]any{
		"chat_id":                  h.Telegram.ChatID,
		"text":                     truncate(Format(ev, HTML), telegramMax),
		"parse_mode":               "HTML",
		"disable_web_page_preview": true,
	})
	return err
}

type telegramResponse struct {
	OK          bool            `json:"ok"`
	Description string          `json:"description"`
	Result      json.RawMessage `json:"result"`
}

// telegramCall POSTs one Bot API method. Errors never contain the token.
func telegramCall(ctx context.Context, client *http.Client, base, token, method string, params map[string]any) (json.RawMessage, error) {
	body, _ := json.Marshal(params)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/bot"+token+"/"+method, bytes.NewReader(body))
	if err != nil {
		return nil, ScrubErr(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, ScrubErr(err)
	}
	defer resp.Body.Close()
	var tr telegramResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&tr); err != nil {
		return nil, fmt.Errorf("telegram: %s: status %d, unreadable body", method, resp.StatusCode)
	}
	if !tr.OK {
		return nil, fmt.Errorf("%w: %s", ErrTelegramRejected, Scrub(tr.Description))
	}
	return tr.Result, nil
}
```

Note `truncate` comes from `format.go` (Task 2). The `Send` for Discord wraps the whole `Format` output; `truncate` cuts at a rune boundary so a Markdown escape can end mid-pair at worst — acceptable for a notification.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/notify/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/notify
git commit -m "feat(notify): webhook, discord and telegram senders with telegram validation"
```

---

### Task 4: Dispatcher uses the sender registry; logging scrubs targets

**Files:**
- Modify: `internal/events/events.go` (struct/`NewDispatcher` ~lines 39–75; `deliver` ~192; `retryDue` ~267; `post` ~321 — delete), `internal/logx/logx.go:88` (`secretKeys`), `cmd/server/main.go:75`, `internal/api/api_test.go:55,113` (`NewDispatcher` call sites)
- Test: `internal/events/events_test.go`

**Interfaces:**
- `events.NewDispatcher(s *store.Store, senders *notify.Registry, log *slog.Logger) *Dispatcher` — signature change; `nil` senders → `notify.NewRegistry(nil)`.
- `Dispatcher.post` is replaced by `Dispatcher.send(ctx, h, dl, attempt) error` that logs `delivery attempt`/`delivery response` exactly as before but with `"target", targetLabel(h)` instead of `"url"` (webhook: URL; discord: `MaskDiscordURL`; telegram: `telegram:<chat_id>`), and passes the error through `notify.ScrubErr` before returning. `schedule` stores the scrubbed error in `LastError`.
- All test files in `internal/events` and `internal/api` must set `db.SetSealKey(...)` in their store helpers (a fixed 32-byte key).

- [ ] **Step 1: Failing test** (append to `internal/events/events_test.go`)

```go
func TestOneEventReachesEachKindInItsOwnShape(t *testing.T) {
	db := newTestStore(t)
	db.SetSealKey([]byte("0123456789abcdef0123456789abcdef"))
	seedTenant(t, db)
	wh := newReceiver(t, 200)
	discord := newReceiver(t, 204)
	telegram := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		discord.mu.Lock() // reuse a mutex; the assertion below reads bodies under it
		discord.bodies = append(discord.bodies, append([]byte("TG "+r.URL.Path+" "), body...))
		discord.mu.Unlock()
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(telegram.Close)
	now := time.Now().UTC()
	for _, h := range []model.Webhook{
		{ID: "wh_a", DeveloperID: "dev_1", Kind: "webhook", URL: wh.URL, CreatedAt: now},
		{ID: "wh_b", DeveloperID: "dev_1", Kind: "discord", URL: discord.URL + "/api/webhooks/1/t", CreatedAt: now},
		{ID: "wh_c", DeveloperID: "dev_1", Kind: "telegram", Telegram: &model.TelegramTarget{BotToken: "1:A", ChatID: "-5"}, CreatedAt: now},
	} {
		if err := db.SaveWebhook(h); err != nil {
			t.Fatal(err)
		}
	}
	reg := notify.NewRegistry(nil)
	reg.SetTelegramBase(telegram.URL)
	d := NewDispatcher(db, reg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.Start(ctx)
	d.Emit(model.Event{Type: model.EventChatReceived, AccountID: "acc_1",
		Chat: &model.Chat{Name: "Team"}, Message: &model.ChatMessage{Sender: model.Attendee{Name: "Ada"}, Text: "hi"}})
	waitFor(t, func() bool { return wh.count() == 1 && discord.count() >= 1 && len(discord.bodies) >= 2 })
	discord.mu.Lock()
	defer discord.mu.Unlock()
	var sawJSON, sawMarkdown, sawHTML bool
	for _, b := range discord.bodies {
		s := string(b)
		switch {
		case strings.HasPrefix(s, "TG /bot1:A/sendMessage") && strings.Contains(s, "<b>Ada</b>"):
			sawHTML = true
		case strings.Contains(s, "**Ada**") && strings.Contains(s, "allowed_mentions"):
			sawMarkdown = true
		}
	}
	if strings.Contains(string(wh.bodies[0]), `"type":"chat_received"`) {
		sawJSON = true
	}
	if !sawJSON || !sawMarkdown || !sawHTML {
		t.Fatalf("json=%v markdown=%v html=%v bodies=%q", sawJSON, sawMarkdown, sawHTML, discord.bodies)
	}
}

func TestFailedDeliveryStoresScrubbedError(t *testing.T) {
	db := newTestStore(t)
	db.SetSealKey([]byte("0123456789abcdef0123456789abcdef"))
	seedTenant(t, db)
	bad := newReceiver(t, 500)
	if err := db.SaveWebhook(model.Webhook{ID: "wh_d", DeveloperID: "dev_1", Kind: "discord",
		URL: bad.URL + "/api/webhooks/9/topsecret", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	d := NewDispatcher(db, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.Start(ctx)
	d.Emit(model.Event{Type: model.EventMailReceived, AccountID: "acc_1", Email: &model.Email{Subject: "s"}})
	waitFor(t, func() bool {
		dls, _ := db.ListDeliveries("dev_1", "wh_d")
		return len(dls) == 1
	})
	dls, _ := db.ListDeliveries("dev_1", "wh_d")
	if strings.Contains(dls[0].LastError, "topsecret") || !strings.Contains(dls[0].LastError, "500") {
		t.Fatalf("last_error = %q", dls[0].LastError)
	}
}
```

Check the real name/signature of the deliveries lister in `internal/store` (used by `handleListWebhookDeliveries`) and use it. Add the `notify` import and `httptest` if missing.

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/events/ -run 'TestOneEventReachesEachKind|TestFailedDeliveryStoresScrubbedError'`
Expected: compile failure (`NewDispatcher` arity).

- [ ] **Step 3: Implement**

In `events.go`:

```go
type Dispatcher struct {
	store   *store.Store
	senders *notify.Registry
	log     *slog.Logger
	// ... unchanged fields (client removed; senders own the HTTP client)
}

func NewDispatcher(s *store.Store, senders *notify.Registry, log *slog.Logger) *Dispatcher {
	if senders == nil {
		senders = notify.NewRegistry(&http.Client{Timeout: 15 * time.Second})
	}
	return &Dispatcher{store: s, senders: senders, /* rest unchanged */}
}
```

Replace `post` with:

```go
// targetLabel names where a delivery goes without leaking a credential.
func targetLabel(h model.Webhook) string {
	switch h.Kind {
	case model.WebhookKindDiscord:
		return notify.MaskDiscordURL(h.URL)
	case model.WebhookKindTelegram:
		if h.Telegram != nil {
			return "telegram:" + h.Telegram.ChatID
		}
		return "telegram:?"
	}
	return h.URL
}

// send performs one attempt through the sender for the hook's kind.
func (d *Dispatcher) send(ctx context.Context, h model.Webhook, dl store.Delivery, attempt int) error {
	log := d.deliveryLog(dl, h.DeveloperID).With("attempt", attempt, "kind", h.Kind)
	log.Debug("delivery attempt", "target", targetLabel(h), "payload_bytes", len(dl.Payload), "signed", h.Secret != "")
	sender, ok := d.senders.For(h.Kind)
	if !ok {
		return fmt.Errorf("unknown webhook kind %q", h.Kind)
	}
	var ev model.Event
	if err := json.Unmarshal(dl.Payload, &ev); err != nil {
		return err
	}
	start := time.Now()
	err := notify.ScrubErr(sender.Send(ctx, h, ev, dl.Payload, attempt))
	if err != nil {
		log.Debug("delivery response", "dur", time.Since(start).Round(time.Millisecond), "err", err)
		log.Warn("webhook delivery failed", "target", targetLabel(h), "err", err)
		return err
	}
	log.Debug("delivery response", "status", "ok", "dur", time.Since(start).Round(time.Millisecond))
	return nil
}
```

Update the two `d.post(` call sites (in `deliver` and `retryDue`) to `d.send(`. Remove the now-unused imports (`bytes`, `crypto/hmac`, `crypto/sha256`, `encoding/hex`) and the `client` field. In `schedule`, `dl.LastError = notify.ScrubErr(cause).Error()`.

`internal/logx/logx.go:88` — add `"bot_token"` to `secretKeys` (already covered by the `token` substring; add explicitly and note it).

Update call sites: `cmd/server/main.go:75` → `events.NewDispatcher(db, notify.NewRegistry(nil), log)` and add `db.SetSealKey(cfg.TokenKey)` right after `store.Open` succeeds (line ~56). `internal/api/api_test.go:55,113` → `events.NewDispatcher(db, nil, log)` and add `db.SetSealKey([]byte("0123456789abcdef0123456789abcdef"))` after each `store.Open` in the test helpers. Do the same in `internal/events/events_test.go`'s `newTestStore` and in any other package that opens a store and saves webhooks (grep `store.Open(` under `internal/`).

- [ ] **Step 4: Run tests**

Run: `go vet ./... && go test ./internal/events/ ./internal/api/ ./cmd/...`
Expected: PASS; existing `TestPayloadIdentifiesWebhook`, `TestFailedDeliveryIsQueuedAndRetried`, `TestDeliveryIsDeadAfterScheduleExhausted` unchanged and green.

- [ ] **Step 5: Commit**

```bash
git add internal/events internal/logx cmd/server internal/api/api_test.go
git commit -m "feat(events): deliver through a per-kind sender; scrub targets in logs and last_error"
```

---

### Task 5: API — per-kind request validation, Telegram check, response shaping, connect-time hooks

**Files:**
- Modify: `internal/api/handlers_misc.go` (`webhookRequest` ~190, `validate` ~197, `newWebhook` ~222, `createAccountWebhook` ~234, `handleCreateWebhook` ~251, list handlers ~329/373), `internal/api/handlers_connect.go:85-96` (pending hook) and `:314` (bind), `internal/api/handlers_link.go:441` (bind), `internal/api/api.go` (`Server` gets `senders *notify.Registry`; `NewServer` receives it — add a parameter after `dispatcher`), `cmd/server/main.go` (pass the registry), `internal/api/api_test.go` (`newTestServer*` helpers pass a registry whose Telegram base points at a per-test `httptest` server).
- Test: `internal/api/api_test.go`

**Interfaces:**
- `webhookRequest{Name, Kind, URL, Secret, BotToken, ChatID string; Events []string}` with JSON tags `kind`, `bot_token`, `chat_id`.
- `func (r *webhookRequest) normalise()` sets `Kind = "webhook"` when empty.
- `func (r webhookRequest) validate() error` — per kind.
- `func (s *Server) checkTelegram(ctx, req) error` — returns `(400, invalid_webhook, desc)` vs `(502, provider_error, msg)` via the existing `apiError` helper (find how handlers return typed errors; if there is none, return `(status int, code, msg string)`).
- Response shaping: `secret` only in the 201 body for `kind=webhook`; list/get responses blank `Secret` (already do); `Telegram.BotToken` is `json:"-"` so it never serialises.
- `NewServer(cfg, s, reg, a, sy, au, chat, dispatcher, senders, log)`.

- [ ] **Step 1: Failing tests** (append to `api_test.go`)

```go
// telegramStub answers getChat/sendMessage; per-test success or rejection.
func telegramStub(t *testing.T, ok bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ok {
			_, _ = w.Write([]byte(`{"ok":true,"result":{}}`))
			return
		}
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"ok":false,"description":"Bad Request: chat not found"}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestCreateWebhookKinds(t *testing.T) {
	s, _ := newTestServer(t)
	s.senders.SetTelegramBase(telegramStub(t, true).URL)
	h := s.Routes()
	_, key := seedDev(t, s, "a@x.com")
	post := func(body string) (*httptest.ResponseRecorder, map[string]any) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, withKey(httptest.NewRequest(http.MethodPost, "/api/v1/webhooks", strings.NewReader(body)), key))
		var m map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &m)
		return rec, m
	}
	// webhook (default kind) keeps returning the secret once.
	rec, m := post(`{"url":"https://hook.example.com","secret":"s3"}`)
	if rec.Code != 201 || m["kind"] != "webhook" || m["secret"] != "s3" {
		t.Fatalf("webhook: %d %v", rec.Code, m)
	}
	// discord: only its own host, no secret.
	rec, m = post(`{"kind":"discord","url":"https://discord.com/api/webhooks/1/abc"}`)
	if rec.Code != 201 || m["kind"] != "discord" || m["url"] != "https://discord.com/api/webhooks/1/abc" || m["secret"] != nil {
		t.Fatalf("discord: %d %v", rec.Code, m)
	}
	rec, _ = post(`{"kind":"discord","url":"https://evil.example.com/api/webhooks/1/abc"}`)
	if rec.Code != 400 {
		t.Fatalf("discord bad host: %d", rec.Code)
	}
	rec, _ = post(`{"kind":"discord","url":"https://discord.com/api/webhooks/1/abc","secret":"x"}`)
	if rec.Code != 400 {
		t.Fatalf("discord with secret: %d", rec.Code)
	}
	// telegram: token never comes back, url absent, chat_id present.
	rec, m = post(`{"kind":"telegram","bot_token":"123:ABC","chat_id":"-100"}`)
	if rec.Code != 201 || m["kind"] != "telegram" || m["url"] != nil || m["bot_token"] != nil ||
		m["telegram"].(map[string]any)["chat_id"] != "-100" || strings.Contains(rec.Body.String(), "123:ABC") {
		t.Fatalf("telegram: %d %s", rec.Code, rec.Body.String())
	}
	rec, _ = post(`{"kind":"telegram","chat_id":"-100"}`)
	if rec.Code != 400 {
		t.Fatalf("telegram missing token: %d", rec.Code)
	}
	rec, _ = post(`{"kind":"slack","url":"https://hooks.slack.com/x"}`)
	if rec.Code != 400 {
		t.Fatalf("unknown kind: %d", rec.Code)
	}
	// Listing never leaks the token either.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, withKey(httptest.NewRequest(http.MethodGet, "/api/v1/webhooks", nil), key))
	if strings.Contains(rec.Body.String(), "123:ABC") || !strings.Contains(rec.Body.String(), `"chat_id":"-100"`) {
		t.Fatalf("list: %s", rec.Body.String())
	}
}

func TestCreateTelegramWebhookRejectedByTelegramIs400(t *testing.T) {
	s, _ := newTestServer(t)
	s.senders.SetTelegramBase(telegramStub(t, false).URL)
	_, key := seedDev(t, s, "a@x.com")
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, withKey(httptest.NewRequest(http.MethodPost, "/api/v1/webhooks",
		strings.NewReader(`{"kind":"telegram","bot_token":"123:ABC","chat_id":"-100"}`)), key))
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "chat not found") || strings.Contains(rec.Body.String(), "123:ABC") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestHostedAuthCarriesDiscordHookToTheAccount(t *testing.T) {
	// Same flow as TestHostedAuthStoresPendingWebhook, with kind=discord: the
	// pending state must keep the kind and the bound hook must be discord.
	s, db := newTestServer(t)
	dev, key := seedDev(t, s, "a@x.com")
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, withKey(httptest.NewRequest(http.MethodPost, "/api/v1/hosted-auth",
		strings.NewReader(`{"webhook":{"kind":"discord","url":"https://discord.com/api/webhooks/1/abc"}}`)), key))
	if rec.Code != 200 {
		t.Fatalf("hosted-auth: %d %s", rec.Code, rec.Body.String())
	}
	var out struct{ State string }
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	pending, err := db.GetOAuthState(out.State)
	if err != nil {
		t.Fatal(err)
	}
	if pending.Webhook == nil || pending.Webhook.Kind != "discord" {
		t.Fatalf("pending = %+v", pending.Webhook)
	}
	_ = dev
}
```

Use the real OAuth-state getter name from `internal/store/aux.go` (the one `TestHostedAuthStoresPendingWebhook` calls) in the last test.

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/api/ -run 'TestCreateWebhookKinds|TestCreateTelegramWebhookRejected|TestHostedAuthCarriesDiscord'`
Expected: compile failure (`s.senders` undefined) / 400s.

- [ ] **Step 3: Implement**

`handlers_misc.go`:

```go
type webhookRequest struct {
	Name     string   `json:"name,omitempty"`
	Kind     string   `json:"kind,omitempty"`
	URL      string   `json:"url,omitempty"`
	Secret   string   `json:"secret,omitempty"`
	BotToken string   `json:"bot_token,omitempty"`
	ChatID   string   `json:"chat_id,omitempty"`
	Events   []string `json:"events,omitempty"`
}

func (r *webhookRequest) normalise() {
	if r.Kind == "" {
		r.Kind = model.WebhookKindWebhook
	}
}

func (r webhookRequest) validate() error {
	if !model.KnownWebhookKind(r.Kind) {
		return errors.New("kind must be webhook, discord or telegram")
	}
	switch r.Kind {
	case model.WebhookKindWebhook:
		if r.URL == "" {
			return errors.New("url is required")
		}
		if err := publicHTTPURL(r.URL); err != nil {
			return err
		}
		if r.BotToken != "" || r.ChatID != "" {
			return errors.New("bot_token and chat_id apply to kind=telegram only")
		}
	case model.WebhookKindDiscord:
		if err := discordWebhookURL(r.URL); err != nil {
			return err
		}
		if r.Secret != "" {
			return errors.New("secret applies to kind=webhook only")
		}
		if r.BotToken != "" || r.ChatID != "" {
			return errors.New("bot_token and chat_id apply to kind=telegram only")
		}
	case model.WebhookKindTelegram:
		if r.BotToken == "" || r.ChatID == "" {
			return errors.New("bot_token and chat_id are required for kind=telegram")
		}
		if r.URL != "" || r.Secret != "" {
			return errors.New("url and secret do not apply to kind=telegram")
		}
	}
	for _, e := range r.Events {
		if !model.KnownEvent(e) {
			return fmt.Errorf("unknown event %q", e)
		}
	}
	return nil
}

// discordWebhookURL accepts only Discord's own incoming-webhook endpoint.
func discordWebhookURL(raw string) error {
	if raw == "" {
		return errors.New("url is required")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" {
		return errors.New("url must be an https Discord webhook URL")
	}
	host := strings.ToLower(u.Hostname())
	if host != "discord.com" && host != "discordapp.com" {
		return errors.New("url must be on discord.com or discordapp.com")
	}
	if !strings.HasPrefix(u.Path, "/api/webhooks/") {
		return errors.New("url must be a Discord incoming-webhook URL (/api/webhooks/…)")
	}
	return nil
}

// checkTelegram verifies a telegram target once at creation. It returns the
// HTTP status and error code to answer with, or 0 when the target is fine.
func (s *Server) checkTelegram(ctx context.Context, req webhookRequest) (int, string, string) {
	if req.Kind != model.WebhookKindTelegram {
		return 0, "", ""
	}
	err := s.senders.ValidateTelegram(ctx, req.BotToken, req.ChatID)
	switch {
	case err == nil:
		return 0, "", ""
	case errors.Is(err, notify.ErrTelegramRejected):
		return http.StatusBadRequest, "invalid_webhook", err.Error()
	default:
		return http.StatusBadGateway, "provider_error", "telegram unreachable: " + err.Error()
	}
}

func newWebhook(developerID, accountID string, req webhookRequest) (model.Webhook, error) {
	id, err := accounts.NewID("wh")
	if err != nil {
		return model.Webhook{}, err
	}
	w := model.Webhook{
		ID: id, DeveloperID: developerID, AccountID: accountID, Name: req.Name, Kind: req.Kind,
		Events: req.Events, CreatedAt: time.Now().UTC(),
	}
	switch req.Kind {
	case model.WebhookKindWebhook:
		w.URL, w.Secret = req.URL, req.Secret
	case model.WebhookKindDiscord:
		w.URL = req.URL
	case model.WebhookKindTelegram:
		w.Telegram = &model.TelegramTarget{ChatID: req.ChatID, BotToken: req.BotToken}
	}
	return w, nil
}
```

In `handleCreateWebhook` and `handleCreateAccountWebhook`: after `decodeJSON`, call `req.normalise()`, then `validate()`, then `if st, code, msg := s.checkTelegram(r.Context(), req); st != 0 { writeError(w, st, code, msg); return }`. Both create handlers also blank the secret for non-webhook kinds before writing the 201 (`if hook.Kind != model.WebhookKindWebhook { hook.Secret = "" }`). `createAccountWebhook` calls `req.normalise()` too (the connect-time paths build a request from `PendingWebhook`; carry `Kind`, `BotToken`, `ChatID` across in `handlers_connect.go:314` and `handlers_link.go:441`, and store them in the pending hook at `handlers_connect.go:91`: `Kind: req.Webhook.Kind, BotToken: req.Webhook.BotToken, ChatID: req.Webhook.ChatID`). In `handleHostedAuth`, call `req.Webhook.normalise()` before `validate()` and run `checkTelegram` there as well so a bad token fails at link-mint time, not silently at bind time.

`api.go`: add `senders *notify.Registry` to `Server`, a `senders` parameter to `NewServer` (after `dispatcher`), default to `notify.NewRegistry(nil)` when nil. Update `cmd/server/main.go` to create one registry and pass it to both `events.NewDispatcher` and `api.NewServer`. Update the `newTestServer*` helpers to pass `nil` and expose `s.senders` (same package, no accessor needed).

- [ ] **Step 4: Run tests**

Run: `go vet ./... && go test ./internal/api/ && go test ./...`
Expected: PASS, including `isolation_test.go` (no new routes) and the existing webhook CRUD tests (default kind `webhook`, secret still returned once).

- [ ] **Step 5: Commit**

```bash
git add internal/api cmd/server
git commit -m "feat(api): webhook kinds — discord and telegram targets with per-kind validation"
```

---

### Task 6: Dashboard — kind selector and badge in the Set-webhook form

**Files:**
- Modify: `internal/api/handlers_ui.go` (`renderWebhook` ~line 320, `set-webhook` handler ~361, CSS block)
- Test: `internal/api/api_test.go` (`TestDashboardRendersWebhookForm` ~739)

**Interfaces:** none new; the form POSTs `{kind, url?, secret?, bot_token?, chat_id?}` to the existing per-account route with `Content-Type: application/json`.

- [ ] **Step 1: Failing test** — extend `TestDashboardRendersWebhookForm` with:

```go
	for _, want := range []string{`name="kind"`, `value="discord"`, `value="telegram"`, `name="bot_token"`, `name="chat_id"`, `data-kind-fields`} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard missing %q", want)
		}
	}
```

(where `body` is the dashboard HTML the test already fetches).

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/api/ -run TestDashboardRendersWebhookForm`
Expected: FAIL — strings missing.

- [ ] **Step 3: Implement** — replace `renderWebhook` and the `set-webhook` branch:

```js
function renderWebhook(el, hook) {
  if (hook) {
    var where = hook.kind === "telegram" ? "chat " + escapeHtml((hook.telegram || {}).chat_id || "")
                                          : "<code>" + escapeHtml(hook.url || "") + "</code>";
    el.innerHTML =
      '<span class="badge">' + escapeHtml(hook.kind || "webhook") + "</span> " + where +
      '<button data-action="remove-webhook" data-wid="' + hook.id + '" class="danger">Remove</button>';
    return;
  }
  el.innerHTML =
    '<select name="kind">' +
      '<option value="webhook">Webhook (JSON)</option>' +
      '<option value="discord">Discord channel</option>' +
      '<option value="telegram">Telegram chat</option>' +
    '</select>' +
    '<span data-kind-fields="webhook">' +
      '<input name="url" type="url" placeholder="https://your-app.example.com/hooks/mail">' +
      '<input name="secret" type="text" placeholder="secret (optional)">' +
    '</span>' +
    '<span data-kind-fields="discord" hidden>' +
      '<input name="discord_url" type="url" placeholder="https://discord.com/api/webhooks/…">' +
    '</span>' +
    '<span data-kind-fields="telegram" hidden>' +
      '<input name="bot_token" type="password" placeholder="bot token from @BotFather" autocomplete="off">' +
      '<input name="chat_id" type="text" placeholder="chat id, e.g. -1001234567890">' +
    '</span>' +
    '<button data-action="set-webhook">Set webhook</button>';
  el.querySelector('select[name=kind]').addEventListener("change", function (e) {
    el.querySelectorAll("[data-kind-fields]").forEach(function (span) {
      span.hidden = span.dataset.kindFields !== e.target.value;
    });
  });
}
```

```js
  if (action === "set-webhook") {
    const box = btn.closest("[data-hook]");
    const kind = box.querySelector('select[name=kind]').value;
    const val = (n) => { const i = box.querySelector('input[name=' + n + ']'); return i ? i.value.trim() : ""; };
    const body = { kind };
    if (kind === "webhook") { body.url = val("url"); body.secret = val("secret"); if (!body.url) return; }
    if (kind === "discord") { body.url = val("discord_url"); if (!body.url) return; }
    if (kind === "telegram") { body.bot_token = val("bot_token"); body.chat_id = val("chat_id"); if (!body.bot_token || !body.chat_id) return; }
    btn.disabled = true;
    try {
      await api("/api/v1/accounts/" + id + "/webhooks", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(body)
      });
      loadWebhook(id);
    } catch (e) {
      showRowMessage(id, "Could not set webhook: " + e.message, true);
      btn.disabled = false;
    }
    return;
  }
```

Add a `.badge` style if the dashboard CSS has none (the chat-card kind badge from the WhatsApp work may already define one — reuse it). Keep `escapeHtml` on every API string.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/api/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/api/handlers_ui.go internal/api/api_test.go
git commit -m "feat(ui): webhook kind selector and badge on the dashboard"
```

---

### Task 7: Docs, manual checklist, end-to-end verification

**Files:**
- Modify: `internal/api/handlers_docs.go` (§6 Webhooks, ~line 229–262; add §6.4 "Delivery targets: Discord and Telegram"), `internal/api/handlers_llms.go` (Webhook object ~line 69, `### Webhooks` ~137, error table, Limits), `README.md` (`### Webhooks` ~line 292: add "Forward to Discord / Telegram")
- Create: `docs/delivery-targets-manual-checklist.md`
- Test: `internal/api/api_test.go` (`TestDocsPageIsPublicAndCoversEveryRoute`, `TestLLMsTxtIsPublicMarkdownCoveringEveryRoute`)

- [ ] **Step 1: Failing tests** — extend both docs tests' expected-substring lists with `"kind"`, `"discord.com/api/webhooks"`, `"bot_token"`, `"@BotFather"`.

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/api/ -run 'TestDocsPage|TestLLMsTxt'`
Expected: FAIL — strings missing.

- [ ] **Step 3: Write the docs**

`/docs` §6.4 (HTML, same style as §6):

```html
<h3 id="targets">6.4 Delivery targets: Discord and Telegram</h3>
<p>A hook has a <code>kind</code>. <code>webhook</code> (the default) receives the JSON event above. <code>discord</code> and <code>telegram</code> receive a short human-readable notification instead &mdash; no signature header, no JSON &mdash; using the same event filter, retry schedule and <code>deliveries</code> log.</p>
<pre><code># Discord: Server settings → Integrations → Webhooks → New webhook → Copy URL
{"kind":"discord","url":"https://discord.com/api/webhooks/1234/abcd…","events":["chat_received","mail_received"]}

# Telegram: create a bot with @BotFather, add it to your group/channel, then find the chat id
#   curl https://api.telegram.org/bot&lt;token&gt;/getUpdates   → "chat":{"id":-1001234567890,…}
{"kind":"telegram","bot_token":"123456:ABC-DEF…","chat_id":"-1001234567890","events":["chat_received"]}</code></pre>
<p>The Telegram target is checked once at creation (<code>getChat</code>): a rejected token or chat answers <b>400 invalid_webhook</b> with Telegram&rsquo;s description. The bot token is stored encrypted and is never returned or logged; the response carries <code>telegram.chat_id</code> only. Discord URLs must be on <code>discord.com</code> or <code>discordapp.com</code>.</p>
<p>What a notification looks like (Discord Markdown; Telegram gets the same in HTML):</p>
<pre><code>💬 **WhatsApp** · Team chat
**Alice**: Can we ship Thursday?

📧 **New mail** · me@example.com
From: Bob &lt;bob@example.com&gt;
**Q3 plan**
Hi — attaching the deck we discussed…</code></pre>
<p>Text is cut at 200 characters for mail and 300 for chat; phone numbers are masked (<code>+91 98••• •855</code>); media shows as <code>[image]</code> etc. Not supported: attachments, replies from the channel, custom templates.</p>
```

`/llms.txt`: Webhook object becomes
```
{id, account_id?: "" | "acc_…", name?, kind: "webhook"|"discord"|"telegram", url? (webhook/discord), secret? (webhook, creation only), telegram?: {chat_id}, events: [..], created_at}
```
Add under `### Webhooks`:
```
- kind=discord: body `{kind:"discord", url:"https://discord.com/api/webhooks/…", name?, events?}`; receives a formatted text message, no signature.
- kind=telegram: body `{kind:"telegram", bot_token, chat_id, name?, events?}`; bot_token is never returned; a Telegram rejection at creation is 400 invalid_webhook, Telegram unreachable is 502 provider_error.
- Notifications: one message per event, mail snippet ≤200 chars, chat ≤300, phones masked. Same retry schedule and deliveries log as kind=webhook.
```
Add `provider_error` note "(also: Telegram unreachable when creating a telegram hook)" to the error table, and a Limits bullet "Discord/Telegram targets are one-way and text-only."

README `### Webhooks` — add a "Forward to Discord / Telegram" paragraph with the two bodies and the BotFather/chat-id steps (same content as the docs, Markdown).

`docs/delivery-targets-manual-checklist.md`:

```markdown
# Delivery targets — manual checklist

Needs: a running server, one connected account (mail or WhatsApp), a Discord
channel you administer, a Telegram group with a bot you created.

1. Discord: Server settings → Integrations → Webhooks → New webhook → Copy URL.
   `POST /api/v1/accounts/{id}/webhooks {"kind":"discord","url":"<url>"}` → 201, `kind: discord`.
2. Trigger an event (send a WhatsApp message to the linked number / send yourself a mail).
   Expect the notification in the Discord channel within seconds; server log shows
   `delivery attempt … kind=discord target=https://discord.com/api/webhooks/<id>/•••`.
3. Telegram: @BotFather → /newbot → copy the token; add the bot to a group; post a message in the
   group; `curl https://api.telegram.org/bot<token>/getUpdates` → note `chat.id`.
   `POST /api/v1/accounts/{id}/webhooks {"kind":"telegram","bot_token":"<token>","chat_id":"<id>"}` → 201,
   response has `telegram.chat_id` and no token.
4. Trigger an event → notification in the Telegram group (bold sender, escaped text).
5. Wrong chat id → `POST` answers 400 with Telegram's "chat not found".
6. Point a Discord hook at a deleted Discord webhook → `GET /api/v1/webhooks/{id}/deliveries` shows
   attempts with `last_error: discord: status 404 …` and no token in the URL.
7. `grep -c '<bot token>' server.log` → 0. `grep 'api/webhooks/' server.log` shows only `/•••`.
8. Dashboard: the Set-webhook form switches fields per kind; the card shows the kind badge.
```

- [ ] **Step 4: Verify end to end** (verifying-services-end-to-end)

Build; run the server in the background on a **copy** of the DB in the scratch dir (`DB_PATH=<copy>`, `DEBUG=1`, env from `.env`), poll `/healthz`. With a session/API key for a throwaway developer:
- `POST /api/v1/webhooks {"kind":"discord","url":"https://discord.com/api/webhooks/1/x"}` → 201; emit an event (e.g. `POST /api/v1/accounts/{id}/resync` on a mail account, or wait for a real WhatsApp message if one is linked) → log shows `kind=discord target=…/•••` and a 4xx from Discord (unless the operator supplies a real URL) → `deliveries` shows the scrubbed `last_error`.
- Telegram: with no real bot, expect 502/400 at creation depending on reachability; record which. If the operator supplies a real token + chat id via the checklist, expect a message in the chat.
- `/docs` and `/llms.txt` contain §6.4; leak greps over the log for `bot`+token and the Discord token segment → 0.
- Stop the server; confirm the port is free. Report every command and result (secrets redacted) in the task report.

- [ ] **Step 5: Run the suite and commit**

Run: `gofmt -l internal cmd; go vet ./...; go test ./...`
Expected: all clean.

```bash
git add internal/api/handlers_docs.go internal/api/handlers_llms.go internal/api/api_test.go README.md docs/delivery-targets-manual-checklist.md
git commit -m "docs: Discord and Telegram delivery targets"
```

---

## Self-review

- **Spec coverage**: §2 model/storage → Task 1; §3 API/validation/`getChat`/response shaping/dashboard → Tasks 5–6; §4 senders/dispatcher/scrubbing → Tasks 3–4; §5 formatting → Task 2; §6 security (sealed token, fixed hosts, escaping, `allowed_mentions`, tenancy) → Tasks 1, 3, 5; §7 config (`SetSealKey` from `TOKEN_ENCRYPTION_KEY`) → Task 4; §8 tests → each task; §9 docs and manual checklist → Task 7.
- **Placeholders**: none; every step carries code or exact strings. Task 5's `checkTelegram` return shape and the OAuth-state getter name are to be confirmed against the code, which the steps say explicitly.
- **Type consistency**: `Sender.Send(ctx, h, ev, payload, attempt)` in Task 3 matches Task 4's `send`; `Registry.For/ValidateTelegram/SetTelegramBase` used identically in Tasks 4–5; `model.WebhookKind*`, `model.TelegramTarget`, `store.SetSealKey` from Task 1 used unchanged later; `NewDispatcher(db, senders, log)` and `NewServer(..., dispatcher, senders, log)` consistent across Tasks 4–5 and `main.go`.
