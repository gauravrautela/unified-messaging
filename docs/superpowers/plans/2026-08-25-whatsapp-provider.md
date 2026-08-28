# WhatsApp Provider Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add WhatsApp as a second, chat-kind provider: end users link their number via a QR on the hosted connect page; developers read and send 1:1 and group text messages, react, edit, delete, and receive real-time `chat_*` webhooks — all tenant-scoped like mail.

**Architecture:** Capability-based providers (`Linker`/`Chatter` next to `Authenticator`/`Mailbox`), an adapter over whatsmeow that publishes domain events to an `EventSink`, a supervisor runtime (`internal/chatsync`) holding one socket per account and feeding the existing `events.Dispatcher`, and a new `chats`/`chat_messages`/`attendees` resource family in store and API. Device credentials live in whatsmeow's own tables inside our SQLite file; `chat_sessions` maps account → device JID.

**Tech Stack:** Go 1.26, stdlib `net/http` ServeMux, raw SQL over `modernc.org/sqlite`, `log/slog`, `go.mau.fi/whatsmeow` (+ `google.golang.org/protobuf`, `github.com/skip2/go-qrcode`).

**Spec:** `docs/superpowers/specs/2026-08-25-whatsapp-provider-design.md`

## Global Constraints

- Stdlib `net/http` + raw SQL only. New dependencies allowed: `go.mau.fi/whatsmeow`, `google.golang.org/protobuf` (transitive), `github.com/skip2/go-qrcode`. Only `internal/provider/whatsapp` may import whatsmeow.
- No handler or store method may branch on the provider *name*; capabilities (`Linker() != nil`, `Chat() != nil`, `Kind()`) decide.
- Tenancy: every new `/api/v1` route is scoped through `resolveID` (developer id in SQL); cross-tenant → **404**. The only permitted 403 is `not_own_message`.
- Device credentials are stored by whatsmeow in `whatsmeow_*` tables in our DB (not row-sealed); `chat_sessions` holds `device_jid` only. `DeleteLinked` must call `Chatter.Forget` so no orphan device rows remain.
- Idempotent event handling on `(account_id, message id)`; no duplicate webhook for a replayed or own-echo message.
- Logging standard from the tenancy spec §5: every chatsync line carries `component=chatsync`, `account_id`, `developer_id`, `conn_id`; **never log** message text, attendee phone numbers, device keys, QR codes. Text is logged as `text_bytes`.
- Connect page: disclosure + consent checkbox before the QR; `GET /connect/{state}/qr` returns `409 consent_required` before consent; QR never exposed under `/api/v1`.
- Link session bounded to 3 minutes; runtime backoff 1s→5m jittered, reset after 10 min connected; 30 consecutive failures → `CREDENTIALS` with `reason: "unreachable"`.
- Config: `WHATSAPP_ENABLED` (default true), `WHATSAPP_MAX_ACCOUNTS` (default 200), `WHATSAPP_DEVICE_NAME` (default "Unified Messaging").
- TDD per task; `gofmt -l internal cmd` empty; `go vet ./...` clean; `go test -race ./...` green; one commit per task with the stated message plus the repo's trailer lines.

## File structure

| File | Responsibility |
|---|---|
| `internal/model/chat.go` (new) | `Chat`, `Attendee`, `ChatMember`, `ChatMessage`, `Reaction`, chat event names, `Account.Kind/Identifier/Connection` |
| `internal/provider/provider.go` | `Provider.Kind/Linker/Chat`; `Linker`, `LinkSession`, `LinkCode`, `LinkResult`, `Chatter`, `ChatConn`, `EventSink`; `Identity.Identifier` |
| `internal/provider/providertest/fakechat.go` (new) | scripted `FakeChat` (Provider+Linker+Chatter) for every test layer |
| `internal/store/schema.go`, `store/chat.go` (new), `store/store_test.go` | tables + migration; chat/attendee/member/message/session/idempotency repositories |
| `internal/accounts/accounts.go` | `ConnectLinked`, `DeleteLinked`, `DeviceJID`; syncer skips non-mail |
| `internal/provider/whatsapp/{whatsapp.go,link.go,conn.go,translate.go,commands.go}` (new) | adapter over whatsmeow |
| `internal/chatsync/{runtime.go,actor.go,sink.go,backoff.go}` (new) + tests | supervisor, actor, EventSink → store + dispatcher |
| `internal/api/handlers_link.go` (new), `handlers_connect.go`, `handlers_misc.go` | consent + QR endpoints, provider-aware hosted-auth/connect page, account kind/connection/reconnect/delete |
| `internal/api/handlers_chat.go` (new), `api.go`, `isolation_test.go` | chats/messages/attendees routes, idempotency, `apiRoutes` |
| `internal/api/handlers_ui.go`, `handlers_chat_ui.go` (new), `handlers_docs.go`, `handlers_llms.go` | picker, cards, `/chat` viewer, docs |
| `internal/config/config.go`, `cmd/server/main.go`, `README.md`, `.env.example`, `docs/whatsapp-manual-checklist.md` | wiring, config, docs |

---

### Task 1: Contracts, model types, event names, and the FakeChat test double

**Files:**
- Create: `internal/model/chat.go`, `internal/provider/providertest/fakechat.go`, `internal/provider/providertest/fakechat_test.go`
- Modify: `internal/model/model.go` (`Account`, `Event`, `KnownEvent`), `internal/provider/provider.go`, `internal/provider/outlook/outlook.go`

**Interfaces:**
- Produces (model): `Chat{ID, AccountID, Kind, Name string; UnreadCount int; LastMessageAt *time.Time; Archived, Muted bool; Members []Attendee}`; `Attendee{ID, Phone, Name string; IsSelf bool}`; `ChatMember{ChatID, AttendeeID, Role string}`; `Reaction{AttendeeID, Emoji string; At time.Time}`; `ChatMessage{ID, AccountID, ChatID string; Sender Attendee; IsFromMe bool; Kind, Text, QuotedMessageID string; SentAt time.Time; EditedAt *time.Time; Deleted bool; Status string; Reactions []Reaction}`; constants `EventChatReceived="chat_received"`, `EventChatSent`, `EventChatUpdated`, `EventChatReaction`, `EventChatDeleted`; `Account.Kind` (`"mail"|"chat"`), `Account.Identifier`, `Account.Connection *Connection{State string; Since time.Time; Reconnects int; LastError string}`; `Event` gains `Message *ChatMessage`, `Chat *Chat`, `MessageIDs []string`, `Status, Change string`, `Reaction *Reaction`; `AccountKindMail/AccountKindChat`.
- Produces (provider): `Provider` interface gains `Kind() string`, `Linker() Linker`, `Chat() Chatter`; `Identity.Identifier`; `Linker`, `LinkSession`, `LinkCode`, `LinkResult`, `Chatter`, `ChatConn`, `EventSink` exactly as the spec §2 (with `deviceJID string`, `Forget`).
- Produces (providertest): `providertest.NewFakeChat(name string) *FakeChat` implementing `provider.Provider`; scripting API: `f.EmitCode(code string)`, `f.Pair(identity provider.Identity, deviceJID string)`, `f.FailLink(err)`, `f.Sink(accountID) provider.EventSink` (the sink the runtime passed to `Connect`), `f.Disconnect(accountID, reason string, loggedOut bool)`, `f.Commands() []string` (recorded calls like `"SendText acc chat text"`), `f.Roster = func(accountID) ([]model.Chat, []model.Attendee, []model.ChatMember, error)`, `f.SendResult = provider.SendResult{MessageID:"…"}`, `f.ConnectErr error`, `f.Forgotten() []string`.

- [ ] **Step 1: Write the failing tests**

```go
// internal/provider/providertest/fakechat_test.go
package providertest

import (
	"context"
	"testing"
	"time"

	"github.com/gauravrautela/unified-messaging/internal/model"
	"github.com/gauravrautela/unified-messaging/internal/provider"
)

type recSink struct{ msgs []model.ChatMessage; disc []string }

func (r *recSink) Message(_ string, m model.ChatMessage, _ model.Chat, _ model.Attendee) { r.msgs = append(r.msgs, m) }
func (r *recSink) Receipt(string, string, []string, string)                        {}
func (r *recSink) Reaction(string, string, string, model.Reaction)                   {}
func (r *recSink) Edited(string, string, string, string, time.Time)                  {}
func (r *recSink) Deleted(string, string, string)                                    {}
func (r *recSink) Disconnected(_ string, reason string, _ bool)                      { r.disc = append(r.disc, reason) }

func TestFakeChatIsAChatProvider(t *testing.T) {
	var p provider.Provider = NewFakeChat("FAKECHAT")
	if p.Kind() != "chat" || p.Linker() == nil || p.Chat() == nil || p.Mailbox() != nil || p.Auth() != nil || p.Push() != nil {
		t.Fatalf("capabilities wrong: kind=%s", p.Kind())
	}
}

func TestFakeChatLinkScript(t *testing.T) {
	f := NewFakeChat("FAKECHAT")
	sess, err := f.Linker().StartLink(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	f.EmitCode("qr-1")
	if c := <-sess.Codes(); c.Code != "qr-1" || c.ExpiresAt.IsZero() {
		t.Fatalf("code = %+v", c)
	}
	f.Pair(provider.Identity{Identifier: "+919888000000", Name: "Test"}, "919888000000:5@s.whatsapp.net")
	res := <-sess.Result()
	if res.Err != nil || res.DeviceJID != "919888000000:5@s.whatsapp.net" || res.Identity.Identifier != "+919888000000" {
		t.Fatalf("result = %+v", res)
	}
}

func TestFakeChatConnectRecordsSinkAndCommands(t *testing.T) {
	f := NewFakeChat("FAKECHAT")
	sink := &recSink{}
	conn, err := f.Chat().Connect(context.Background(), "acc_1", "dev@jid", sink)
	if err != nil {
		t.Fatal(err)
	}
	f.Sink("acc_1").Message("acc_1", model.ChatMessage{ID: "M1", Text: "hi"}, model.Chat{ID: "c1"}, model.Attendee{ID: "a1"})
	if len(sink.msgs) != 1 || sink.msgs[0].ID != "M1" {
		t.Fatalf("sink did not receive: %+v", sink.msgs)
	}
	if _, err := f.Chat().SendText(context.Background(), "acc_1", "c1", "hello", ""); err != nil {
		t.Fatal(err)
	}
	if got := f.Commands(); len(got) != 1 || got[0] != "SendText acc_1 c1 hello" {
		t.Fatalf("commands = %v", got)
	}
	f.Disconnect("acc_1", "gone", true)
	if len(sink.disc) != 1 || sink.disc[0] != "gone" {
		t.Fatalf("disconnect not delivered: %v", sink.disc)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Chat().Forget(context.Background(), "dev@jid"); err != nil || len(f.Forgotten()) != 1 {
		t.Fatalf("forget: %v %v", err, f.Forgotten())
	}
}

func TestChatEventNamesAreKnown(t *testing.T) {
	for _, e := range []string{model.EventChatReceived, model.EventChatSent, model.EventChatUpdated, model.EventChatReaction, model.EventChatDeleted} {
		if !model.KnownEvent(e) {
			t.Errorf("%s not known", e)
		}
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/provider/providertest/`
Expected: build failure — package and types missing.

- [ ] **Step 3: Model types**

Create `internal/model/chat.go`:

```go
package model

import "time"

// Account kinds. A mail account is read through a Mailbox; a chat account is a
// live linked device read through a Chatter.
const (
	AccountKindMail = "mail"
	AccountKindChat = "chat"
)

// Connection is the live state of a chat account's socket, reported by the
// chat runtime. Nil for mail accounts.
type Connection struct {
	State      string    `json:"state"` // connecting | connected | backoff | stopped | error
	Since      time.Time `json:"since"`
	Reconnects int       `json:"reconnects"`
	LastError  string    `json:"last_error,omitempty"`
}

type Chat struct {
	ID            string     `json:"id"`
	AccountID     string     `json:"account_id"`
	Kind          string     `json:"kind"` // direct | group
	Name          string     `json:"name"`
	UnreadCount   int        `json:"unread_count"`
	LastMessageAt *time.Time `json:"last_message_at,omitempty"`
	Archived      bool       `json:"archived"`
	Muted         bool       `json:"muted"`
	Members       []Attendee `json:"members,omitempty"`
}

// Attendee is a person in an account's chats. ID is the stable provider id
// (phone JID when known, else a privacy id); Phone is E.164 when resolvable.
type Attendee struct {
	ID     string `json:"id"`
	Phone  string `json:"phone,omitempty"`
	Name   string `json:"name"`
	IsSelf bool   `json:"is_self"`
}

type ChatMember struct {
	ChatID     string `json:"chat_id"`
	AttendeeID string `json:"attendee_id"`
	Role       string `json:"role,omitempty"` // admin | ""
}

type Reaction struct {
	AttendeeID string    `json:"attendee_id"`
	Emoji      string    `json:"emoji"`
	At         time.Time `json:"at"`
}

type ChatMessage struct {
	ID              string     `json:"id"`
	AccountID       string     `json:"account_id"`
	ChatID          string     `json:"chat_id"`
	Sender          Attendee   `json:"sender"`
	IsFromMe        bool       `json:"is_from_me"`
	Kind            string     `json:"kind"` // text | unsupported
	Text            string     `json:"text"`
	QuotedMessageID string     `json:"quoted_message_id,omitempty"`
	SentAt          time.Time  `json:"sent_at"`
	EditedAt        *time.Time `json:"edited_at,omitempty"`
	Deleted         bool       `json:"deleted"`
	Status          string     `json:"status,omitempty"` // own messages: sending | sent | delivered | read
	Reactions       []Reaction `json:"reactions"`
}

// Chat event names.
const (
	EventChatReceived = "chat_received"
	EventChatSent     = "chat_sent"
	EventChatUpdated  = "chat_updated"
	EventChatReaction = "chat_reaction"
	EventChatDeleted  = "chat_deleted"
)
```

In `internal/model/model.go`:
- `Account` gains, after `Email`: `Kind string \`json:"kind"\``, `Identifier string \`json:"identifier"\``, and after `LastSyncedAt`: `Connection *Connection \`json:"connection,omitempty"\``.
- `Event` gains: `Message *ChatMessage \`json:"message,omitempty"\``, `Chat *Chat \`json:"chat,omitempty"\``, `MessageIDs []string \`json:"message_ids,omitempty"\``, `Status string \`json:"status,omitempty"\``, `Change string \`json:"change,omitempty"\``, `Reaction *Reaction \`json:"reaction,omitempty"\``.
- `KnownEvent` adds the five chat names to its `case`.

- [ ] **Step 4: Provider contracts**

In `internal/provider/provider.go`, change `Identity` to `{Identifier, Email, Name string}` and the `Provider` interface to:

```go
// Provider is one backend. Capabilities are optional: a mail provider returns
// nil from Linker() and Chat(); a chat provider returns nil from Auth(),
// Mailbox() and Push(). Callers test capabilities, never names.
type Provider interface {
	Name() string
	Kind() string // model.AccountKindMail | model.AccountKindChat
	Auth() Authenticator
	Linker() Linker
	Mailbox() Mailbox
	Chat() Chatter
	Push() Pusher
}
```

Append the chat contracts (verbatim from the spec, post-custody amendment):

```go
// ---- chat providers (linked device, live socket) ----

type LinkCode struct {
	Code      string
	ExpiresAt time.Time
}

type LinkResult struct {
	Identity  Identity
	DeviceJID string
	Err       error
}

// LinkSession is one pairing attempt. Codes stream until paired, expired or
// closed; Result resolves exactly once.
type LinkSession interface {
	Codes() <-chan LinkCode
	Result() <-chan LinkResult
	Close()
}

type Linker interface {
	StartLink(ctx context.Context) (LinkSession, error)
}

// EventSink receives domain events from a live connection. Implementations
// must be safe for concurrent use; the adapter calls them from its own
// goroutines.
type EventSink interface {
	Message(accountID string, m model.ChatMessage, chat model.Chat, sender model.Attendee)
	Receipt(accountID, chatID string, messageIDs []string, status string) // delivered | read
	Reaction(accountID, chatID, messageID string, r model.Reaction)
	Edited(accountID, chatID, messageID, text string, at time.Time)
	Deleted(accountID, chatID, messageID string)
	Disconnected(accountID, reason string, loggedOut bool)
}

type ChatConn interface {
	Close() error
}

type Chatter interface {
	Connect(ctx context.Context, accountID, deviceJID string, sink EventSink) (ChatConn, error)
	Forget(ctx context.Context, deviceJID string) error
	Chats(ctx context.Context, accountID string) ([]model.Chat, []model.Attendee, []model.ChatMember, error)
	SendText(ctx context.Context, accountID, chatID, text, quotedID string) (SendResult, error)
	StartDirect(ctx context.Context, accountID, phoneE164 string) (chatID string, err error)
	React(ctx context.Context, accountID, chatID, messageID, emoji string) error
	Edit(ctx context.Context, accountID, chatID, messageID, text string) error
	Delete(ctx context.Context, accountID, chatID, messageID string) error
	MarkRead(ctx context.Context, accountID, chatID string, messageIDs []string) error
	Logout(ctx context.Context, accountID string) error
}
```

`internal/provider/outlook/outlook.go` adds `func (p *Provider) Kind() string { return model.AccountKindMail }`, `Linker() provider.Linker { return nil }`, `Chat() provider.Chatter { return nil }`; `Identify` sets `Identifier: email` alongside `Email`.

- [ ] **Step 5: FakeChat**

```go
// internal/provider/providertest/fakechat.go
// Package providertest holds a scripted chat provider used by store, runtime,
// API and page tests. It records commands and lets tests drive link sessions
// and inbound events without any network.
package providertest

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/gauravrautela/unified-messaging/internal/model"
	"github.com/gauravrautela/unified-messaging/internal/provider"
)

type FakeChat struct {
	name string

	mu        sync.Mutex
	sessions  []*fakeSession
	sinks     map[string]provider.EventSink
	commands  []string
	forgotten []string

	// Script knobs.
	Roster     func(accountID string) ([]model.Chat, []model.Attendee, []model.ChatMember, error)
	SendResult provider.SendResult
	ConnectErr error
	CommandErr error
	DirectChat string // returned by StartDirect
}

func NewFakeChat(name string) *FakeChat {
	return &FakeChat{name: name, sinks: map[string]provider.EventSink{},
		SendResult: provider.SendResult{MessageID: "FAKE1"}, DirectChat: "new@s.whatsapp.net"}
}

func (f *FakeChat) Name() string                 { return f.name }
func (f *FakeChat) Kind() string                 { return model.AccountKindChat }
func (f *FakeChat) Auth() provider.Authenticator { return nil }
func (f *FakeChat) Mailbox() provider.Mailbox    { return nil }
func (f *FakeChat) Push() provider.Pusher        { return nil }
func (f *FakeChat) Linker() provider.Linker      { return f }
func (f *FakeChat) Chat() provider.Chatter       { return f }

// ---- Linker ----

type fakeSession struct {
	codes  chan provider.LinkCode
	result chan provider.LinkResult
	once   sync.Once
}

func (s *fakeSession) Codes() <-chan provider.LinkCode    { return s.codes }
func (s *fakeSession) Result() <-chan provider.LinkResult { return s.result }
func (s *fakeSession) Close()                             { s.once.Do(func() { close(s.codes) }) }

func (f *FakeChat) StartLink(ctx context.Context) (provider.LinkSession, error) {
	s := &fakeSession{codes: make(chan provider.LinkCode, 16), result: make(chan provider.LinkResult, 1)}
	f.mu.Lock()
	f.sessions = append(f.sessions, s)
	f.mu.Unlock()
	return s, nil
}

func (f *FakeChat) latest() *fakeSession {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sessions) == 0 {
		return nil
	}
	return f.sessions[len(f.sessions)-1]
}

// EmitCode pushes a QR code to the most recent link session.
func (f *FakeChat) EmitCode(code string) {
	if s := f.latest(); s != nil {
		s.codes <- provider.LinkCode{Code: code, ExpiresAt: time.Now().Add(20 * time.Second)}
	}
}

// Pair completes the most recent link session successfully.
func (f *FakeChat) Pair(id provider.Identity, deviceJID string) {
	if s := f.latest(); s != nil {
		s.result <- provider.LinkResult{Identity: id, DeviceJID: deviceJID}
		s.Close()
	}
}

func (f *FakeChat) FailLink(err error) {
	if s := f.latest(); s != nil {
		s.result <- provider.LinkResult{Err: err}
		s.Close()
	}
}

// ---- Chatter ----

type fakeConn struct {
	f         *FakeChat
	accountID string
}

func (c *fakeConn) Close() error {
	c.f.mu.Lock()
	delete(c.f.sinks, c.accountID)
	c.f.mu.Unlock()
	return nil
}

func (f *FakeChat) Connect(ctx context.Context, accountID, deviceJID string, sink provider.EventSink) (provider.ChatConn, error) {
	if f.ConnectErr != nil {
		return nil, f.ConnectErr
	}
	f.mu.Lock()
	f.sinks[accountID] = sink
	f.mu.Unlock()
	return &fakeConn{f: f, accountID: accountID}, nil
}

// Sink returns the sink the runtime passed for accountID (nil if not connected).
func (f *FakeChat) Sink(accountID string) provider.EventSink {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sinks[accountID]
}

// Disconnect pushes a Disconnected event to the connected sink.
func (f *FakeChat) Disconnect(accountID, reason string, loggedOut bool) {
	if s := f.Sink(accountID); s != nil {
		s.Disconnected(accountID, reason, loggedOut)
	}
}

func (f *FakeChat) record(format string, a ...any) error {
	f.mu.Lock()
	f.commands = append(f.commands, fmt.Sprintf(format, a...))
	f.mu.Unlock()
	return f.CommandErr
}

func (f *FakeChat) Commands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.commands...)
}

func (f *FakeChat) Forget(ctx context.Context, deviceJID string) error {
	f.mu.Lock()
	f.forgotten = append(f.forgotten, deviceJID)
	f.mu.Unlock()
	return nil
}

func (f *FakeChat) Forgotten() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.forgotten...)
}

func (f *FakeChat) Chats(ctx context.Context, accountID string) ([]model.Chat, []model.Attendee, []model.ChatMember, error) {
	if f.Roster != nil {
		return f.Roster(accountID)
	}
	return nil, nil, nil, nil
}

func (f *FakeChat) SendText(ctx context.Context, accountID, chatID, text, quotedID string) (provider.SendResult, error) {
	if err := f.record("SendText %s %s %s", accountID, chatID, text); err != nil {
		return provider.SendResult{}, err
	}
	return f.SendResult, nil
}

func (f *FakeChat) StartDirect(ctx context.Context, accountID, phone string) (string, error) {
	if err := f.record("StartDirect %s %s", accountID, phone); err != nil {
		return "", err
	}
	return f.DirectChat, nil
}

func (f *FakeChat) React(ctx context.Context, accountID, chatID, messageID, emoji string) error {
	return f.record("React %s %s %s %q", accountID, chatID, messageID, emoji)
}

func (f *FakeChat) Edit(ctx context.Context, accountID, chatID, messageID, text string) error {
	return f.record("Edit %s %s %s %s", accountID, chatID, messageID, text)
}

func (f *FakeChat) Delete(ctx context.Context, accountID, chatID, messageID string) error {
	return f.record("Delete %s %s %s", accountID, chatID, messageID)
}

func (f *FakeChat) MarkRead(ctx context.Context, accountID, chatID string, ids []string) error {
	return f.record("MarkRead %s %s %v", accountID, chatID, ids)
}

func (f *FakeChat) Logout(ctx context.Context, accountID string) error {
	return f.record("Logout %s", accountID)
}
```

- [ ] **Step 6: Run tests**

Run: `gofmt -l internal cmd; go vet ./... && go test ./...`
Expected: PASS everywhere (existing tests unaffected; outlook still satisfies `Provider`).

- [ ] **Step 7: Commit**

```bash
git add internal/model internal/provider
git commit -m "feat(provider): chat capability contracts, chat model types, FakeChat test double"
```

---

### Task 2: Store — chat tables, `accounts.kind` migration, repositories, idempotency keys

**Files:**
- Modify: `internal/store/schema.go`, `internal/store/store.go` (`scanAccount`/`UpsertAccount`/`accountSelect`, migrations)
- Create: `internal/store/chat.go`
- Test: `internal/store/store_test.go`

**Interfaces:**
- Produces: `UpsertChat(c model.Chat) error`; `GetChat(accountID, id) (model.Chat, error)` (with members); `ListChats(q ChatQuery{AccountID, Kind string; Unread *bool; Search string; Limit, Offset int}) ([]model.Chat, error)`; `SetChatFlags(accountID, id string, archived, muted *bool) error`; `BumpChat(accountID, id string, at time.Time, unreadDelta int) error`; `ClearUnread(accountID, id string) error`; `UpsertAttendee(a model.Attendee, accountID string) error`; `GetAttendee(accountID, id) (model.Attendee, error)`; `ListAttendees(accountID, search string, limit, offset int) ([]model.Attendee, error)`; `ReplaceChatMembers(accountID, chatID string, members []model.ChatMember) error`; `UpsertChatMessage(m model.ChatMessage) (inserted bool, err error)` (returns false when the id already existed — no field changes on replay); `GetChatMessage(accountID, id) (model.ChatMessage, error)`; `ListChatMessages(accountID, chatID, before string, limit int) ([]model.ChatMessage, nextBefore string, err error)` (keyset on `(sent_at, id)` desc); `RenameChatMessage(accountID, oldID, newID string) error`; `DeleteChatMessageRow(accountID, id string) error`; `SetMessageStatus(accountID string, ids []string, status string) error`; `ApplyReaction(accountID, id string, r model.Reaction) error` (empty emoji removes that attendee's reaction); `EditChatMessage(accountID, id, text string, at time.Time) error`; `RevokeChatMessage(accountID, id string) error`; `SaveChatSession(accountID, provider, deviceJID string) error`; `ChatSession(accountID) (deviceJID string, err error)`; `DeleteChatSession(accountID) error`; `ListChatAccounts() ([]model.Account, error)` (unscoped, `kind='chat' AND status='OK'`); `PutIdempotency(developerID, key string, response []byte) error`; `GetIdempotency(developerID, key string) ([]byte, error)`; `PurgeIdempotency(olderThan time.Time)`.
- `scanAccount` reads/writes `kind`; `UpsertAccount` inserts `kind`; `Account.Identifier` is filled from `email` on scan.

- [ ] **Step 1: Write the failing tests** (append to `store_test.go`; `seedAccount` already creates `dev_1`/`acc_1`)

```go
func seedChatAccount(t *testing.T, s *Store) string {
	t.Helper()
	seedDeveloper(t, s, "dev_1", "dev1@example.com")
	a := model.Account{ID: "acc_wa", DeveloperID: "dev_1", Provider: "WHATSAPP", Kind: model.AccountKindChat,
		Email: "+919888000000", Status: model.AccountOK}
	if err := s.UpsertAccount(a); err != nil {
		t.Fatal(err)
	}
	return a.ID
}

func TestAccountKindRoundTripsAndDefaultsToMail(t *testing.T) {
	s := newTestStore(t)
	acct := seedAccount(t, s)
	got, _ := s.GetAnyAccount(acct)
	if got.Kind != model.AccountKindMail || got.Identifier != got.Email {
		t.Fatalf("mail account = %+v", got)
	}
	wa := seedChatAccount(t, s)
	got, _ = s.GetAnyAccount(wa)
	if got.Kind != model.AccountKindChat || got.Identifier != "+919888000000" {
		t.Fatalf("chat account = %+v", got)
	}
	all, _ := s.ListChatAccounts()
	if len(all) != 1 || all[0].ID != wa {
		t.Fatalf("ListChatAccounts = %+v", all)
	}
}

func TestChatMessagesAreIdempotentAndPaged(t *testing.T) {
	s := newTestStore(t)
	acct := seedChatAccount(t, s)
	if err := s.UpsertChat(model.Chat{AccountID: acct, ID: "c1", Kind: "direct", Name: "Ada"}); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 8, 25, 10, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		ins, err := s.UpsertChatMessage(model.ChatMessage{AccountID: acct, ID: fmt.Sprintf("m%d", i), ChatID: "c1",
			Sender: model.Attendee{ID: "a1"}, Kind: "text", Text: fmt.Sprintf("t%d", i), SentAt: base.Add(time.Duration(i) * time.Minute)})
		if err != nil || !ins {
			t.Fatalf("insert %d: %v %v", i, ins, err)
		}
	}
	// Replay of an existing id changes nothing and reports not-inserted.
	ins, err := s.UpsertChatMessage(model.ChatMessage{AccountID: acct, ID: "m2", ChatID: "c1", Sender: model.Attendee{ID: "a1"}, Kind: "text", Text: "changed", SentAt: base})
	if err != nil || ins {
		t.Fatalf("replay inserted=%v err=%v", ins, err)
	}
	if m, _ := s.GetChatMessage(acct, "m2"); m.Text != "t2" {
		t.Fatalf("replay mutated text: %q", m.Text)
	}
	page1, next, err := s.ListChatMessages(acct, "c1", "", 2)
	if err != nil || len(page1) != 2 || page1[0].ID != "m4" || page1[1].ID != "m3" || next != "m3" {
		t.Fatalf("page1 = %v next=%q err=%v", ids(page1), next, err)
	}
	// A new message arriving does not disturb the next page.
	_, _ = s.UpsertChatMessage(model.ChatMessage{AccountID: acct, ID: "m9", ChatID: "c1", Sender: model.Attendee{ID: "a1"}, Kind: "text", Text: "new", SentAt: base.Add(time.Hour)})
	page2, next2, _ := s.ListChatMessages(acct, "c1", next, 2)
	if len(page2) != 2 || page2[0].ID != "m2" || page2[1].ID != "m1" || next2 != "m1" {
		t.Fatalf("page2 = %v next=%q", ids(page2), next2)
	}
	last, next3, _ := s.ListChatMessages(acct, "c1", next2, 2)
	if len(last) != 1 || last[0].ID != "m0" || next3 != "" {
		t.Fatalf("last = %v next=%q", ids(last), next3)
	}
}

func ids(ms []model.ChatMessage) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.ID
	}
	return out
}

func TestReactionsMergeAndRemove(t *testing.T) {
	s := newTestStore(t)
	acct := seedChatAccount(t, s)
	_ = s.UpsertChat(model.Chat{AccountID: acct, ID: "c1", Kind: "direct"})
	_, _ = s.UpsertChatMessage(model.ChatMessage{AccountID: acct, ID: "m1", ChatID: "c1", Sender: model.Attendee{ID: "a1"}, Kind: "text", Text: "x", SentAt: time.Now()})
	now := time.Now().UTC().Truncate(time.Second)
	if err := s.ApplyReaction(acct, "m1", model.Reaction{AttendeeID: "a1", Emoji: "👍", At: now}); err != nil {
		t.Fatal(err)
	}
	_ = s.ApplyReaction(acct, "m1", model.Reaction{AttendeeID: "a2", Emoji: "❤️", At: now})
	_ = s.ApplyReaction(acct, "m1", model.Reaction{AttendeeID: "a1", Emoji: "😂", At: now}) // replaces a1's
	m, _ := s.GetChatMessage(acct, "m1")
	if len(m.Reactions) != 2 {
		t.Fatalf("reactions = %+v", m.Reactions)
	}
	_ = s.ApplyReaction(acct, "m1", model.Reaction{AttendeeID: "a1", Emoji: "", At: now}) // removes a1's
	m, _ = s.GetChatMessage(acct, "m1")
	if len(m.Reactions) != 1 || m.Reactions[0].AttendeeID != "a2" {
		t.Fatalf("after remove = %+v", m.Reactions)
	}
}

func TestChatUnreadFlagsEditRevokeStatusRename(t *testing.T) {
	s := newTestStore(t)
	acct := seedChatAccount(t, s)
	_ = s.UpsertChat(model.Chat{AccountID: acct, ID: "c1", Kind: "group", Name: "Team"})
	at := time.Now().UTC().Truncate(time.Second)
	if err := s.BumpChat(acct, "c1", at, 1); err != nil {
		t.Fatal(err)
	}
	_ = s.BumpChat(acct, "c1", at.Add(time.Second), 1)
	c, _ := s.GetChat(acct, "c1")
	if c.UnreadCount != 2 || c.LastMessageAt == nil || !c.LastMessageAt.Equal(at.Add(time.Second)) {
		t.Fatalf("bump = %+v", c)
	}
	_ = s.ClearUnread(acct, "c1")
	tru := true
	_ = s.SetChatFlags(acct, "c1", &tru, nil)
	c, _ = s.GetChat(acct, "c1")
	if c.UnreadCount != 0 || !c.Archived || c.Muted {
		t.Fatalf("flags = %+v", c)
	}
	_ = s.ReplaceChatMembers(acct, "c1", []model.ChatMember{{ChatID: "c1", AttendeeID: "a1", Role: "admin"}, {ChatID: "c1", AttendeeID: "a2"}})
	_ = s.UpsertAttendee(model.Attendee{ID: "a1", Phone: "+911", Name: "One"}, acct)
	c, _ = s.GetChat(acct, "c1")
	if len(c.Members) != 2 || c.Members[0].ID != "a1" || c.Members[0].Phone != "+911" {
		t.Fatalf("members = %+v", c.Members)
	}
	_, _ = s.UpsertChatMessage(model.ChatMessage{AccountID: acct, ID: "tmp1", ChatID: "c1", Sender: model.Attendee{ID: "self"}, IsFromMe: true, Kind: "text", Text: "hello", SentAt: at, Status: "sending"})
	if err := s.RenameChatMessage(acct, "tmp1", "REAL1"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetMessageStatus(acct, []string{"REAL1"}, "read"); err != nil {
		t.Fatal(err)
	}
	if err := s.EditChatMessage(acct, "REAL1", "hello!", at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	m, _ := s.GetChatMessage(acct, "REAL1")
	if m.Status != "read" || m.Text != "hello!" || m.EditedAt == nil {
		t.Fatalf("after edit = %+v", m)
	}
	_ = s.RevokeChatMessage(acct, "REAL1")
	m, _ = s.GetChatMessage(acct, "REAL1")
	if !m.Deleted || m.Text != "" {
		t.Fatalf("after revoke = %+v", m)
	}
	lst, _ := s.ListChats(ChatQuery{AccountID: acct, Kind: "group"})
	if len(lst) != 1 {
		t.Fatalf("ListChats kind=group = %d", len(lst))
	}
}

func TestChatSessionAndIdempotency(t *testing.T) {
	s := newTestStore(t)
	acct := seedChatAccount(t, s)
	if _, err := s.ChatSession(acct); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing session err = %v", err)
	}
	if err := s.SaveChatSession(acct, "WHATSAPP", "919888000000:5@s.whatsapp.net"); err != nil {
		t.Fatal(err)
	}
	if jid, _ := s.ChatSession(acct); jid != "919888000000:5@s.whatsapp.net" {
		t.Fatalf("jid = %q", jid)
	}
	_ = s.DeleteChatSession(acct)
	if _, err := s.ChatSession(acct); !errors.Is(err, ErrNotFound) {
		t.Fatal("session survived delete")
	}
	if err := s.PutIdempotency("dev_1", "k1", []byte(`{"id":"x"}`)); err != nil {
		t.Fatal(err)
	}
	if b, err := s.GetIdempotency("dev_1", "k1"); err != nil || string(b) != `{"id":"x"}` {
		t.Fatalf("get = %s %v", b, err)
	}
	if _, err := s.GetIdempotency("dev_other", "k1"); !errors.Is(err, ErrNotFound) {
		t.Fatal("idempotency key leaked across developers")
	}
	s.PurgeIdempotency(time.Now().Add(time.Hour))
	if _, err := s.GetIdempotency("dev_1", "k1"); !errors.Is(err, ErrNotFound) {
		t.Fatal("purge did not remove")
	}
}

func TestDeletingChatAccountCascadesChatTables(t *testing.T) {
	s := newTestStore(t)
	acct := seedChatAccount(t, s)
	_ = s.UpsertChat(model.Chat{AccountID: acct, ID: "c1", Kind: "direct"})
	_ = s.ReplaceChatMembers(acct, "c1", []model.ChatMember{{ChatID: "c1", AttendeeID: "a1"}})
	_ = s.UpsertAttendee(model.Attendee{ID: "a1"}, acct)
	_, _ = s.UpsertChatMessage(model.ChatMessage{AccountID: acct, ID: "m1", ChatID: "c1", Sender: model.Attendee{ID: "a1"}, Kind: "text", SentAt: time.Now()})
	_ = s.SaveChatSession(acct, "WHATSAPP", "j")
	if err := s.DeleteAccount(acct); err != nil {
		t.Fatal(err)
	}
	for table, want := range map[string]int{"chats": 0, "chat_members": 0, "attendees": 0, "chat_messages": 0, "chat_sessions": 0} {
		var n int
		_ = s.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n)
		if n != want {
			t.Errorf("%s has %d rows after delete", table, n)
		}
	}
}

func TestAccountsKindMigrationOnTenancyDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tenancy.db")
	db, _ := sql.Open("sqlite", path)
	// Minimal multi-tenancy-era accounts table (has developer_id, no kind).
	if _, err := db.Exec(`CREATE TABLE developers (id TEXT PRIMARY KEY, email TEXT NOT NULL UNIQUE, password_hash TEXT NOT NULL, name TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL);
		CREATE TABLE accounts (id TEXT PRIMARY KEY, developer_id TEXT NOT NULL REFERENCES developers(id), provider TEXT NOT NULL, email TEXT NOT NULL,
		 name TEXT NOT NULL DEFAULT '', status TEXT NOT NULL, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL, last_synced_at INTEGER);
		INSERT INTO developers VALUES ('dev_1','d@x.com','h','',0);
		INSERT INTO accounts (id, developer_id, provider, email, status, created_at, updated_at) VALUES ('acc_old','dev_1','OUTLOOK','o@x.com','OK',0,0);`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	a, err := s.GetAnyAccount("acc_old")
	if err != nil || a.Kind != model.AccountKindMail {
		t.Fatalf("migrated account = %+v %v", a, err)
	}
}
```

Add imports `fmt`, `database/sql`, `errors`, `path/filepath` to the test file if missing.

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/store/`
Expected: build failure (`UpsertChat` etc. undefined; `Kind` unknown in scan).

- [ ] **Step 3: Schema + migration**

Append to `schema` in `internal/store/schema.go` (before the closing backtick):

```sql
-- ---- chat providers (WhatsApp) ----
CREATE TABLE IF NOT EXISTS chats (
  account_id      TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  id              TEXT NOT NULL,
  kind            TEXT NOT NULL,
  name            TEXT NOT NULL DEFAULT '',
  unread_count    INTEGER NOT NULL DEFAULT 0,
  last_message_at INTEGER,
  archived        INTEGER NOT NULL DEFAULT 0,
  muted           INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (account_id, id)
);
CREATE INDEX IF NOT EXISTS chats_by_activity ON chats(account_id, last_message_at DESC);

CREATE TABLE IF NOT EXISTS attendees (
  account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  id         TEXT NOT NULL,
  lid        TEXT NOT NULL DEFAULT '',
  phone      TEXT NOT NULL DEFAULT '',
  name       TEXT NOT NULL DEFAULT '',
  is_self    INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (account_id, id)
);

CREATE TABLE IF NOT EXISTS chat_members (
  account_id  TEXT NOT NULL,
  chat_id     TEXT NOT NULL,
  attendee_id TEXT NOT NULL,
  role        TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (account_id, chat_id, attendee_id),
  FOREIGN KEY (account_id, chat_id) REFERENCES chats(account_id, id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS chat_messages (
  account_id     TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  id             TEXT NOT NULL,
  chat_id        TEXT NOT NULL,
  sender_id      TEXT NOT NULL,
  is_from_me     INTEGER NOT NULL DEFAULT 0,
  kind           TEXT NOT NULL,
  text           TEXT NOT NULL DEFAULT '',
  quoted_id      TEXT NOT NULL DEFAULT '',
  sent_at        INTEGER NOT NULL,
  edited_at      INTEGER,
  deleted        INTEGER NOT NULL DEFAULT 0,
  status         TEXT NOT NULL DEFAULT '',
  reactions_json TEXT NOT NULL DEFAULT '[]',
  PRIMARY KEY (account_id, id)
);
CREATE INDEX IF NOT EXISTS chat_messages_by_chat ON chat_messages(account_id, chat_id, sent_at DESC, id DESC);

CREATE TABLE IF NOT EXISTS chat_sessions (
  account_id TEXT PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
  provider   TEXT NOT NULL,
  device_jid TEXT NOT NULL,
  updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS idempotency_keys (
  developer_id TEXT NOT NULL REFERENCES developers(id) ON DELETE CASCADE,
  key          TEXT NOT NULL,
  response     BLOB NOT NULL,
  created_at   INTEGER NOT NULL,
  PRIMARY KEY (developer_id, key)
);
```

Add `kind TEXT NOT NULL DEFAULT 'mail',` to the `accounts` CREATE (after `developer_id`), and re-introduce the additive-migration mechanism removed in tenancy:

```go
// migrations are additive column changes for databases created before the
// column existed. Each is safe to re-run: "duplicate column" is ignored.
var migrations = []string{
	`ALTER TABLE accounts ADD COLUMN kind TEXT NOT NULL DEFAULT 'mail'`,
}
```

and in `Open`, after `db.Exec(schema)`:

```go
	for _, m := range migrations {
		if _, err := db.Exec(m); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			return nil, fmt.Errorf("migrate: %w", err)
		}
	}
```

- [ ] **Step 4: Accounts scan/upsert**

`accountSelect` becomes `SELECT id, developer_id, kind, provider, email, name, status, created_at, updated_at, last_synced_at FROM accounts`; `scanAccount` scans `&a.Kind` after `&a.DeveloperID` and sets `a.Identifier = a.Email` at the end; `UpsertAccount` inserts `kind` (default to `model.AccountKindMail` when empty) and the `ON CONFLICT` keeps the stored `kind`. Add:

```go
// ListChatAccounts is UNSCOPED: the chat runtime attaches every live chat
// account at boot.
func (s *Store) ListChatAccounts() ([]model.Account, error) {
	return s.queryAccounts(accountSelect+` WHERE kind = ? AND status = ? ORDER BY created_at`, model.AccountKindChat, model.AccountOK)
}
```

- [ ] **Step 5: `internal/store/chat.go`** — implement every method in the Interfaces list with `?` placeholders. Key SQL:

```go
// UpsertChatMessage inserts a message and reports whether it was new. A
// replayed id (reconnect replay, own-message echo) is a no-op.
func (s *Store) UpsertChatMessage(m model.ChatMessage) (bool, error) {
	if m.Reactions == nil {
		m.Reactions = []model.Reaction{}
	}
	rj, _ := json.Marshal(m.Reactions)
	res, err := s.db.Exec(`
		INSERT INTO chat_messages (account_id, id, chat_id, sender_id, is_from_me, kind, text, quoted_id, sent_at, edited_at, deleted, status, reactions_json)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(account_id, id) DO NOTHING`,
		m.AccountID, m.ID, m.ChatID, m.Sender.ID, b2i(m.IsFromMe), m.Kind, m.Text, m.QuotedMessageID, m.SentAt.Unix(),
		nullUnix(m.EditedAt), b2i(m.Deleted), m.Status, string(rj))
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// ListChatMessages pages newest-first with a keyset cursor: `before` is the
// id of the last message of the previous page (its (sent_at, id) is the bound).
func (s *Store) ListChatMessages(accountID, chatID, before string, limit int) ([]model.ChatMessage, string, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	args := []any{accountID, chatID}
	where := `account_id = ? AND chat_id = ?`
	if before != "" {
		var sentAt int64
		err := s.db.QueryRow(`SELECT sent_at FROM chat_messages WHERE account_id = ? AND id = ?`, accountID, before).Scan(&sentAt)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, "", ErrNotFound
		} else if err != nil {
			return nil, "", err
		}
		where += ` AND (sent_at < ? OR (sent_at = ? AND id < ?))`
		args = append(args, sentAt, sentAt, before)
	}
	args = append(args, limit+1)
	rows, err := s.db.Query(chatMessageSelect+` WHERE `+where+` ORDER BY sent_at DESC, id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	out := []model.ChatMessage{}
	for rows.Next() {
		m, err := scanChatMessage(rows)
		if err != nil {
			return nil, "", err
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	next := ""
	if len(out) > limit {
		out = out[:limit]
		next = out[limit-1].ID
	}
	return out, next, nil
}
```

`ApplyReaction` reads `reactions_json`, filters out the attendee's entry, appends the new one when `Emoji != ""`, writes back. `RevokeChatMessage` sets `deleted=1, text=''`. `GetChat` joins members → attendees. `ListChats` filters `kind`, `unread_count > 0`, `name LIKE`, orders `last_message_at DESC NULLS LAST` (SQLite: `ORDER BY last_message_at IS NULL, last_message_at DESC`). `DeleteAccount` needs no change (cascades). `PurgeIdempotency(olderThan)` deletes `created_at < olderThan`.

- [ ] **Step 6: Run tests**

Run: `go test ./internal/store/ && go vet ./... && go test ./...`
Expected: all PASS (api tests seeding accounts still pass since `kind` defaults to mail).

- [ ] **Step 7: Commit**

```bash
git add internal/store
git commit -m "feat(store): chat tables, accounts.kind migration, chat repositories, idempotency keys"
```

---

### Task 3: Custodian — `ConnectLinked`, `DeleteLinked`, `DeviceJID`; syncer skips chat accounts

**Files:**
- Modify: `internal/accounts/accounts.go`, `internal/syncer/syncer.go` (`pollLoop`), `internal/syncer/subscriptions.go` (`reconcileSubscriptions`)
- Test: `internal/accounts/accounts_test.go` (new), `internal/syncer/syncer_test.go`

**Interfaces:**
- Produces: `(*Manager).ConnectLinked(ctx, developerID, providerName string, id provider.Identity, deviceJID string) (model.Account, error)` — creates/reuses `(developer_id, email=identifier)` with `Kind=chat`, `Status=OK`, saves `chat_sessions`; `(*Manager).DeviceJID(accountID) (string, error)`; `(*Manager).DeleteLinked(ctx, accountID string) error` — reads session, calls `provider.Chat().Forget(deviceJID)` (best effort, logged), deletes the session row and the account; `(*Manager).MarkLoggedOut(accountID, reason string)` — status `CREDENTIALS`, `OnStatusChange`, session deleted, `Forget`.
- Syncer: `pollLoop` and `reconcileSubscriptions` iterate `ListAllAccounts()` but `continue` when `registry.Get(a.Provider).Kind() != model.AccountKindMail`.

- [ ] **Step 1: Write the failing tests**

```go
// internal/accounts/accounts_test.go
package accounts

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/gauravrautela/unified-messaging/internal/model"
	"github.com/gauravrautela/unified-messaging/internal/provider"
	"github.com/gauravrautela/unified-messaging/internal/provider/providertest"
	"github.com/gauravrautela/unified-messaging/internal/store"
)

func newMgr(t *testing.T) (*Manager, *store.Store, *providertest.FakeChat) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "acc.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.CreateDeveloper(model.Developer{ID: "dev_1", Email: "d@x.com"}, "h"); err != nil {
		t.Fatal(err)
	}
	fake := providertest.NewFakeChat("WHATSAPP")
	m := NewManager(db, make([]byte, 32), slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.SetRegistry(provider.NewRegistry(fake))
	return m, db, fake
}

func TestConnectLinkedCreatesChatAccountAndReusesOnSameNumber(t *testing.T) {
	m, db, _ := newMgr(t)
	ctx := context.Background()
	a, err := m.ConnectLinked(ctx, "dev_1", "WHATSAPP", provider.Identity{Identifier: "+919888000000", Name: "G"}, "919888000000:5@s.whatsapp.net")
	if err != nil {
		t.Fatal(err)
	}
	if a.Kind != model.AccountKindChat || a.Identifier != "+919888000000" || a.Status != model.AccountOK || a.DeveloperID != "dev_1" {
		t.Fatalf("account = %+v", a)
	}
	if jid, _ := m.DeviceJID(a.ID); jid != "919888000000:5@s.whatsapp.net" {
		t.Fatalf("jid = %q", jid)
	}
	_ = db.SetAccountStatus(a.ID, model.AccountCredentials)
	b, err := m.ConnectLinked(ctx, "dev_1", "WHATSAPP", provider.Identity{Identifier: "+919888000000"}, "919888000000:6@s.whatsapp.net")
	if err != nil || b.ID != a.ID || b.Status != model.AccountOK {
		t.Fatalf("relink = %+v %v", b, err)
	}
	if jid, _ := m.DeviceJID(a.ID); jid != "919888000000:6@s.whatsapp.net" {
		t.Fatalf("jid after relink = %q", jid)
	}
}

func TestDeleteLinkedForgetsDevice(t *testing.T) {
	m, db, fake := newMgr(t)
	ctx := context.Background()
	a, _ := m.ConnectLinked(ctx, "dev_1", "WHATSAPP", provider.Identity{Identifier: "+91"}, "j1")
	if err := m.DeleteLinked(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if got := fake.Forgotten(); len(got) != 1 || got[0] != "j1" {
		t.Fatalf("forgotten = %v", got)
	}
	if _, err := db.GetAnyAccount(a.ID); err == nil {
		t.Fatal("account survived DeleteLinked")
	}
}

func TestMarkLoggedOutFlipsToCredentialsAndForgets(t *testing.T) {
	m, db, fake := newMgr(t)
	ctx := context.Background()
	a, _ := m.ConnectLinked(ctx, "dev_1", "WHATSAPP", provider.Identity{Identifier: "+91"}, "j1")
	var status string
	m.OnStatusChange = func(id, st string) { status = st }
	m.MarkLoggedOut(a.ID, "device removed")
	got, _ := db.GetAnyAccount(a.ID)
	if got.Status != model.AccountCredentials || status != model.AccountCredentials || len(fake.Forgotten()) != 1 {
		t.Fatalf("status=%s cb=%s forgotten=%v", got.Status, status, fake.Forgotten())
	}
	if _, err := db.ChatSession(a.ID); err == nil {
		t.Fatal("session survived logout")
	}
}
```

In `internal/syncer/syncer_test.go` add:

```go
// The poll loop must not try to delta-sync a chat account: it has no Mailbox.
func TestPollSkipsChatAccounts(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "sync.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_ = db.CreateDeveloper(model.Developer{ID: "dev_1", Email: "dev@example.com"}, "hash")
	_ = db.UpsertAccount(model.Account{ID: "acc_wa", DeveloperID: "dev_1", Provider: "FAKECHAT", Kind: model.AccountKindChat, Email: "+91", Status: model.AccountOK})
	fake := providertest.NewFakeChat("FAKECHAT")
	log, recs := logx.Capture()
	s := New(db, provider.NewRegistry(fake), nil, events.NewDispatcher(db, log), log, Options{PollInterval: time.Hour})
	s.pollOnce(context.Background())
	if recs.Contains("sync run started") {
		t.Fatalf("chat account was polled: %v", recs.All())
	}
	if !recs.Contains("skipping non-mail account") {
		t.Fatalf("expected skip decision: %v", recs.All())
	}
}
```

(Extract the body of the ticker case in `pollLoop` into `func (s *Syncer) pollOnce(ctx)` so it is testable.)

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/accounts/ ./internal/syncer/`
Expected: build failure (`ConnectLinked` etc. undefined; `pollOnce` undefined).

- [ ] **Step 3: Implement the custodian methods** (append to `accounts.go`)

```go
// ConnectLinked records a chat account after a successful device link. The
// device's credentials live in the provider's own store; we keep only the
// mapping account -> device JID. Relinking the same number under the same
// developer reuses the account id and replaces the device.
func (m *Manager) ConnectLinked(ctx context.Context, developerID, providerName string, id provider.Identity, deviceJID string) (model.Account, error) {
	log := logx.From(ctx).With("component", "accounts", "developer_id", developerID, "provider", providerName)
	if id.Identifier == "" {
		return model.Account{}, errors.New("accounts: provider did not report an identifier")
	}
	accountID, err := m.store.AccountIDByEmail(developerID, id.Identifier)
	relink := err == nil
	if errors.Is(err, store.ErrNotFound) {
		if accountID, err = newID("acc"); err != nil {
			return model.Account{}, err
		}
	} else if err != nil {
		return model.Account{}, err
	}
	if err := m.store.UpsertAccount(model.Account{
		ID: accountID, DeveloperID: developerID, Kind: model.AccountKindChat, Provider: providerName,
		Email: id.Identifier, Name: id.Name, Status: model.AccountOK,
	}); err != nil {
		return model.Account{}, err
	}
	realID, err := m.store.AccountIDByEmail(developerID, id.Identifier)
	if err != nil {
		return model.Account{}, err
	}
	if relink {
		// Drop the previous device so its keys do not linger.
		if old, err := m.store.ChatSession(realID); err == nil && old != deviceJID {
			m.forget(ctx, providerName, old)
		}
	}
	if err := m.store.SaveChatSession(realID, providerName, deviceJID); err != nil {
		return model.Account{}, err
	}
	log.Info("chat account linked", "account_id", realID, "relink", relink)
	return m.store.GetAnyAccount(realID)
}

func (m *Manager) DeviceJID(accountID string) (string, error) { return m.store.ChatSession(accountID) }

// DeleteLinked removes a chat account and its device credentials.
func (m *Manager) DeleteLinked(ctx context.Context, accountID string) error {
	acct, err := m.store.GetAnyAccount(accountID)
	if err != nil {
		return err
	}
	if jid, err := m.store.ChatSession(accountID); err == nil {
		m.forget(ctx, acct.Provider, jid)
	}
	if err := m.store.DeleteChatSession(accountID); err != nil {
		return err
	}
	return m.store.DeleteAccount(accountID)
}

// MarkLoggedOut is the chat counterpart of a rejected refresh token: the
// phone removed the linked device, so only the end user can fix it.
func (m *Manager) MarkLoggedOut(accountID, reason string) {
	acct, err := m.store.GetAnyAccount(accountID)
	if err != nil {
		return
	}
	if jid, err := m.store.ChatSession(accountID); err == nil {
		m.forget(context.Background(), acct.Provider, jid)
	}
	_ = m.store.DeleteChatSession(accountID)
	m.log.Warn("chat account logged out", "account_id", accountID, "reason", reason)
	m.markCredentials(accountID)
}

func (m *Manager) forget(ctx context.Context, providerName, deviceJID string) {
	p, err := m.registry.Get(providerName)
	if err != nil || p.Chat() == nil {
		return
	}
	if err := p.Chat().Forget(ctx, deviceJID); err != nil {
		m.log.Warn("forgetting device", "provider", providerName, "err", err)
	}
}
```

- [ ] **Step 4: Syncer guards**

In `pollLoop`'s tick and `reconcileSubscriptions`, before waking/ensuring an account:

```go
			p, err := s.registry.Get(a.Provider)
			if err != nil || p.Kind() != model.AccountKindMail {
				s.log.Debug("skipping non-mail account", "account_id", a.ID, "provider", a.Provider)
				continue
			}
```

Extract `pollOnce`.

- [ ] **Step 5: Run tests**

Run: `gofmt -l internal cmd; go vet ./... && go test ./...`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/accounts internal/syncer
git commit -m "feat(accounts): linked-device custody; syncer ignores chat accounts"
```

---

### Task 4: WhatsApp adapter — container, Linker (QR), Connect/Forget, inbound translation, roster

**Files:**
- Create: `internal/provider/whatsapp/whatsapp.go`, `link.go`, `conn.go`, `translate.go`, `translate_test.go`
- Modify: `go.mod`/`go.sum` (`go get go.mau.fi/whatsmeow@v0.0.0-20260821141805-33cfac511629 github.com/skip2/go-qrcode@latest`)

**Interfaces:**
- Produces: `whatsapp.New(db *sql.DB, deviceName string, log *slog.Logger) (*Provider, error)` — builds `sqlstore.NewWithDB(db, "sqlite3", …)`, runs `Upgrade`, sets `store.DeviceProps.Os = deviceName`; `Provider` implements `provider.Provider` with `Kind()=chat`, `Linker()`, `Chat()`; `const Name = "WHATSAPP"`.
- `translate.go` (pure, tested without network): `chatKind(jid types.JID) string`; `attendeeFrom(jid, alt types.JID, pushName string) model.Attendee` (id = phone JID user part when `Server==DefaultUserServer`, `phone = "+"+user`; else id = `lid:<user>`); `messageFrom(evt *events.Message) (model.ChatMessage, kind string)` handling `Conversation`, `ExtendedTextMessage` (text + `ContextInfo.StanzaID` → quoted), `ReactionMessage`, `ProtocolMessage{REVOKE|MESSAGE_EDIT}`, media types → `unsupported` with label; `receiptStatus(t types.ReceiptType) (string, bool)` (`""`/`ReceiptTypeDelivered` → "delivered", `ReceiptTypeRead`/`ReadSelf` → "read", others → skip).

- [ ] **Step 1: Failing translation tests**

```go
// internal/provider/whatsapp/translate_test.go
package whatsapp

import (
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

func TestAttendeeFromPhoneAndLID(t *testing.T) {
	a := attendeeFrom(types.NewJID("919888000000", types.DefaultUserServer), types.JID{}, "Ada")
	if a.ID != "919888000000@s.whatsapp.net" || a.Phone != "+919888000000" || a.Name != "Ada" {
		t.Fatalf("phone attendee = %+v", a)
	}
	l := attendeeFrom(types.NewJID("234488185487529", types.HiddenUserServer), types.JID{}, "Nim")
	if l.ID != "234488185487529@lid" || l.Phone != "" {
		t.Fatalf("lid attendee = %+v", l)
	}
	// A LID sender whose phone alt is known resolves to the phone id.
	r := attendeeFrom(types.NewJID("234488185487529", types.HiddenUserServer), types.NewJID("919888000001", types.DefaultUserServer), "Nim")
	if r.ID != "919888000001@s.whatsapp.net" || r.Phone != "+919888000001" {
		t.Fatalf("resolved attendee = %+v", r)
	}
}

func TestChatKind(t *testing.T) {
	if chatKind(types.NewJID("1203", types.GroupServer)) != "group" || chatKind(types.NewJID("91", types.DefaultUserServer)) != "direct" {
		t.Fatal("chatKind wrong")
	}
}

func evt(chat types.JID, id string, msg *waE2E.Message) *events.Message {
	return &events.Message{Info: types.MessageInfo{
		MessageSource: types.MessageSource{Chat: chat, Sender: types.NewJID("91", types.DefaultUserServer), IsGroup: chat.Server == types.GroupServer},
		ID: id, PushName: "Ada", Timestamp: time.Unix(1_700_000_000, 0)}, Message: msg}
}

func TestMessageFromTextQuoteReactionRevokeEditMedia(t *testing.T) {
	chat := types.NewJID("91", types.DefaultUserServer)
	m, kind := messageFrom(evt(chat, "A", &waE2E.Message{Conversation: proto.String("hi")}))
	if kind != "message" || m.Text != "hi" || m.Kind != "text" || m.ChatID != chat.String() || m.Sender.Name != "Ada" {
		t.Fatalf("text = %+v kind=%s", m, kind)
	}
	m, _ = messageFrom(evt(chat, "B", &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{
		Text: proto.String("reply"), ContextInfo: &waE2E.ContextInfo{StanzaID: proto.String("A")}}}))
	if m.Text != "reply" || m.QuotedMessageID != "A" {
		t.Fatalf("quote = %+v", m)
	}
	m, kind = messageFrom(evt(chat, "C", &waE2E.Message{ReactionMessage: &waE2E.ReactionMessage{
		Key: &waCommon.MessageKey{ID: proto.String("A")}, Text: proto.String("👍")}}))
	if kind != "reaction" || m.QuotedMessageID != "A" || m.Text != "👍" {
		t.Fatalf("reaction = %+v kind=%s", m, kind)
	}
	m, kind = messageFrom(evt(chat, "D", &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
		Type: waE2E.ProtocolMessage_REVOKE.Enum(), Key: &waCommon.MessageKey{ID: proto.String("A")}}}))
	if kind != "revoke" || m.QuotedMessageID != "A" {
		t.Fatalf("revoke = %+v kind=%s", m, kind)
	}
	m, kind = messageFrom(evt(chat, "E", &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
		Type: waE2E.ProtocolMessage_MESSAGE_EDIT.Enum(), Key: &waCommon.MessageKey{ID: proto.String("A")},
		EditedMessage: &waE2E.Message{Conversation: proto.String("hi!")}}}))
	if kind != "edit" || m.QuotedMessageID != "A" || m.Text != "hi!" {
		t.Fatalf("edit = %+v kind=%s", m, kind)
	}
	m, kind = messageFrom(evt(chat, "F", &waE2E.Message{ImageMessage: &waE2E.ImageMessage{}}))
	if kind != "message" || m.Kind != "unsupported" || m.Text != "[image]" {
		t.Fatalf("media = %+v", m)
	}
}

func TestReceiptStatus(t *testing.T) {
	if s, ok := receiptStatus(types.ReceiptTypeDelivered); !ok || s != "delivered" {
		t.Fatal("delivered")
	}
	if s, ok := receiptStatus(types.ReceiptTypeRead); !ok || s != "read" {
		t.Fatal("read")
	}
	if _, ok := receiptStatus(types.ReceiptTypeSender); ok {
		t.Fatal("sender receipts must be ignored")
	}
}
```

- [ ] **Step 2: Add deps, run to verify failure**

Run: `go get go.mau.fi/whatsmeow@v0.0.0-20260821141805-33cfac511629 github.com/skip2/go-qrcode@latest && go mod tidy && go test ./internal/provider/whatsapp/`
Expected: build failure (functions undefined).

- [ ] **Step 3: `translate.go`**

```go
package whatsapp

import (
	"strings"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"

	"github.com/gauravrautela/unified-messaging/internal/model"
)

func chatKind(jid types.JID) string {
	if jid.Server == types.GroupServer {
		return "group"
	}
	return "direct"
}

// attendeeFrom builds the API identity for a WhatsApp user. Phone JIDs are the
// stable public id; a LID (privacy id) is used only when no phone is known.
func attendeeFrom(jid, alt types.JID, pushName string) model.Attendee {
	pick := jid
	if jid.Server == types.HiddenUserServer && alt.Server == types.DefaultUserServer {
		pick = alt
	}
	a := model.Attendee{ID: pick.ToNonAD().String(), Name: pushName}
	if pick.Server == types.DefaultUserServer {
		a.Phone = "+" + pick.User
	}
	return a
}

// messageFrom classifies an inbound event: kind is one of
// message | reaction | revoke | edit. For reaction/revoke/edit the target id
// is returned in QuotedMessageID and the new text/emoji in Text.
func messageFrom(e *events.Message) (model.ChatMessage, string) {
	m := model.ChatMessage{
		ID: e.Info.ID, ChatID: e.Info.Chat.String(), IsFromMe: e.Info.IsFromMe,
		Sender: attendeeFrom(e.Info.Sender, e.Info.SenderAlt, e.Info.PushName),
		SentAt: e.Info.Timestamp.UTC(), Kind: "text", Reactions: []model.Reaction{},
	}
	msg := e.Message
	switch {
	case msg.GetReactionMessage() != nil:
		m.QuotedMessageID = msg.GetReactionMessage().GetKey().GetID()
		m.Text = msg.GetReactionMessage().GetText()
		return m, "reaction"
	case msg.GetProtocolMessage() != nil:
		p := msg.GetProtocolMessage()
		m.QuotedMessageID = p.GetKey().GetID()
		switch p.GetType() {
		case waE2E.ProtocolMessage_REVOKE:
			return m, "revoke"
		case waE2E.ProtocolMessage_MESSAGE_EDIT:
			m.Text = textOf(p.GetEditedMessage())
			return m, "edit"
		}
		m.Kind = "unsupported"
		m.Text = "[protocol]"
		return m, "message"
	case msg.GetConversation() != "":
		m.Text = msg.GetConversation()
	case msg.GetExtendedTextMessage() != nil:
		m.Text = msg.GetExtendedTextMessage().GetText()
		m.QuotedMessageID = msg.GetExtendedTextMessage().GetContextInfo().GetStanzaID()
	default:
		m.Kind = "unsupported"
		m.Text = mediaLabel(msg)
	}
	return m, "message"
}

func textOf(msg *waE2E.Message) string {
	if msg.GetConversation() != "" {
		return msg.GetConversation()
	}
	return msg.GetExtendedTextMessage().GetText()
}

func mediaLabel(msg *waE2E.Message) string {
	switch {
	case msg.GetImageMessage() != nil:
		return "[image]"
	case msg.GetVideoMessage() != nil:
		return "[video]"
	case msg.GetAudioMessage() != nil:
		return "[audio]"
	case msg.GetDocumentMessage() != nil:
		return "[document]"
	case msg.GetStickerMessage() != nil:
		return "[sticker]"
	case msg.GetLocationMessage() != nil, msg.GetLiveLocationMessage() != nil:
		return "[location]"
	case msg.GetContactMessage() != nil, msg.GetContactsArrayMessage() != nil:
		return "[contact]"
	case msg.GetPollCreationMessage() != nil:
		return "[poll]"
	default:
		return "[unsupported]"
	}
}

func receiptStatus(t types.ReceiptType) (string, bool) {
	switch t {
	case types.ReceiptTypeDelivered:
		return "delivered", true
	case types.ReceiptTypeRead, types.ReceiptTypeReadSelf:
		return "read", true
	}
	return "", false
}

func phoneToJID(e164 string) types.JID {
	return types.NewJID(strings.TrimPrefix(e164, "+"), types.DefaultUserServer)
}

var _ = time.Now
```

(Check exact getter names against the vendored `waE2E` package — `GetPollCreationMessage`, `GetLiveLocationMessage`, `GetContactsArrayMessage` exist in this version; if a getter is missing, drop that case.)

- [ ] **Step 4: `whatsapp.go` (factory + provider), `link.go` (Linker), `conn.go` (Connect/Forget/roster + event handler)**

```go
// whatsapp.go
package whatsapp

const Name = "WHATSAPP"

type Provider struct {
	container  *sqlstore.Container
	deviceName string
	log        *slog.Logger

	mu    sync.Mutex
	conns map[string]*conn // accountID -> live connection (for commands)
}

func New(db *sql.DB, deviceName string, log *slog.Logger) (*Provider, error) {
	c := sqlstore.NewWithDB(db, "sqlite3", waLog.Noop) // whatsmeow's own logger is silenced; we log ourselves
	if err := c.Upgrade(context.Background()); err != nil {
		return nil, fmt.Errorf("whatsapp: store upgrade: %w", err)
	}
	store.DeviceProps.Os = proto.String(deviceName)
	return &Provider{container: c, deviceName: deviceName, log: log.With("component", "whatsapp"), conns: map[string]*conn{}}, nil
}

func (p *Provider) Name() string                 { return Name }
func (p *Provider) Kind() string                 { return model.AccountKindChat }
func (p *Provider) Auth() provider.Authenticator { return nil }
func (p *Provider) Mailbox() provider.Mailbox    { return nil }
func (p *Provider) Push() provider.Pusher        { return nil }
func (p *Provider) Linker() provider.Linker      { return p }
func (p *Provider) Chat() provider.Chatter       { return p }
```

```go
// link.go — StartLink: a fresh device, a client, a QR channel.
func (p *Provider) StartLink(ctx context.Context) (provider.LinkSession, error) {
	device := p.container.NewDevice()
	client := whatsmeow.NewClient(device, waLog.Noop)
	qr, err := client.GetQRChannel(ctx)
	if err != nil {
		return nil, err
	}
	s := &linkSession{codes: make(chan provider.LinkCode, 8), result: make(chan provider.LinkResult, 1), client: client}
	client.AddEventHandler(func(evt any) {
		if ps, ok := evt.(*events.PairSuccess); ok {
			s.done(provider.LinkResult{
				Identity:  provider.Identity{Identifier: "+" + ps.ID.User, Name: ps.BusinessName},
				DeviceJID: ps.ID.String(),
			})
		}
	})
	if err := client.Connect(); err != nil {
		return nil, err
	}
	go func() {
		for item := range qr {
			switch item.Event {
			case "code":
				s.codes <- provider.LinkCode{Code: item.Code, ExpiresAt: time.Now().Add(item.Timeout)}
			case "timeout":
				s.done(provider.LinkResult{Err: provider.ErrLinkTimeout})
			case "error":
				s.done(provider.LinkResult{Err: item.Error})
			}
		}
	}()
	return s, nil
}
```

`linkSession.done` uses `sync.Once`, closes `codes`, sends the result, and — on success — **disconnects the pairing client** (`client.Disconnect()`) so the runtime can open the real connection with `GetDevice(jid)`; on failure it also calls `container.DeleteDevice(device)` to avoid orphan rows. `Close()` = `done(ErrLinkCancelled)`. Add `ErrLinkTimeout`/`ErrLinkCancelled` to `provider`.

```go
// conn.go
type conn struct {
	p         *Provider
	accountID string
	client    *whatsmeow.Client
	sink      provider.EventSink
}

func (p *Provider) Connect(ctx context.Context, accountID, deviceJID string, sink provider.EventSink) (provider.ChatConn, error) {
	jid, err := types.ParseJID(deviceJID)
	if err != nil {
		return nil, err
	}
	device, err := p.container.GetDevice(ctx, jid)
	if err != nil {
		return nil, err
	}
	if device == nil {
		return nil, provider.ErrReauthRequired // device rows gone → must relink
	}
	c := &conn{p: p, accountID: accountID, client: whatsmeow.NewClient(device, waLog.Noop), sink: sink}
	c.client.AddEventHandler(c.handle)
	if err := c.client.Connect(); err != nil {
		return nil, err
	}
	p.mu.Lock()
	p.conns[accountID] = c
	p.mu.Unlock()
	return c, nil
}

func (c *conn) Close() error {
	c.p.mu.Lock()
	delete(c.p.conns, c.accountID)
	c.p.mu.Unlock()
	c.client.Disconnect()
	return nil
}

func (c *conn) handle(evt any) {
	switch v := evt.(type) {
	case *events.Message:
		m, kind := messageFrom(v)
		m.AccountID = c.accountID
		switch kind {
		case "message":
			chat := model.Chat{AccountID: c.accountID, ID: m.ChatID, Kind: chatKind(v.Info.Chat)}
			if chat.Kind == "direct" {
				chat.Name = m.Sender.Name
			}
			c.sink.Message(c.accountID, m, chat, m.Sender)
		case "reaction":
			c.sink.Reaction(c.accountID, m.ChatID, m.QuotedMessageID, model.Reaction{AttendeeID: m.Sender.ID, Emoji: m.Text, At: m.SentAt})
		case "revoke":
			c.sink.Deleted(c.accountID, m.ChatID, m.QuotedMessageID)
		case "edit":
			c.sink.Edited(c.accountID, m.ChatID, m.QuotedMessageID, m.Text, m.SentAt)
		}
	case *events.Receipt:
		if st, ok := receiptStatus(v.Type); ok {
			ids := make([]string, len(v.MessageIDs))
			for i, id := range v.MessageIDs {
				ids[i] = string(id)
			}
			c.sink.Receipt(c.accountID, v.Chat.String(), ids, st)
		}
	case *events.LoggedOut:
		c.sink.Disconnected(c.accountID, "logged out: "+v.Reason.String(), true)
	case *events.TemporaryBan:
		c.sink.Disconnected(c.accountID, "temporary ban: "+v.Code.String(), true)
	case *events.StreamReplaced:
		c.sink.Disconnected(c.accountID, "stream replaced", false)
	case *events.Disconnected:
		c.sink.Disconnected(c.accountID, "disconnected", false)
	}
}

func (p *Provider) Forget(ctx context.Context, deviceJID string) error {
	jid, err := types.ParseJID(deviceJID)
	if err != nil {
		return err
	}
	device, err := p.container.GetDevice(ctx, jid)
	if err != nil || device == nil {
		return err
	}
	return p.container.DeleteDevice(ctx, device)
}

// Chats returns the roster we can know without history: joined groups (with
// members) and known contacts as direct chats. Names come from the contact
// store; numbers are only exposed for phone JIDs.
func (p *Provider) Chats(ctx context.Context, accountID string) ([]model.Chat, []model.Attendee, []model.ChatMember, error) {
	c := p.connFor(accountID)
	if c == nil {
		return nil, nil, nil, provider.ErrNotFound
	}
	groups, err := c.client.GetJoinedGroups(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	var chats []model.Chat
	var members []model.ChatMember
	seen := map[string]model.Attendee{}
	for _, g := range groups {
		chats = append(chats, model.Chat{AccountID: accountID, ID: g.JID.String(), Kind: "group", Name: g.Name})
		for _, part := range g.Participants {
			a := attendeeFrom(part.JID, part.PhoneNumber, "")
			if info, err := c.client.Store.Contacts.GetContact(ctx, part.JID); err == nil && info.Found {
				a.Name = firstNonEmpty(info.FullName, info.PushName, info.BusinessName)
			}
			seen[a.ID] = a
			role := ""
			if part.IsAdmin || part.IsSuperAdmin {
				role = "admin"
			}
			members = append(members, model.ChatMember{ChatID: g.JID.String(), AttendeeID: a.ID, Role: role})
		}
	}
	contacts, err := c.client.Store.Contacts.GetAllContacts(ctx)
	if err == nil {
		for jid, info := range contacts {
			if jid.Server != types.DefaultUserServer {
				continue
			}
			a := attendeeFrom(jid, types.JID{}, firstNonEmpty(info.FullName, info.PushName, info.BusinessName))
			seen[a.ID] = a
			chats = append(chats, model.Chat{AccountID: accountID, ID: jid.String(), Kind: "direct", Name: a.Name})
		}
	}
	self := attendeeFrom(*c.client.Store.ID, types.JID{}, c.client.Store.PushName)
	self.IsSelf = true
	seen[self.ID] = self
	attendees := make([]model.Attendee, 0, len(seen))
	for _, a := range seen {
		attendees = append(attendees, a)
	}
	return chats, attendees, members, nil
}
```

- [ ] **Step 5: Run tests**

Run: `gofmt -l internal cmd; go vet ./... && go test ./...`
Expected: translation tests PASS; everything else still green. Check `go build ./...` produces a cgo-free binary (`go env CGO_ENABLED` unchanged; whatsmeow is pure Go).

- [ ] **Step 6: Commit**

```bash
git add go.mod go.sum internal/provider
git commit -m "feat(whatsapp): adapter over whatsmeow — linking, connection, inbound translation, roster"
```

---

### Task 5: WhatsApp adapter — outbound commands

**Files:**
- Create: `internal/provider/whatsapp/commands.go`
- Test: `internal/provider/whatsapp/commands_test.go` (pure builders only)

**Interfaces:**
- Produces: `SendText`, `StartDirect`, `React`, `Edit`, `Delete`, `MarkRead`, `Logout` on `*Provider`, each resolving the live `conn` via `connFor(accountID)` (`provider.ErrNotFound` when not connected); builders `textMessage(text, quotedID string, quotedSender types.JID) *waE2E.Message` (pure, tested).

- [ ] **Step 1: Failing builder test**

```go
func TestTextMessageBuilderQuotes(t *testing.T) {
	m := textMessage("hi", "", types.JID{})
	if m.GetConversation() != "hi" || m.GetExtendedTextMessage() != nil {
		t.Fatalf("plain = %+v", m)
	}
	q := textMessage("re", "A", types.NewJID("91", types.DefaultUserServer))
	if q.GetExtendedTextMessage().GetText() != "re" || q.GetExtendedTextMessage().GetContextInfo().GetStanzaID() != "A" {
		t.Fatalf("quoted = %+v", q)
	}
}
```

- [ ] **Step 2: Run to verify failure** — `go test ./internal/provider/whatsapp/` → undefined `textMessage`.

- [ ] **Step 3: Implement**

```go
func textMessage(text, quotedID string, quotedSender types.JID) *waE2E.Message {
	if quotedID == "" {
		return &waE2E.Message{Conversation: proto.String(text)}
	}
	return &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{
		Text: proto.String(text),
		ContextInfo: &waE2E.ContextInfo{StanzaID: proto.String(quotedID), Participant: proto.String(quotedSender.String())},
	}}
}

func (p *Provider) connFor(accountID string) *conn {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.conns[accountID]
}

func (p *Provider) SendText(ctx context.Context, accountID, chatID, text, quotedID string) (provider.SendResult, error) {
	c := p.connFor(accountID)
	if c == nil {
		return provider.SendResult{}, provider.ErrNotFound
	}
	chat, err := types.ParseJID(chatID)
	if err != nil {
		return provider.SendResult{}, err
	}
	resp, err := c.client.SendMessage(ctx, chat, textMessage(text, quotedID, chat))
	if err != nil {
		return provider.SendResult{}, err
	}
	return provider.SendResult{MessageID: string(resp.ID)}, nil
}

func (p *Provider) StartDirect(ctx context.Context, accountID, phone string) (string, error) {
	c := p.connFor(accountID)
	if c == nil {
		return "", provider.ErrNotFound
	}
	res, err := c.client.IsOnWhatsApp(ctx, []string{phone})
	if err != nil {
		return "", err
	}
	if len(res) == 0 || !res[0].IsIn {
		return "", fmt.Errorf("%w: %s is not on WhatsApp", provider.ErrNotFound, phone)
	}
	return res[0].JID.ToNonAD().String(), nil
}

func (p *Provider) React(ctx context.Context, accountID, chatID, messageID, emoji string) error {
	c := p.connFor(accountID)
	if c == nil {
		return provider.ErrNotFound
	}
	chat, err := types.ParseJID(chatID)
	if err != nil {
		return err
	}
	_, err = c.client.SendMessage(ctx, chat, c.client.BuildReaction(chat, *c.client.Store.ID, types.MessageID(messageID), emoji))
	return err
}

func (p *Provider) Edit(ctx context.Context, accountID, chatID, messageID, text string) error {
	c := p.connFor(accountID)
	if c == nil {
		return provider.ErrNotFound
	}
	chat, err := types.ParseJID(chatID)
	if err != nil {
		return err
	}
	_, err = c.client.SendMessage(ctx, chat, c.client.BuildEdit(chat, types.MessageID(messageID), textMessage(text, "", chat)))
	return err
}

func (p *Provider) Delete(ctx context.Context, accountID, chatID, messageID string) error {
	c := p.connFor(accountID)
	if c == nil {
		return provider.ErrNotFound
	}
	chat, err := types.ParseJID(chatID)
	if err != nil {
		return err
	}
	_, err = c.client.SendMessage(ctx, chat, c.client.BuildRevoke(chat, *c.client.Store.ID, types.MessageID(messageID)))
	return err
}

func (p *Provider) MarkRead(ctx context.Context, accountID, chatID string, ids []string) error {
	c := p.connFor(accountID)
	if c == nil {
		return provider.ErrNotFound
	}
	chat, err := types.ParseJID(chatID)
	if err != nil {
		return err
	}
	mids := make([]types.MessageID, len(ids))
	for i, id := range ids {
		mids[i] = types.MessageID(id)
	}
	return c.client.MarkRead(ctx, mids, time.Now(), chat, chat)
}

func (p *Provider) Logout(ctx context.Context, accountID string) error {
	c := p.connFor(accountID)
	if c == nil {
		return nil
	}
	return c.client.Logout(ctx)
}
```

(For group chats `MarkRead`'s `sender` should be the message sender; v1 passes the chat JID, which whatsmeow accepts for direct chats and marks the chat read in groups — note this in the code comment.)

- [ ] **Step 4: Run tests, commit**

Run: `gofmt -l internal cmd; go vet ./... && go test ./...`

```bash
git add internal/provider/whatsapp
git commit -m "feat(whatsapp): outbound commands — send, start direct, react, edit, revoke, mark read, logout"
```

---

### Task 6: Chat runtime — supervisor, actor, backoff, EventSink → store + dispatcher

**Files:**
- Create: `internal/chatsync/runtime.go`, `actor.go`, `backoff.go`, `sink.go`, `runtime_test.go`, `sink_test.go`

**Interfaces:**
- Produces: `chatsync.New(s *store.Store, reg *provider.Registry, accts *accounts.Manager, d *events.Dispatcher, log *slog.Logger, opts Options{MaxAccounts int; Clock func() time.Time; Sleep func(context.Context, time.Duration)}) *Runtime`; `Start(ctx)`, `Attach(accountID) error` (`ErrCapacity` when full), `Detach(accountID)`, `Health() []model.AccountHealth`… concretely `HealthFor(accountID) (model.Connection, bool)`, `Count() int`, `Max() int`, `Wait()`.
- `backoff.go`: `next(attempt int) time.Duration` — `min(1s<<attempt, 5m)` with ±20 % jitter; `const maxFailures = 30`, `const stableAfter = 10 * time.Minute`.
- `sink.go`: `type sink struct{...}` implementing `provider.EventSink`, one per actor, serialising through the actor's channel so store writes and emits are ordered per account.

- [ ] **Step 1: Failing tests**

```go
// internal/chatsync/runtime_test.go
package chatsync

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gauravrautela/unified-messaging/internal/accounts"
	"github.com/gauravrautela/unified-messaging/internal/events"
	"github.com/gauravrautela/unified-messaging/internal/logx"
	"github.com/gauravrautela/unified-messaging/internal/model"
	"github.com/gauravrautela/unified-messaging/internal/provider"
	"github.com/gauravrautela/unified-messaging/internal/provider/providertest"
	"github.com/gauravrautela/unified-messaging/internal/store"
)

type harness struct {
	db     *store.Store
	fake   *providertest.FakeChat
	mgr    *accounts.Manager
	rt     *Runtime
	recs   *logx.Records
	sleeps []time.Duration
	mu     sync.Mutex
	hook   *httptest.Server
	got    []model.Event
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "cs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	_ = db.CreateDeveloper(model.Developer{ID: "dev_1", Email: "d@x.com"}, "h")
	h := &harness{db: db, fake: providertest.NewFakeChat("FAKECHAT")}
	h.hook = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var ev model.Event
		_ = json.NewDecoder(r.Body).Decode(&ev)
		h.mu.Lock(); h.got = append(h.got, ev); h.mu.Unlock()
	}))
	t.Cleanup(h.hook.Close)
	_ = db.SaveWebhook(model.Webhook{ID: "wh_1", DeveloperID: "dev_1", URL: h.hook.URL, CreatedAt: time.Now()})
	log, recs := logx.Capture()
	h.recs = recs
	h.mgr = accounts.NewManager(db, make([]byte, 32), log)
	reg := provider.NewRegistry(h.fake)
	h.mgr.SetRegistry(reg)
	disp := events.NewDispatcher(db, log)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	disp.Start(ctx)
	h.rt = New(db, reg, h.mgr, disp, log, Options{MaxAccounts: 2,
		Sleep: func(_ context.Context, d time.Duration) { h.mu.Lock(); h.sleeps = append(h.sleeps, d); h.mu.Unlock() }})
	h.rt.Start(ctx)
	return h
}

func (h *harness) link(t *testing.T, id string) string {
	t.Helper()
	a, err := h.mgr.ConnectLinked(context.Background(), "dev_1", "FAKECHAT", provider.Identity{Identifier: "+91" + id}, "j"+id)
	if err != nil {
		t.Fatal(err)
	}
	return a.ID
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatal("condition not met")
}

func (h *harness) events() []model.Event { h.mu.Lock(); defer h.mu.Unlock(); return append([]model.Event(nil), h.got...) }

func TestAttachConnectsAndReceivesMessage(t *testing.T) {
	h := newHarness(t)
	acc := h.link(t, "1")
	if err := h.rt.Attach(acc); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return h.fake.Sink(acc) != nil })
	if c, ok := h.rt.HealthFor(acc); !ok || c.State != "connected" {
		t.Fatalf("health = %+v %v", c, ok)
	}
	m := model.ChatMessage{ID: "M1", ChatID: "c1", Kind: "text", Text: "hello", SentAt: time.Now(), Sender: model.Attendee{ID: "a1", Phone: "+9199", Name: "Ada"}}
	h.fake.Sink(acc).Message(acc, m, model.Chat{ID: "c1", Kind: "direct", Name: "Ada"}, m.Sender)
	waitFor(t, func() bool { return len(h.events()) == 1 })
	ev := h.events()[0]
	if ev.Type != model.EventChatReceived || ev.Message == nil || ev.Message.ID != "M1" || ev.Chat == nil || ev.AccountID != acc {
		t.Fatalf("event = %+v", ev)
	}
	// Replay: no second event, store unchanged.
	h.fake.Sink(acc).Message(acc, m, model.Chat{ID: "c1", Kind: "direct"}, m.Sender)
	time.Sleep(100 * time.Millisecond)
	if len(h.events()) != 1 {
		t.Fatalf("replay emitted: %d events", len(h.events()))
	}
	c, _ := h.db.GetChat(acc, "c1")
	if c.UnreadCount != 1 {
		t.Fatalf("unread = %d", c.UnreadCount)
	}
	if h.recs.Contains("hello") || h.recs.Contains("+9199") {
		t.Fatal("message text or phone leaked into logs")
	}
	if !h.recs.Contains("component=chatsync") || !h.recs.Contains("conn_id=") || !h.recs.Contains("decision=new") || !h.recs.Contains("decision=replay") {
		t.Fatalf("expected decision logs: %v", h.recs.All())
	}
}

func TestOwnEchoAfterAPISendEmitsNothing(t *testing.T) {
	h := newHarness(t)
	acc := h.link(t, "1")
	_ = h.rt.Attach(acc)
	waitFor(t, func() bool { return h.fake.Sink(acc) != nil })
	// The API layer inserts the row before the echo arrives (Task 8); simulate that.
	_, _ = h.db.UpsertChatMessage(model.ChatMessage{AccountID: acc, ID: "S1", ChatID: "c1", IsFromMe: true, Kind: "text", Text: "x", SentAt: time.Now(), Status: "sent", Sender: model.Attendee{ID: "self"}})
	h.fake.Sink(acc).Message(acc, model.ChatMessage{ID: "S1", ChatID: "c1", IsFromMe: true, Kind: "text", Text: "x", SentAt: time.Now(), Sender: model.Attendee{ID: "self"}}, model.Chat{ID: "c1"}, model.Attendee{ID: "self"})
	time.Sleep(100 * time.Millisecond)
	if len(h.events()) != 0 {
		t.Fatalf("own echo emitted %d events", len(h.events()))
	}
	// A phone-originated own message (not in store) does emit chat_sent.
	h.fake.Sink(acc).Message(acc, model.ChatMessage{ID: "P1", ChatID: "c1", IsFromMe: true, Kind: "text", Text: "from phone", SentAt: time.Now(), Sender: model.Attendee{ID: "self"}}, model.Chat{ID: "c1"}, model.Attendee{ID: "self"})
	waitFor(t, func() bool { return len(h.events()) == 1 && h.events()[0].Type == model.EventChatSent })
}

func TestReceiptReactionEditDeleteMapping(t *testing.T) {
	h := newHarness(t)
	acc := h.link(t, "1")
	_ = h.rt.Attach(acc)
	waitFor(t, func() bool { return h.fake.Sink(acc) != nil })
	s := h.fake.Sink(acc)
	s.Message(acc, model.ChatMessage{ID: "M1", ChatID: "c1", Kind: "text", Text: "a", SentAt: time.Now(), Sender: model.Attendee{ID: "a1"}}, model.Chat{ID: "c1", Kind: "direct"}, model.Attendee{ID: "a1"})
	_, _ = h.db.UpsertChatMessage(model.ChatMessage{AccountID: acc, ID: "S1", ChatID: "c1", IsFromMe: true, Kind: "text", Text: "x", SentAt: time.Now(), Status: "sent", Sender: model.Attendee{ID: "self"}})
	s.Receipt(acc, "c1", []string{"S1"}, "read")
	s.Reaction(acc, "c1", "M1", model.Reaction{AttendeeID: "a1", Emoji: "👍", At: time.Now()})
	s.Edited(acc, "c1", "M1", "a!", time.Now())
	s.Deleted(acc, "c1", "M1")
	waitFor(t, func() bool { return len(h.events()) == 5 })
	types := []string{}
	for _, e := range h.events() {
		types = append(types, e.Type)
	}
	want := []string{model.EventChatReceived, model.EventChatUpdated, model.EventChatReaction, model.EventChatUpdated, model.EventChatDeleted}
	for i := range want {
		if types[i] != want[i] {
			t.Fatalf("events = %v want %v", types, want)
		}
	}
	m, _ := h.db.GetChatMessage(acc, "S1")
	if m.Status != "read" {
		t.Fatalf("receipt not applied: %+v", m)
	}
	m, _ = h.db.GetChatMessage(acc, "M1")
	if !m.Deleted || m.EditedAt == nil || len(m.Reactions) != 1 {
		t.Fatalf("mutations not applied: %+v", m)
	}
	if h.events()[3].Change != "edited" || h.events()[1].Status != "read" {
		t.Fatalf("payload fields: %+v %+v", h.events()[1], h.events()[3])
	}
}

func TestLogoutIsTerminalAndForgetsDevice(t *testing.T) {
	h := newHarness(t)
	acc := h.link(t, "1")
	_ = h.rt.Attach(acc)
	waitFor(t, func() bool { return h.fake.Sink(acc) != nil })
	h.fake.Disconnect(acc, "device removed", true)
	waitFor(t, func() bool { a, _ := h.db.GetAnyAccount(acc); return a.Status == model.AccountCredentials })
	waitFor(t, func() bool { for _, e := range h.events() { if e.Type == model.EventAccountError { return true } }; return false })
	if len(h.fake.Forgotten()) != 1 {
		t.Fatal("device not forgotten")
	}
	if c, ok := h.rt.HealthFor(acc); ok && c.State != "stopped" {
		t.Fatalf("actor still alive: %+v", c)
	}
}

func TestTransientDisconnectBacksOffAndReconnects(t *testing.T) {
	h := newHarness(t)
	acc := h.link(t, "1")
	_ = h.rt.Attach(acc)
	waitFor(t, func() bool { return h.fake.Sink(acc) != nil })
	h.fake.Disconnect(acc, "network", false)
	waitFor(t, func() bool { h.mu.Lock(); defer h.mu.Unlock(); return len(h.sleeps) >= 1 })
	waitFor(t, func() bool { c, _ := h.rt.HealthFor(acc); return c.State == "connected" && c.Reconnects == 1 })
	h.mu.Lock()
	if h.sleeps[0] < 800*time.Millisecond || h.sleeps[0] > 1200*time.Millisecond {
		t.Fatalf("first backoff = %v, want ~1s ±20%%", h.sleeps[0])
	}
	h.mu.Unlock()
}

func TestConnectFailuresExhaustToCredentials(t *testing.T) {
	h := newHarness(t)
	acc := h.link(t, "1")
	h.fake.ConnectErr = errors.New("boom")
	_ = h.rt.Attach(acc)
	waitFor(t, func() bool { a, _ := h.db.GetAnyAccount(acc); return a.Status == model.AccountCredentials })
	h.mu.Lock()
	n := len(h.sleeps)
	h.mu.Unlock()
	if n != maxFailures-1 && n != maxFailures {
		t.Fatalf("sleeps = %d, want ~%d", n, maxFailures)
	}
}

func TestCapacityAndStartAttachesOnlyOKChatAccounts(t *testing.T) {
	h := newHarness(t)
	a := h.link(t, "1")
	b := h.link(t, "2")
	c := h.link(t, "3")
	_ = h.db.SetAccountStatus(c, model.AccountCredentials)
	_ = h.db.UpsertAccount(model.Account{ID: "acc_mail", DeveloperID: "dev_1", Provider: "OUTLOOK", Email: "m@x.com", Status: model.AccountOK})
	h.rt.Start(context.Background()) // idempotent re-scan
	waitFor(t, func() bool { return h.fake.Sink(a) != nil && h.fake.Sink(b) != nil })
	if h.fake.Sink(c) != nil || h.rt.Count() != 2 {
		t.Fatalf("attached wrong set: count=%d", h.rt.Count())
	}
	d := h.link(t, "4")
	if err := h.rt.Attach(d); !errors.Is(err, ErrCapacity) {
		t.Fatalf("over capacity err = %v", err)
	}
	h.rt.Detach(a)
	waitFor(t, func() bool { return h.fake.Sink(a) == nil })
	if err := h.rt.Attach(d); err != nil {
		t.Fatal(err)
	}
}
```

(add `encoding/json`, `errors` imports)

- [ ] **Step 2: Run to verify failure** — build fails (package missing).

- [ ] **Step 3: Implement**

`backoff.go`:

```go
package chatsync

import (
	"math/rand"
	"time"
)

const (
	maxFailures = 30
	stableAfter = 10 * time.Minute
	maxBackoff  = 5 * time.Minute
)

// next returns the wait before reconnect attempt n (0-based): 1s, 2s, 4s …
// capped at 5m, with ±20% jitter so a fleet of accounts does not reconnect
// in lockstep.
func next(attempt int) time.Duration {
	d := time.Second << uint(min(attempt, 9))
	if d > maxBackoff {
		d = maxBackoff
	}
	j := 1 + (rand.Float64()*0.4 - 0.2)
	return time.Duration(float64(d) * j)
}
```

`runtime.go`:

```go
// Package chatsync keeps one live connection per chat account and turns the
// provider's events into stored rows and outbound webhooks. It is the chat
// counterpart of the syncer: where mail is pulled on cursors, chat is pushed
// over a socket the provider holds open.
package chatsync

var ErrCapacity = errors.New("chatsync: account capacity reached")

type Options struct {
	MaxAccounts int
	Sleep       func(context.Context, time.Duration) // test hook; nil = time.Sleep with ctx
}

type Runtime struct {
	store  *store.Store
	reg    *provider.Registry
	accts  *accounts.Manager
	events *events.Dispatcher
	log    *slog.Logger
	opts   Options

	mu     sync.Mutex
	actors map[string]*actor
	wg     sync.WaitGroup
	ctx    context.Context
}

func New(s *store.Store, reg *provider.Registry, a *accounts.Manager, d *events.Dispatcher, log *slog.Logger, opts Options) *Runtime {
	if opts.MaxAccounts <= 0 { opts.MaxAccounts = 200 }
	if opts.Sleep == nil {
		opts.Sleep = func(ctx context.Context, d time.Duration) {
			select { case <-ctx.Done(): case <-time.After(d): }
		}
	}
	return &Runtime{store: s, reg: reg, accts: a, events: d, log: log.With("component", "chatsync"), opts: opts, actors: map[string]*actor{}}
}

// Start attaches every live chat account. Safe to call again (idempotent).
func (r *Runtime) Start(ctx context.Context) {
	r.ctx = ctx
	accts, err := r.store.ListChatAccounts()
	if err != nil { r.log.Error("listing chat accounts", "err", err); return }
	for _, a := range accts {
		if err := r.Attach(a.ID); err != nil {
			r.log.Warn("attach at start", "account_id", a.ID, "err", err)
		}
	}
}

func (r *Runtime) Attach(accountID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.actors[accountID]; ok { return nil }
	if len(r.actors) >= r.opts.MaxAccounts { return ErrCapacity }
	acct, err := r.store.GetAnyAccount(accountID)
	if err != nil { return err }
	p, err := r.reg.Get(acct.Provider)
	if err != nil { return err }
	if p.Chat() == nil { return fmt.Errorf("chatsync: %s is not a chat provider", acct.Provider) }
	a := newActor(r, acct, p.Chat())
	r.actors[accountID] = a
	r.wg.Add(1)
	go func() { defer r.wg.Done(); a.run(r.ctx); r.mu.Lock(); delete(r.actors, accountID); r.mu.Unlock() }()
	return nil
}

func (r *Runtime) Detach(accountID string) {
	r.mu.Lock(); a := r.actors[accountID]; r.mu.Unlock()
	if a != nil { a.stop() }
}

func (r *Runtime) HealthFor(accountID string) (model.Connection, bool) {
	r.mu.Lock(); a := r.actors[accountID]; r.mu.Unlock()
	if a == nil { return model.Connection{}, false }
	return a.health(), true
}

func (r *Runtime) Count() int { r.mu.Lock(); defer r.mu.Unlock(); return len(r.actors) }
func (r *Runtime) Max() int   { return r.opts.MaxAccounts }
func (r *Runtime) Wait()      { r.wg.Wait() }
```

`actor.go` — the state machine:

```go
type actor struct {
	rt      *Runtime
	acct    model.Account
	chat    provider.Chatter
	log     *slog.Logger
	connID  string
	inbox   chan func()      // serialised sink work
	stopCh  chan struct{}
	stopped sync.Once

	mu         sync.Mutex
	conn       provider.ChatConn
	state      string
	since      time.Time
	reconnects int
	failures   int
	lastErr    string
}

func newActor(rt *Runtime, acct model.Account, chat provider.Chatter) *actor {
	return &actor{rt: rt, acct: acct, chat: chat, inbox: make(chan func(), 1024), stopCh: make(chan struct{}),
		log: rt.log.With("account_id", acct.ID, "developer_id", acct.DeveloperID), state: "connecting"}
}

func (a *actor) run(ctx context.Context) {
	defer a.setState("stopped")
	for {
		a.connID = "conn_" + strings.TrimPrefix(logx.NewRequestID(), "req_")
		log := a.log.With("conn_id", a.connID)
		jid, err := a.rt.accts.DeviceJID(a.acct.ID)
		if err != nil { log.Warn("no device for account", "err", err); a.rt.accts.MarkLoggedOut(a.acct.ID, "no device"); return }
		a.setState("connecting")
		disconnected := make(chan disconnect, 1)
		s := &sink{a: a, log: log, disc: disconnected}
		conn, err := a.chat.Connect(ctx, a.acct.ID, jid, s)
		if err != nil {
			a.failures++
			a.mu.Lock(); a.lastErr = err.Error(); a.mu.Unlock()
			log.Warn("connect failed", "attempt", a.failures, "err", err)
			if a.failures >= maxFailures {
				a.rt.accts.MarkLoggedOut(a.acct.ID, "unreachable")
				return
			}
			a.setState("backoff")
			if !a.sleep(ctx, next(a.failures-1)) { return }
			continue
		}
		a.mu.Lock(); a.conn = conn; a.mu.Unlock()
		a.setState("connected")
		log.Info("connected", "reconnects", a.reconnects)
		a.roster(ctx, log)
		stable := time.AfterFunc(stableAfter, func() { a.mu.Lock(); a.failures = 0; a.mu.Unlock() })
		d := a.serve(ctx, disconnected) // runs inbox until disconnect/stop/ctx
		stable.Stop()
		_ = conn.Close()
		if d.stop || ctx.Err() != nil { return }
		if d.loggedOut {
			log.Info("logged out", "reason", d.reason)
			a.rt.accts.MarkLoggedOut(a.acct.ID, d.reason)
			return
		}
		a.reconnects++; a.failures++
		log.Info("disconnected, backing off", "reason", d.reason, "attempt", a.failures)
		a.setState("backoff")
		if a.failures >= maxFailures { a.rt.accts.MarkLoggedOut(a.acct.ID, "unreachable"); return }
		if !a.sleep(ctx, next(a.failures-1)) { return }
	}
}
```

`serve` loops `select { case f := <-a.inbox: f(); case d := <-disconnected: return d; case <-a.stopCh: return disconnect{stop:true}; case <-ctx.Done(): … }`. `roster` calls `Chatter.Chats` and upserts (errors logged, not fatal). `health()` snapshots the fields. `stop()` closes `stopCh` once.

`sink.go` — each method enqueues a closure on `a.inbox` (dropping with a WARN only if the buffer is full and the actor is stopping), and the closure does the store write + emit:

```go
func (s *sink) Message(accountID string, m model.ChatMessage, chat model.Chat, sender model.Attendee) {
	s.enqueue(func() {
		m.AccountID = accountID
		log := s.log.With("chat_id", m.ChatID, "message_id", m.ID)
		_ = s.a.rt.store.UpsertAttendee(sender, accountID)
		if !m.IsFromMe {
			_ = s.a.rt.store.UpsertChat(chatOrExisting(s.a.rt.store, accountID, chat))
		}
		inserted, err := s.a.rt.store.UpsertChatMessage(m)
		if err != nil { log.Error("storing message", "err", err); return }
		switch {
		case !inserted && m.IsFromMe:
			log.Debug("chat event", "kind", "message", "decision", "own-echo", "text_bytes", len(m.Text)); return
		case !inserted:
			log.Debug("chat event", "kind", "message", "decision", "replay", "text_bytes", len(m.Text)); return
		}
		delta := 1
		if m.IsFromMe { delta = 0 }
		_ = s.a.rt.store.BumpChat(accountID, m.ChatID, m.SentAt, delta)
		typ := model.EventChatReceived
		if m.IsFromMe { typ = model.EventChatSent }
		log.Debug("chat event", "kind", "message", "decision", "new", "from_me", m.IsFromMe, "text_bytes", len(m.Text), "event", typ)
		full, _ := s.a.rt.store.GetChatMessage(accountID, m.ID)
		c, _ := s.a.rt.store.GetChat(accountID, m.ChatID)
		s.a.rt.events.Emit(model.Event{Type: typ, AccountID: accountID, Message: &full, Chat: &c})
	})
}
```

`Receipt` → `SetMessageStatus` (+ `ClearUnread` on `read` for inbound) → `chat_updated{MessageIDs, Status}`; `Reaction` → `ApplyReaction` → `chat_reaction{MessageIDs:[id], Reaction}`; `Edited` → `EditChatMessage` → `chat_updated{MessageIDs:[id], Change:"edited", Message}`; `Deleted` → `RevokeChatMessage` → `chat_deleted{MessageIDs:[id]}`; `Disconnected` → send on `disc` (non-blocking). `chatOrExisting` keeps an existing chat's name when the event's chat has none.

- [ ] **Step 4: Run tests**

Run: `gofmt -l internal cmd; go vet ./... && go test -race ./internal/chatsync/ && go test ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/chatsync
git commit -m "feat(chatsync): per-account connection supervisor with backoff; events to store and webhooks"
```

---

### Task 7: API — provider-aware hosted-auth and the QR connect page (consent + `/qr` + completion)

**Files:**
- Create: `internal/api/handlers_link.go`
- Modify: `internal/api/handlers_connect.go` (`handleHostedAuth`, `handleConnectRedirect`), `internal/api/api.go` (routes, `Server` gets `chat *chatsync.Runtime`), `internal/store/aux.go` (`OAuthState.ConsentedAt`), `internal/store/schema.go` (`oauth_states.consented_at INTEGER` — new column, additive migration), `internal/api/api_test.go` (test server registers a `FakeChat` alongside outlook and constructs a `chatsync.Runtime`)

**Interfaces:**
- Produces routes: `POST /connect/{state}/consent` → 204 (records `consented_at`); `GET /connect/{state}/qr` → `{status, png_base64?, expires_in?}`; the connect page for Linker providers.
- `Server.links *linkRegistry` — in-memory map `state → *link{session provider.LinkSession; latest provider.LinkCode; result *provider.LinkResult; started time.Time}` with a sweeper goroutine closing sessions older than 3 min.
- `handleHostedAuth`: resolves the provider; when `p.Linker() != nil` and the runtime is at capacity → `503 capacity`. `notify_url`/webhook handling unchanged.
- Completion: `finishLink(ctx, pending, res)` → `accts.ConnectLinked` → `chat.Attach` → notify `CREATED` → bind connect-time webhook → consume the pending state (`TakeOAuthState`).

- [ ] **Step 1: Failing tests** (append to `api_test.go`; `newTestServerWithLog` gains a `FakeChat` registered as `"FAKECHAT"` and exposes `s.fakeChat`):

```go
func TestLinkerConnectPageRequiresConsentThenServesQR(t *testing.T) {
	s, db := newTestServer(t)
	_, key := seedDev(t, s, "a@x.com")
	h := s.Routes()
	mint := func(body string) string {
		req := withKey(httptest.NewRequest(http.MethodPost, "/api/v1/hosted-auth", strings.NewReader(body)), key)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 200 { t.Fatalf("hosted-auth: %d %s", rec.Code, rec.Body.String()) }
		var r hostedAuthResponse; _ = json.Unmarshal(rec.Body.Bytes(), &r); return r.State
	}
	state := mint(`{"provider":"FAKECHAT","notify_url":"https://api.example.com/hook","webhook":{"url":"https://api.example.com/wa"}}`)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/connect/"+state, nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `name="consent"`) || strings.Contains(rec.Body.String(), "login.microsoftonline.com") {
		t.Fatalf("linker page: %d %s", rec.Code, rec.Body.String()[:200])
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/connect/"+state+"/qr", nil))
	if rec.Code != http.StatusConflict {
		t.Fatalf("qr before consent: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/connect/"+state+"/consent", nil))
	if rec.Code != http.StatusNoContent { t.Fatalf("consent: %d", rec.Code) }
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/connect/"+state+"/qr", nil))
	var q struct{ Status string `json:"status"`; PNG string `json:"png_base64"` }
	_ = json.Unmarshal(rec.Body.Bytes(), &q)
	if rec.Code != 200 || q.Status != "waiting" {
		t.Fatalf("first qr poll: %d %+v", rec.Code, q)
	}
	s.fakeChat.EmitCode("qr-abc")
	waitFor(t, func() bool {
		rec := httptest.NewRecorder(); h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/connect/"+state+"/qr", nil))
		_ = json.Unmarshal(rec.Body.Bytes(), &q); return q.PNG != ""
	})
	// Pair → account under the minting developer, notify + webhook bound, state consumed.
	notified := make(chan map[string]any, 1)
	s.notifyTransport = func(url string, payload map[string]any) { notified <- payload }
	s.fakeChat.Pair(provider.Identity{Identifier: "+919888000000", Name: "G"}, "919888000000:5@s.whatsapp.net")
	waitFor(t, func() bool {
		rec := httptest.NewRecorder(); h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/connect/"+state+"/qr", nil))
		_ = json.Unmarshal(rec.Body.Bytes(), &q); return q.Status == "paired"
	})
	dev, _, _ := db.DeveloperByEmail("a@x.com")
	accts, _ := db.ListAccounts(dev.ID)
	if len(accts) != 1 || accts[0].Kind != model.AccountKindChat || accts[0].Identifier != "+919888000000" {
		t.Fatalf("accounts = %+v", accts)
	}
	select {
	case p := <-notified:
		if p["status"] != "CREATED" || p["account_id"] != accts[0].ID { t.Fatalf("notify = %v", p) }
	case <-time.After(2 * time.Second):
		t.Fatal("notify_url not called")
	}
	hooks, _ := db.ListAccountWebhooks(dev.ID, accts[0].ID)
	if len(hooks) != 1 { t.Fatalf("connect-time webhook not bound: %+v", hooks) }
	if _, err := db.PeekOAuthState(state); err == nil { t.Fatal("state not consumed") }
	if _, ok := s.chat.HealthFor(accts[0].ID); !ok { t.Fatal("runtime did not attach the new account") }
}

func TestLinkTimeoutNotifiesFailed(t *testing.T) {
	s, _ := newTestServer(t)
	_, key := seedDev(t, s, "a@x.com")
	h := s.Routes()
	req := withKey(httptest.NewRequest(http.MethodPost, "/api/v1/hosted-auth", strings.NewReader(`{"provider":"FAKECHAT","notify_url":"https://api.example.com/hook"}`)), key)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder(); h.ServeHTTP(rec, req)
	var r hostedAuthResponse; _ = json.Unmarshal(rec.Body.Bytes(), &r)
	notified := make(chan map[string]any, 1)
	s.notifyTransport = func(url string, payload map[string]any) { notified <- payload }
	rec = httptest.NewRecorder(); h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/connect/"+r.State+"/consent", nil))
	rec = httptest.NewRecorder(); h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/connect/"+r.State+"/qr", nil))
	s.fakeChat.FailLink(provider.ErrLinkTimeout)
	select {
	case p := <-notified:
		if p["status"] != "FAILED" || p["error"] != "link_timeout" { t.Fatalf("notify = %v", p) }
	case <-time.After(2 * time.Second):
		t.Fatal("FAILED not notified")
	}
}

func TestHostedAuthReturnsCapacityWhenRuntimeFull(t *testing.T) {
	s, db := newTestServerWithChatCapacity(t, 1) // same as newTestServer but chatsync.Options{MaxAccounts: 1}
	dev, key := seedDev(t, s, "a@x.com")
	_ = seedChat(t, s, db, dev.ID) // links + attaches one account → runtime full
	req := withKey(httptest.NewRequest(http.MethodPost, "/api/v1/hosted-auth", strings.NewReader(`{"provider":"FAKECHAT"}`)), key)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "capacity") {
		t.Fatalf("over capacity: %d %s", rec.Code, rec.Body.String())
	}
	// Mail providers are unaffected by chat capacity.
	req = withKey(httptest.NewRequest(http.MethodPost, "/api/v1/hosted-auth", strings.NewReader(`{"provider":"OUTLOOK"}`)), key)
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("mail hosted-auth under chat capacity: %d", rec.Code)
	}
}
```

`newTestServerWithChatCapacity(t, n)` is the general constructor; `newTestServerWithLog` calls it with `n = 10`. `seedChat` (defined in Task 8's tests; move it into the shared helpers here) links a fake account via `s.accts.ConnectLinked` and `s.chat.Attach`, and seeds chat `c1` + attendee `a1`.

`s.notifyTransport func(url string, payload map[string]any)` is a new optional hook on `Server` used by `notify` when non-nil (tests) — the production path keeps posting.

- [ ] **Step 2: Run to verify failure** — 404s / missing symbols.

- [ ] **Step 3: Implement**

Store: `OAuthState.ConsentedAt *time.Time`; `SetOAuthConsent(state string, at time.Time) error`; add the column to `oauth_states` CREATE and a migration line `ALTER TABLE oauth_states ADD COLUMN consented_at INTEGER`.

`handlers_link.go`:

```go
type link struct {
	mu      sync.Mutex
	session provider.LinkSession
	code    provider.LinkCode
	result  *provider.LinkResult
	started time.Time
}

type linkRegistry struct {
	mu    sync.Mutex
	links map[string]*link
}

const linkTTL = 3 * time.Minute

func (s *Server) handleConsent(w http.ResponseWriter, r *http.Request) {
	state := r.PathValue("state")
	if _, err := s.store.PeekOAuthState(state); err != nil { writeError(w, 404, "not_found", "unknown link"); return }
	if err := s.store.SetOAuthConsent(state, time.Now().UTC()); err != nil { writeError(w, 500, "internal", err.Error()); return }
	logx.From(r.Context()).Info("link consent recorded", "state_prefix", statePrefix(state))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleLinkQR(w http.ResponseWriter, r *http.Request) {
	state := r.PathValue("state")
	pending, err := s.store.PeekOAuthState(state)
	if err != nil { writeError(w, 404, "not_found", "unknown link"); return }
	if pending.ConsentedAt == nil { writeError(w, http.StatusConflict, "consent_required", "accept the disclosure first"); return }
	p, err := s.registry.Get(pending.Provider)
	if err != nil || p.Linker() == nil { writeError(w, 400, "unsupported_for_kind", "not a linkable provider"); return }
	l := s.links.get(state)
	if l == nil {
		sess, err := p.Linker().StartLink(context.Background()) // outlives the request
		if err != nil { writeError(w, 502, "provider_error", err.Error()); return }
		l = s.links.put(state, sess)
		go s.pumpLink(state, pending, l)
	}
	l.mu.Lock(); defer l.mu.Unlock()
	resp := map[string]any{"status": "waiting"}
	switch {
	case l.result != nil && l.result.Err == nil:
		resp["status"] = "paired"
		if pending.SuccessURL != "" { resp["redirect"] = appendQuery(pending.SuccessURL, url.Values{"account_id": {l.accountID}}) }
	case l.result != nil:
		resp["status"] = "expired"
		if errors.Is(l.result.Err, provider.ErrLinkTimeout) { resp["status"] = "expired" } else { resp["status"] = "failed" }
	case l.code.Code != "":
		png, _ := qrcode.Encode(l.code.Code, qrcode.Medium, 512)
		resp["png_base64"] = base64.StdEncoding.EncodeToString(png)
		resp["expires_in"] = int(time.Until(l.code.ExpiresAt).Seconds())
	}
	writeJSON(w, 200, resp)
}

// pumpLink forwards QR codes into the link and completes it when the
// provider reports a result, or after linkTTL.
func (s *Server) pumpLink(state string, pending store.OAuthState, l *link) {
	timeout := time.NewTimer(linkTTL)
	defer timeout.Stop()
	for {
		select {
		case c, ok := <-l.session.Codes():
			if !ok { continue }
			l.mu.Lock(); l.code = c; l.mu.Unlock()
		case res := <-l.session.Result():
			s.finishLink(state, pending, l, res); return
		case <-timeout.C:
			l.session.Close()
			s.finishLink(state, pending, l, provider.LinkResult{Err: provider.ErrLinkTimeout}); return
		}
	}
}

func (s *Server) finishLink(state string, pending store.OAuthState, l *link, res provider.LinkResult) {
	ctx := logx.With(context.Background(), s.log.With("component", "api", "state_prefix", statePrefix(state), "developer_id", pending.DeveloperID))
	log := logx.From(ctx)
	if _, err := s.store.TakeOAuthState(state); err != nil { log.Warn("link state already consumed"); }
	if res.Err != nil {
		code := "link_failed"
		if errors.Is(res.Err, provider.ErrLinkTimeout) { code = "link_timeout" } else if errors.Is(res.Err, provider.ErrLinkCancelled) { code = "link_cancelled" }
		log.Info("link failed", "code", code)
		if pending.NotifyURL != "" { s.notify(pending.NotifyURL, map[string]any{"status": "FAILED", "error": code, "message": res.Err.Error()}) }
		l.mu.Lock(); l.result = &res; l.mu.Unlock()
		return
	}
	acct, err := s.accts.ConnectLinked(ctx, pending.DeveloperID, pending.Provider, res.Identity, res.DeviceJID)
	if err != nil { log.Error("recording linked account", "err", err); r := provider.LinkResult{Err: err}; l.mu.Lock(); l.result = &r; l.mu.Unlock(); return }
	if pending.Webhook != nil {
		if _, err := s.createAccountWebhook(pending.DeveloperID, acct.ID, webhookRequest{Name: pending.Webhook.Name, URL: pending.Webhook.URL, Secret: pending.Webhook.Secret, Events: pending.Webhook.Events}); err != nil {
			log.Error("binding connect-time webhook", "err", err)
		}
	}
	if err := s.chat.Attach(acct.ID); err != nil { log.Warn("attaching linked account", "err", err) }
	if pending.NotifyURL != "" {
		s.notify(pending.NotifyURL, map[string]any{"status": "CREATED", "account_id": acct.ID, "identifier": acct.Identifier, "provider": acct.Provider})
	}
	log.Info("account linked", "account_id", acct.ID)
	l.mu.Lock(); l.result = &res; l.accountID = acct.ID; l.mu.Unlock()
}
```

`handleConnectRedirect`: when `p.Linker() != nil` render `linkTmpl` (disclosure + consent checkbox + QR `<img>` + status + polling JS: POST consent → poll `/qr` every 2 s → on `paired` follow `redirect` or show confirmation; on `expired/failed` show the message). `handleHostedAuth`: after resolving `p`, if `p.Linker() != nil && s.chat != nil && s.chat.Count() >= s.chat.Max()` → `503 capacity`.

`Routes()`: `GET /connect/{state}` unchanged; add `POST /connect/{state}/consent`, `GET /connect/{state}/qr` (both outside `/api/v1`, no credential — the state is the secret). A sweeper goroutine started in `NewServer` closes links older than `linkTTL + 1m`.

- [ ] **Step 4: Run tests, commit**

Run: `gofmt -l internal cmd; go vet ./... && go test -race ./internal/api/ && go test ./...`

```bash
git add internal/api internal/store
git commit -m "feat(api): QR linking flow on the hosted connect page with consent gate"
```

---

### Task 8: API — chats, messages, attendees routes with idempotency; isolation coverage

**Files:**
- Create: `internal/api/handlers_chat.go`
- Modify: `internal/api/api.go` (`apiRoutes` + handler map), `internal/api/isolation_test.go`, `internal/api/api_test.go`

**Interfaces:**
- Routes exactly as spec §4. Helper `resolveChatAccount(w, r, id) (model.Account, provider.Chatter, bool)` — `resolveID` then `p.Chat() == nil → 400 unsupported_for_kind`.
- `Idempotency-Key`: `withIdempotency(dev, key string, w, do func() (status int, body any))` — on hit replays stored `{status, body}`; on miss runs `do`, stores when 2xx; a stored key whose new request body hash differs → `409 idempotency_conflict` (hash the raw request body; store `{hash, status, body}`).
- Send: `store.UpsertChatMessage` with temp id `tmp_<random>` + `status: sending`, `Chatter.SendText`, `RenameChatMessage(tmp→provider id)` + `SetMessageStatus(sent)`, `BumpChat(…, 0)`, emit `chat_sent`; failure → `DeleteChatMessageRow(tmp)` + `502 provider_error`.

- [ ] **Step 1: Failing tests** — add to `api_test.go` (`seedChat(t, s, db, devID) (accountID string)` helper that `ConnectLinked`s through `s.accts` and attaches to `s.chat`, then seeds `c1`/`a1` rows):

```go
func TestChatRoutesHappyPath(t *testing.T) {
	s, db := newTestServer(t)
	dev, key := seedDev(t, s, "a@x.com")
	acc := seedChat(t, s, db, dev.ID)
	h := s.Routes()
	j := func(method, path, body string, hdr ...string) (*httptest.ResponseRecorder) {
		req := withKey(httptest.NewRequest(method, path, strings.NewReader(body)), key)
		req.Header.Set("Content-Type", "application/json")
		for i := 0; i+1 < len(hdr); i += 2 { req.Header.Set(hdr[i], hdr[i+1]) }
		rec := httptest.NewRecorder(); h.ServeHTTP(rec, req); return rec
	}
	if rec := j("GET", "/api/v1/chats?account_id="+acc, ""); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"id":"c1"`) {
		t.Fatalf("list chats: %d %s", rec.Code, rec.Body.String())
	}
	s.fakeChat.SendResult = provider.SendResult{MessageID: "REAL1"}
	rec := j("POST", "/api/v1/chats/c1/messages?account_id="+acc, `{"text":"hello"}`, "Idempotency-Key", "k1")
	if rec.Code != 201 || !strings.Contains(rec.Body.String(), `"id":"REAL1"`) || !strings.Contains(rec.Body.String(), `"status":"sent"`) {
		t.Fatalf("send: %d %s", rec.Code, rec.Body.String())
	}
	replay := j("POST", "/api/v1/chats/c1/messages?account_id="+acc, `{"text":"hello"}`, "Idempotency-Key", "k1")
	if replay.Code != 201 || replay.Body.String() != rec.Body.String() { t.Fatalf("idempotent replay differs: %d %s", replay.Code, replay.Body.String()) }
	if got := s.fakeChat.Commands(); len(got) != 1 { t.Fatalf("send called %d times", len(got)) }
	if c := j("POST", "/api/v1/chats/c1/messages?account_id="+acc, `{"text":"different"}`, "Idempotency-Key", "k1"); c.Code != 409 { t.Fatalf("conflict: %d", c.Code) }
	if rec := j("GET", "/api/v1/chats/c1/messages?account_id="+acc+"&limit=10", ""); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"id":"REAL1"`) {
		t.Fatalf("list messages: %d %s", rec.Code, rec.Body.String())
	}
	if rec := j("PUT", "/api/v1/chats/c1/messages/REAL1/reaction?account_id="+acc, `{"emoji":"👍"}`); rec.Code != 204 { t.Fatalf("react: %d", rec.Code) }
	if rec := j("PATCH", "/api/v1/chats/c1/messages/REAL1?account_id="+acc, `{"text":"hello!"}`); rec.Code != 200 { t.Fatalf("edit: %d %s", rec.Code, rec.Body.String()) }
	if rec := j("DELETE", "/api/v1/chats/c1/messages/REAL1?account_id="+acc, ""); rec.Code != 204 { t.Fatalf("delete: %d", rec.Code) }
	if rec := j("PATCH", "/api/v1/chats/c1?account_id="+acc, `{"read":true}`); rec.Code != 200 { t.Fatalf("mark read: %d", rec.Code) }
	if rec := j("POST", "/api/v1/chats", `{"account_id":"`+acc+`","phone":"+919888000001","text":"hey"}`); rec.Code != 201 || !strings.Contains(rec.Body.String(), `"chat"`) {
		t.Fatalf("start direct: %d %s", rec.Code, rec.Body.String())
	}
	if rec := j("GET", "/api/v1/attendees?account_id="+acc, ""); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"id":"a1"`) { t.Fatalf("attendees: %d", rec.Code) }
	// Editing someone else's message is the one legitimate 403.
	_, _ = db.UpsertChatMessage(model.ChatMessage{AccountID: acc, ID: "THEIRS", ChatID: "c1", Sender: model.Attendee{ID: "a1"}, Kind: "text", Text: "x", SentAt: time.Now()})
	if rec := j("PATCH", "/api/v1/chats/c1/messages/THEIRS?account_id="+acc, `{"text":"nope"}`); rec.Code != 403 || !strings.Contains(rec.Body.String(), "not_own_message") { t.Fatalf("edit theirs: %d", rec.Code) }
	// A mail account cannot use chat routes.
	_ = db.UpsertAccount(model.Account{ID: "acc_mail", DeveloperID: dev.ID, Provider: "OUTLOOK", Email: "m@x.com", Status: model.AccountOK})
	if rec := j("GET", "/api/v1/chats?account_id=acc_mail", ""); rec.Code != 400 || !strings.Contains(rec.Body.String(), "unsupported_for_kind") { t.Fatalf("mail on chat route: %d", rec.Code) }
}

func TestSendFailureLeavesNoRow(t *testing.T) {
	s, db := newTestServer(t)
	dev, key := seedDev(t, s, "a@x.com")
	acc := seedChat(t, s, db, dev.ID)
	s.fakeChat.CommandErr = errors.New("socket closed")
	req := withKey(httptest.NewRequest("POST", "/api/v1/chats/c1/messages?account_id="+acc, strings.NewReader(`{"text":"x"}`)), key)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder(); s.Routes().ServeHTTP(rec, req)
	if rec.Code != 502 { t.Fatalf("send failure: %d", rec.Code) }
	msgs, _, _ := db.ListChatMessages(acc, "c1", "", 10)
	if len(msgs) != 0 { t.Fatalf("row left behind: %+v", msgs) }
}
```

Extend `isolation_test.go`: seed under A a chat account `acc_wa` with chat `c1`, message `M1`, attendee `a1`; add rows for every new route (`GET/POST /chats`, `GET/PATCH /chats/{id}`, `GET/POST /chats/{id}/messages`, `GET/PATCH/DELETE /chats/{id}/messages/{mid}`, `PUT …/reaction`, `GET /attendees`, `GET /attendees/{id}`) expecting 404 as B, and add the patterns to `apiRoutes`.

- [ ] **Step 2: Run to verify failure** — isolation test reports uncovered routes / 404s on missing routes.

- [ ] **Step 3: Implement `handlers_chat.go`** — thin command handlers over the store and `Chatter`, each starting with `dev, _ := developerFrom(ctx)` + `resolveChatAccount`. Send path:

```go
func (s *Server) handleSendChatMessage(w http.ResponseWriter, r *http.Request) {
	acct, chatter, ok := s.resolveChatAccount(w, r, r.URL.Query().Get("account_id"))
	if !ok { return }
	chatID := r.PathValue("id")
	var req struct {
		Text            string `json:"text"`
		QuotedMessageID string `json:"quoted_message_id,omitempty"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	if strings.TrimSpace(req.Text) == "" {
		writeError(w, http.StatusBadRequest, "missing_text", "text is required")
		return
	}
	dev, _ := developerFrom(r.Context())
	s.withIdempotency(w, r, dev.ID, func() (int, any) {
		if _, err := s.store.GetChat(acct.ID, chatID); err != nil { return 404, apiErr("not_found", "no such chat") }
		tmp := "tmp_" + randomID()
		self, _ := s.store.SelfAttendee(acct.ID) // attendee with is_self=1, or {ID: acct.Identifier}
		row := model.ChatMessage{AccountID: acct.ID, ID: tmp, ChatID: chatID, Sender: self, IsFromMe: true, Kind: "text", Text: req.Text, QuotedMessageID: req.QuotedMessageID, SentAt: time.Now().UTC(), Status: "sending"}
		if _, err := s.store.UpsertChatMessage(row); err != nil { return 500, apiErr("internal", err.Error()) }
		res, err := chatter.SendText(r.Context(), acct.ID, chatID, req.Text, req.QuotedMessageID)
		if err != nil {
			_ = s.store.DeleteChatMessageRow(acct.ID, tmp)
			return 502, apiErr("provider_error", err.Error())
		}
		_ = s.store.RenameChatMessage(acct.ID, tmp, res.MessageID)
		_ = s.store.SetMessageStatus(acct.ID, []string{res.MessageID}, "sent")
		_ = s.store.BumpChat(acct.ID, chatID, row.SentAt, 0)
		full, _ := s.store.GetChatMessage(acct.ID, res.MessageID)
		c, _ := s.store.GetChat(acct.ID, chatID)
		s.dispatcher.Emit(model.Event{Type: model.EventChatSent, AccountID: acct.ID, Message: &full, Chat: &c})
		return 201, full
	})
}
```

(`Server` needs the dispatcher: add `dispatcher *events.Dispatcher` to `NewServer`, passed from `main`; the test server already constructs one.) `withIdempotency` reads the header; if absent, just runs `do`. Edit/Delete check `m.IsFromMe` else `403 not_own_message`. `PATCH /chats/{id}` with `read:true` → `Chatter.MarkRead` + `ClearUnread`. `POST /chats` → `StartDirect` → `UpsertChat{direct}` + `UpsertAttendee` → then the same send path.

- [ ] **Step 4: Run tests, commit**

Run: `gofmt -l internal cmd; go vet ./... && go test -race ./internal/api/ && go test ./...`

```bash
git add internal/api
git commit -m "feat(api): chats, messages and attendees routes with idempotent sends; isolation coverage"
```

---

### Task 9: Accounts API for chat accounts — kind/identifier/connection, reconnect, delete unlinks, providers listing

**Files:**
- Modify: `internal/api/handlers_misc.go` (`handleGetAccount`, `handleListAccounts`, `handleDeleteAccount`, `handleResync`, `handleListProviders`), `internal/api/api.go` (`apiRoutes` + `POST /api/v1/accounts/{id}/reconnect`), `internal/api/isolation_test.go`, `internal/api/api_test.go`

**Interfaces:**
- `GET /api/v1/accounts[/{id}]` decorate chat accounts with `connection` from `s.chat.HealthFor`.
- `POST /api/v1/accounts/{id}/resync` on a chat account → `400 unsupported_for_kind`; new `POST /api/v1/accounts/{id}/reconnect` (chat only) → `Detach` then `Attach` → `202 {status:"reconnecting"}`; on a mail account → `400 unsupported_for_kind`.
- `DELETE /api/v1/accounts/{id}` on a chat account → `s.chat.Detach`, `chatter.Logout` (best effort, logged), `accts.DeleteLinked`.
- `GET /api/v1/providers` items gain `kind` and `auth: "oauth"|"link"`.

- [ ] **Step 1: Failing tests**

```go
func TestChatAccountLifecycleRoutes(t *testing.T) {
	s, db := newTestServer(t)
	dev, key := seedDev(t, s, "a@x.com")
	acc := seedChat(t, s, db, dev.ID)
	h := s.Routes()
	get := func(path string) *httptest.ResponseRecorder { rec := httptest.NewRecorder(); h.ServeHTTP(rec, withKey(httptest.NewRequest("GET", path, nil), key)); return rec }
	post := func(path string) *httptest.ResponseRecorder { rec := httptest.NewRecorder(); h.ServeHTTP(rec, withKey(httptest.NewRequest("POST", path, nil), key)); return rec }
	body := get("/api/v1/accounts/" + acc).Body.String()
	if !strings.Contains(body, `"kind":"chat"`) || !strings.Contains(body, `"identifier":"+91`) || !strings.Contains(body, `"connection":{"state":"connected"`) {
		t.Fatalf("account = %s", body)
	}
	if rec := post("/api/v1/accounts/" + acc + "/resync"); rec.Code != 400 || !strings.Contains(rec.Body.String(), "unsupported_for_kind") { t.Fatalf("resync chat: %d", rec.Code) }
	if rec := post("/api/v1/accounts/" + acc + "/reconnect"); rec.Code != 202 { t.Fatalf("reconnect: %d %s", rec.Code, rec.Body.String()) }
	waitFor(t, func() bool { c, ok := s.chat.HealthFor(acc); return ok && c.State == "connected" })
	prov := get("/api/v1/providers").Body.String()
	if !strings.Contains(prov, `"name":"FAKECHAT"`) || !strings.Contains(prov, `"auth":"link"`) || !strings.Contains(prov, `"kind":"chat"`) { t.Fatalf("providers = %s", prov) }
	rec := httptest.NewRecorder(); h.ServeHTTP(rec, withKey(httptest.NewRequest("DELETE", "/api/v1/accounts/"+acc, nil), key))
	if rec.Code != 204 { t.Fatalf("delete: %d", rec.Code) }
	cmds := s.fakeChat.Commands()
	if len(cmds) == 0 || !strings.HasPrefix(cmds[len(cmds)-1], "Logout "+acc) { t.Fatalf("logout not sent: %v", cmds) }
	if len(s.fakeChat.Forgotten()) != 1 { t.Fatal("device not forgotten") }
	if _, ok := s.chat.HealthFor(acc); ok { t.Fatal("runtime still has the account") }
}
```

Isolation: add `POST /api/v1/accounts/{id}/reconnect` row (B → 404).

- [ ] **Step 2: Run to verify failure** — `go test ./internal/api/ -run TestChatAccountLifecycleRoutes` → 404 on `/reconnect`, missing `connection`.

- [ ] **Step 3: Implement** (in `handlers_misc.go`)

```go
// decorate adds runtime state to chat accounts before they are serialised.
func (s *Server) decorate(a model.Account) model.Account {
	if a.Kind == model.AccountKindChat && s.chat != nil {
		if c, ok := s.chat.HealthFor(a.ID); ok {
			a.Connection = &c
		} else {
			a.Connection = &model.Connection{State: "stopped"}
		}
	}
	return a
}

func (s *Server) handleReconnect(w http.ResponseWriter, r *http.Request) {
	dev, _ := developerFrom(r.Context())
	acct, err := s.store.GetAccount(dev.ID, r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "account_not_found", "no such account")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if acct.Kind != model.AccountKindChat {
		writeError(w, http.StatusBadRequest, "unsupported_for_kind", "reconnect applies to chat accounts; use resync for mail")
		return
	}
	if acct.Status != model.AccountOK {
		writeError(w, http.StatusConflict, "account_not_ok", "account status is "+acct.Status+"; it must be relinked first")
		return
	}
	logx.From(r.Context()).Info("reconnect requested", "account_id", acct.ID)
	s.chat.Detach(acct.ID)
	if err := s.chat.Attach(acct.ID); err != nil {
		writeError(w, http.StatusServiceUnavailable, "capacity", err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "reconnecting"})
}
```

`handleResync`: after loading the account, `if acct.Kind != model.AccountKindMail { writeError(w, 400, "unsupported_for_kind", "resync applies to mail accounts; use reconnect for chat"); return }`.

`handleDeleteAccount`: after the ownership check,

```go
	if acct.Kind == model.AccountKindChat {
		s.chat.Detach(id)
		if p, err := s.registry.Get(acct.Provider); err == nil && p.Chat() != nil {
			if err := p.Chat().Logout(ctx, id); err != nil {
				logx.From(r.Context()).Warn("logout on delete", "account_id", id, "err", err)
			}
		}
		if err := s.accts.DeleteLinked(ctx, id); err != nil {
			writeError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
```

`handleListAccounts`/`handleGetAccount` map through `s.decorate`. `handleListProviders`: `providerInfo` gains `Kind string \`json:"kind"\`` and `Auth string \`json:"auth"\`` (`"link"` when `p.Linker() != nil`, else `"oauth"`). Register `"POST /api/v1/accounts/{id}/reconnect": s.handleReconnect` in the handler map and `apiRoutes`.

- [ ] **Step 4: Run tests** — `gofmt -l internal cmd; go vet ./... && go test ./...`

- [ ] **Step 5: Commit**

```bash
git add internal/api
git commit -m "feat(api): chat account lifecycle — connection state, reconnect, unlink on delete, provider kinds"
```

---

### Task 10: Dashboard provider picker and chat cards; `/chat` viewer

**Files:**
- Modify: `internal/api/handlers_ui.go`
- Create: `internal/api/handlers_chat_ui.go`
- Modify: `internal/api/api.go` (route `GET /chat`), `internal/api/api_test.go`

**Interfaces:**
- Dashboard: **Connect account** opens a provider picker (`<select id="provider">` populated from `/api/v1/providers`) when more than one provider; hosted-auth call sends `provider`. Cards: `kind` badge; identifier (phone masked client-side: keep country code + first two digits + last three: `+91 88••• •855`); for chat accounts `connection.state` text and **Reconnect** (→ `POST …/reconnect`) instead of **Resync**; `View chat` link → `/chat?account_id=`.
- `/chat` page (session-gated like `/mail`): left pane chats (`GET /chats`), right pane messages (`GET /chats/{id}/messages` with `before` paging), send box (`POST /chats/{id}/messages` with `Content-Type: application/json`), auto-refresh every 5 s, Log out form, links to `/dashboard` and `/docs`.

- [ ] **Step 1: Failing tests**

```go
func TestDashboardShowsProviderPickerAndChatCards(t *testing.T) {
	s, db := newTestServer(t)
	dev, _ := seedDev(t, s, "a@x.com")
	_ = seedChat(t, s, db, dev.ID)
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, withSession(t, s, httptest.NewRequest("GET", "/dashboard", nil), dev.ID))
	body := rec.Body.String()
	for _, want := range []string{`id="provider"`, `data-action="reconnect"`, `/chat?account_id=`, `maskPhone(`} {
		if !strings.Contains(body, want) { t.Errorf("dashboard missing %q", want) }
	}
}

func TestChatViewerIsSessionGated(t *testing.T) {
	s, _ := newTestServer(t)
	dev, _ := seedDev(t, s, "a@x.com")
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, httptest.NewRequest("GET", "/chat?account_id=x", nil))
	if rec.Code != 302 { t.Fatalf("no session: %d", rec.Code) }
	rec = httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, withSession(t, s, httptest.NewRequest("GET", "/chat?account_id=x", nil), dev.ID))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `id="chats"`) || !strings.Contains(rec.Body.String(), `id="messages"`) || !strings.Contains(rec.Body.String(), `/api/v1/chats`) {
		t.Fatalf("viewer: %d", rec.Code)
	}
}
```

- [ ] **Step 2–5:** RED → implement (`chatHTML` const, plain string like `mailHTML`; dashboard JS additions; `handleChatPage` gating with `sessionDeveloper`) → `go test ./...` → commit

```bash
git add internal/api
git commit -m "feat(ui): provider picker, chat account cards, and a session-gated chat viewer"
```

---

### Task 11: Config, wiring, docs (`/docs`, `/llms.txt`, README), logging check

**Files:**
- Modify: `internal/config/config.go` (`WhatsAppEnabled bool`, `WhatsAppMaxAccounts int`, `WhatsAppDeviceName string`), `cmd/server/main.go`, `.env.example`, `README.md`, `internal/api/handlers_docs.go`, `internal/api/handlers_llms.go`, `internal/api/api_test.go`
- Create: `docs/whatsapp-manual-checklist.md`

**Interfaces:**
- `main.go`: after `registry`, `if cfg.WhatsAppEnabled { wa, err := whatsapp.New(db.DB(), cfg.WhatsAppDeviceName, log); registry.Add(wa) }` (add `Registry.Add` and `store.(*Store).DB() *sql.DB`), `chat := chatsync.New(db, registry, acctMgr, dispatcher, log, chatsync.Options{MaxAccounts: cfg.WhatsAppMaxAccounts})`, `chat.Start(ctx)`, pass `chat` and `dispatcher` into `api.NewServer`; shutdown order HTTP → `chat.Wait()` (after ctx cancel) → `dispatcher.Wait()`. Startup log adds `whatsapp=true max_chat_accounts=…`.
- `/docs`: new section **7. Chat (WhatsApp)** covering linking, objects, routes, events, disclosure, limits; endpoint table already auto-includes routes; `/llms.txt`: "Chat" flow + objects + rules (`account_status` may arrive any time; `not_own_message` 403; JSON `Idempotency-Key`).
- README: provider section for WhatsApp (linked-device model, disclosure, ban risk, unsealed device keys → DB-level encryption), config vars, connect flow, "Known limits" bullets.
- `docs/whatsapp-manual-checklist.md`: link real number → receive → send via API → react/edit/delete → unlink on phone → `CREDENTIALS` + webhook → restart → auto-reconnect → delete account unlinks device.

- [ ] **Step 1: Failing tests** — extend `TestDocsPageIsPublicAndCoversEveryRoute` and `TestLLMsTxt…` expectations with `"chat_received"`, `"Linked devices"`, `"Idempotency-Key"`; add `TestStartupLogsWhatsAppConfig`? (skip — `main` isn't unit-tested; verify in Step 4).
- [ ] **Step 2: Run to verify failure** — docs tests fail on missing strings.
- [ ] **Step 3: Implement** config/wiring/docs.
- [ ] **Step 4: Verify (verifying-services-end-to-end):** start with `DEBUG=1` on the current DB (migration adds `accounts.kind`, whatsmeow tables appear: `sqlite3 unified-messaging.db '.tables' | grep whatsmeow_`); startup line shows `whatsapp=true`; `GET /api/v1/providers` lists `WHATSAPP kind=chat auth=link`; `POST /hosted-auth {"provider":"WHATSAPP"}` → connect page renders consent + QR endpoint returns 409 before consent, `waiting` after; `/docs` and `/llms.txt` show the chat section; existing mail flows unaffected (`scripts/smoke.sh` still 11/11). The real-number steps are the operator's checklist (documented). Grep the log for QR strings (`qr-`/base64 PNG), phone numbers of attendees, and message text — none.
- [ ] **Step 5: Commit**

```bash
git add internal/config cmd/server internal/api README.md .env.example docs/whatsapp-manual-checklist.md internal/store
git commit -m "feat: wire WhatsApp provider and chat runtime; config, docs, manual checklist"
```

---

### Task 12: Race/soak pass and whole-branch tidy

**Files:** whatever the checks touch (tests only unless a defect is found)

- [ ] **Step 1:** `go test -race -count=3 ./internal/chatsync/ ./internal/api/` — flakes are defects (actor/sink ordering, link registry sweeper).
- [ ] **Step 2:** A soak test in `chatsync` (`-short` skips): 50 fake accounts, 1,000 messages each interleaved with disconnects; assert no duplicate events and all rows stored (`go test -run TestSoak -count=1 ./internal/chatsync/ -timeout 120s`).
- [ ] **Step 3:** `gofmt -l internal cmd; go vet ./...; go test -race ./...` all clean.
- [ ] **Step 4:** Commit fixes if any: `git commit -m "test(chatsync): race and soak coverage"`.
