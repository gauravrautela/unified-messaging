package syncer

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/gauravrautela/unified-messaging/internal/events"
	"github.com/gauravrautela/unified-messaging/internal/model"
	"github.com/gauravrautela/unified-messaging/internal/provider"
	"github.com/gauravrautela/unified-messaging/internal/store"
)

// simpleMailbox stands in for a provider shaped nothing like Outlook: a single
// mailbox-wide cursor, no folders, and no push mechanism — Gmail's historyId or
// an IMAP server, roughly.
//
// It exists to keep the sync engine honest. If the core ever grows an
// assumption that scopes are folders, or that push always exists, this test
// fails rather than the assumption surviving until a second provider is written.
type simpleMailbox struct {
	// rounds is consumed one entry per SyncMessages call.
	rounds []provider.Changes
	calls  int

	scopeCursors []string
}

func (m *simpleMailbox) SyncScopes(_ context.Context, _, cursor string) (provider.ScopeSet, error) {
	m.scopeCursors = append(m.scopeCursors, cursor)
	// One scope covering the whole mailbox, and no folders at all.
	return provider.ScopeSet{
		Scopes: []provider.Scope{{ID: "all", Name: "All mail"}},
	}, nil
}

func (m *simpleMailbox) SyncMessages(_ context.Context, _ string, _ provider.Scope,
	_ string, _ time.Time) (provider.Changes, error) {
	if m.calls >= len(m.rounds) {
		return provider.Changes{Cursor: "end"}, nil
	}
	out := m.rounds[m.calls]
	m.calls++
	return out, nil
}

func (m *simpleMailbox) GetMessage(context.Context, string, string) (model.Email, error) {
	return model.Email{}, provider.ErrNotFound
}
func (m *simpleMailbox) UpdateMessage(context.Context, string, string, provider.MessageUpdate) error {
	return nil
}
func (m *simpleMailbox) ListAttachments(context.Context, string, string) ([]model.Attachment, error) {
	return nil, nil
}
func (m *simpleMailbox) DownloadAttachment(context.Context, string, string, string) ([]byte, error) {
	return nil, nil
}
func (m *simpleMailbox) Send(context.Context, string, model.SendRequest) (provider.SendResult, error) {
	return provider.SendResult{}, nil
}
func (m *simpleMailbox) Reply(context.Context, string, string, model.SendRequest) (provider.SendResult, error) {
	return provider.SendResult{}, nil
}
func (m *simpleMailbox) Forward(context.Context, string, string, model.SendRequest) (provider.SendResult, error) {
	return provider.SendResult{}, nil
}
func (m *simpleMailbox) CreateDraft(context.Context, string, model.SendRequest) (model.Email, error) {
	return model.Email{}, nil
}
func (m *simpleMailbox) SendDraft(context.Context, string, string) error { return nil }

type simpleProvider struct{ mb *simpleMailbox }

func (p *simpleProvider) Name() string                 { return "SIMPLE" }
func (p *simpleProvider) Kind() string                 { return model.AccountKindMail }
func (p *simpleProvider) Auth() provider.Authenticator { return nil }
func (p *simpleProvider) Linker() provider.Linker      { return nil }
func (p *simpleProvider) Mailbox() provider.Mailbox    { return p.mb }
func (p *simpleProvider) Chat() provider.Chatter       { return nil }

// Push returns nil: this provider has no push mechanism, so the core must fall
// back to polling instead of erroring.
func (p *simpleProvider) Push() provider.Pusher { return nil }

var _ provider.Provider = (*simpleProvider)(nil)

func TestSyncWorksForAProviderWithoutFoldersOrPush(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "simple.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if err := db.CreateDeveloper(model.Developer{ID: "dev_1", Email: "dev@example.com"}, "hash"); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertAccount(model.Account{
		ID: "acc_1", DeveloperID: "dev_1", Provider: "SIMPLE", Email: "user@example.com", Status: model.AccountOK,
	}); err != nil {
		t.Fatal(err)
	}

	mb := &simpleMailbox{rounds: []provider.Changes{
		{ // initial backfill
			Changed: []model.Email{
				{AccountID: "acc_1", ID: "A", Subject: "one", Date: time.Now()},
				{AccountID: "acc_1", ID: "B", Subject: "two", Date: time.Now()},
			},
			Cursor: "c1",
		},
		{ // incremental
			Changed: []model.Email{{AccountID: "acc_1", ID: "C", Subject: "three", Date: time.Now()}},
			Removed: []string{"A"},
			Cursor:  "c2",
		},
	}}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	disp := events.NewDispatcher(db, nil, log)
	disp.Start(ctx)

	registry := provider.NewRegistry(&simpleProvider{mb: mb})
	s := New(db, registry, nil, disp, log, Options{PollInterval: time.Hour})

	if err := s.SyncAccount(ctx, "acc_1"); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	mails, err := db.ListEmails(store.EmailQuery{AccountID: "acc_1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(mails) != 2 {
		t.Fatalf("backfill stored %d messages, want 2", len(mails))
	}

	// A provider without folders must not have folder rows invented for it.
	folders, err := db.ListFolders("acc_1")
	if err != nil {
		t.Fatal(err)
	}
	if len(folders) != 0 {
		t.Fatalf("stored %d folders for a folderless provider", len(folders))
	}

	if err := s.SyncAccount(ctx, "acc_1"); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if _, err := db.GetEmail("acc_1", "A"); err == nil {
		t.Fatal("removed message A is still stored")
	}
	if _, err := db.GetEmail("acc_1", "C"); err != nil {
		t.Fatalf("message C was not synced: %v", err)
	}

	// The scope cursor must be persisted and replayed, not re-derived.
	cursor, err := db.GetCursor("acc_1", "all")
	if err != nil {
		t.Fatal(err)
	}
	if cursor != "c2" {
		t.Fatalf("stored cursor = %q, want %q", cursor, "c2")
	}
}

// A provider that cannot push must not break subscription reconciliation.
func TestEnsureSubscriptionIsNoOpWithoutPush(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "nopush.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if err := db.CreateDeveloper(model.Developer{ID: "dev_1", Email: "dev@example.com"}, "hash"); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertAccount(model.Account{
		ID: "acc_1", DeveloperID: "dev_1", Provider: "SIMPLE", Email: "user@example.com", Status: model.AccountOK,
	}); err != nil {
		t.Fatal(err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	registry := provider.NewRegistry(&simpleProvider{mb: &simpleMailbox{}})
	s := New(db, registry, nil, events.NewDispatcher(db, nil, log), log,
		Options{PublicBaseURL: "https://example.test"})

	if err := s.EnsureSubscription(context.Background(), "acc_1"); err != nil {
		t.Fatalf("expected a silent no-op, got %v", err)
	}
	subs, err := db.SubscriptionsForAccount("acc_1")
	if err != nil {
		t.Fatal(err)
	}
	if len(subs) != 0 {
		t.Fatalf("recorded %d subscriptions for a push-less provider", len(subs))
	}
}
