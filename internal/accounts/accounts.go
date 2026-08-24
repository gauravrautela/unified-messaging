// Package accounts owns connected-account identity and token custody.
//
// It is the only place refresh tokens are handled, and it is what lets a
// provider ask for "a valid token for account X" without knowing anything about
// OAuth storage. It is deliberately provider-agnostic: which backend an account
// belongs to is a lookup, not a branch.
package accounts

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/gauravrautela/unified-messaging/internal/model"
	"github.com/gauravrautela/unified-messaging/internal/provider"
	"github.com/gauravrautela/unified-messaging/internal/secretbox"
	"github.com/gauravrautela/unified-messaging/internal/store"
)

type Manager struct {
	store *store.Store
	key   []byte
	log   *slog.Logger

	// registry is wired after construction: providers need this Manager as their
	// token source, so it cannot exist before them.
	registry *provider.Registry

	// One refresh at a time per account. Without this, a burst of concurrent
	// calls after expiry would each redeem the refresh token, and providers that
	// rotate them would leave all but one holder with a dead token.
	mu    sync.Mutex
	locks map[string]*sync.Mutex

	// OnStatusChange lets the server emit an account_status event when an
	// account falls off.
	OnStatusChange func(accountID, status string)
}

func NewManager(s *store.Store, key []byte, log *slog.Logger) *Manager {
	return &Manager{store: s, key: key, log: log, locks: map[string]*sync.Mutex{}}
}

// SetRegistry completes the wiring. It must be called before any Graph or
// provider call is made.
func (m *Manager) SetRegistry(r *provider.Registry) { m.registry = r }

func (m *Manager) lockFor(accountID string) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	if l, ok := m.locks[accountID]; ok {
		return l
	}
	l := &sync.Mutex{}
	m.locks[accountID] = l
	return l
}

// Connect completes an OAuth handshake and records the account.
//
// Reconnecting an already-known address updates the existing record in place
// rather than creating a duplicate, so callers keep the same account_id.
func (m *Manager) Connect(ctx context.Context, providerName, code, verifier string) (model.Account, error) {
	p, err := m.registry.Get(providerName)
	if err != nil {
		return model.Account{}, err
	}
	auth := p.Auth()

	tok, err := auth.Exchange(ctx, code, verifier)
	if err != nil {
		return model.Account{}, err
	}
	if tok.RefreshToken == "" {
		return model.Account{}, errors.New("accounts: no refresh token returned; is offline_access in the requested scopes?")
	}

	identity, err := auth.Identify(ctx, tok.AccessToken)
	if err != nil {
		return model.Account{}, fmt.Errorf("identifying account: %w", err)
	}
	if identity.Email == "" {
		return model.Account{}, errors.New("accounts: provider did not report an address")
	}

	id, err := m.store.AccountIDByEmail(identity.Email)
	if errors.Is(err, store.ErrNotFound) {
		id, err = newID("acc")
		if err != nil {
			return model.Account{}, err
		}
	} else if err != nil {
		return model.Account{}, err
	}

	if err := m.store.UpsertAccount(model.Account{
		ID:       id,
		Provider: p.Name(),
		Email:    identity.Email,
		Name:     identity.Name,
		Status:   model.AccountOK,
	}); err != nil {
		return model.Account{}, err
	}
	// The upsert conflicts on email, so re-read to get the ID that actually won.
	realID, err := m.store.AccountIDByEmail(identity.Email)
	if err != nil {
		return model.Account{}, err
	}
	if err := m.persist(realID, tok); err != nil {
		return model.Account{}, err
	}
	return m.store.GetAccount(realID)
}

func (m *Manager) persist(accountID string, tok provider.Token) error {
	sealed, err := secretbox.Seal(m.key, tok.RefreshToken)
	if err != nil {
		return err
	}
	return m.store.SaveTokens(accountID, store.TokenRecord{
		AccessToken:     tok.AccessToken,
		AccessExpiresAt: tok.ExpiresAt,
		RefreshTokenEnc: sealed,
		Scope:           tok.Scope,
	})
}

// AccessToken implements provider.TokenSource.
func (m *Manager) AccessToken(ctx context.Context, accountID string, force bool) (string, error) {
	lock := m.lockFor(accountID)
	lock.Lock()
	defer lock.Unlock()

	rec, err := m.store.GetTokens(accountID)
	if err != nil {
		return "", err
	}
	if !force && rec.AccessToken != "" && time.Now().Before(rec.AccessExpiresAt) {
		return rec.AccessToken, nil
	}

	acct, err := m.store.GetAccount(accountID)
	if err != nil {
		return "", err
	}
	p, err := m.registry.Get(acct.Provider)
	if err != nil {
		return "", err
	}

	refresh, err := secretbox.Open(m.key, rec.RefreshTokenEnc)
	if err != nil {
		return "", fmt.Errorf("accounts: cannot decrypt refresh token (wrong TOKEN_ENCRYPTION_KEY?): %w", err)
	}

	tok, err := p.Auth().Refresh(ctx, refresh)
	if err != nil {
		if errors.Is(err, provider.ErrReauthRequired) {
			m.markCredentials(accountID)
		}
		return "", err
	}
	// Providers may rotate refresh tokens; keep the newest, falling back to the
	// one we already hold when the response omits it.
	if tok.RefreshToken == "" {
		tok.RefreshToken = refresh
	}
	if err := m.persist(accountID, tok); err != nil {
		return "", err
	}
	return tok.AccessToken, nil
}

func (m *Manager) markCredentials(accountID string) {
	if err := m.store.SetAccountStatus(accountID, model.AccountCredentials); err != nil {
		m.log.Error("marking account as needing re-consent", "account_id", accountID, "err", err)
		return
	}
	m.log.Warn("account needs reconnection", "account_id", accountID)
	if m.OnStatusChange != nil {
		m.OnStatusChange(accountID, model.AccountCredentials)
	}
}
