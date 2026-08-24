package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/gauravrautela/unified-messaging/internal/model"
)

// ---------- Graph change-notification subscriptions ----------

type Subscription struct {
	ID          string
	AccountID   string
	Resource    string
	ClientState string
	ExpiresAt   time.Time
	CreatedAt   time.Time
}

func (s *Store) SaveSubscription(sub Subscription) error {
	_, err := s.db.Exec(`
		INSERT INTO subscriptions (id, account_id, resource, client_state, expires_at, created_at)
		VALUES (?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET expires_at = excluded.expires_at`,
		sub.ID, sub.AccountID, sub.Resource, sub.ClientState,
		sub.ExpiresAt.Unix(), time.Now().Unix())
	return err
}

func (s *Store) GetSubscription(id string) (Subscription, error) {
	var sub Subscription
	var exp, created int64
	err := s.db.QueryRow(`
		SELECT id, account_id, resource, client_state, expires_at, created_at
		FROM subscriptions WHERE id = ?`, id).
		Scan(&sub.ID, &sub.AccountID, &sub.Resource, &sub.ClientState, &exp, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return sub, ErrNotFound
	}
	sub.ExpiresAt = time.Unix(exp, 0).UTC()
	sub.CreatedAt = time.Unix(created, 0).UTC()
	return sub, err
}

func (s *Store) ListSubscriptions() ([]Subscription, error) {
	rows, err := s.db.Query(`
		SELECT id, account_id, resource, client_state, expires_at, created_at FROM subscriptions`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Subscription{}
	for rows.Next() {
		var sub Subscription
		var exp, created int64
		if err := rows.Scan(&sub.ID, &sub.AccountID, &sub.Resource, &sub.ClientState,
			&exp, &created); err != nil {
			return nil, err
		}
		sub.ExpiresAt = time.Unix(exp, 0).UTC()
		sub.CreatedAt = time.Unix(created, 0).UTC()
		out = append(out, sub)
	}
	return out, rows.Err()
}

func (s *Store) SubscriptionsForAccount(accountID string) ([]Subscription, error) {
	all, err := s.ListSubscriptions()
	if err != nil {
		return nil, err
	}
	out := []Subscription{}
	for _, sub := range all {
		if sub.AccountID == accountID {
			out = append(out, sub)
		}
	}
	return out, nil
}

func (s *Store) DeleteSubscription(id string) error {
	_, err := s.db.Exec(`DELETE FROM subscriptions WHERE id = ?`, id)
	return err
}

// ---------- outbound webhooks ----------

func (s *Store) SaveWebhook(w model.Webhook) error {
	ev, _ := json.Marshal(w.Events)
	_, err := s.db.Exec(`
		INSERT INTO webhooks (id, url, secret, events_json, created_at) VALUES (?,?,?,?,?)`,
		w.ID, w.URL, w.Secret, string(ev), w.CreatedAt.Unix())
	return err
}

func (s *Store) ListWebhooks() ([]model.Webhook, error) {
	rows, err := s.db.Query(`SELECT id, url, secret, events_json, created_at FROM webhooks`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Webhook{}
	for rows.Next() {
		var w model.Webhook
		var ev string
		var created int64
		if err := rows.Scan(&w.ID, &w.URL, &w.Secret, &ev, &created); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(ev), &w.Events)
		w.CreatedAt = time.Unix(created, 0).UTC()
		out = append(out, w)
	}
	return out, rows.Err()
}

func (s *Store) DeleteWebhook(id string) error {
	_, err := s.db.Exec(`DELETE FROM webhooks WHERE id = ?`, id)
	return err
}

// ---------- OAuth PKCE state ----------

type OAuthState struct {
	State string
	// Provider names which backend this connect attempt is for.
	Provider   string
	Verifier   string
	SuccessURL string
	FailureURL string
	NotifyURL  string
	ExpiresAt  time.Time
}

func (s *Store) SaveOAuthState(o OAuthState) error {
	_, err := s.db.Exec(`
		INSERT INTO oauth_states (state, provider, verifier, success_url, failure_url, notify_url, created_at, expires_at)
		VALUES (?,?,?,?,?,?,?,?)`,
		o.State, o.Provider, o.Verifier, o.SuccessURL, o.FailureURL, o.NotifyURL,
		time.Now().Unix(), o.ExpiresAt.Unix())
	return err
}

// TakeOAuthState consumes the state single-use, which is what makes it a real
// CSRF defence rather than decoration.
func (s *Store) TakeOAuthState(state string) (OAuthState, error) {
	var o OAuthState
	var exp int64
	err := s.db.QueryRow(`
		SELECT state, provider, verifier, success_url, failure_url, notify_url, expires_at
		FROM oauth_states WHERE state = ?`, state).
		Scan(&o.State, &o.Provider, &o.Verifier, &o.SuccessURL, &o.FailureURL, &o.NotifyURL, &exp)
	if errors.Is(err, sql.ErrNoRows) {
		return o, ErrNotFound
	}
	if err != nil {
		return o, err
	}
	if _, err := s.db.Exec(`DELETE FROM oauth_states WHERE state = ?`, state); err != nil {
		return o, err
	}
	o.ExpiresAt = time.Unix(exp, 0).UTC()
	if time.Now().After(o.ExpiresAt) {
		return o, errors.New("oauth state expired")
	}
	return o, nil
}

func (s *Store) PurgeExpiredOAuthStates() {
	_, _ = s.db.Exec(`DELETE FROM oauth_states WHERE expires_at < ?`, time.Now().Unix())
}

// PeekOAuthState reads a pending connect attempt without consuming it. The
// /connect redirect needs it before the user has been anywhere near Microsoft,
// so consuming here would break the flow at its first step.
func (s *Store) PeekOAuthState(state string) (OAuthState, error) {
	var o OAuthState
	var exp int64
	err := s.db.QueryRow(`
		SELECT state, provider, verifier, success_url, failure_url, notify_url, expires_at
		FROM oauth_states WHERE state = ?`, state).
		Scan(&o.State, &o.Provider, &o.Verifier, &o.SuccessURL, &o.FailureURL, &o.NotifyURL, &exp)
	if errors.Is(err, sql.ErrNoRows) {
		return o, ErrNotFound
	}
	o.ExpiresAt = time.Unix(exp, 0).UTC()
	return o, err
}
