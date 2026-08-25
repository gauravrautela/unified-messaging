package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/gauravrautela/unified-messaging/internal/model"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func seedDeveloper(t *testing.T, s *Store, id, email string) string {
	t.Helper()
	if err := s.CreateDeveloper(model.Developer{ID: id, Email: email, Name: "Dev"}, "$2a$12$hash"); err != nil {
		t.Fatal(err)
	}
	return id
}

func seedAccount(t *testing.T, s *Store) string {
	t.Helper()
	dev := seedDeveloper(t, s, "dev_1", "dev1@example.com")
	acct := model.Account{
		ID: "acc_1", DeveloperID: dev, Provider: "OUTLOOK", Email: "user@outlook.com",
		Name: "User", Status: model.AccountOK,
	}
	if err := s.UpsertAccount(acct); err != nil {
		t.Fatal(err)
	}
	return acct.ID
}

// Delta replays the same message across rounds, so re-applying one must be a
// no-op rather than a duplicate row.
func TestUpsertEmailIsIdempotent(t *testing.T) {
	s := newTestStore(t)
	acct := seedAccount(t, s)

	e := model.Email{
		AccountID: acct, ID: "M1", ThreadID: "C1", FolderID: "F1",
		Subject: "Hi", Date: time.Now().Truncate(time.Second),
		Body: "<p>hello</p>", BodyType: "html",
		From: model.Recipient{Email: "a@b.com"},
		To:   []model.Recipient{{Email: "c@d.com"}},
	}
	for range 3 {
		if err := s.UpsertEmail(e); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.ListEmails(EmailQuery{AccountID: acct})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d rows, want 1", len(got))
	}
	if got[0].To[0].Email != "c@d.com" {
		t.Fatalf("recipients lost: %+v", got[0].To)
	}
}

// A delta "updated" page may carry only the changed fields. Blanking a body we
// already hold would silently destroy data.
func TestUpsertEmailPreservesBodyOnPartialUpdate(t *testing.T) {
	s := newTestStore(t)
	acct := seedAccount(t, s)

	full := model.Email{
		AccountID: acct, ID: "M1", Subject: "Original",
		Body: "<p>the real body</p>", BodyType: "html",
	}
	if err := s.UpsertEmail(full); err != nil {
		t.Fatal(err)
	}

	partial := model.Email{AccountID: acct, ID: "M1", Subject: "Original", Read: true}
	if err := s.UpsertEmail(partial); err != nil {
		t.Fatal(err)
	}

	got, err := s.GetEmail(acct, "M1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Body != "<p>the real body</p>" {
		t.Fatalf("body was clobbered: %q", got.Body)
	}
	if !got.Read {
		t.Fatal("read flag not applied")
	}
}

func TestListEmailsFilters(t *testing.T) {
	s := newTestStore(t)
	acct := seedAccount(t, s)

	base := time.Now().Truncate(time.Second)
	mails := []model.Email{
		{AccountID: acct, ID: "M1", FolderID: "INBOX", Subject: "invoice due", Date: base, Read: false},
		{AccountID: acct, ID: "M2", FolderID: "INBOX", Subject: "lunch", Date: base.Add(time.Minute), Read: true},
		{AccountID: acct, ID: "M3", FolderID: "SENT", Subject: "re: invoice", Date: base.Add(2 * time.Minute), Read: true},
	}
	for _, m := range mails {
		if err := s.UpsertEmail(m); err != nil {
			t.Fatal(err)
		}
	}

	inbox, err := s.ListEmails(EmailQuery{AccountID: acct, FolderID: "INBOX"})
	if err != nil {
		t.Fatal(err)
	}
	if len(inbox) != 2 {
		t.Fatalf("folder filter returned %d, want 2", len(inbox))
	}
	// Newest first.
	if inbox[0].ID != "M2" {
		t.Fatalf("ordering wrong: first is %s", inbox[0].ID)
	}

	unread := true
	un, err := s.ListEmails(EmailQuery{AccountID: acct, Unread: &unread})
	if err != nil {
		t.Fatal(err)
	}
	if len(un) != 1 || un[0].ID != "M1" {
		t.Fatalf("unread filter = %+v", un)
	}

	found, err := s.ListEmails(EmailQuery{AccountID: acct, Search: "invoice"})
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 2 {
		t.Fatalf("search returned %d, want 2", len(found))
	}
}

// Reconnecting a mailbox must land on the existing account rather than
// creating a second row for the same address.
func TestUpsertAccountConflictsOnEmail(t *testing.T) {
	s := newTestStore(t)
	seedAccount(t, s)

	if err := s.UpsertAccount(model.Account{
		ID: "acc_2", DeveloperID: "dev_1", Provider: "OUTLOOK", Email: "user@outlook.com",
		Name: "Renamed", Status: model.AccountOK,
	}); err != nil {
		t.Fatal(err)
	}

	all, err := s.ListAccounts("dev_1")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("got %d accounts, want 1", len(all))
	}
	if all[0].ID != "acc_1" {
		t.Fatalf("account id changed to %s; callers would lose their handle", all[0].ID)
	}
	if all[0].Name != "Renamed" {
		t.Fatalf("profile not refreshed: %q", all[0].Name)
	}
}

func TestOAuthStateIsSingleUse(t *testing.T) {
	s := newTestStore(t)
	seedDeveloper(t, s, "dev_1", "a@x.com")
	st := OAuthState{State: "abc", DeveloperID: "dev_1", Verifier: "v", ExpiresAt: time.Now().Add(time.Minute)}
	if err := s.SaveOAuthState(st); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PeekOAuthState("abc"); err != nil {
		t.Fatalf("peek should not consume: %v", err)
	}
	if _, err := s.TakeOAuthState("abc"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TakeOAuthState("abc"); err == nil {
		t.Fatal("state was reusable; replay is possible")
	}
}

func TestExpiredOAuthStateRejected(t *testing.T) {
	s := newTestStore(t)
	seedDeveloper(t, s, "dev_1", "a@x.com")
	if err := s.SaveOAuthState(OAuthState{
		State: "old", DeveloperID: "dev_1", Verifier: "v", ExpiresAt: time.Now().Add(-time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TakeOAuthState("old"); err == nil {
		t.Fatal("expired state accepted")
	}
}

// A webhook bound to an account fires only for that account; one with no
// account is global and fires for everyone.
func TestListWebhooksForScopesByAccount(t *testing.T) {
	s := newTestStore(t)
	acct := seedAccount(t, s)
	other := model.Account{ID: "acc_2", DeveloperID: "dev_1", Provider: "OUTLOOK", Email: "other@outlook.com", Status: model.AccountOK}
	if err := s.UpsertAccount(other); err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	for _, w := range []model.Webhook{
		{ID: "wh_global", DeveloperID: "dev_1", URL: "https://g.example.com", CreatedAt: now},
		{ID: "wh_a1", DeveloperID: "dev_1", URL: "https://a1.example.com", AccountID: acct, CreatedAt: now},
		{ID: "wh_a2", DeveloperID: "dev_1", URL: "https://a2.example.com", AccountID: other.ID, CreatedAt: now},
	} {
		if err := s.SaveWebhook(w); err != nil {
			t.Fatal(err)
		}
	}

	got, err := s.ListWebhooksFor(acct)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, w := range got {
		ids[w.ID] = true
	}
	if len(got) != 2 || !ids["wh_global"] || !ids["wh_a1"] {
		t.Fatalf("ListWebhooksFor(%s) = %v, want global + wh_a1", acct, ids)
	}
	if got[0].AccountID != "" && got[0].AccountID != acct {
		t.Fatalf("account_id not round-tripped: %+v", got)
	}
}

// Deleting an account must take its webhooks with it, or a re-used account id
// would inherit a stranger's endpoint.
func TestAccountWebhooksCascadeOnDelete(t *testing.T) {
	s := newTestStore(t)
	acct := seedAccount(t, s)
	if err := s.SaveWebhook(model.Webhook{ID: "wh_a1", DeveloperID: "dev_1", URL: "https://a1.example.com", AccountID: acct, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteAccount(acct); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListWebhooks("dev_1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("webhooks survived account deletion: %+v", got)
	}
}

// A webhook requested at connect time rides along on the pending OAuth state
// so the callback can attach it to whatever account gets created.
func TestOAuthStateCarriesPendingWebhook(t *testing.T) {
	s := newTestStore(t)
	seedDeveloper(t, s, "dev_1", "a@x.com")
	st := OAuthState{
		State: "abc", DeveloperID: "dev_1", Verifier: "v", ExpiresAt: time.Now().Add(time.Minute),
		Webhook: &PendingWebhook{URL: "https://hook.example.com", Secret: "s3", Events: []string{"mail_received"}},
	}
	if err := s.SaveOAuthState(st); err != nil {
		t.Fatal(err)
	}
	got, err := s.TakeOAuthState("abc")
	if err != nil {
		t.Fatal(err)
	}
	if got.Webhook == nil || got.Webhook.URL != "https://hook.example.com" ||
		got.Webhook.Secret != "s3" || len(got.Webhook.Events) != 1 {
		t.Fatalf("pending webhook lost: %+v", got.Webhook)
	}

	// And absent stays absent.
	if err := s.SaveOAuthState(OAuthState{State: "none", DeveloperID: "dev_1", Verifier: "v", ExpiresAt: time.Now().Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.TakeOAuthState("none")
	if got.Webhook != nil {
		t.Fatalf("expected no webhook, got %+v", got.Webhook)
	}
}

// A failed delivery waits in the queue until its next_attempt_at passes.
func TestDueDeliveriesHonoursSchedule(t *testing.T) {
	s := newTestStore(t)
	seedDeveloper(t, s, "dev_1", "a@x.com")
	now := time.Now().UTC().Truncate(time.Second)
	if err := s.SaveWebhook(model.Webhook{ID: "wh_1", DeveloperID: "dev_1", URL: "https://x.example.com", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveDelivery(Delivery{
		ID: "dl_soon", WebhookID: "wh_1", AccountID: "acc_1", EventType: "mail_received",
		Payload: []byte(`{}`), Attempts: 1, NextAttemptAt: now.Add(-time.Second), CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveDelivery(Delivery{
		ID: "dl_later", WebhookID: "wh_1", AccountID: "acc_1", EventType: "mail_received",
		Payload: []byte(`{}`), Attempts: 1, NextAttemptAt: now.Add(time.Hour), CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	due, err := s.DueDeliveries(now, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 || due[0].ID != "dl_soon" || string(due[0].Payload) != `{}` {
		t.Fatalf("due = %+v, want only dl_soon", due)
	}

	// Rescheduling pushes it out again; a dead one is never returned.
	if err := s.SaveDelivery(Delivery{
		ID: "dl_soon", WebhookID: "wh_1", AccountID: "acc_1", EventType: "mail_received",
		Payload: []byte(`{}`), Attempts: 2, NextAttemptAt: now.Add(time.Minute), LastError: "status 500", CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveDelivery(Delivery{
		ID: "dl_dead", WebhookID: "wh_1", AccountID: "acc_1", EventType: "mail_received",
		Payload: []byte(`{}`), Attempts: 8, NextAttemptAt: now.Add(-time.Hour), Dead: true, CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	due, _ = s.DueDeliveries(now, 10)
	if len(due) != 0 {
		t.Fatalf("due = %+v, want none", due)
	}

	if err := s.DeleteDelivery("dl_soon"); err != nil {
		t.Fatal(err)
	}
	all, err := s.ListDeliveries("wh_1")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("ListDeliveries = %d rows, want 2 (dl_later, dl_dead)", len(all))
	}
}

// Removing a webhook drops its queued deliveries; nothing should keep posting
// to an endpoint the caller unregistered.
func TestDeleteWebhookDropsQueuedDeliveries(t *testing.T) {
	s := newTestStore(t)
	seedDeveloper(t, s, "dev_1", "a@x.com")
	now := time.Now()
	if err := s.SaveWebhook(model.Webhook{ID: "wh_1", DeveloperID: "dev_1", URL: "https://x.example.com", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveDelivery(Delivery{
		ID: "dl_1", WebhookID: "wh_1", EventType: "mail_received", Payload: []byte(`{}`),
		NextAttemptAt: now, CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteWebhook("dev_1", "wh_1"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.ListDeliveries("wh_1"); len(got) != 0 {
		t.Fatalf("deliveries survived webhook deletion: %+v", got)
	}
}

// A database created before per-account webhooks existed must still open:
// the additive migrations have to run before anything that references the
// new columns.
func TestWebhookNameRoundTrips(t *testing.T) {
	s := newTestStore(t)
	seedDeveloper(t, s, "dev_1", "a@x.com")
	if err := s.SaveWebhook(model.Webhook{ID: "wh_1", DeveloperID: "dev_1", Name: "crm-sync", URL: "https://x.example.com", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetWebhook("dev_1", "wh_1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "crm-sync" {
		t.Fatalf("name = %q", got.Name)
	}
}

func TestScanEmailDerivesBodyPlain(t *testing.T) {
	s := newTestStore(t)
	acct := seedAccount(t, s)
	if err := s.UpsertEmail(model.Email{
		AccountID: acct, ID: "M1", Subject: "x", Date: time.Now(),
		Body: "<style>p{}</style><p>Hello&nbsp;&amp; bye</p>", BodyType: "html",
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetEmail(acct, "M1")
	if err != nil {
		t.Fatal(err)
	}
	if got.BodyPlain != "Hello & bye" {
		t.Fatalf("body_plain = %q", got.BodyPlain)
	}
}

func TestOpenRefusesPreTenancyDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE accounts (id TEXT PRIMARY KEY, provider TEXT, email TEXT,
		name TEXT, status TEXT, created_at INTEGER, updated_at INTEGER, last_synced_at INTEGER)`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	_, err = Open(path)
	if err == nil {
		t.Fatal("pre-tenancy database was opened")
	}
	want := "database " + path + " predates multi-tenancy; delete it (and its -wal/-shm files) and reconnect your mailboxes"
	if err.Error() != want {
		t.Fatalf("error = %q\nwant  %q", err.Error(), want)
	}
	if !errors.Is(err, ErrPreTenancy) {
		t.Fatal("refusal must match ErrPreTenancy")
	}
}

func TestDeveloperRoundTripAndUniqueEmail(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateDeveloper(model.Developer{ID: "dev_1", Email: "a@x.com", Name: "A"}, "h1"); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateDeveloper(model.Developer{ID: "dev_2", Email: "a@x.com"}, "h2"); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate email err = %v, want ErrConflict", err)
	}
	d, hash, err := s.DeveloperByEmail("a@x.com")
	if err != nil || d.ID != "dev_1" || hash != "h1" || d.Name != "A" {
		t.Fatalf("DeveloperByEmail = %+v %q %v", d, hash, err)
	}
	if _, _, err := s.DeveloperByEmail("nobody@x.com"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown email err = %v", err)
	}
}

func TestSessionsExpireAndExtend(t *testing.T) {
	s := newTestStore(t)
	seedDeveloper(t, s, "dev_1", "a@x.com")
	now := time.Now().UTC().Truncate(time.Second)
	if err := s.CreateSession("sess1", "dev_1", now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	d, exp, err := s.SessionDeveloper("sess1", now)
	if err != nil || d.ID != "dev_1" || !exp.Equal(now.Add(time.Hour)) {
		t.Fatalf("SessionDeveloper = %+v %v %v", d, exp, err)
	}
	if _, _, err := s.SessionDeveloper("sess1", now.Add(2*time.Hour)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired session err = %v", err)
	}
	if err := s.ExtendSession("sess1", now.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.SessionDeveloper("sess1", now.Add(2*time.Hour)); err != nil {
		t.Fatalf("extended session rejected: %v", err)
	}
	if err := s.DeleteSession("sess1"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.SessionDeveloper("sess1", now); !errors.Is(err, ErrNotFound) {
		t.Fatal("deleted session still resolves")
	}
}

func TestAPIKeysResolveListAndRevoke(t *testing.T) {
	s := newTestStore(t)
	seedDeveloper(t, s, "dev_1", "a@x.com")
	seedDeveloper(t, s, "dev_2", "b@x.com")
	now := time.Now().UTC().Truncate(time.Second)
	if err := s.CreateAPIKey(model.APIKey{ID: "key_1", Name: "prod", Prefix: "um_abcdefghi", CreatedAt: now}, "dev_1", "hash1"); err != nil {
		t.Fatal(err)
	}
	d, k, err := s.DeveloperByKeyHash("hash1")
	if err != nil || d.ID != "dev_1" || k.ID != "key_1" || k.Prefix != "um_abcdefghi" {
		t.Fatalf("DeveloperByKeyHash = %+v %+v %v", d, k, err)
	}
	if err := s.TouchAPIKey("key_1", now); err != nil {
		t.Fatal(err)
	}
	keys, err := s.ListAPIKeys("dev_1")
	if err != nil || len(keys) != 1 || keys[0].LastUsedAt == nil || !keys[0].LastUsedAt.Equal(now) {
		t.Fatalf("ListAPIKeys = %+v %v", keys, err)
	}
	if other, _ := s.ListAPIKeys("dev_2"); len(other) != 0 {
		t.Fatalf("dev_2 sees dev_1's keys: %+v", other)
	}
	if err := s.RevokeAPIKey("dev_2", "key_1", now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-developer revoke err = %v, want ErrNotFound", err)
	}
	if err := s.RevokeAPIKey("dev_1", "key_1", now); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.DeveloperByKeyHash("hash1"); !errors.Is(err, ErrNotFound) {
		t.Fatal("revoked key still resolves")
	}
	keys, _ = s.ListAPIKeys("dev_1")
	if len(keys) != 1 || keys[0].RevokedAt == nil {
		t.Fatalf("revoked key should still be listed with revoked_at: %+v", keys)
	}
}

func TestAccountsAreScopedByDeveloper(t *testing.T) {
	s := newTestStore(t)
	seedDeveloper(t, s, "dev_1", "a@x.com")
	seedDeveloper(t, s, "dev_2", "b@x.com")
	for _, a := range []model.Account{
		{ID: "acc_1", DeveloperID: "dev_1", Provider: "OUTLOOK", Email: "m@outlook.com", Status: model.AccountOK},
		{ID: "acc_2", DeveloperID: "dev_2", Provider: "OUTLOOK", Email: "m@outlook.com", Status: model.AccountOK}, // same mailbox, other tenant
	} {
		if err := s.UpsertAccount(a); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.GetAccount("dev_1", "acc_2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("dev_1 read dev_2's account: %v", err)
	}
	if a, err := s.GetAccount("dev_1", "acc_1"); err != nil || a.DeveloperID != "dev_1" {
		t.Fatalf("GetAccount own = %+v %v", a, err)
	}
	if l, _ := s.ListAccounts("dev_2"); len(l) != 1 || l[0].ID != "acc_2" {
		t.Fatalf("ListAccounts(dev_2) = %+v", l)
	}
	if all, _ := s.ListAllAccounts(); len(all) != 2 {
		t.Fatalf("ListAllAccounts = %d, want 2", len(all))
	}
	if id, _ := s.AccountIDByEmail("dev_2", "m@outlook.com"); id != "acc_2" {
		t.Fatalf("AccountIDByEmail(dev_2) = %q", id)
	}
	if _, err := s.GetAnyAccount("acc_2"); err != nil {
		t.Fatalf("GetAnyAccount: %v", err)
	}
}

func TestDeletingDeveloperCascades(t *testing.T) {
	s := newTestStore(t)
	acct := seedAccount(t, s) // dev_1 / acc_1
	if err := s.CreateAPIKey(model.APIKey{ID: "key_1", Name: "n", Prefix: "p", CreatedAt: time.Now()}, "dev_1", "h"); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSession("sess1", "dev_1", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveWebhook(model.Webhook{ID: "wh_1", DeveloperID: "dev_1", URL: "https://x", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertEmail(model.Email{AccountID: acct, ID: "M1", Subject: "x", Date: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DELETE FROM developers WHERE id = 'dev_1'`); err != nil {
		t.Fatal(err)
	}
	for table, want := range map[string]int{"api_keys": 0, "sessions": 0, "accounts": 0, "emails": 0, "webhooks": 0} {
		var n int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != want {
			t.Fatalf("%s has %d rows after developer delete", table, n)
		}
	}
}

func TestWebhooksAreScopedByDeveloper(t *testing.T) {
	s := newTestStore(t)
	seedDeveloper(t, s, "dev_1", "a@x.com")
	seedDeveloper(t, s, "dev_2", "b@x.com")
	if err := s.SaveWebhook(model.Webhook{ID: "wh_1", DeveloperID: "dev_1", URL: "https://x", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetWebhook("dev_2", "wh_1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-developer GetWebhook err = %v", err)
	}
	if err := s.DeleteWebhook("dev_2", "wh_1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-developer DeleteWebhook err = %v", err)
	}
	if l, _ := s.ListWebhooks("dev_2"); len(l) != 0 {
		t.Fatalf("dev_2 lists dev_1's hooks: %+v", l)
	}
	if err := s.DeleteWebhook("dev_1", "wh_1"); err != nil {
		t.Fatal(err)
	}
}

func TestOAuthStateCarriesDeveloper(t *testing.T) {
	s := newTestStore(t)
	seedDeveloper(t, s, "dev_1", "a@x.com")
	if err := s.SaveOAuthState(OAuthState{State: "st", DeveloperID: "dev_1", Verifier: "v", ExpiresAt: time.Now().Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	got, err := s.TakeOAuthState("st")
	if err != nil || got.DeveloperID != "dev_1" {
		t.Fatalf("TakeOAuthState = %+v %v", got, err)
	}
}
