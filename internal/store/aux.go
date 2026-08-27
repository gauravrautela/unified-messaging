package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/gauravrautela/unified-messaging/internal/model"
	"github.com/gauravrautela/unified-messaging/internal/secretbox"
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

const webhookSelect = `SELECT id, developer_id, account_id, name, url, secret, events_json, created_at, kind, config FROM webhooks`

// webhookConfig is the sealed part of a hook: credentials that must not sit
// in the row in clear. Only telegram hooks have one today.
type webhookConfig struct {
	BotToken string `json:"bot_token,omitempty"`
	ChatID   string `json:"chat_id,omitempty"`
}

var errNoSealKey = errors.New("store: seal key not set")

// errTelegramNeedsTarget guards against a telegram hook with nowhere to
// deliver to: the store, not a caller, is the enforcement point for that.
var errTelegramNeedsTarget = errors.New("store: telegram hook needs a target")

func (s *Store) SaveWebhook(w model.Webhook) error {
	if w.Kind == "" {
		w.Kind = model.WebhookKindWebhook
	}
	ev, _ := json.Marshal(w.Events)
	config := ""
	if w.Kind == model.WebhookKindTelegram {
		if w.Telegram == nil {
			return errTelegramNeedsTarget
		}
		if s.sealKey == nil {
			return errNoSealKey
		}
		raw, _ := json.Marshal(webhookConfig{BotToken: w.Telegram.BotToken, ChatID: w.Telegram.ChatID})
		sealed, err := secretbox.Seal(s.sealKey, string(raw))
		if err != nil {
			return err
		}
		config = sealed
	}
	_, err := s.db.Exec(`
		INSERT INTO webhooks (id, developer_id, account_id, name, url, secret, events_json, created_at, kind, config)
		VALUES (?,?,?,?,?,?,?,?,?,?)`,
		w.ID, w.DeveloperID, w.AccountID, w.Name, w.URL, w.Secret, string(ev), w.CreatedAt.Unix(), w.Kind, config)
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
		var ev, kind, config string
		var created int64
		if err := rows.Scan(&w.ID, &w.DeveloperID, &w.AccountID, &w.Name, &w.URL, &w.Secret, &ev, &created, &kind, &config); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(ev), &w.Events)
		w.CreatedAt = time.Unix(created, 0).UTC()
		w.Kind = kind
		if w.Kind == "" {
			w.Kind = model.WebhookKindWebhook
		}
		if w.Kind == model.WebhookKindTelegram {
			w.Telegram = &model.TelegramTarget{}
			if config != "" {
				switch {
				case s.sealKey == nil:
					// No key at all in this process: cannot even attempt to open it.
					// Keep the hook listable, deliveries to it fail with a clear
					// error (see notify.telegramSender).
					s.warnConfigUnreadable(w.ID, "no seal key")
				default:
					if raw, err := secretbox.Open(s.sealKey, config); err == nil {
						var c webhookConfig
						_ = json.Unmarshal([]byte(raw), &c)
						w.Telegram.BotToken, w.Telegram.ChatID = c.BotToken, c.ChatID
					} else {
						// Wrong key or corrupt row: keep the hook listable, deliveries
						// to it fail with a clear error (see notify.telegramSender).
						s.warnConfigUnreadable(w.ID, "open failed")
					}
				}
			}
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// warnConfigUnreadable reports a hook whose sealed config cannot be opened.
// The first sighting of a given webhook id is a WARN; every later one is a
// DEBUG, because ListWebhooksFor runs on every dispatched event and the
// condition is permanent until someone fixes the key — one un-openable row
// would otherwise be an unbounded stream of identical warnings.
func (s *Store) warnConfigUnreadable(webhookID, reason string) {
	if s.log == nil {
		return
	}
	if _, seen := s.warnedConfig.LoadOrStore(webhookID, struct{}{}); seen {
		s.log.Debug("webhook config unreadable", "webhook_id", webhookID, "reason", reason)
		return
	}
	s.log.Warn("webhook config unreadable", "webhook_id", webhookID, "reason", reason)
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

// ListDeliveries returns one page of what is queued or dead for one webhook,
// oldest first. An outage can pile up hundreds of dead deliveries with full
// message payloads, so an unbounded listing is not safe to expose.
func (s *Store) ListDeliveries(webhookID string, limit, offset int) ([]Delivery, error) {
	return s.queryDeliveries(`
		SELECT id, webhook_id, account_id, event_type, payload, attempts, next_attempt_at, last_error, dead, created_at
		FROM webhook_deliveries WHERE webhook_id = ? ORDER BY created_at LIMIT ? OFFSET ?`, webhookID, limit, offset)
}

func (s *Store) DeleteDelivery(id string) error {
	_, err := s.db.Exec(`DELETE FROM webhook_deliveries WHERE id = ?`, id)
	return err
}

// PurgeDeadDeliveries removes abandoned deliveries older than before. A dead
// delivery keeps the full message payload it failed to send, so leaving them
// forever is an unbounded (and increasingly sensitive) amount of retained
// mail/chat content. Live deliveries — still retrying — are never touched.
func (s *Store) PurgeDeadDeliveries(before time.Time) (int64, error) {
	res, err := s.db.Exec(`DELETE FROM webhook_deliveries WHERE dead = 1 AND created_at < ?`, before.Unix())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
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
	// ConsentedAt is set once the end user has accepted the linker disclosure
	// on the connect page. Nil means the QR endpoint must refuse with
	// consent_required. Mail providers never set this; consent there is
	// Microsoft's own screen.
	ConsentedAt *time.Time
}

// PendingWebhook is a webhook requested at connect time, before the account
// it will belong to exists.
type PendingWebhook struct {
	Name     string   `json:"name,omitempty"`
	Kind     string   `json:"kind,omitempty"`
	URL      string   `json:"url,omitempty"`
	Secret   string   `json:"secret,omitempty"`
	BotToken string   `json:"bot_token,omitempty"`
	ChatID   string   `json:"chat_id,omitempty"`
	Events   []string `json:"events,omitempty"`
}

// pendingWebhookSealedPrefix marks a webhook_json value that has been sealed
// because it carries a bot token, so decode knows to open it first.
const pendingWebhookSealedPrefix = "sealed:"

// encodePendingWebhook serialises a pending hook for the webhook_json column.
// When it carries a bot token, the whole JSON blob is sealed (prefixed
// "sealed:") rather than stored in clear: this row lives for up to 30
// minutes waiting on the OAuth callback, same as any other secret at rest.
func (s *Store) encodePendingWebhook(w *PendingWebhook) (string, error) {
	if w == nil {
		return "", nil
	}
	b, _ := json.Marshal(w)
	if w.BotToken == "" {
		return string(b), nil
	}
	if s.sealKey == nil {
		return "", errNoSealKey
	}
	sealed, err := secretbox.Seal(s.sealKey, string(b))
	if err != nil {
		return "", err
	}
	return pendingWebhookSealedPrefix + sealed, nil
}

// warnPendingUnreadable reports a connect-time hook that cannot be unsealed.
// Unlike the webhook-config warning this is not gated: an oauth_states row is
// read a handful of times in its 30-minute life, not once per event.
func (s *Store) warnPendingUnreadable(reason string) {
	if s.log != nil {
		s.log.Warn("pending webhook unreadable", "reason", reason)
	}
}

func (s *Store) decodePendingWebhook(raw string) *PendingWebhook {
	if raw == "" {
		return nil
	}
	if sealed, ok := strings.CutPrefix(raw, pendingWebhookSealedPrefix); ok {
		// A sealed blob we cannot open means the hook the developer paid for at
		// mint time will never be bound to the account the callback creates.
		// Nothing downstream can tell that apart from "no hook was requested",
		// so say so here — reason only, never the token.
		if s.sealKey == nil {
			s.warnPendingUnreadable("no seal key")
			return nil
		}
		opened, err := secretbox.Open(s.sealKey, sealed)
		if err != nil {
			s.warnPendingUnreadable("open failed")
			return nil
		}
		raw = opened
	}
	var w PendingWebhook
	if err := json.Unmarshal([]byte(raw), &w); err != nil {
		return nil
	}
	return &w
}

func (s *Store) SaveOAuthState(o OAuthState) error {
	wh, err := s.encodePendingWebhook(o.Webhook)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`
		INSERT INTO oauth_states (state, developer_id, provider, verifier, success_url, failure_url, notify_url, webhook_json, created_at, expires_at)
		VALUES (?,?,?,?,?,?,?,?,?,?)`,
		o.State, o.DeveloperID, o.Provider, o.Verifier, o.SuccessURL, o.FailureURL, o.NotifyURL,
		wh, time.Now().Unix(), o.ExpiresAt.Unix())
	return err
}

// SetOAuthConsent records that the end user accepted the linker disclosure on
// the connect page, which is what /connect/{state}/qr requires before it will
// start a pairing session.
func (s *Store) SetOAuthConsent(state string, at time.Time) error {
	_, err := s.db.Exec(`UPDATE oauth_states SET consented_at = ? WHERE state = ?`, at.Unix(), state)
	return err
}

// SetOAuthStateExpiry backdates (or extends) a pending connect attempt's
// expiry. Production code never needs to move an expiry once minted; this
// exists so tests can simulate a connect link expiring out from under an
// in-flight pairing session without reaching into the database directly.
func (s *Store) SetOAuthStateExpiry(state string, at time.Time) error {
	_, err := s.db.Exec(`UPDATE oauth_states SET expires_at = ? WHERE state = ?`, at.Unix(), state)
	return err
}

// TakeOAuthState consumes the state single-use, which is what makes it a real
// CSRF defence rather than decoration.
func (s *Store) TakeOAuthState(state string) (OAuthState, error) {
	var o OAuthState
	var exp int64
	var wh string
	var consented sql.NullInt64
	err := s.db.QueryRow(`
		SELECT state, developer_id, provider, verifier, success_url, failure_url, notify_url, webhook_json, expires_at, consented_at
		FROM oauth_states WHERE state = ?`, state).
		Scan(&o.State, &o.DeveloperID, &o.Provider, &o.Verifier, &o.SuccessURL, &o.FailureURL, &o.NotifyURL, &wh, &exp, &consented)
	o.Webhook = s.decodePendingWebhook(wh)
	if errors.Is(err, sql.ErrNoRows) {
		return o, ErrNotFound
	}
	if err != nil {
		return o, err
	}
	if consented.Valid {
		t := time.Unix(consented.Int64, 0).UTC()
		o.ConsentedAt = &t
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
	var consented sql.NullInt64
	err := s.db.QueryRow(`
		SELECT state, developer_id, provider, verifier, success_url, failure_url, notify_url, webhook_json, expires_at, consented_at
		FROM oauth_states WHERE state = ?`, state).
		Scan(&o.State, &o.DeveloperID, &o.Provider, &o.Verifier, &o.SuccessURL, &o.FailureURL, &o.NotifyURL, &wh, &exp, &consented)
	o.Webhook = s.decodePendingWebhook(wh)
	if errors.Is(err, sql.ErrNoRows) {
		return o, ErrNotFound
	}
	if consented.Valid {
		t := time.Unix(consented.Int64, 0).UTC()
		o.ConsentedAt = &t
	}
	o.ExpiresAt = time.Unix(exp, 0).UTC()
	return o, err
}
