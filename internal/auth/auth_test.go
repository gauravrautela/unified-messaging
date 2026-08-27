// internal/auth/auth_test.go
package auth

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gauravrautela/unified-messaging/internal/logx"
	"github.com/gauravrautela/unified-messaging/internal/store"
)

func newService(t *testing.T) (*Service, *store.Store, *logx.Records) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	log, recs := logx.Capture()
	return New(db, log, 30*24*time.Hour, 90*24*time.Hour), db, recs
}

func TestSignupThenLogin(t *testing.T) {
	svc, _, recs := newService(t)
	ctx := context.Background()
	d, err := svc.Signup(ctx, " Dev@Example.com ", "correct horse battery", "Dev")
	if err != nil {
		t.Fatal(err)
	}
	if d.Email != "dev@example.com" || !strings.HasPrefix(d.ID, "dev_") {
		t.Fatalf("developer = %+v", d)
	}
	got, err := svc.Login(ctx, "dev@example.com", "correct horse battery")
	if err != nil || got.ID != d.ID {
		t.Fatalf("login = %+v %v", got, err)
	}
	if recs.Contains("correct horse battery") {
		t.Fatal("password appeared in logs")
	}
}

func TestLoginFailuresAreUniform(t *testing.T) {
	svc, _, _ := newService(t)
	ctx := context.Background()
	if _, err := svc.Signup(ctx, "a@x.com", "longenoughpassword", ""); err != nil {
		t.Fatal(err)
	}
	_, errWrong := svc.Login(ctx, "a@x.com", "wrongpassword!")
	_, errUnknown := svc.Login(ctx, "nobody@x.com", "wrongpassword!")
	if !errors.Is(errWrong, ErrInvalidCredentials) || !errors.Is(errUnknown, ErrInvalidCredentials) {
		t.Fatalf("errors = %v / %v, want both ErrInvalidCredentials", errWrong, errUnknown)
	}
	if errWrong.Error() != errUnknown.Error() {
		t.Fatal("error text differs between unknown email and wrong password")
	}
}

func TestSignupValidation(t *testing.T) {
	svc, _, _ := newService(t)
	ctx := context.Background()
	if _, err := svc.Signup(ctx, "not-an-email", "longenoughpassword", ""); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("bad email err = %v", err)
	}
	if _, err := svc.Signup(ctx, "a@x.com", "short", ""); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("short password err = %v", err)
	}
	if _, err := svc.Signup(ctx, "a@x.com", "longenoughpassword", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Signup(ctx, "A@X.COM", "longenoughpassword", ""); !errors.Is(err, ErrEmailTaken) {
		t.Fatalf("duplicate email err = %v", err)
	}
}

func TestSessionsResolveExpireAndSlide(t *testing.T) {
	svc, db, recs := newService(t)
	ctx := context.Background()
	d, _ := svc.Signup(ctx, "a@x.com", "longenoughpassword", "")
	tok, exp, err := svc.NewSession(ctx, d.ID)
	if err != nil || tok == "" || exp.Before(time.Now().Add(29*24*time.Hour)) {
		t.Fatalf("NewSession = %q %v %v", tok, exp, err)
	}
	got, gotExp, err := svc.SessionDeveloper(ctx, tok)
	if err != nil || got.ID != d.ID {
		t.Fatalf("SessionDeveloper = %+v %v", got, err)
	}
	// Expiry is stored as a Unix second, so compare at that resolution.
	if !gotExp.Equal(exp.Truncate(time.Second)) {
		t.Fatalf("fresh session expiry = %v, want the stored %v", gotExp, exp)
	}
	if recs.Contains(tok) {
		t.Fatal("session token appeared in logs")
	}
	// Age the session by two days; the next use must push expiry forward, and
	// must report the new expiry so the caller can re-issue the cookie.
	aged := exp.Add(-2 * 24 * time.Hour)
	if err := db.ExtendSession(HashKey(tok), aged); err != nil {
		t.Fatal(err)
	}
	_, slidExp, err := svc.SessionDeveloper(ctx, tok)
	if err != nil {
		t.Fatal(err)
	}
	if !slidExp.After(aged) {
		t.Fatalf("returned expiry %v did not move forward from %v", slidExp, aged)
	}
	if _, newExp, _, _ := db.SessionDeveloper(HashKey(tok), time.Now()); !newExp.After(exp.Add(-24 * time.Hour)) {
		t.Fatalf("expiry not slid: %v", newExp)
	} else if !slidExp.Truncate(time.Second).Equal(newExp) {
		t.Fatalf("returned expiry %v does not match the stored %v", slidExp, newExp)
	}
	if err := svc.DeleteSession(ctx, tok); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.SessionDeveloper(ctx, tok); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("deleted session err = %v", err)
	}
	if _, _, err := svc.SessionDeveloper(ctx, "garbage"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("garbage session err = %v", err)
	}
}

func TestAPIKeysLifecycle(t *testing.T) {
	svc, db, recs := newService(t)
	ctx := context.Background()
	d, _ := svc.Signup(ctx, "a@x.com", "longenoughpassword", "")
	full, k, err := svc.NewAPIKey(ctx, d.ID, "prod")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(full, "um_") || len(full) != 43 {
		t.Fatalf("key = %q, want um_ + 40 chars", full)
	}
	if k.Prefix != full[:12] || k.Name != "prod" {
		t.Fatalf("APIKey = %+v", k)
	}
	if recs.Contains(full) || recs.Contains(HashKey(full)) {
		t.Fatal("full key or its hash appeared in logs")
	}
	if _, _, err := db.DeveloperByKeyHash(full); err == nil {
		t.Fatal("key stored in plaintext")
	}
	got, gk, err := svc.KeyDeveloper(ctx, full)
	if err != nil || got.ID != d.ID || gk.ID != k.ID {
		t.Fatalf("KeyDeveloper = %+v %+v %v", got, gk, err)
	}
	// Flip the last character to something it is not, so the "tampered" key is
	// never accidentally the real one.
	tampered := full[:42] + "X"
	if strings.HasSuffix(full, "X") {
		tampered = full[:42] + "Y"
	}
	if _, _, err := svc.KeyDeveloper(ctx, tampered); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("tampered key err = %v", err)
	}
	keys, _ := db.ListAPIKeys(d.ID)
	if len(keys) != 1 || keys[0].LastUsedAt == nil {
		t.Fatalf("last_used_at not touched: %+v", keys)
	}
	if err := svc.RevokeKey(ctx, d.ID, k.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.KeyDeveloper(ctx, full); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("revoked key err = %v", err)
	}
	if err := svc.RevokeKey(ctx, "dev_other", k.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cross-developer revoke err = %v", err)
	}
}

// A session token is a bearer credential: the sessions table must hold only
// its hash, so a database read cannot be replayed as a login.
func TestSessionTokenIsStoredHashed(t *testing.T) {
	svc, db, _ := newService(t)
	ctx := context.Background()
	d, _ := svc.Signup(ctx, "a@x.com", "longenoughpassword", "")
	tok, _, err := svc.NewSession(ctx, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := db.SessionDeveloper(tok, time.Now()); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("the raw token resolves in the store: it is not hashed at rest")
	}
	if _, _, _, err := db.SessionDeveloper(HashKey(tok), time.Now()); err != nil {
		t.Fatalf("hashed row missing: %v", err)
	}
	if _, _, err := svc.SessionDeveloper(ctx, tok); err != nil {
		t.Fatalf("the token itself must still resolve: %v", err)
	}
}

// Sliding expiry alone lets a stolen cookie live forever, so a session also
// dies a fixed time after it was created, however active it has been.
func TestSessionAbsoluteMaxAge(t *testing.T) {
	svc, db, _ := newService(t)
	ctx := context.Background()
	d, _ := svc.Signup(ctx, "a@x.com", "longenoughpassword", "")
	tok, _, _ := svc.NewSession(ctx, d.ID)
	// Age the row past the absolute limit while keeping it unexpired by the
	// sliding rule.
	if _, err := db.DB().Exec(`UPDATE sessions SET created_at = ? WHERE id = ?`,
		time.Now().Add(-91*24*time.Hour).Unix(), HashKey(tok)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.SessionDeveloper(ctx, tok); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("session older than the max age must be rejected, got %v", err)
	}
	// And it is gone, not merely refused.
	if _, _, _, err := db.SessionDeveloper(HashKey(tok), time.Now()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the over-age row was not deleted: %v", err)
	}
}

func TestChangePassword(t *testing.T) {
	svc, _, recs := newService(t)
	ctx := context.Background()
	d, _ := svc.Signup(ctx, "a@x.com", "longenoughpassword", "")

	if err := svc.ChangePassword(ctx, d.ID, "longenoughpassword", "short"); !errors.Is(err, ErrWeakPassword) {
		t.Fatalf("weak new password err = %v", err)
	}
	if err := svc.ChangePassword(ctx, d.ID, "not the password", "another strong one"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("wrong current password err = %v", err)
	}
	if err := svc.ChangePassword(ctx, d.ID, "longenoughpassword", "another strong one"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Login(ctx, "a@x.com", "another strong one"); err != nil {
		t.Fatalf("new password does not work: %v", err)
	}
	if _, err := svc.Login(ctx, "a@x.com", "longenoughpassword"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatal("the old password still works")
	}
	if recs.Contains("longenoughpassword") || recs.Contains("another strong one") {
		t.Fatal("a password appeared in logs")
	}
}

func TestDeleteOtherSessionsKeepsTheCurrentOne(t *testing.T) {
	svc, _, _ := newService(t)
	ctx := context.Background()
	d, _ := svc.Signup(ctx, "a@x.com", "longenoughpassword", "")
	keep, _, _ := svc.NewSession(ctx, d.ID)
	gone, _, _ := svc.NewSession(ctx, d.ID)
	if err := svc.DeleteOtherSessions(ctx, d.ID, keep); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.SessionDeveloper(ctx, gone); err == nil {
		t.Fatal("the other session survived")
	}
	if _, _, err := svc.SessionDeveloper(ctx, keep); err != nil {
		t.Fatalf("the current session was signed out too: %v", err)
	}
}
