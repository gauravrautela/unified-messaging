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

	mu             sync.Mutex
	sessions       []*fakeSession
	sinks          map[string]provider.EventSink
	commands       []string
	forgotten      []string
	startLinkDelay time.Duration

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

// fakeSession is one scripted pairing attempt. result is 1-buffered and
// resolves exactly once: whichever of Pair or FailLink runs first delivers
// the value and marks the session closed; the other call, and any EmitCode
// after that point, become safe no-ops rather than a panic or a block. mu
// guards closed so "is it safe to send" and "mark it closed" happen as one
// atomic step — a plain done-channel-plus-select is not enough, since a send
// racing a close of the same channel can still panic even inside a select.
type fakeSession struct {
	codes  chan provider.LinkCode
	result chan provider.LinkResult

	mu     sync.Mutex
	closed bool
	once   sync.Once
}

func (s *fakeSession) Codes() <-chan provider.LinkCode    { return s.codes }
func (s *fakeSession) Result() <-chan provider.LinkResult { return s.result }

// Close marks the session done, stopping further code emission, and closes
// the codes channel. Idempotent and safe to call more than once.
func (s *fakeSession) Close() {
	s.once.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
		close(s.codes)
	})
}

// resolve delivers r on the result channel exactly once. It reports false,
// without sending or panicking, if the session was already resolved/closed.
func (s *fakeSession) resolve(r provider.LinkResult) bool {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return false
	}
	s.closed = true
	s.mu.Unlock()
	s.result <- r
	s.Close() // idempotent; also closes the codes channel.
	return true
}

// SetStartLinkDelay makes every subsequent StartLink call sleep for d before
// returning, so a test can simulate a slow (or, with a large d, effectively
// hung) provider dial without any real network dependency. Guarded by mu:
// unlike the other script knobs, tests mutate this one while a StartLink
// call may already be reading it concurrently.
func (f *FakeChat) SetStartLinkDelay(d time.Duration) {
	f.mu.Lock()
	f.startLinkDelay = d
	f.mu.Unlock()
}

func (f *FakeChat) StartLink(ctx context.Context) (provider.LinkSession, error) {
	f.mu.Lock()
	delay := f.startLinkDelay
	f.mu.Unlock()
	if delay > 0 {
		time.Sleep(delay)
	}
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

// SessionCount reports how many link sessions StartLink has produced in
// total. Tests use it to prove that a burst of concurrent /qr polls started
// pairing exactly once rather than once per poll.
func (f *FakeChat) SessionCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sessions)
}

// EmitCode pushes a QR code to the most recent link session. A no-op, not a
// panic, once that session has been paired, failed or closed — later tasks'
// tests rely on QR rotation racing pairing being safe.
func (f *FakeChat) EmitCode(code string) {
	s := f.latest()
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	select {
	case s.codes <- provider.LinkCode{Code: code, ExpiresAt: time.Now().Add(20 * time.Second)}:
	default:
		// Buffer full: drop rather than block while holding the session lock.
	}
}

// Pair completes the most recent link session successfully. A no-op if that
// session was already resolved.
func (f *FakeChat) Pair(id provider.Identity, deviceJID string) {
	if s := f.latest(); s != nil {
		s.resolve(provider.LinkResult{Identity: id, DeviceJID: deviceJID})
	}
}

// FailLink fails the most recent link session. A no-op if that session was
// already resolved.
func (f *FakeChat) FailLink(err error) {
	if s := f.latest(); s != nil {
		s.resolve(provider.LinkResult{Err: err})
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
