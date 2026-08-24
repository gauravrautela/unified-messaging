package store

import (
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
