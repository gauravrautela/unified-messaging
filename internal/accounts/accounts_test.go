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
