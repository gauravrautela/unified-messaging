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
	return New(db, log, 30*24*time.Hour), db, recs
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
	if err := db.ExtendSession(tok, aged); err != nil {
		t.Fatal(err)
	}
	_, slidExp, err := svc.SessionDeveloper(ctx, tok)
	if err != nil {
		t.Fatal(err)
	}
	if !slidExp.After(aged) {
		t.Fatalf("returned expiry %v did not move forward from %v", slidExp, aged)
	}
	if _, newExp, _ := db.SessionDeveloper(tok, time.Now()); !newExp.After(exp.Add(-24 * time.Hour)) {
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
	if _, _, err := svc.KeyDeveloper(ctx, full[:42]+"X"); !errors.Is(err, ErrInvalidCredentials) {
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
