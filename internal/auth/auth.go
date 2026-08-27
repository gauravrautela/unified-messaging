// Package auth owns developer identity: password hashing, browser sessions,
// and API keys. It is the only package that imports bcrypt or knows the key
// format, so the rest of the service handles opaque tokens at most.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/gauravrautela/unified-messaging/internal/accounts"
	"github.com/gauravrautela/unified-messaging/internal/logx"
	"github.com/gauravrautela/unified-messaging/internal/model"
	"github.com/gauravrautela/unified-messaging/internal/store"
)

var (
	ErrInvalidInput       = errors.New("invalid email or password format")
	ErrEmailTaken         = errors.New("could not create account")
	ErrInvalidCredentials = errors.New("invalid email or password")
	ErrWeakPassword       = errors.New("password must be at least 10 characters")

	// ErrNoSession is what a cookie that resolves to nothing usable returns:
	// unknown, expired, or past the absolute lifetime. It wraps
	// ErrInvalidCredentials so every existing caller — all of which turn a
	// bad session into "sign in again" — keeps working unchanged.
	ErrNoSession = fmt.Errorf("no live session: %w", ErrInvalidCredentials)
)

const (
	bcryptCost  = 12
	minPassword = 10
	keyPrefix   = "um_"
	keyRandom   = 40
	keyAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	prefixLen   = 12
	touchEvery  = time.Minute
	slideAfter  = 24 * time.Hour

	// defaultSessionMaxAge backstops a caller that passes a non-positive
	// absolute lifetime. Failing open (no cap at all) would be a silent
	// security downgrade, and failing closed would log everyone out on a
	// zero-value config, so New clamps to the same 90 days config defaults to.
	defaultSessionMaxAge = 90 * 24 * time.Hour
)

// dummyHash is compared when the email is unknown so both login failure
// paths cost one bcrypt verification and cannot be told apart by timing.
var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("dummy-password-for-timing"), bcryptCost)

type Service struct {
	store      *store.Store
	log        *slog.Logger
	sessionTTL time.Duration
	maxAge     time.Duration

	mu          sync.Mutex
	lastTouched map[string]time.Time // key id -> last last_used_at write
}

// New builds the service. sessionTTL is the idle window a session slides
// within; maxAge is the absolute lifetime past which it dies however active
// it has been, so a stolen cookie cannot be renewed forever.
func New(s *store.Store, log *slog.Logger, sessionTTL, maxAge time.Duration) *Service {
	if maxAge <= 0 {
		maxAge = defaultSessionMaxAge
	}
	return &Service{store: s, log: log.With("component", "auth"), sessionTTL: sessionTTL,
		maxAge: maxAge, lastTouched: map[string]time.Time{}}
}

// logger prefers a per-request logger attached to ctx (via logx.With), but
// falls back to the Service's own logger rather than slog.Default() when the
// context carries none. logx.From returns slog.Default() in that case, and
// slog.Default() is process-global state a caller could swap out from under
// us (or that tests never touch), so treating it as "no ctx logger" and
// using a.log keeps every log line attributable to this Service.
func (a *Service) logger(ctx context.Context) *slog.Logger {
	if l := logx.From(ctx); l != slog.Default() {
		return l.With("component", "auth")
	}
	return a.log
}

// ---- developers ----

func (a *Service) Signup(ctx context.Context, email, password, name string) (model.Developer, error) {
	log := a.logger(ctx)
	email = strings.ToLower(strings.TrimSpace(email))
	at := strings.Index(email, "@")
	if at < 1 || !strings.Contains(email[at:], ".") {
		log.Debug("signup rejected", "reason", "malformed email", "email", email)
		return model.Developer{}, ErrInvalidInput
	}
	if len(password) < minPassword {
		log.Debug("signup rejected", "reason", "password too short", "email", email, "len", len(password))
		return model.Developer{}, ErrInvalidInput
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	if err != nil {
		return model.Developer{}, err
	}
	id, err := accounts.NewID("dev")
	if err != nil {
		return model.Developer{}, err
	}
	d := model.Developer{ID: id, Email: email, Name: strings.TrimSpace(name), CreatedAt: time.Now().UTC()}
	if err := a.store.CreateDeveloper(d, string(hash)); err != nil {
		if errors.Is(err, store.ErrConflict) {
			log.Debug("signup rejected", "reason", "email taken", "email", email)
			return model.Developer{}, ErrEmailTaken
		}
		return model.Developer{}, err
	}
	log.Info("developer signed up", "developer_id", d.ID, "email", d.Email)
	return d, nil
}

func (a *Service) Login(ctx context.Context, email, password string) (model.Developer, error) {
	log := a.logger(ctx)
	email = strings.ToLower(strings.TrimSpace(email))
	start := time.Now()
	d, hash, err := a.store.DeveloperByEmail(email)
	if errors.Is(err, store.ErrNotFound) {
		// Burn the same bcrypt cost as a real comparison.
		_ = bcrypt.CompareHashAndPassword(dummyHash, []byte(password))
		log.Debug("login failed", "reason", "unknown email", "email", email, "bcrypt_ms", time.Since(start).Milliseconds())
		return model.Developer{}, ErrInvalidCredentials
	}
	if err != nil {
		return model.Developer{}, err
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)); err != nil {
		log.Debug("login failed", "reason", "wrong password", "developer_id", d.ID, "bcrypt_ms", time.Since(start).Milliseconds())
		return model.Developer{}, ErrInvalidCredentials
	}
	log.Info("developer logged in", "developer_id", d.ID, "bcrypt_ms", time.Since(start).Milliseconds())
	return d, nil
}

// ---- sessions ----

func (a *Service) NewSession(ctx context.Context, developerID string) (string, time.Time, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", time.Time{}, err
	}
	tok := base64.RawURLEncoding.EncodeToString(b)
	exp := time.Now().UTC().Add(a.sessionTTL)
	// Only the hash is stored: the sessions table then holds nothing that can
	// be replayed as a cookie by whoever reads it.
	if err := a.store.CreateSession(HashKey(tok), developerID, exp); err != nil {
		return "", time.Time{}, err
	}
	a.logger(ctx).Info("session created", "developer_id", developerID, "expires_at", exp)
	return tok, exp, nil
}

// SessionDeveloper resolves a cookie value. Expiry slides forward once more
// than slideAfter of the TTL has been consumed, so an active developer is
// never logged out mid-work.
//
// It returns the expiry the session now carries — the extended one when it
// just slid — because sliding the database row alone is invisible to the
// browser: the cookie's own Expires is fixed at login, so a caller has to
// re-issue the cookie with this value for the slide to mean anything.
func (a *Service) SessionDeveloper(ctx context.Context, token string) (model.Developer, time.Time, error) {
	log := a.logger(ctx)
	if token == "" {
		return model.Developer{}, time.Time{}, ErrNoSession
	}
	now := time.Now().UTC()
	hash := HashKey(token)
	d, exp, created, err := a.store.SessionDeveloper(hash, now)
	if errors.Is(err, store.ErrNotFound) {
		log.Debug("session lookup", "result", "miss or expired")
		return model.Developer{}, time.Time{}, ErrNoSession
	}
	if err != nil {
		return model.Developer{}, time.Time{}, err
	}
	// The absolute lifetime is measured from creation, which sliding cannot
	// move: without it an attacker who takes a cookie keeps it alive forever
	// simply by using it.
	if now.Sub(created) > a.maxAge {
		log.Info("session rejected", "reason", "past absolute max age",
			"developer_id", d.ID, "age", now.Sub(created).Round(time.Hour), "max_age", a.maxAge)
		if err := a.store.DeleteSession(hash); err != nil {
			log.Warn("deleting over-age session", "developer_id", d.ID, "err", err)
		}
		return model.Developer{}, time.Time{}, ErrNoSession
	}
	if exp.Sub(now) < a.sessionTTL-slideAfter {
		newExp := now.Add(a.sessionTTL)
		if err := a.store.ExtendSession(hash, newExp); err != nil {
			log.Warn("extending session", "developer_id", d.ID, "err", err)
		} else {
			exp = newExp
			log.Debug("session extended", "developer_id", d.ID, "expires_at", newExp)
		}
	}
	log.Debug("session lookup", "result", "hit", "developer_id", d.ID)
	return d, exp, nil
}

func (a *Service) DeleteSession(ctx context.Context, token string) error {
	a.logger(ctx).Info("session deleted")
	return a.store.DeleteSession(HashKey(token))
}

// DeleteOtherSessions signs a developer out of every browser but the one
// holding keepToken. It is the "and everywhere else" half of a password
// change: without it, whoever knew the old password keeps their cookie.
func (a *Service) DeleteOtherSessions(ctx context.Context, developerID, keepToken string) error {
	if err := a.store.DeleteSessionsExcept(developerID, HashKey(keepToken)); err != nil {
		return err
	}
	a.logger(ctx).Info("other sessions signed out", "developer_id", developerID)
	return nil
}

// ChangePassword re-hashes after verifying the current password. It does not
// touch sessions: the caller decides which one to keep (see
// DeleteOtherSessions), because only the caller knows which browser is asking.
func (a *Service) ChangePassword(ctx context.Context, developerID, current, next string) error {
	log := a.logger(ctx)
	if len(next) < minPassword {
		log.Debug("password change rejected", "reason", "too short", "developer_id", developerID, "len", len(next))
		return ErrWeakPassword
	}
	hash, err := a.store.DeveloperPasswordHash(developerID)
	if err != nil {
		return err
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(current)) != nil {
		log.Debug("password change rejected", "reason", "wrong current password", "developer_id", developerID)
		return ErrInvalidCredentials
	}
	newHash, err := bcrypt.GenerateFromPassword([]byte(next), bcryptCost)
	if err != nil {
		return err
	}
	if err := a.store.UpdatePassword(developerID, string(newHash)); err != nil {
		return err
	}
	log.Info("password changed", "developer_id", developerID)
	return nil
}

// ---- API keys ----

// HashKey is how keys are stored and looked up. Keys carry ~238 bits of
// entropy, so a fast hash is sufficient and keeps per-request cost trivial.
func HashKey(full string) string {
	sum := sha256.Sum256([]byte(full))
	return hex.EncodeToString(sum[:])
}

func (a *Service) NewAPIKey(ctx context.Context, developerID, name string) (string, model.APIKey, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", model.APIKey{}, ErrInvalidInput
	}
	full, err := randomKey()
	if err != nil {
		return "", model.APIKey{}, err
	}
	id, err := accounts.NewID("key")
	if err != nil {
		return "", model.APIKey{}, err
	}
	k := model.APIKey{ID: id, Name: name, Prefix: full[:prefixLen], CreatedAt: time.Now().UTC()}
	if err := a.store.CreateAPIKey(k, developerID, HashKey(full)); err != nil {
		return "", model.APIKey{}, err
	}
	a.logger(ctx).Info("api key created", "developer_id", developerID, "key_id", k.ID, "name", k.Name, "prefix", k.Prefix)
	return full, k, nil
}

func randomKey() (string, error) {
	b := make([]byte, keyRandom)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	out := make([]byte, keyRandom)
	for i, v := range b {
		out[i] = keyAlphabet[int(v)%len(keyAlphabet)]
	}
	return keyPrefix + string(out), nil
}

func (a *Service) KeyDeveloper(ctx context.Context, full string) (model.Developer, model.APIKey, error) {
	log := a.logger(ctx)
	if !strings.HasPrefix(full, keyPrefix) || len(full) != len(keyPrefix)+keyRandom {
		log.Debug("api key lookup", "result", "malformed")
		return model.Developer{}, model.APIKey{}, ErrInvalidCredentials
	}
	d, k, err := a.store.DeveloperByKeyHash(HashKey(full))
	if errors.Is(err, store.ErrNotFound) {
		log.Debug("api key lookup", "result", "miss or revoked", "prefix", full[:prefixLen])
		return model.Developer{}, model.APIKey{}, ErrInvalidCredentials
	}
	if err != nil {
		return model.Developer{}, model.APIKey{}, err
	}
	log.Debug("api key lookup", "result", "hit", "developer_id", d.ID, "key_id", k.ID, "prefix", k.Prefix)
	a.touch(ctx, k.ID)
	return d, k, nil
}

// touch records last_used_at at most once per touchEvery per key, so a busy
// integration does not turn every request into a write.
func (a *Service) touch(ctx context.Context, keyID string) {
	now := time.Now().UTC()
	a.mu.Lock()
	last, seen := a.lastTouched[keyID]
	if seen && now.Sub(last) < touchEvery {
		a.mu.Unlock()
		return
	}
	a.lastTouched[keyID] = now
	a.mu.Unlock()
	if err := a.store.TouchAPIKey(keyID, now); err != nil {
		a.logger(ctx).Warn("touching api key", "key_id", keyID, "err", err)
	}
}

func (a *Service) RevokeKey(ctx context.Context, developerID, keyID string) error {
	err := a.store.RevokeAPIKey(developerID, keyID, time.Now().UTC())
	if err == nil {
		a.logger(ctx).Info("api key revoked", "developer_id", developerID, "key_id", keyID)
	}
	return err
}
