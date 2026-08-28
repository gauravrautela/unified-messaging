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
	// A command against a disconnected account is not a missing resource: 404
	// is the shape this API reserves for "belongs to another developer", and a
	// client seeing it would conclude the message vanished. It is a distinct
	// sentinel the API maps to 409 reconnect_required.
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"SendText", func() error { _, err := p.SendText(ctx, "acc_1", "chat", "hi", ""); return err }()},
		{"StartDirect", func() error { _, err := p.StartDirect(ctx, "acc_1", "+919888000000"); return err }()},
		{"React", p.React(ctx, "acc_1", "chat", "m", "👍")},
		{"Edit", p.Edit(ctx, "acc_1", "chat", "m", "hi")},
		{"Delete", p.Delete(ctx, "acc_1", "chat", "m")},
		{"MarkRead", p.MarkRead(ctx, "acc_1", "chat", []string{"m"})},
	} {
		if !errors.Is(tc.err, provider.ErrNotConnected) {
			t.Fatalf("%s = %v, want ErrNotConnected", tc.name, tc.err)
		}
		if errors.Is(tc.err, provider.ErrNotFound) {
			t.Fatalf("%s = %v, must not also read as ErrNotFound (that maps to 404)", tc.name, tc.err)
		}
	}
	// Logout is the exception: no live connection already is the end state a
	// logout wants, not a failure.
	if err := p.Logout(ctx, "acc_1"); err != nil {
		t.Fatalf("Logout without connection = %v, want nil", err)
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
	// The pairing client needs a second flag: WhatsApp always sends a 515
	// stream error straight after pair-success, and whatsmeow's handler for it
	// is gated on DisableLoginAutoReconnect alone, not EnableAutoReconnect. Left
	// unset, that spawns a background reconnect on the library's own context —
	// a second live socket for a device the chat runtime is about to Attach.
	pc := p.newPairingClient(p.container.NewDevice())
	if pc.EnableAutoReconnect {
		t.Fatal("pairing client: whatsmeow auto-reconnect must be disabled")
	}
	if !pc.DisableLoginAutoReconnect {
		t.Fatal("pairing client: whatsmeow post-pairing (515) relogin must be disabled")
	}
}
