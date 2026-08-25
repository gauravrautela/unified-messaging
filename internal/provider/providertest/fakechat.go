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

// Compile-time proof FakeChat satisfies every contract it claims to.
var (
	_ provider.Provider = (*FakeChat)(nil)
	_ provider.Linker   = (*FakeChat)(nil)
	_ provider.Chatter  = (*FakeChat)(nil)
)
