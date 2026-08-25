package whatsapp

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/gauravrautela/unified-messaging/internal/model"
	"github.com/gauravrautela/unified-messaging/internal/provider"
)

// openDB gives each test its own database file, driven by the same cgo-free
// driver the service uses.
func openDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "test.db")+"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// The device container has to come up on our own SQLite handle: whatsmeow's
// migrations run against the same file as the rest of the service.
func TestNewUpgradesDeviceStore(t *testing.T) {
	db := openDB(t)
	p, err := New(db, "unified-messaging-test", nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if p.Name() != Name || p.Kind() != model.AccountKindChat {
		t.Fatalf("identity = %s/%s", p.Name(), p.Kind())
	}
	if p.Linker() == nil || p.Chat() == nil {
		t.Fatal("chat provider must expose Linker and Chat")
	}
	if p.Auth() != nil || p.Mailbox() != nil || p.Push() != nil {
		t.Fatal("chat provider must not claim mail capabilities")
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM whatsmeow_device`).Scan(&n); err != nil {
		t.Fatalf("device table missing after upgrade: %v", err)
	}
}

// A device whose credentials are gone can only be fixed by relinking, so
// Connect must say so rather than returning a retryable error.
func TestConnectWithoutDeviceNeedsReauth(t *testing.T) {
	p, err := New(openDB(t), "unified-messaging-test", nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = p.Connect(context.Background(), "acc_1", "919888000000:1@s.whatsapp.net", nil)
	if !errors.Is(err, provider.ErrReauthRequired) {
		t.Fatalf("connect without device = %v, want ErrReauthRequired", err)
	}
	// A device id that cannot be parsed is a caller error, not a relink prompt.
	_, err = p.Connect(context.Background(), "acc_1", "919888000000:notanumber@s.whatsapp.net", nil)
	if err == nil || errors.Is(err, provider.ErrReauthRequired) {
		t.Fatalf("malformed device jid = %v, want a parse error", err)
	}
}

// Forgetting an unknown device is a no-op, not a failure: the caller's goal —
// no credentials on disk — is already true.
func TestForgetUnknownDevice(t *testing.T) {
	p, err := New(openDB(t), "unified-messaging-test", nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := p.Forget(context.Background(), "919888000000:1@s.whatsapp.net"); err != nil {
		t.Fatalf("forget unknown device: %v", err)
	}
}

// Commands and the roster need a live connection; without one the account is
// simply not there as far as the adapter is concerned.
func TestCommandsWithoutConnection(t *testing.T) {
	p, err := New(openDB(t), "unified-messaging-test", nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	if _, _, _, err := p.Chats(ctx, "acc_1"); !errors.Is(err, provider.ErrNotFound) {
		t.Fatalf("Chats = %v, want ErrNotFound", err)
	}
	if _, err := p.SendText(ctx, "acc_1", "chat", "hi", ""); !errors.Is(err, provider.ErrNotFound) {
		t.Fatalf("SendText = %v, want ErrNotFound", err)
	}
	if err := p.React(ctx, "acc_1", "chat", "m", "👍"); !errors.Is(err, provider.ErrNotFound) {
		t.Fatalf("React = %v, want ErrNotFound", err)
	}
	if err := p.MarkRead(ctx, "acc_1", "chat", []string{"m"}); !errors.Is(err, provider.ErrNotFound) {
		t.Fatalf("MarkRead = %v, want ErrNotFound", err)
	}
}

// The library's own reconnect loop must stay off: the chat runtime owns
// reconnection and builds a new client for each attempt.
func TestNewClientDisablesAutoReconnect(t *testing.T) {
	p, err := New(openDB(t), "unified-messaging-test", nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c := p.newClient(p.container.NewDevice()); c.EnableAutoReconnect {
		t.Fatal("whatsmeow auto-reconnect must be disabled")
	}
}
