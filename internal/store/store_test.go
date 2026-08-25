package store

import (
	"database/sql"
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

func seedAccount(t *testing.T, s *Store) string {
	t.Helper()
	acct := model.Account{
		ID: "acc_1", Provider: "OUTLOOK", Email: "user@outlook.com",
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
		ID: "acc_2", Provider: "OUTLOOK", Email: "user@outlook.com",
		Name: "Renamed", Status: model.AccountOK,
	}); err != nil {
		t.Fatal(err)
	}

	all, err := s.ListAccounts()
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
	st := OAuthState{State: "abc", Verifier: "v", ExpiresAt: time.Now().Add(time.Minute)}
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
	if err := s.SaveOAuthState(OAuthState{
		State: "old", Verifier: "v", ExpiresAt: time.Now().Add(-time.Minute),
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
	other := model.Account{ID: "acc_2", Provider: "OUTLOOK", Email: "other@outlook.com", Status: model.AccountOK}
	if err := s.UpsertAccount(other); err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	for _, w := range []model.Webhook{
		{ID: "wh_global", URL: "https://g.example.com", CreatedAt: now},
		{ID: "wh_a1", URL: "https://a1.example.com", AccountID: acct, CreatedAt: now},
		{ID: "wh_a2", URL: "https://a2.example.com", AccountID: other.ID, CreatedAt: now},
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
	if err := s.SaveWebhook(model.Webhook{ID: "wh_a1", URL: "https://a1.example.com", AccountID: acct, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteAccount(acct); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListWebhooks()
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
	st := OAuthState{
		State: "abc", Verifier: "v", ExpiresAt: time.Now().Add(time.Minute),
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
	if err := s.SaveOAuthState(OAuthState{State: "none", Verifier: "v", ExpiresAt: time.Now().Add(time.Minute)}); err != nil {
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
	now := time.Now().UTC().Truncate(time.Second)
	if err := s.SaveWebhook(model.Webhook{ID: "wh_1", URL: "https://x.example.com", CreatedAt: now}); err != nil {
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
	now := time.Now()
	if err := s.SaveWebhook(model.Webhook{ID: "wh_1", URL: "https://x.example.com", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveDelivery(Delivery{
		ID: "dl_1", WebhookID: "wh_1", EventType: "mail_received", Payload: []byte(`{}`),
		NextAttemptAt: now, CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteWebhook("wh_1"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.ListDeliveries("wh_1"); len(got) != 0 {
		t.Fatalf("deliveries survived webhook deletion: %+v", got)
	}
}

// A database created before per-account webhooks existed must still open:
// the additive migrations have to run before anything that references the
// new columns.
func TestOpenMigratesPreWebhookAccountDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
		CREATE TABLE webhooks (id TEXT PRIMARY KEY, url TEXT NOT NULL, secret TEXT NOT NULL DEFAULT '',
		  events_json TEXT NOT NULL DEFAULT '[]', created_at INTEGER NOT NULL);
		CREATE TABLE oauth_states (state TEXT PRIMARY KEY, provider TEXT NOT NULL DEFAULT '', verifier TEXT NOT NULL,
		  success_url TEXT NOT NULL DEFAULT '', failure_url TEXT NOT NULL DEFAULT '', notify_url TEXT NOT NULL DEFAULT '',
		  created_at INTEGER NOT NULL, expires_at INTEGER NOT NULL);
		INSERT INTO webhooks (id, url, created_at) VALUES ('wh_old', 'https://old.example.com', 0);`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open on old database: %v", err)
	}
	defer s.Close()
	hooks, err := s.ListWebhooksFor("acc_any")
	if err != nil {
		t.Fatal(err)
	}
	if len(hooks) != 1 || hooks[0].AccountID != "" {
		t.Fatalf("old hook should survive as global: %+v", hooks)
	}
}

func TestWebhookNameRoundTrips(t *testing.T) {
	s := newTestStore(t)
	if err := s.SaveWebhook(model.Webhook{ID: "wh_1", Name: "crm-sync", URL: "https://x.example.com", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetWebhook("wh_1")
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
