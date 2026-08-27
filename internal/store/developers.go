package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/gauravrautela/unified-messaging/internal/model"
)

// ErrConflict is returned when a unique constraint rejects a write.
var ErrConflict = errors.New("conflict")

// ---------- developers ----------

func (s *Store) CreateDeveloper(d model.Developer, passwordHash string) error {
	if d.CreatedAt.IsZero() {
		d.CreatedAt = time.Now().UTC()
	}
	_, err := s.db.Exec(`
		INSERT INTO developers (id, email, password_hash, name, created_at) VALUES (?,?,?,?,?)`,
		d.ID, strings.ToLower(strings.TrimSpace(d.Email)), passwordHash, d.Name, d.CreatedAt.Unix())
	if err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed") {
		return ErrConflict
	}
	return err
}

// DeveloperByEmail returns the developer and their password hash. The hash
// leaves the store only to internal/auth.
func (s *Store) DeveloperByEmail(email string) (model.Developer, string, error) {
	var d model.Developer
	var hash string
	var created int64
	var rdJSON string
	err := s.db.QueryRow(`
		SELECT id, email, name, password_hash, created_at, redirect_domains_json FROM developers WHERE email = ?`,
		strings.ToLower(strings.TrimSpace(email))).Scan(&d.ID, &d.Email, &d.Name, &hash, &created, &rdJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return d, "", ErrNotFound
	}
	if err != nil {
		return d, "", err
	}
	d.CreatedAt = time.Unix(created, 0).UTC()
	d.RedirectDomains = decodeRedirectDomains(rdJSON)
	return d, hash, nil
}

func (s *Store) GetDeveloper(id string) (model.Developer, error) {
	var d model.Developer
	var created int64
	var rdJSON string
	err := s.db.QueryRow(`SELECT id, email, name, created_at, redirect_domains_json FROM developers WHERE id = ?`, id).
		Scan(&d.ID, &d.Email, &d.Name, &created, &rdJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return d, ErrNotFound
	}
	if err != nil {
		return d, err
	}
	d.CreatedAt = time.Unix(created, 0).UTC()
	d.RedirectDomains = decodeRedirectDomains(rdJSON)
	return d, nil
}

// GetRedirectDomains returns the hosted-auth redirect allowlist for a
// developer. Unset (fresh row, pre-migration default) reads as an empty,
// non-nil slice.
func (s *Store) GetRedirectDomains(developerID string) ([]string, error) {
	var rdJSON string
	err := s.db.QueryRow(`SELECT redirect_domains_json FROM developers WHERE id = ?`, developerID).Scan(&rdJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return decodeRedirectDomains(rdJSON), nil
}

// SetRedirectDomains replaces a developer's hosted-auth redirect allowlist.
// Validation of each entry's shape happens in internal/api; this is a plain
// JSON-encoded write.
func (s *Store) SetRedirectDomains(developerID string, domains []string) error {
	if domains == nil {
		domains = []string{}
	}
	raw, err := json.Marshal(domains)
	if err != nil {
		return err
	}
	res, err := s.db.Exec(`UPDATE developers SET redirect_domains_json = ? WHERE id = ?`, string(raw), developerID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// decodeRedirectDomains turns the stored JSON column into a slice that is
// always non-nil, so callers (not least JSON responses) never have to
// special-case nil vs empty.
func decodeRedirectDomains(raw string) []string {
	var out []string
	if raw != "" {
		_ = json.Unmarshal([]byte(raw), &out)
	}
	if out == nil {
		out = []string{}
	}
	return out
}

// DeveloperPasswordHash returns the stored bcrypt hash. Like
// DeveloperByEmail, the hash leaves the store only to internal/auth.
func (s *Store) DeveloperPasswordHash(developerID string) (string, error) {
	var hash string
	err := s.db.QueryRow(`SELECT password_hash FROM developers WHERE id = ?`, developerID).Scan(&hash)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return hash, err
}

// UpdatePassword replaces the stored hash. Callers are responsible for
// invalidating sessions afterwards; see DeleteSessionsExcept.
func (s *Store) UpdatePassword(developerID, hash string) error {
	_, err := s.db.Exec(`UPDATE developers SET password_hash = ? WHERE id = ?`, hash, developerID)
	return err
}

// ---------- sessions ----------
//
// Every function here is keyed by a *hash* of the cookie value, never the
// cookie value itself: the sessions table holds no credential that could be
// replayed by anyone who reads it. internal/auth owns the hashing, so the id
// column is opaque to this package — but the parameter is named hash to keep
// the contract visible at every call site.

func (s *Store) CreateSession(hash, developerID string, expiresAt time.Time) error {
	_, err := s.db.Exec(`
		INSERT INTO sessions (id, developer_id, created_at, expires_at) VALUES (?,?,?,?)`,
		hash, developerID, time.Now().Unix(), expiresAt.Unix())
	return err
}

// SessionDeveloper resolves a live session, reporting both when it expires
// and when it was created — the second is what an absolute-lifetime rule is
// measured against, and sliding expiry cannot forge it.
//
// Expired sessions read as ErrNotFound; they are not deleted here so a slow
// clock skew is recoverable.
func (s *Store) SessionDeveloper(hash string, now time.Time) (model.Developer, time.Time, time.Time, error) {
	var d model.Developer
	var devCreated, sessCreated, exp int64
	var rdJSON string
	err := s.db.QueryRow(`
		SELECT d.id, d.email, d.name, d.created_at, d.redirect_domains_json, s.expires_at, s.created_at
		FROM sessions s JOIN developers d ON d.id = s.developer_id
		WHERE s.id = ? AND s.expires_at > ?`, hash, now.Unix()).
		Scan(&d.ID, &d.Email, &d.Name, &devCreated, &rdJSON, &exp, &sessCreated)
	if errors.Is(err, sql.ErrNoRows) {
		return d, time.Time{}, time.Time{}, ErrNotFound
	}
	if err != nil {
		return d, time.Time{}, time.Time{}, err
	}
	d.CreatedAt = time.Unix(devCreated, 0).UTC()
	d.RedirectDomains = decodeRedirectDomains(rdJSON)
	return d, time.Unix(exp, 0).UTC(), time.Unix(sessCreated, 0).UTC(), nil
}

func (s *Store) ExtendSession(hash string, expiresAt time.Time) error {
	_, err := s.db.Exec(`UPDATE sessions SET expires_at = ? WHERE id = ?`, expiresAt.Unix(), hash)
	return err
}

func (s *Store) DeleteSession(hash string) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE id = ?`, hash)
	return err
}

// DeleteSessionsExcept is "sign out everywhere else": it drops every session
// this developer holds but the one identified by keepHash. Passing a hash
// that matches nothing (or "") signs them out everywhere, full stop.
func (s *Store) DeleteSessionsExcept(developerID, keepHash string) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE developer_id = ? AND id <> ?`, developerID, keepHash)
	return err
}

// ---------- API keys ----------

func (s *Store) CreateAPIKey(k model.APIKey, developerID, hash string) error {
	_, err := s.db.Exec(`
		INSERT INTO api_keys (id, developer_id, name, prefix, hash, created_at) VALUES (?,?,?,?,?,?)`,
		k.ID, developerID, k.Name, k.Prefix, hash, k.CreatedAt.Unix())
	return err
}

// DeveloperByKeyHash resolves a live (unrevoked) key to its owner.
func (s *Store) DeveloperByKeyHash(hash string) (model.Developer, model.APIKey, error) {
	var d model.Developer
	var k model.APIKey
	var dCreated, kCreated int64
	var rdJSON string
	var last, revoked sql.NullInt64
	err := s.db.QueryRow(`
		SELECT d.id, d.email, d.name, d.created_at, d.redirect_domains_json, k.id, k.name, k.prefix, k.created_at, k.last_used_at, k.revoked_at
		FROM api_keys k JOIN developers d ON d.id = k.developer_id
		WHERE k.hash = ? AND k.revoked_at IS NULL`, hash).
		Scan(&d.ID, &d.Email, &d.Name, &dCreated, &rdJSON, &k.ID, &k.Name, &k.Prefix, &kCreated, &last, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return d, k, ErrNotFound
	}
	if err != nil {
		return d, k, err
	}
	d.CreatedAt = time.Unix(dCreated, 0).UTC()
	d.RedirectDomains = decodeRedirectDomains(rdJSON)
	k.CreatedAt = time.Unix(kCreated, 0).UTC()
	k.LastUsedAt = nullTime(last)
	k.RevokedAt = nullTime(revoked)
	return d, k, nil
}

func (s *Store) TouchAPIKey(id string, at time.Time) error {
	_, err := s.db.Exec(`UPDATE api_keys SET last_used_at = ? WHERE id = ?`, at.Unix(), id)
	return err
}

func (s *Store) ListAPIKeys(developerID string) ([]model.APIKey, error) {
	rows, err := s.db.Query(`
		SELECT id, name, prefix, created_at, last_used_at, revoked_at
		FROM api_keys WHERE developer_id = ? ORDER BY created_at`, developerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.APIKey{}
	for rows.Next() {
		var k model.APIKey
		var created int64
		var last, revoked sql.NullInt64
		if err := rows.Scan(&k.ID, &k.Name, &k.Prefix, &created, &last, &revoked); err != nil {
			return nil, err
		}
		k.CreatedAt = time.Unix(created, 0).UTC()
		k.LastUsedAt = nullTime(last)
		k.RevokedAt = nullTime(revoked)
		out = append(out, k)
	}
	return out, rows.Err()
}

// RevokeAPIKey soft-revokes a key the developer owns. Revoking twice is a
// no-op that still succeeds.
func (s *Store) RevokeAPIKey(developerID, id string, at time.Time) error {
	res, err := s.db.Exec(`
		UPDATE api_keys SET revoked_at = COALESCE(revoked_at, ?) WHERE id = ? AND developer_id = ?`,
		at.Unix(), id, developerID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func nullTime(n sql.NullInt64) *time.Time {
	if !n.Valid {
		return nil
	}
	t := time.Unix(n.Int64, 0).UTC()
	return &t
}
