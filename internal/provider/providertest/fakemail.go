package providertest

import (
	"context"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gauravrautela/unified-messaging/internal/model"
	"github.com/gauravrautela/unified-messaging/internal/provider"
)

// FakeMail is a minimal scripted mail provider.Provider used by API tests
// that need a real Mailbox (and, for the push-notification tests, a real
// Pusher) without any network dependency on Microsoft Graph.
//
// GetMessage always reports provider.ErrNotFound and counts its calls, which
// is exactly what a mirror-miss / negative-cache test needs: every other
// Mailbox read method also returns provider.ErrNotFound (or an empty list),
// since nothing in this package exercises them, while every write method
// (Send, Reply, Forward, CreateDraft, SendDraft) reports success so a
// send-path test can observe a 2xx.
type FakeMail struct {
	name string

	getMessageCalls atomic.Int64

	// parseBlock, when set via SetParseBlock, is read from before every
	// subsequent ParseNotifications call returns, letting a test hold a
	// batch of calls open to observe how many run concurrently.
	parseBlock atomic.Value // <-chan struct{}

	inFlight    atomic.Int64
	maxInFlight atomic.Int64

	// DuplicateOnce makes the very next Create call report
	// provider.ErrSubscriptionExists instead of succeeding — enough to drive
	// a duplicate-then-adopt test without any special-casing by account or
	// subscription id. Every Create after that first one succeeds normally.
	DuplicateOnce bool
	dupUsed       atomic.Bool

	// AlwaysDuplicate makes every Create call report
	// provider.ErrSubscriptionExists, simulating a provider that keeps
	// reporting a duplicate even after every subscription it listed has been
	// deleted — the pathological case the retried-bool recursion guard exists
	// for.
	AlwaysDuplicate bool

	// ListSubs is what List reports for every account; nil means "none",
	// matching a provider that genuinely has nothing registered.
	ListSubs []provider.Subscription

	createCalls atomic.Int64
	listCalls   atomic.Int64

	mu            sync.Mutex
	deleted       []string
	notifications []provider.Notification
}

func NewFakeMail(name string) *FakeMail {
	return &FakeMail{name: name}
}

func (f *FakeMail) Name() string                 { return f.name }
func (f *FakeMail) Kind() string                 { return model.AccountKindMail }
func (f *FakeMail) Auth() provider.Authenticator { return nil }
func (f *FakeMail) Linker() provider.Linker      { return nil }
func (f *FakeMail) Chat() provider.Chatter       { return nil }
func (f *FakeMail) Mailbox() provider.Mailbox    { return f }
func (f *FakeMail) Push() provider.Pusher        { return f }

// ---- Mailbox ----

func (f *FakeMail) SyncScopes(ctx context.Context, accountID, cursor string) (provider.ScopeSet, error) {
	return provider.ScopeSet{}, provider.ErrNotFound
}

func (f *FakeMail) SyncMessages(ctx context.Context, accountID string, scope provider.Scope, cursor string, since time.Time) (provider.Changes, error) {
	return provider.Changes{}, provider.ErrNotFound
}

// GetMessageCalls reports how many times GetMessage has been invoked, so a
// negative-cache test can prove a repeated miss stopped reaching the
// "provider" after the first call.
func (f *FakeMail) GetMessageCalls() int64 { return f.getMessageCalls.Load() }

func (f *FakeMail) GetMessage(ctx context.Context, accountID, messageID string) (model.Email, error) {
	f.getMessageCalls.Add(1)
	return model.Email{}, provider.ErrNotFound
}

func (f *FakeMail) UpdateMessage(ctx context.Context, accountID, messageID string, upd provider.MessageUpdate) error {
	return provider.ErrNotFound
}

func (f *FakeMail) ListAttachments(ctx context.Context, accountID, messageID string) ([]model.Attachment, error) {
	return nil, provider.ErrNotFound
}

func (f *FakeMail) DownloadAttachment(ctx context.Context, accountID, messageID, attachmentID string) ([]byte, error) {
	return nil, provider.ErrNotFound
}

func (f *FakeMail) Send(ctx context.Context, accountID string, req model.SendRequest) (provider.SendResult, error) {
	return provider.SendResult{MessageID: "FAKEMAIL1"}, nil
}

func (f *FakeMail) Reply(ctx context.Context, accountID, messageID string, req model.SendRequest) (provider.SendResult, error) {
	return provider.SendResult{MessageID: "FAKEMAIL1"}, nil
}

func (f *FakeMail) Forward(ctx context.Context, accountID, messageID string, req model.SendRequest) (provider.SendResult, error) {
	return provider.SendResult{MessageID: "FAKEMAIL1"}, nil
}

func (f *FakeMail) CreateDraft(ctx context.Context, accountID string, req model.SendRequest) (model.Email, error) {
	return model.Email{AccountID: accountID, ID: "FAKEDRAFT1", Subject: req.Subject, Body: req.Body}, nil
}

func (f *FakeMail) SendDraft(ctx context.Context, accountID, draftID string) error {
	return nil
}

// ---- Pusher ----
//
// Only ParseNotifications carries any behaviour worth scripting; every other
// method is a trivial success so a test can register this provider as a
// push-capable one without further wiring.

func (f *FakeMail) Create(ctx context.Context, accountID string, cfg provider.PushConfig) (provider.Subscription, error) {
	f.createCalls.Add(1)
	if f.AlwaysDuplicate {
		return provider.Subscription{}, provider.ErrSubscriptionExists
	}
	if f.DuplicateOnce && !f.dupUsed.Swap(true) {
		return provider.Subscription{}, provider.ErrSubscriptionExists
	}
	return provider.Subscription{ID: "FAKESUB1", ExpiresAt: time.Now().Add(48 * time.Hour)}, nil
}

// CreateCalls reports how many times Create has been invoked.
func (f *FakeMail) CreateCalls() int64 { return f.createCalls.Load() }

func (f *FakeMail) Renew(ctx context.Context, accountID, subscriptionID string) (provider.Subscription, error) {
	return provider.Subscription{ID: subscriptionID}, nil
}

func (f *FakeMail) Delete(ctx context.Context, accountID, subscriptionID string) error {
	f.mu.Lock()
	f.deleted = append(f.deleted, subscriptionID)
	f.mu.Unlock()
	return nil
}

// Deleted reports every subscription ID passed to Delete, in call order.
func (f *FakeMail) Deleted() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.deleted...)
}

func (f *FakeMail) List(ctx context.Context, accountID string) ([]provider.Subscription, error) {
	f.listCalls.Add(1)
	return f.ListSubs, nil
}

// ListCalls reports how many times List has been invoked.
func (f *FakeMail) ListCalls() int64 { return f.listCalls.Load() }

func (f *FakeMail) RenewBefore() time.Duration { return 10 * time.Minute }

// SetParseBlock makes every subsequent ParseNotifications call wait on ch
// before returning, so a test can hold a batch of notification-handler
// invocations open at once and observe their concurrency. Pass nil to stop
// blocking.
func (f *FakeMail) SetParseBlock(ch <-chan struct{}) {
	f.parseBlock.Store(ch)
}

// InFlight reports how many ParseNotifications calls are currently blocked
// waiting on the channel set by SetParseBlock.
func (f *FakeMail) InFlight() int64 { return f.inFlight.Load() }

// MaxInFlight reports the high-water mark of concurrent ParseNotifications
// calls seen so far.
func (f *FakeMail) MaxInFlight() int64 { return f.maxInFlight.Load() }

// SetNotifications makes every subsequent ParseNotifications call return ns,
// regardless of raw, so a test can drive HandleNotifications with a scripted
// clientState without depending on any real wire format.
func (f *FakeMail) SetNotifications(ns []provider.Notification) {
	f.mu.Lock()
	f.notifications = ns
	f.mu.Unlock()
}

func (f *FakeMail) ParseNotifications(raw []byte) ([]provider.Notification, error) {
	n := f.inFlight.Add(1)
	defer f.inFlight.Add(-1)
	for {
		old := f.maxInFlight.Load()
		if n <= old || f.maxInFlight.CompareAndSwap(old, n) {
			break
		}
	}

	if v := f.parseBlock.Load(); v != nil {
		if ch, ok := v.(<-chan struct{}); ok && ch != nil {
			<-ch
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.notifications, nil
}

func (f *FakeMail) ValidationResponse(query url.Values) (string, bool) {
	return "", false
}

// Compile-time proof FakeMail satisfies every contract it claims to.
var (
	_ provider.Provider = (*FakeMail)(nil)
	_ provider.Mailbox  = (*FakeMail)(nil)
	_ provider.Pusher   = (*FakeMail)(nil)
)
