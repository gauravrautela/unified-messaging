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

	"github.com/gauravrautela/unified-messaging/internal/logx"
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
	// Tagged once, so every line this package writes off m.log carries its
	// component without repeating the attribute at each call site.
	return &Manager{store: s, key: key, log: log.With("component", "accounts"),
		locks: map[string]*sync.Mutex{}}
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

// Connect completes an OAuth handshake and records the account under the
// developer who minted the connect link.
//
// Reconnecting an address that developer already has updates the existing
// record in place rather than creating a duplicate, so callers keep the same
// account_id. The same address under a different developer is a different
// account.
func (m *Manager) Connect(ctx context.Context, developerID, providerName, code, verifier string) (model.Account, error) {
	log := logx.From(ctx).With("component", "accounts", "developer_id", developerID, "provider", providerName)
	p, err := m.registry.Get(providerName)
	if err != nil {
		return model.Account{}, err
	}
	auth := p.Auth()

	log.Debug("exchanging authorization code")
	tok, err := auth.Exchange(ctx, code, verifier)
	if err != nil {
		log.Warn("code exchange failed", "err", err)
		return model.Account{}, err
	}
	if tok.RefreshToken == "" {
		return model.Account{}, errors.New("accounts: no refresh token returned; is offline_access in the requested scopes?")
	}
	log.Debug("code exchanged", "access_expires_at", tok.ExpiresAt, "scope", tok.Scope)

	identity, err := auth.Identify(ctx, tok.AccessToken)
	if err != nil {
		return model.Account{}, fmt.Errorf("identifying account: %w", err)
	}
	if identity.Email == "" {
		return model.Account{}, errors.New("accounts: provider did not report an address")
	}

	id, err := m.store.AccountIDByEmail(developerID, identity.Email)
	reconnect := err == nil
	if errors.Is(err, store.ErrNotFound) {
		id, err = newID("acc")
		if err != nil {
			return model.Account{}, err
		}
	} else if err != nil {
		return model.Account{}, err
	}

	if err := m.store.UpsertAccount(model.Account{
		ID:          id,
		DeveloperID: developerID,
		Provider:    p.Name(),
		Email:       identity.Email,
		Name:        identity.Name,
		Status:      model.AccountOK,
	}); err != nil {
		return model.Account{}, err
	}
	realID, err := m.store.AccountIDByEmail(developerID, identity.Email)
	if err != nil {
		return model.Account{}, err
	}
	if err := m.persist(realID, tok); err != nil {
		return model.Account{}, err
	}
	log.Info("account connected", "account_id", realID, "email_digest", logx.Digest(identity.Email), "reconnect", reconnect)
	return m.store.GetAnyAccount(realID)
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

	// Token values never appear here; only whether one was held, and for how
	// much longer it would have been valid.
	//
	// Only component is added: the ids (run_id/request_id, account_id,
	// developer_id) are already on the context logger, put there by whoever
	// started the run or the request, and re-adding them would emit duplicates.
	log := logx.From(ctx).With("component", "accounts")

	rec, err := m.store.GetTokens(accountID)
	if err != nil {
		return "", err
	}
	if !force && rec.AccessToken != "" && time.Now().Before(rec.AccessExpiresAt) {
		log.Debug("token decision", "decision", "cached", "forced", force,
			"expires_in", time.Until(rec.AccessExpiresAt).Round(time.Second))
		return rec.AccessToken, nil
	}
	log.Debug("token decision", "decision", "refreshing", "forced", force,
		"had_access_token", rec.AccessToken != "",
		"expires_in", time.Until(rec.AccessExpiresAt).Round(time.Second))

	acct, err := m.store.GetAnyAccount(accountID)
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
		log.Warn("token refresh failed", "err", err)
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
	log.Info("token refreshed", "expires_at", tok.ExpiresAt, "rotated", tok.RefreshToken != refresh)
	return tok.AccessToken, nil
}

// ConnectLinked records a chat account after a successful device link. The
// device's credentials live in the provider's own store; we keep only the
// mapping account -> device JID. Relinking the same number under the same
// developer reuses the account id and replaces the device.
func (m *Manager) ConnectLinked(ctx context.Context, developerID, providerName string, id provider.Identity, deviceJID string) (model.Account, error) {
	log := logx.From(ctx).With("component", "accounts", "developer_id", developerID, "provider", providerName)
	if id.Identifier == "" {
		return model.Account{}, errors.New("accounts: provider did not report an identifier")
	}
	accountID, err := m.store.AccountIDByEmail(developerID, id.Identifier)
	relink := err == nil
	if errors.Is(err, store.ErrNotFound) {
		if accountID, err = newID("acc"); err != nil {
			return model.Account{}, err
		}
	} else if err != nil {
		return model.Account{}, err
	}
	if err := m.store.UpsertAccount(model.Account{
		ID: accountID, DeveloperID: developerID, Kind: model.AccountKindChat, Provider: providerName,
		Email: id.Identifier, Name: id.Name, Status: model.AccountOK,
	}); err != nil {
		return model.Account{}, err
	}
	realID, err := m.store.AccountIDByEmail(developerID, id.Identifier)
	if err != nil {
		return model.Account{}, err
	}
	if relink {
		// Drop the previous device so its keys do not linger.
		if old, err := m.store.ChatSession(realID); err == nil && old != deviceJID {
			m.forget(ctx, providerName, old)
		}
	}
	if err := m.store.SaveChatSession(realID, providerName, deviceJID); err != nil {
		return model.Account{}, err
	}
	log.Info("chat account linked", "account_id", realID, "relink", relink)
	return m.store.GetAnyAccount(realID)
}

func (m *Manager) DeviceJID(accountID string) (string, error) { return m.store.ChatSession(accountID) }

// DeleteLinked removes a chat account and its device credentials.
func (m *Manager) DeleteLinked(ctx context.Context, accountID string) error {
	acct, err := m.store.GetAnyAccount(accountID)
	if err != nil {
		return err
	}
	if jid, err := m.store.ChatSession(accountID); err == nil {
		m.forget(ctx, acct.Provider, jid)
	}
	if err := m.store.DeleteChatSession(accountID); err != nil {
		return err
	}
	return m.store.DeleteAccount(accountID)
}

// MarkLoggedOut is the chat counterpart of a rejected refresh token: the
// phone removed the linked device, so only the end user can fix it.
func (m *Manager) MarkLoggedOut(accountID, reason string) {
	acct, err := m.store.GetAnyAccount(accountID)
	if err != nil {
		return
	}
	if jid, err := m.store.ChatSession(accountID); err == nil {
		m.forget(context.Background(), acct.Provider, jid)
	}
	_ = m.store.DeleteChatSession(accountID)
	m.log.Warn("chat account logged out", "account_id", accountID, "reason", reason)
	m.markCredentials(accountID)
}

func (m *Manager) forget(ctx context.Context, providerName, deviceJID string) {
	p, err := m.registry.Get(providerName)
	if err != nil || p.Chat() == nil {
		return
	}
	if err := p.Chat().Forget(ctx, deviceJID); err != nil {
		m.log.Warn("forgetting device", "provider", providerName, "err", err)
	}
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
