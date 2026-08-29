package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gauravrautela/unified-messaging/internal/model"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	return OpenForTest(t)
}

// seedDeveloper is idempotent: several seed helpers (seedAccount,
// seedChatAccount) call it for the same "dev_1" fixture, and a test that
// exercises both a mail and a chat account for one developer seeds it twice.
func seedDeveloper(t *testing.T, s *Store, id, email string) string {
	t.Helper()
	if err := s.CreateDeveloper(model.Developer{ID: id, Email: email, Name: "Dev"}, "$2a$12$hash"); err != nil && !errors.Is(err, ErrConflict) {
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

// ClaimOAuthStateBrowser is what closes the post-failure re-claim window: the
// first caller to name a state wins it for its hash, and every later caller —
// including a retry after an in-memory pairing attempt was dropped — must
// present the same one.
func TestClaimOAuthStateBrowserBindsFirstCallerAndRejectsOthers(t *testing.T) {
	s := newTestStore(t)
	seedDeveloper(t, s, "dev_1", "a@x.com")
	if err := s.SaveOAuthState(OAuthState{
		State: "st1", DeveloperID: "dev_1", Verifier: "v", ExpiresAt: time.Now().Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}

	ok, err := s.ClaimOAuthStateBrowser("st1", "hash-a")
	if err != nil || !ok {
		t.Fatalf("first claim = %v, %v, want true, nil", ok, err)
	}
	got, err := s.PeekOAuthState("st1")
	if err != nil || got.BrowserHash != "hash-a" {
		t.Fatalf("PeekOAuthState after claim = %+v, %v", got, err)
	}

	// The same browser re-claiming (e.g. a second /qr poll) still succeeds.
	if ok, err := s.ClaimOAuthStateBrowser("st1", "hash-a"); err != nil || !ok {
		t.Fatalf("repeat claim by the same browser = %v, %v, want true, nil", ok, err)
	}

	// A different browser is refused outright.
	if ok, err := s.ClaimOAuthStateBrowser("st1", "hash-b"); err != nil || ok {
		t.Fatalf("claim by a different browser = %v, %v, want false, nil", ok, err)
	}
	got, err = s.PeekOAuthState("st1")
	if err != nil || got.BrowserHash != "hash-a" {
		t.Fatalf("browser_hash changed after a rejected claim: %+v, %v", got, err)
	}

	// An unknown state is refused the same way, without an error a caller
	// could use to distinguish "unknown" from "someone else already claimed
	// it" — this endpoint is browser-facing.
	if ok, err := s.ClaimOAuthStateBrowser("nope", "hash-a"); err != nil || ok {
		t.Fatalf("claim on unknown state = %v, %v, want false, nil", ok, err)
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
// account is global and fires for every account of the same developer.
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

// A pending telegram hook carries a bot token, which is a credential like any
// other: it must not sit in the oauth_states row in clear for the up-to-30-
// minute lifetime of the connect attempt.
func TestOAuthStateSealsPendingTelegramBotToken(t *testing.T) {
	s := newTestStore(t)
	s.SetSealKey([]byte("0123456789abcdef0123456789abcdef"))
	seedDeveloper(t, s, "dev_1", "a@x.com")
	st := OAuthState{
		State: "tg1", DeveloperID: "dev_1", Verifier: "v", ExpiresAt: time.Now().Add(time.Minute),
		Webhook: &PendingWebhook{Kind: model.WebhookKindTelegram, BotToken: "123:ABC", ChatID: "-100123",
			Events: []string{"chat_received"}},
	}
	if err := s.SaveOAuthState(st); err != nil {
		t.Fatal(err)
	}

	var raw string
	if err := s.db.QueryRow(s.q(`SELECT webhook_json FROM oauth_states WHERE state = ?`), "tg1").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if raw == "" || strings.Contains(raw, "123:ABC") {
		t.Fatalf("bot token stored unsealed: %q", raw)
	}

	got, err := s.TakeOAuthState("tg1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Webhook == nil || got.Webhook.BotToken != "123:ABC" || got.Webhook.ChatID != "-100123" {
		t.Fatalf("pending telegram webhook lost: %+v", got.Webhook)
	}
}

// Without a seal key, a pending hook carrying a bot token cannot be saved:
// storing it unsealed would leak the credential, and there is nothing to
// unseal it with later anyway.
func TestOAuthStateWithPendingBotTokenRequiresSealKey(t *testing.T) {
	s := newTestStore(t)
	seedDeveloper(t, s, "dev_1", "a@x.com")
	st := OAuthState{
		State: "tg2", DeveloperID: "dev_1", Verifier: "v", ExpiresAt: time.Now().Add(time.Minute),
		Webhook: &PendingWebhook{Kind: model.WebhookKindTelegram, BotToken: "123:ABC", ChatID: "-100123"},
	}
	if err := s.SaveOAuthState(st); err == nil {
		t.Fatal("expected an error without a seal key")
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
	all, err := s.ListDeliveries("wh_1", 200, 0)
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
	if got, _ := s.ListDeliveries("wh_1", 200, 0); len(got) != 0 {
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

// The SQLite file holds sealed OAuth tokens and webhook secrets; a mode that
// lets other local users read it defeats sealing-at-rest entirely, so Open
// must always leave it 0600 regardless of the umask that created it.
func TestOpenSetsDatabaseFilePermsTo0600(t *testing.T) {
	path := filepath.Join(t.TempDir(), "perms.db")
	s, err := Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("db file mode = %o, want 0600", perm)
	}
}

// A pre-existing file with looser permissions (e.g. created under a lax
// umask before this hardening landed) must be tightened on the next Open,
// not just on first creation.
func TestOpenTightensExistingLoosePerms(t *testing.T) {
	path := filepath.Join(t.TempDir(), "loose.db")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()

	s, err := Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("db file mode = %o, want 0600", perm)
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

	_, err = Open("sqlite", path)
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
	d, exp, _, err := s.SessionDeveloper("sess1", now)
	if err != nil || d.ID != "dev_1" || !exp.Equal(now.Add(time.Hour)) {
		t.Fatalf("SessionDeveloper = %+v %v %v", d, exp, err)
	}
	if _, _, _, err := s.SessionDeveloper("sess1", now.Add(2*time.Hour)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired session err = %v", err)
	}
	if err := s.ExtendSession("sess1", now.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.SessionDeveloper("sess1", now.Add(2*time.Hour)); err != nil {
		t.Fatalf("extended session rejected: %v", err)
	}
	if err := s.DeleteSession("sess1"); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.SessionDeveloper("sess1", now); !errors.Is(err, ErrNotFound) {
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

// PRAGMA foreign_keys is per-connection, so setting it once in the migration
// only holds while the pool hands out that one connection. Dropping idle
// connections forces a fresh one, which must still have it on — that is only
// true if the DSN carries the pragma.
func TestForeignKeysAreOnForEveryConnection(t *testing.T) {
	s := newTestStore(t)
	if s.DriverName() != "sqlite" {
		t.Skip("foreign_keys is a SQLite per-connection PRAGMA; Postgres enforces foreign keys unconditionally")
	}
	s.db.SetMaxIdleConns(0) // every query now opens a new connection
	for i := 0; i < 3; i++ {
		var on int
		if err := s.db.QueryRow(`PRAGMA foreign_keys`).Scan(&on); err != nil {
			t.Fatal(err)
		}
		if on != 1 {
			t.Fatalf("foreign_keys = %d on a fresh connection, want 1", on)
		}
	}
}

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

func TestReserveIdempotency(t *testing.T) {
	s := newTestStore(t)
	seedDeveloper(t, s, "dev_1", "dev1@example.com")
	seedDeveloper(t, s, "dev_2", "dev2@example.com")

	won, err := s.ReserveIdempotency("dev_1", "k1")
	if err != nil || !won {
		t.Fatalf("first reserve: won=%v err=%v", won, err)
	}
	if won, err := s.ReserveIdempotency("dev_1", "k1"); err != nil || won {
		t.Fatalf("second reserve should lose: won=%v err=%v", won, err)
	}
	// Reservations are developer-scoped: the same key is free for another developer.
	if won, err := s.ReserveIdempotency("dev_2", "k1"); err != nil || !won {
		t.Fatalf("other developer reserve: won=%v err=%v", won, err)
	}

	// The placeholder response is empty (distinguishable from "no key at all"
	// via GetIdempotency's ErrNotFound, and from a completed response, which
	// is never empty JSON).
	b, err := s.GetIdempotency("dev_1", "k1")
	if err != nil || len(b) != 0 {
		t.Fatalf("placeholder response = %q err=%v, want empty", b, err)
	}

	if err := s.PutIdempotency("dev_1", "k1", []byte(`{"ok":true}`)); err != nil {
		t.Fatal(err)
	}
	if b, err := s.GetIdempotency("dev_1", "k1"); err != nil || string(b) != `{"ok":true}` {
		t.Fatalf("get after put = %s %v", b, err)
	}

	if err := s.DeleteIdempotency("dev_1", "k1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetIdempotency("dev_1", "k1"); !errors.Is(err, ErrNotFound) {
		t.Fatal("delete did not remove key")
	}
	// A fresh reservation succeeds again once the old one is gone — this is
	// what lets a retry proceed after an earlier attempt failed.
	if won, err := s.ReserveIdempotency("dev_1", "k1"); err != nil || !won {
		t.Fatalf("reserve after delete: won=%v err=%v", won, err)
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
	s, err := Open("sqlite", path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	a, err := s.GetAnyAccount("acc_old")
	if err != nil || a.Kind != model.AccountKindMail {
		t.Fatalf("migrated account = %+v %v", a, err)
	}
}

// SelfAttendee resolves the account's own attendee row (is_self = 1); an
// account with none is ErrNotFound rather than a zero-value Attendee, since
// callers use it to tag outgoing messages.
func TestSelfAttendee(t *testing.T) {
	s := newTestStore(t)
	acct := seedChatAccount(t, s)
	if _, err := s.SelfAttendee(acct); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no self attendee err = %v", err)
	}
	if err := s.UpsertAttendee(model.Attendee{ID: "self", Phone: "+919888000000", Name: "Me", IsSelf: true}, acct); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertAttendee(model.Attendee{ID: "a1", Name: "Other"}, acct); err != nil {
		t.Fatal(err)
	}
	got, err := s.SelfAttendee(acct)
	if err != nil || got.ID != "self" || !got.IsSelf {
		t.Fatalf("SelfAttendee = %+v %v", got, err)
	}
}

// ApplyReaction must be safe when multiple goroutines merge into the same
// message concurrently: a chat-runtime actor applying an inbound reaction and
// an API handler applying a local one must not race and lose an update. Each
// goroutine here reacts as a distinct attendee, so a correct implementation
// keeps every one of them.
func TestApplyReactionIsAtomicUnderConcurrency(t *testing.T) {
	s := newTestStore(t)
	acct := seedChatAccount(t, s)
	if err := s.UpsertChat(model.Chat{AccountID: acct, ID: "c1", Kind: "direct"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertChatMessage(model.ChatMessage{AccountID: acct, ID: "m1", ChatID: "c1",
		Sender: model.Attendee{ID: "a1"}, Kind: "text", Text: "x", SentAt: time.Now()}); err != nil {
		t.Fatal(err)
	}

	const n = 20
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs <- s.ApplyReaction(acct, "m1", model.Reaction{
				AttendeeID: fmt.Sprintf("a%d", i), Emoji: "👍", At: time.Now(),
			})
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	m, err := s.GetChatMessage(acct, "m1")
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Reactions) != n {
		t.Fatalf("reactions = %d, want %d: %+v", len(m.Reactions), n, m.Reactions)
	}
}

// A duplicate attendee id in the input violates the chat_members primary key
// partway through ReplaceChatMembers' insert loop; the whole replace must
// roll back rather than leave a half-written roster.
func TestReplaceChatMembersIsAllOrNothing(t *testing.T) {
	s := newTestStore(t)
	acct := seedChatAccount(t, s)
	if err := s.UpsertChat(model.Chat{AccountID: acct, ID: "c1", Kind: "group"}); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceChatMembers(acct, "c1", []model.ChatMember{
		{ChatID: "c1", AttendeeID: "a1"}, {ChatID: "c1", AttendeeID: "a2"},
	}); err != nil {
		t.Fatal(err)
	}

	err := s.ReplaceChatMembers(acct, "c1", []model.ChatMember{
		{ChatID: "c1", AttendeeID: "b1"},
		{ChatID: "c1", AttendeeID: "b1"}, // duplicate: violates the primary key
	})
	if err == nil {
		t.Fatal("expected an error from the duplicate attendee id")
	}

	c, err := s.GetChat(acct, "c1")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, m := range c.Members {
		got[m.ID] = true
	}
	if len(got) != 2 || !got["a1"] || !got["a2"] {
		t.Fatalf("previous roster not intact after a failed replace: %+v", c.Members)
	}
}

// A message's sender is an Attendee, not a bare id: spec §4 promises
// sender: Attendee, and §1 exists so that a sender resolves to a person. Every
// read path (list, get, and every chat_* webhook payload built from
// GetChatMessage) went out with name: "" and is_self: false because
// chatMessageSelect never joined attendees.
func TestChatMessageSenderResolvesToAnAttendee(t *testing.T) {
	s := newTestStore(t)
	acct := seedChatAccount(t, s)
	if err := s.UpsertChat(model.Chat{AccountID: acct, ID: "c1", Kind: "group", Name: "Team"}); err != nil {
		t.Fatal(err)
	}
	for _, a := range []model.Attendee{
		{ID: "919888000001@s.whatsapp.net", Phone: "+919888000001", Name: "Ada"},
		{ID: "919888000000@s.whatsapp.net", Phone: "+919888000000", Name: "Me", IsSelf: true},
	} {
		if err := s.UpsertAttendee(a, acct); err != nil {
			t.Fatal(err)
		}
	}
	base := time.Date(2026, 8, 25, 10, 0, 0, 0, time.UTC)
	for _, m := range []model.ChatMessage{
		{AccountID: acct, ID: "m1", ChatID: "c1", Sender: model.Attendee{ID: "919888000001@s.whatsapp.net"},
			Kind: "text", Text: "hi", SentAt: base},
		{AccountID: acct, ID: "m2", ChatID: "c1", Sender: model.Attendee{ID: "919888000000@s.whatsapp.net"},
			IsFromMe: true, Kind: "text", Text: "hello", SentAt: base.Add(time.Minute)},
		// A sender we have no attendee row for yet must still round-trip its id.
		{AccountID: acct, ID: "m3", ChatID: "c1", Sender: model.Attendee{ID: "919888000009@s.whatsapp.net"},
			Kind: "text", Text: "who", SentAt: base.Add(2 * time.Minute)},
	} {
		if _, err := s.UpsertChatMessage(m); err != nil {
			t.Fatal(err)
		}
	}

	got, err := s.GetChatMessage(acct, "m1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Sender.Name != "Ada" || got.Sender.Phone != "+919888000001" || got.Sender.IsSelf {
		t.Fatalf("GetChatMessage sender = %+v", got.Sender)
	}
	self, _ := s.GetChatMessage(acct, "m2")
	if !self.Sender.IsSelf || self.Sender.Name != "Me" {
		t.Fatalf("own message sender = %+v", self.Sender)
	}
	unknown, _ := s.GetChatMessage(acct, "m3")
	if unknown.Sender.ID != "919888000009@s.whatsapp.net" || unknown.Sender.Name != "" {
		t.Fatalf("unrostered sender = %+v", unknown.Sender)
	}

	page, _, err := s.ListChatMessages(acct, "c1", "", 10)
	if err != nil || len(page) != 3 {
		t.Fatalf("list = %v err=%v", ids(page), err)
	}
	for _, m := range page {
		if m.ID == "m1" && m.Sender.Name != "Ada" {
			t.Fatalf("ListChatMessages sender = %+v", m.Sender)
		}
	}
}

func TestDeleteSessionsExceptKeepsOnlyTheGivenHash(t *testing.T) {
	s := newTestStore(t)
	seedDeveloper(t, s, "dev_1", "a@x.com")
	seedDeveloper(t, s, "dev_2", "b@x.com")
	exp := time.Now().Add(time.Hour)
	for _, id := range []string{"keep", "drop1", "drop2"} {
		if err := s.CreateSession(id, "dev_1", exp); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.CreateSession("other_dev", "dev_2", exp); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSessionsExcept("dev_1", "keep"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"drop1", "drop2"} {
		if _, _, _, err := s.SessionDeveloper(id, time.Now()); !errors.Is(err, ErrNotFound) {
			t.Errorf("session %q survived: %v", id, err)
		}
	}
	if _, _, _, err := s.SessionDeveloper("keep", time.Now()); err != nil {
		t.Errorf("kept session was deleted: %v", err)
	}
	// Another developer's sessions are untouched.
	if _, _, _, err := s.SessionDeveloper("other_dev", time.Now()); err != nil {
		t.Errorf("another developer's session was deleted: %v", err)
	}
}

func TestSessionDeveloperReportsCreatedAt(t *testing.T) {
	s := newTestStore(t)
	seedDeveloper(t, s, "dev_1", "a@x.com")
	before := time.Now().Add(-time.Second)
	if err := s.CreateSession("sess1", "dev_1", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	_, _, created, err := s.SessionDeveloper("sess1", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if created.Before(before) || created.After(time.Now().Add(time.Second)) {
		t.Fatalf("created_at = %v, want ~now", created)
	}
}

func TestUpdatePasswordAndDeveloperPasswordHash(t *testing.T) {
	s := newTestStore(t)
	seedDeveloper(t, s, "dev_1", "a@x.com")
	hash, err := s.DeveloperPasswordHash("dev_1")
	if err != nil || hash != "$2a$12$hash" {
		t.Fatalf("DeveloperPasswordHash = %q %v", hash, err)
	}
	if err := s.UpdatePassword("dev_1", "$2a$12$newhash"); err != nil {
		t.Fatal(err)
	}
	if hash, _ := s.DeveloperPasswordHash("dev_1"); hash != "$2a$12$newhash" {
		t.Fatalf("hash after update = %q", hash)
	}
	if _, err := s.DeveloperPasswordHash("dev_nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown developer err = %v", err)
	}
}

// PurgeDeadDeliveries is the only thing standing between an abandoned
// delivery's full message payload and unbounded growth of webhook_deliveries;
// it must only remove rows that are both dead and old, never a live retry.
func TestPurgeDeadDeliveriesRemovesRowsOlderThanCutoff(t *testing.T) {
	s := newTestStore(t)
	seedDeveloper(t, s, "dev_1", "a@x.com")
	if err := s.SaveWebhook(model.Webhook{ID: "wh_1", DeveloperID: "dev_1", URL: "https://x.example.com", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	// Five dead deliveries spanning 10 days: two older than the 7-day cutoff,
	// three within it.
	for i, daysOld := range []int{10, 8, 5, 2, 0} {
		created := now.AddDate(0, 0, -daysOld)
		if err := s.SaveDelivery(Delivery{
			ID: fmt.Sprintf("dl_%d", i), WebhookID: "wh_1", AccountID: "acc_1", EventType: "mail_received",
			Payload: []byte(`{"big":"payload"}`), Attempts: 8, Dead: true,
			NextAttemptAt: created, CreatedAt: created,
		}); err != nil {
			t.Fatal(err)
		}
	}
	// A live (non-dead) delivery, however old, must survive the purge.
	live := now.AddDate(0, 0, -30)
	if err := s.SaveDelivery(Delivery{
		ID: "dl_live", WebhookID: "wh_1", AccountID: "acc_1", EventType: "mail_received",
		Payload: []byte(`{}`), Attempts: 1, Dead: false, NextAttemptAt: live, CreatedAt: live,
	}); err != nil {
		t.Fatal(err)
	}

	n, err := s.PurgeDeadDeliveries(now.Add(-7 * 24 * time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("PurgeDeadDeliveries removed %d rows, want 2 (the 10d and 8d old dead rows)", n)
	}
	all, err := s.ListDeliveries("wh_1", 200, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 4 {
		t.Fatalf("ListDeliveries after purge = %d rows, want 4 (3 recent dead + 1 live)", len(all))
	}
	for _, d := range all {
		if d.ID == "dl_0" || d.ID == "dl_1" {
			t.Fatalf("purge left an old dead row behind: %+v", d)
		}
	}
}

// ListDeliveries paginates: a webhook with many rows returns only the page
// asked for.
func TestListDeliveriesIsPaginated(t *testing.T) {
	s := newTestStore(t)
	seedDeveloper(t, s, "dev_1", "a@x.com")
	if err := s.SaveWebhook(model.Webhook{ID: "wh_1", DeveloperID: "dev_1", URL: "https://x.example.com", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for i := 0; i < 5; i++ {
		created := now.Add(time.Duration(i) * time.Second)
		if err := s.SaveDelivery(Delivery{
			ID: fmt.Sprintf("dl_page_%d", i), WebhookID: "wh_1", AccountID: "acc_1", EventType: "mail_received",
			Payload: []byte(`{}`), Attempts: 1, NextAttemptAt: created, CreatedAt: created,
		}); err != nil {
			t.Fatal(err)
		}
	}
	page, err := s.ListDeliveries("wh_1", 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 2 || page[0].ID != "dl_page_2" || page[1].ID != "dl_page_3" {
		t.Fatalf("page = %+v, want [dl_page_2 dl_page_3]", page)
	}
}

// DeleteAccount must take the account's queued deliveries with it: otherwise
// a full message payload from a deleted tenant sits in webhook_deliveries
// forever, and a stray retry keeps posting on the deleted account's behalf.
func TestDeleteAccountRemovesItsDeliveries(t *testing.T) {
	s := newTestStore(t)
	seedDeveloper(t, s, "dev_1", "a@x.com")
	if err := s.UpsertAccount(model.Account{ID: "acc_1", DeveloperID: "dev_1", Provider: "OUTLOOK", Email: "u@x.com", Status: model.AccountOK}); err != nil {
		t.Fatal(err)
	}
	// Developer-wide hook (no account_id), so DeleteAccount must not drop the
	// hook itself, only the account's own queued deliveries.
	if err := s.SaveWebhook(model.Webhook{ID: "wh_1", DeveloperID: "dev_1", URL: "https://x.example.com", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := s.SaveDelivery(Delivery{
		ID: "dl_acc1", WebhookID: "wh_1", AccountID: "acc_1", EventType: "mail_received",
		Payload: []byte(`{}`), Attempts: 1, NextAttemptAt: now, CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	if err := s.DeleteAccount("acc_1"); err != nil {
		t.Fatal(err)
	}

	all, err := s.ListDeliveries("wh_1", 200, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range all {
		if d.AccountID == "acc_1" {
			t.Fatalf("delivery for deleted account survived: %+v", d)
		}
	}
	// The developer-wide hook itself is untouched: it belongs to the
	// developer, not the deleted account.
	if _, err := s.GetWebhook("dev_1", "wh_1"); err != nil {
		t.Fatalf("developer-wide hook was removed: %v", err)
	}
}

// LIKE search inputs must be escaped: a literal "%" or "_" in a caller's
// query should match itself, not act as a wildcard over every row.
func TestSearchEscapesLikeWildcards(t *testing.T) {
	s := newTestStore(t)
	acct := seedAccount(t, s)
	now := time.Now()
	for _, e := range []model.Email{
		{AccountID: acct, ID: "m1", Subject: "50% off", Date: now},
		{AccountID: acct, ID: "m2", Subject: "500 off", Date: now},
		{AccountID: acct, ID: "m3", Subject: "foo_bar", Date: now},
		{AccountID: acct, ID: "m4", Subject: "fooxbar", Date: now},
	} {
		if err := s.UpsertEmail(e); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.ListEmails(EmailQuery{AccountID: acct, Search: "50%"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "m1" {
		t.Fatalf("ListEmails(q=\"50%%\") = %+v, want only m1", got)
	}

	// "_" is LIKE's single-character wildcard; escaped, it must match only
	// the literal underscore, not "fooxbar" (where "_" would otherwise match
	// the "x").
	got, err = s.ListEmails(EmailQuery{AccountID: acct, Search: "foo_bar"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "m3" {
		t.Fatalf("ListEmails(q=\"foo_bar\") = %+v, want only m3", got)
	}
}

// M-9: the browser_hash migration added a column but left the sessions table
// alone, so rows written before sessions were hashed still held the token
// itself as the primary key — inert (a lookup hashes first, and
// sha256(tok) != tok) but exactly the "a DB read yields every live session"
// artefact the hashing was meant to remove, sitting in every backup taken
// before the cut-over. The one-off delete is keyed on length rather than
// truncating the table, so live hashed sessions survive the upgrade.
func TestMigrationDropsPreHashSessionRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.db")
	s, err := Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateDeveloper(model.Developer{ID: "dev_1", Email: "a@x.com"}, "hash"); err != nil {
		t.Fatal(err)
	}
	exp := time.Now().Add(time.Hour)
	// A hashed id is 64 hex characters; a raw token is 43 (32 bytes of
	// unpadded base64url), which is what the pre-hash rows hold.
	const hashed = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	const plaintext = "0123456789012345678901234567890123456789012"
	for _, id := range []string{hashed, plaintext} {
		if err := s.CreateSession(id, "dev_1", exp); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s, err = Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	var got []string
	rows, err := s.DB().Query(`SELECT id FROM sessions ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		got = append(got, id)
	}
	if len(got) != 1 || got[0] != hashed {
		t.Fatalf("sessions after reopen = %v, want only the hashed row", got)
	}
}

// stored_at is ingestion time, not the provider's timestamp: a backfill of an
// old mailbox must not look ancient to the retention sweep.
func TestUpsertEmailStampsStoredAtOnInsertOnly(t *testing.T) {
	s := newTestStore(t)
	acct := seedAccount(t, s)
	old := time.Now().Add(-3 * 365 * 24 * time.Hour).UTC().Truncate(time.Second)

	if err := s.UpsertEmail(model.Email{
		AccountID: acct, ID: "m1", Subject: "hello", Body: "body", Date: old,
	}); err != nil {
		t.Fatal(err)
	}

	var first int64
	if err := s.DB().QueryRow(s.Q(`SELECT stored_at FROM emails WHERE account_id = ? AND id = ?`), acct, "m1").Scan(&first); err != nil {
		t.Fatal(err)
	}
	if first < time.Now().Add(-time.Minute).Unix() {
		t.Fatalf("stored_at = %d, want a recent timestamp, not the message date %d", first, old.Unix())
	}

	// A later update must not restamp it, or nothing would ever age out.
	if err := s.UpsertEmail(model.Email{
		AccountID: acct, ID: "m1", Subject: "hello again", Body: "body", Date: old,
	}); err != nil {
		t.Fatal(err)
	}
	var second int64
	if err := s.DB().QueryRow(s.Q(`SELECT stored_at FROM emails WHERE account_id = ? AND id = ?`), acct, "m1").Scan(&second); err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Fatalf("stored_at changed on update: %d -> %d", first, second)
	}
}

func TestUpsertChatMessageStampsStoredAt(t *testing.T) {
	s := newTestStore(t)
	acct := seedChatAccount(t, s)
	if err := s.UpsertChat(model.Chat{AccountID: acct, ID: "c1", Kind: "dm"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertChatMessage(model.ChatMessage{
		AccountID: acct, ID: "cm1", ChatID: "c1", Kind: "text", Text: "hi",
		SentAt: time.Now().Add(-48 * time.Hour).UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	var storedAt int64
	if err := s.DB().QueryRow(s.Q(`SELECT stored_at FROM chat_messages WHERE account_id = ? AND id = ?`), acct, "cm1").Scan(&storedAt); err != nil {
		t.Fatal(err)
	}
	if storedAt < time.Now().Add(-time.Minute).Unix() {
		t.Fatalf("stored_at = %d, want a recent timestamp", storedAt)
	}
}

func TestDevelopersHaveRetentionColumnDefaultingToZero(t *testing.T) {
	s := newTestStore(t)
	dev := seedDeveloper(t, s, "dev_1", "dev1@example.com")
	var secs int64
	if err := s.DB().QueryRow(s.Q(`SELECT retention_max_age_secs FROM developers WHERE id = ?`), dev).Scan(&secs); err != nil {
		t.Fatal(err)
	}
	if secs != 0 {
		t.Fatalf("retention_max_age_secs = %d, want 0 (retention off by default)", secs)
	}
}

// Eviction blanks content and participants but keeps every column sync depends
// on. EmailExists in particular is the only thing stopping a resync from
// re-firing mail_received for the whole mailbox.
func TestEvictEmailContentBlanksContentAndKeepsEnvelope(t *testing.T) {
	s := newTestStore(t)
	acct := seedAccount(t, s)
	if err := s.UpsertEmail(model.Email{
		AccountID: acct, ID: "m1", ThreadID: "t1", FolderID: "f1",
		Subject: "quarterly numbers", Snippet: "here they are",
		From: model.Recipient{Name: "Alice", Email: "alice@example.com"},
		To:   []model.Recipient{{Name: "Bob", Email: "bob@example.com"}},
		Body: "<p>secret</p>", BodyType: "html", Date: time.Now().UTC(),
		Read: true, HasAttachments: true, InternetMessageID: "<abc@example.com>",
		Attachments: []model.Attachment{{ID: "a1", Name: "payroll.xlsx"}},
	}); err != nil {
		t.Fatal(err)
	}

	if err := s.EvictEmailContent(acct, "m1", time.Now()); err != nil {
		t.Fatal(err)
	}

	got, err := s.GetEmail(acct, "m1")
	if err != nil {
		t.Fatal(err)
	}
	for name, v := range map[string]string{
		"subject": got.Subject, "snippet": got.Snippet, "body": got.Body,
		"body_type": got.BodyType, "from.name": got.From.Name, "from.email": got.From.Email,
	} {
		if v != "" {
			t.Errorf("%s = %q after eviction, want empty", name, v)
		}
	}
	if len(got.To) != 0 || len(got.Attachments) != 0 {
		t.Errorf("to=%v attachments=%v after eviction, want both empty", got.To, got.Attachments)
	}
	if got.ThreadID != "t1" || got.FolderID != "f1" || got.InternetMessageID != "<abc@example.com>" {
		t.Errorf("envelope lost: thread=%q folder=%q imid=%q", got.ThreadID, got.FolderID, got.InternetMessageID)
	}
	if !got.Read || !got.HasAttachments {
		t.Errorf("flags lost: read=%v has_attachments=%v", got.Read, got.HasAttachments)
	}
	// The invariant that keeps a resync from replaying the mailbox.
	if ok, err := s.EmailExists(acct, "m1"); err != nil || !ok {
		t.Fatalf("EmailExists = %v, %v after eviction; want true (a resync would re-fire mail_received)", ok, err)
	}
}

func TestEvictEmailContentIsIdempotent(t *testing.T) {
	s := newTestStore(t)
	acct := seedAccount(t, s)
	if err := s.UpsertEmail(model.Email{AccountID: acct, ID: "m1", Subject: "x", Body: "y", Date: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	first := time.Now().Add(-time.Hour).UTC()
	if err := s.EvictEmailContent(acct, "m1", first); err != nil {
		t.Fatal(err)
	}
	if err := s.EvictEmailContent(acct, "m1", time.Now()); err != nil {
		t.Fatal(err)
	}
	var at int64
	if err := s.DB().QueryRow(s.Q(`SELECT content_evicted_at FROM emails WHERE account_id = ? AND id = ?`), acct, "m1").Scan(&at); err != nil {
		t.Fatal(err)
	}
	if at != first.Unix() {
		t.Fatalf("content_evicted_at = %d, want the first eviction %d", at, first.Unix())
	}
}

// The guard that matters most: sync runs unattended, for every message,
// forever. Without it a resync refills every evicted row.
func TestUpsertEmailDoesNotRefillEvictedRow(t *testing.T) {
	s := newTestStore(t)
	acct := seedAccount(t, s)
	full := model.Email{
		AccountID: acct, ID: "m1", Subject: "quarterly numbers",
		From:    model.Recipient{Email: "alice@example.com"},
		To:      []model.Recipient{{Email: "bob@example.com"}},
		Snippet: "here they are", Body: "<p>secret</p>", BodyType: "html",
		Date: time.Now().UTC(), Attachments: []model.Attachment{{ID: "a1", Name: "payroll.xlsx"}},
	}
	if err := s.UpsertEmail(full); err != nil {
		t.Fatal(err)
	}
	if err := s.EvictEmailContent(acct, "m1", time.Now()); err != nil {
		t.Fatal(err)
	}

	// A resync hands us the whole message again.
	full.Read = true
	if err := s.UpsertEmail(full); err != nil {
		t.Fatal(err)
	}

	got, err := s.GetEmail(acct, "m1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Body != "" || got.Subject != "" || got.From.Email != "" || len(got.Attachments) != 0 {
		t.Fatalf("resync refilled an evicted row: subject=%q body=%q from=%q atts=%d",
			got.Subject, got.Body, got.From.Email, len(got.Attachments))
	}
	// Flags are not content and must still track the provider.
	if !got.Read {
		t.Error("read flag did not update on an evicted row; flags are not content")
	}
}

func TestEvictChatMessageContentBlanksTextOnly(t *testing.T) {
	s := newTestStore(t)
	acct := seedChatAccount(t, s)
	if err := s.UpsertChat(model.Chat{AccountID: acct, ID: "c1", Kind: "dm"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertChatMessage(model.ChatMessage{
		AccountID: acct, ID: "cm1", ChatID: "c1", Kind: "text",
		Text: "meet me at six", SentAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.EvictChatMessageContent(acct, "cm1", time.Now()); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetChatMessage(acct, "cm1")
	if err != nil {
		t.Fatalf("GetChatMessage after eviction: %v (a late reaction must still resolve)", err)
	}
	if got.Text != "" {
		t.Errorf("text = %q after eviction, want empty", got.Text)
	}
	if got.ChatID != "c1" || got.Kind != "text" {
		t.Errorf("envelope lost: chat=%q kind=%q", got.ChatID, got.Kind)
	}
}

func TestGetEmailReportsContentEvicted(t *testing.T) {
	s := newTestStore(t)
	acct := seedAccount(t, s)
	if err := s.UpsertEmail(model.Email{AccountID: acct, ID: "m1", Subject: "x", Body: "y", Date: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetEmail(acct, "m1")
	if err != nil {
		t.Fatal(err)
	}
	if got.ContentEvicted {
		t.Error("ContentEvicted = true on a fresh message, want false")
	}
	if err := s.EvictEmailContent(acct, "m1", time.Now()); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetEmail(acct, "m1")
	if err != nil {
		t.Fatal(err)
	}
	if !got.ContentEvicted {
		t.Error("ContentEvicted = false after eviction, want true")
	}
}

// The list form needs the flag too: list responses already omit the body, so
// without it a client cannot tell "not included here" from "destroyed".
func TestListEmailsReportsContentEvicted(t *testing.T) {
	s := newTestStore(t)
	acct := seedAccount(t, s)
	if err := s.UpsertEmail(model.Email{AccountID: acct, ID: "m1", Subject: "x", Body: "y", Date: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if err := s.EvictEmailContent(acct, "m1", time.Now()); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListEmails(EmailQuery{AccountID: acct})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !got[0].ContentEvicted {
		t.Fatalf("ListEmails = %+v, want one row with ContentEvicted true", got)
	}
}

// Eviction blanks subject, snippet and from_email — the three columns
// ListEmails searches (store.go:537) — so an evicted message can never be a
// search hit again. That is correct (there is nothing left to match) but
// surprising, so it is pinned here rather than discovered in production.
func TestSearchDoesNotMatchEvictedMail(t *testing.T) {
	s := newTestStore(t)
	acct := seedAccount(t, s)
	if err := s.UpsertEmail(model.Email{
		AccountID: acct, ID: "m1", Subject: "quarterly numbers",
		Snippet: "here they are", From: model.Recipient{Email: "alice@example.com"},
		Body: "<p>secret</p>", Date: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListEmails(EmailQuery{AccountID: acct, Search: "quarterly"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("baseline search returned %d rows, want 1", len(got))
	}

	if err := s.EvictEmailContent(acct, "m1", time.Now()); err != nil {
		t.Fatal(err)
	}
	got, err = s.ListEmails(EmailQuery{AccountID: acct, Search: "quarterly"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("search returned %d rows for an evicted message, want 0", len(got))
	}
}

func TestGetChatMessageReportsContentEvicted(t *testing.T) {
	s := newTestStore(t)
	acct := seedChatAccount(t, s)
	if err := s.UpsertChat(model.Chat{AccountID: acct, ID: "c1", Kind: "dm"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertChatMessage(model.ChatMessage{
		AccountID: acct, ID: "cm1", ChatID: "c1", Kind: "text", Text: "hi", SentAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.EvictChatMessageContent(acct, "cm1", time.Now()); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetChatMessage(acct, "cm1")
	if err != nil {
		t.Fatal(err)
	}
	if !got.ContentEvicted {
		t.Error("ContentEvicted = false after eviction, want true")
	}
}
