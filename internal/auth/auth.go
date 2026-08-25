// internal/auth/auth.go
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
	"log/slog"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/gauravrautela/unified-messaging/internal/logx"
	"github.com/gauravrautela/unified-messaging/internal/model"
	"github.com/gauravrautela/unified-messaging/internal/store"
)

var (
	ErrInvalidInput       = errors.New("invalid email or password format")
	ErrEmailTaken         = errors.New("could not create account")
	ErrInvalidCredentials = errors.New("invalid email or password")
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
)

// dummyHash is compared when the email is unknown so both login failure
// paths cost one bcrypt verification and cannot be told apart by timing.
var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("dummy-password-for-timing"), bcryptCost)

// newID mirrors internal/accounts.newID (12 random bytes, hex-encoded,
// "<prefix>_<hex>"). It is duplicated here rather than imported: accounts
// currently fails to build while Tasks 5-6 are in flight, and importing it
// would make this package fail to build too. Once accounts is stable again
// this can be revisited, but the shape must stay identical either way.
func newID(prefix string) (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return prefix + "_" + hex.EncodeToString(b), nil
}

type Service struct {
	store      *store.Store
	log        *slog.Logger
	sessionTTL time.Duration

	mu          sync.Mutex
	lastTouched map[string]time.Time // key id -> last last_used_at write
}

func New(s *store.Store, log *slog.Logger, sessionTTL time.Duration) *Service {
	return &Service{store: s, log: log.With("component", "auth"), sessionTTL: sessionTTL, lastTouched: map[string]time.Time{}}
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
	id, err := newID("dev")
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
	if err := a.store.CreateSession(tok, developerID, exp); err != nil {
		return "", time.Time{}, err
	}
	a.logger(ctx).Info("session created", "developer_id", developerID, "expires_at", exp)
	return tok, exp, nil
}

// SessionDeveloper resolves a cookie value. Expiry slides forward once more
// than slideAfter of the TTL has been consumed, so an active developer is
// never logged out mid-work.
func (a *Service) SessionDeveloper(ctx context.Context, token string) (model.Developer, error) {
	log := a.logger(ctx)
	if token == "" {
		return model.Developer{}, ErrInvalidCredentials
	}
	now := time.Now().UTC()
	d, exp, err := a.store.SessionDeveloper(token, now)
	if errors.Is(err, store.ErrNotFound) {
		log.Debug("session lookup", "result", "miss or expired")
		return model.Developer{}, ErrInvalidCredentials
	}
	if err != nil {
		return model.Developer{}, err
	}
	if exp.Sub(now) < a.sessionTTL-slideAfter {
		newExp := now.Add(a.sessionTTL)
		if err := a.store.ExtendSession(token, newExp); err != nil {
			log.Warn("extending session", "developer_id", d.ID, "err", err)
		} else {
			log.Debug("session extended", "developer_id", d.ID, "expires_at", newExp)
		}
	}
	log.Debug("session lookup", "result", "hit", "developer_id", d.ID)
	return d, nil
}

func (a *Service) DeleteSession(ctx context.Context, token string) error {
	a.logger(ctx).Info("session deleted")
	return a.store.DeleteSession(token)
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
	id, err := newID("key")
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
