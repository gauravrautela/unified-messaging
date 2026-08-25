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

const webhookSelect = `SELECT id, developer_id, account_id, name, url, secret, events_json, created_at FROM webhooks`

func (s *Store) SaveWebhook(w model.Webhook) error {
	ev, _ := json.Marshal(w.Events)
	_, err := s.db.Exec(`
		INSERT INTO webhooks (id, developer_id, account_id, name, url, secret, events_json, created_at)
		VALUES (?,?,?,?,?,?,?,?)`,
		w.ID, w.DeveloperID, w.AccountID, w.Name, w.URL, w.Secret, string(ev), w.CreatedAt.Unix())
	return err
}

// ListWebhooks returns every hook a developer owns, developer-wide and
// account-scoped.
func (s *Store) ListWebhooks(developerID string) ([]model.Webhook, error) {
	return s.queryWebhooks(webhookSelect+` WHERE developer_id = ? ORDER BY created_at`, developerID)
}

// ListWebhooksFor is UNSCOPED by developer: the dispatcher resolves the
// hooks an account's event should reach — those bound to the account plus
// the developer-wide ones of the account's owner.
func (s *Store) ListWebhooksFor(accountID string) ([]model.Webhook, error) {
	// Both clauses are gated on the hook belonging to the account's owner, the
	// account-bound one included. That is defence in depth rather than a fix:
	// a hook's account_id can only be set to an account the same developer
	// owns. If that ever stopped holding, this query would still not deliver
	// one tenant's mail events to another tenant's endpoint.
	return s.queryWebhooks(webhookSelect+`
		WHERE developer_id = (SELECT developer_id FROM accounts WHERE id = ?)
		  AND (account_id = ? OR account_id = '')`,
		accountID, accountID)
}

func (s *Store) ListAccountWebhooks(developerID, accountID string) ([]model.Webhook, error) {
	return s.queryWebhooks(webhookSelect+` WHERE developer_id = ? AND account_id = ? ORDER BY created_at`,
		developerID, accountID)
}

func (s *Store) GetWebhook(developerID, id string) (model.Webhook, error) {
	return s.oneWebhook(webhookSelect+` WHERE developer_id = ? AND id = ?`, developerID, id)
}

// GetAnyWebhook is UNSCOPED: for the retry loop, which holds only a hook id.
func (s *Store) GetAnyWebhook(id string) (model.Webhook, error) {
	return s.oneWebhook(webhookSelect+` WHERE id = ?`, id)
}

func (s *Store) DeleteWebhook(developerID, id string) error {
	res, err := s.db.Exec(`DELETE FROM webhooks WHERE developer_id = ? AND id = ?`, developerID, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) oneWebhook(q string, args ...any) (model.Webhook, error) {
	hooks, err := s.queryWebhooks(q, args...)
	if err != nil {
		return model.Webhook{}, err
	}
	if len(hooks) == 0 {
		return model.Webhook{}, ErrNotFound
	}
	return hooks[0], nil
}

func (s *Store) queryWebhooks(q string, args ...any) ([]model.Webhook, error) {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Webhook{}
	for rows.Next() {
		var w model.Webhook
		var ev string
		var created int64
		if err := rows.Scan(&w.ID, &w.DeveloperID, &w.AccountID, &w.Name, &w.URL, &w.Secret, &ev, &created); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(ev), &w.Events)
		w.CreatedAt = time.Unix(created, 0).UTC()
		out = append(out, w)
	}
	return out, rows.Err()
}

// ---------- webhook delivery retry queue ----------

// Delivery is one webhook POST that has failed at least once and is waiting
// for its next attempt.
type Delivery struct {
	ID            string    `json:"id"`
	WebhookID     string    `json:"webhook_id"`
	AccountID     string    `json:"account_id,omitempty"`
	EventType     string    `json:"event_type"`
	Payload       []byte    `json:"-"`
	Attempts      int       `json:"attempts"`
	NextAttemptAt time.Time `json:"next_attempt_at"`
	LastError     string    `json:"last_error,omitempty"`
	Dead          bool      `json:"dead"`
	CreatedAt     time.Time `json:"created_at"`
}

// SaveDelivery inserts or replaces a queued delivery.
func (s *Store) SaveDelivery(d Delivery) error {
	// payload is a byte count, never content.
	defer s.trace("SaveDelivery", time.Now(), "delivery_id", d.ID, "webhook_id", d.WebhookID,
		"account_id", d.AccountID, "attempts", d.Attempts, "dead", d.Dead, "payload_bytes", len(d.Payload))
	_, err := s.db.Exec(`
		INSERT INTO webhook_deliveries
		  (id, webhook_id, account_id, event_type, payload, attempts, next_attempt_at, last_error, dead, created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
		  attempts = excluded.attempts, next_attempt_at = excluded.next_attempt_at,
		  last_error = excluded.last_error, dead = excluded.dead`,
		d.ID, d.WebhookID, d.AccountID, d.EventType, d.Payload, d.Attempts,
		d.NextAttemptAt.Unix(), d.LastError, d.Dead, d.CreatedAt.Unix())
	return err
}

// DueDeliveries returns live deliveries whose retry time has passed, oldest
// first.
func (s *Store) DueDeliveries(now time.Time, limit int) ([]Delivery, error) {
	start := time.Now()
	out, err := s.queryDeliveries(`
		SELECT id, webhook_id, account_id, event_type, payload, attempts, next_attempt_at, last_error, dead, created_at
		FROM webhook_deliveries WHERE dead = 0 AND next_attempt_at <= ?
		ORDER BY next_attempt_at LIMIT ?`, now.Unix(), limit)
	s.trace("DueDeliveries", start, "limit", limit, "rows", len(out))
	return out, err
}

// ListDeliveries returns everything queued or dead for one webhook.
func (s *Store) ListDeliveries(webhookID string) ([]Delivery, error) {
	return s.queryDeliveries(`
		SELECT id, webhook_id, account_id, event_type, payload, attempts, next_attempt_at, last_error, dead, created_at
		FROM webhook_deliveries WHERE webhook_id = ? ORDER BY created_at`, webhookID)
}

func (s *Store) DeleteDelivery(id string) error {
	_, err := s.db.Exec(`DELETE FROM webhook_deliveries WHERE id = ?`, id)
	return err
}

func (s *Store) queryDeliveries(q string, args ...any) ([]Delivery, error) {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Delivery{}
	for rows.Next() {
		var d Delivery
		var next, created int64
		if err := rows.Scan(&d.ID, &d.WebhookID, &d.AccountID, &d.EventType, &d.Payload,
			&d.Attempts, &next, &d.LastError, &d.Dead, &created); err != nil {
			return nil, err
		}
		d.NextAttemptAt = time.Unix(next, 0).UTC()
		d.CreatedAt = time.Unix(created, 0).UTC()
		out = append(out, d)
	}
	return out, rows.Err()
}

// ---------- OAuth PKCE state ----------

type OAuthState struct {
	State string
	// DeveloperID is the tenant who minted this connect attempt.
	DeveloperID string
	// Provider names which backend this connect attempt is for.
	Provider   string
	Verifier   string
	SuccessURL string
	FailureURL string
	NotifyURL  string
	// Webhook, when set, is registered against the account the moment it is
	// created by the callback.
	Webhook   *PendingWebhook
	ExpiresAt time.Time
}

// PendingWebhook is a webhook requested at connect time, before the account
// it will belong to exists.
type PendingWebhook struct {
	Name   string   `json:"name,omitempty"`
	URL    string   `json:"url"`
	Secret string   `json:"secret,omitempty"`
	Events []string `json:"events,omitempty"`
}

func encodePendingWebhook(w *PendingWebhook) string {
	if w == nil {
		return ""
	}
	b, _ := json.Marshal(w)
	return string(b)
}

func decodePendingWebhook(raw string) *PendingWebhook {
	if raw == "" {
		return nil
	}
	var w PendingWebhook
	if err := json.Unmarshal([]byte(raw), &w); err != nil {
		return nil
	}
	return &w
}

func (s *Store) SaveOAuthState(o OAuthState) error {
	_, err := s.db.Exec(`
		INSERT INTO oauth_states (state, developer_id, provider, verifier, success_url, failure_url, notify_url, webhook_json, created_at, expires_at)
		VALUES (?,?,?,?,?,?,?,?,?,?)`,
		o.State, o.DeveloperID, o.Provider, o.Verifier, o.SuccessURL, o.FailureURL, o.NotifyURL,
		encodePendingWebhook(o.Webhook), time.Now().Unix(), o.ExpiresAt.Unix())
	return err
}

// TakeOAuthState consumes the state single-use, which is what makes it a real
// CSRF defence rather than decoration.
func (s *Store) TakeOAuthState(state string) (OAuthState, error) {
	var o OAuthState
	var exp int64
	var wh string
	err := s.db.QueryRow(`
		SELECT state, developer_id, provider, verifier, success_url, failure_url, notify_url, webhook_json, expires_at
		FROM oauth_states WHERE state = ?`, state).
		Scan(&o.State, &o.DeveloperID, &o.Provider, &o.Verifier, &o.SuccessURL, &o.FailureURL, &o.NotifyURL, &wh, &exp)
	o.Webhook = decodePendingWebhook(wh)
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
	var wh string
	err := s.db.QueryRow(`
		SELECT state, developer_id, provider, verifier, success_url, failure_url, notify_url, webhook_json, expires_at
		FROM oauth_states WHERE state = ?`, state).
		Scan(&o.State, &o.DeveloperID, &o.Provider, &o.Verifier, &o.SuccessURL, &o.FailureURL, &o.NotifyURL, &wh, &exp)
	o.Webhook = decodePendingWebhook(wh)
	if errors.Is(err, sql.ErrNoRows) {
		return o, ErrNotFound
	}
	o.ExpiresAt = time.Unix(exp, 0).UTC()
	return o, err
}
