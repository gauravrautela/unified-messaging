// Package store is the POC's persistence layer: connected accounts, sealed
// tokens, per-folder delta state, and the locally synced mail cache.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gauravrautela/unified-messaging/internal/model"
	// database/sql drivers: "sqlite" (modernc, cgo-free) and "pgx" (Postgres).
	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"
)

var ErrNotFound = errors.New("not found")

var ErrPreTenancy = errors.New("database predates multi-tenancy")

type Store struct {
	db *sql.DB
	// d is the SQL flavour this store speaks: it renders the schema, supplies
	// the migration list, and rebinds ? placeholders for the engine in use.
	d *dialect
	// log is nil until SetLogger is called. Tests never set it, so the store
	// stays silent unless a process wires one in.
	log *slog.Logger
	// sealKey seals per-hook credentials (a Telegram bot token). Nil until
	// SetSealKey; saving a hook that needs sealing without it is an error.
	sealKey []byte
	// warnedConfig remembers which webhook ids have already had their
	// unreadable config reported. ListWebhooksFor runs on every dispatched
	// event, so without this one un-openable row is a WARN per event forever;
	// the spec asks for it once. Later encounters drop to DEBUG.
	warnedConfig sync.Map // webhook id -> struct{}
}

// SetLogger attaches the logger the hot query paths trace to. Pass one already
// tagged with component=store.
func (s *Store) SetLogger(l *slog.Logger) { s.log = l }

// SetSealKey installs the key used to seal per-hook credentials at rest. It
// is the same TOKEN_ENCRYPTION_KEY the account manager uses for OAuth tokens.
func (s *Store) SetSealKey(key []byte) { s.sealKey = key }

// DB exposes the underlying handle for a provider that needs to keep its own
// tables in the same SQLite file — the WhatsApp adapter's whatsmeow device
// store, notably — rather than opening a second connection to it.
func (s *Store) DB() *sql.DB { return s.db }

// trace records one query at DEBUG: which operation, how long it took, and the
// ids involved. Never called with a token, secret, or hash column.
func (s *Store) trace(op string, start time.Time, kv ...any) {
	if s.log == nil {
		return
	}
	s.log.Debug("query", append([]any{"op", op, "dur", time.Since(start).Round(time.Microsecond)}, kv...)...)
}

// Open connects the store to driver ("sqlite" — the default — or "postgres")
// at dsn: a file path for SQLite, a postgres:// URL for Postgres. The schema
// and the additive migrations are applied on the way in, for both engines.
func Open(driver, dsn string) (*Store, error) {
	var (
		d   *dialect
		db  *sql.DB
		err error
	)
	switch driver {
	case "sqlite", "":
		d = sqliteDialect()
		if err := ensureSQLiteFile(dsn); err != nil {
			return nil, err
		}
		// foreign_keys is per-connection state, so it belongs in the DSN rather
		// than in a one-off PRAGMA: a PRAGMA in the migration only covers whichever
		// connection ran it, which today is every connection solely because the
		// pool is capped at one. In the DSN it holds for any connection the pool
		// ever opens, whatever that cap becomes.
		db, err = sql.Open("sqlite", dsn+"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
		if err != nil {
			return nil, err
		}
		// modernc's driver serializes fine, but a single writer avoids SQLITE_BUSY
		// churn between the sync loop and API handlers.
		db.SetMaxOpenConns(1)

		// Refuse a database from before tenancy rather than failing on the first
		// query. We never delete on the operator's behalf. Only SQLite can hold
		// one: Postgres support arrived after tenancy did.
		if old, err := preTenancy(db); err != nil {
			db.Close()
			return nil, err
		} else if old {
			db.Close()
			return nil, &preTenancyError{path: dsn}
		}
	case "postgres":
		d = postgresDialect()
		db, err = sql.Open("pgx", dsn)
		if err != nil {
			return nil, err
		}
		// A managed Postgres (Supabase's pooler included) caps connections well
		// below what an unbounded pool will open. These are conservative
		// defaults; the process overrides the cap via SetMaxOpenConns.
		db.SetMaxOpenConns(10)
		db.SetMaxIdleConns(2)
		db.SetConnMaxLifetime(30 * time.Minute)
	default:
		return nil, fmt.Errorf("store: unknown driver %q (want \"sqlite\" or \"postgres\")", driver)
	}

	// One statement at a time: Postgres' extended protocol refuses a
	// multi-statement Exec, and SQLite does not mind either way.
	for _, stmt := range splitStatements(d.schema) {
		if _, err := db.Exec(stmt); err != nil {
			db.Close()
			return nil, fmt.Errorf("migrate: %w", err)
		}
	}
	for _, m := range d.migrate {
		if _, err := db.Exec(m); err != nil && !(d.name == "sqlite" && strings.Contains(err.Error(), "duplicate column")) {
			db.Close()
			return nil, fmt.Errorf("migrate: %w", err)
		}
	}
	return &Store{db: db, d: d}, nil
}

// ensureSQLiteFile creates the database file 0600 before the driver can, and
// tightens one that already exists. The file holds sealed OAuth tokens and
// webhook secrets: creating it up front leaves no window where the umask's
// default mode is on disk, and an existing file that predates this hardening
// (or was created under a laxer umask) is not trusted as it stands.
func ensureSQLiteFile(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	f.Close()
	if fi, err := os.Stat(path); err == nil && fi.Mode().Perm()&0o077 != 0 {
		_ = os.Chmod(path, 0o600)
	}
	return nil
}

// q renders a query written with ? placeholders for the engine in use: an
// identity on SQLite, $1…$n on Postgres.
func (s *Store) q(query string) string { return s.d.rebind(query) }

// Dialect is the engine's name as whatsmeow's sqlstore spells it, for a
// provider keeping its own tables in this database.
func (s *Store) Dialect() string { return s.d.wmName }

// DriverName is "sqlite" or "postgres" — the value Open was given.
func (s *Store) DriverName() string { return s.d.name }

// SetMaxOpenConns caps the connection pool. It is ignored on SQLite, which is
// deliberately pinned to a single writer.
func (s *Store) SetMaxOpenConns(n int) {
	if s.d.name == "postgres" && n > 0 {
		s.db.SetMaxOpenConns(n)
	}
}

// Ping checks the database is reachable. On SQLite that is nearly free; on
// Postgres it is the first thing worth knowing at startup.
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

// preTenancyError carries the operator-facing message while still matching
// ErrPreTenancy under errors.Is.
type preTenancyError struct{ path string }

func (e *preTenancyError) Error() string {
	return fmt.Sprintf("database %s predates multi-tenancy; delete it (and its -wal/-shm files) and reconnect your mailboxes", e.path)
}

func (e *preTenancyError) Is(target error) bool { return target == ErrPreTenancy }

// preTenancy reports whether an accounts table exists without developer_id.
func preTenancy(db *sql.DB) (bool, error) {
	rows, err := db.Query(`PRAGMA table_info(accounts)`)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	seen := false
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			return false, err
		}
		seen = true
		if name == "developer_id" {
			return false, nil
		}
	}
	return seen, rows.Err()
}

func (s *Store) Close() error { return s.db.Close() }

// ---------- accounts ----------

func (s *Store) UpsertAccount(a model.Account) error {
	now := time.Now().Unix()
	kind := a.Kind
	if kind == "" {
		kind = model.AccountKindMail
	}
	_, err := s.db.Exec(s.q(`
		INSERT INTO accounts (id, developer_id, kind, provider, email, name, status, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?)
		ON CONFLICT(developer_id, email) DO UPDATE SET
		  name = excluded.name, status = excluded.status, updated_at = excluded.updated_at`),
		a.ID, a.DeveloperID, kind, a.Provider, a.Email, a.Name, a.Status, now, now)
	return err
}

// AccountIDByEmail lets the OAuth callback reconnect an existing mailbox
// instead of creating a duplicate account row — within one developer.
func (s *Store) AccountIDByEmail(developerID, email string) (string, error) {
	var id string
	err := s.db.QueryRow(s.q(`SELECT id FROM accounts WHERE developer_id = ? AND email = ?`),
		developerID, email).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return id, err
}

const accountSelect = `
	SELECT id, developer_id, kind, provider, email, name, status, created_at, updated_at, last_synced_at
	FROM accounts`

// GetAccount is the tenant-scoped read every API handler uses. A row owned
// by another developer is ErrNotFound, not an authorization error, so ids
// cannot be probed across tenants.
func (s *Store) GetAccount(developerID, id string) (model.Account, error) {
	defer s.trace("GetAccount", time.Now(), "developer_id", developerID, "account_id", id)
	return scanAccount(s.db.QueryRow(s.q(accountSelect+` WHERE developer_id = ? AND id = ?`), developerID, id))
}

// GetAnyAccount is UNSCOPED: for internal callers (sync, token custody,
// push) that hold an account id and no tenant. Never call from a handler.
func (s *Store) GetAnyAccount(id string) (model.Account, error) {
	return scanAccount(s.db.QueryRow(s.q(accountSelect+` WHERE id = ?`), id))
}

func (s *Store) ListAccounts(developerID string) ([]model.Account, error) {
	return s.queryAccounts(accountSelect+` WHERE developer_id = ? ORDER BY created_at`, developerID)
}

// ListAllAccounts is UNSCOPED: for the poll and subscription loops.
func (s *Store) ListAllAccounts() ([]model.Account, error) {
	return s.queryAccounts(accountSelect + ` ORDER BY created_at`)
}

func (s *Store) queryAccounts(q string, args ...any) ([]model.Account, error) {
	rows, err := s.db.Query(s.q(q), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Account{}
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) SetAccountStatus(id, status string) error {
	_, err := s.db.Exec(s.q(`UPDATE accounts SET status = ?, updated_at = ? WHERE id = ?`),
		status, time.Now().Unix(), id)
	return err
}

func (s *Store) MarkSynced(id string) error {
	_, err := s.db.Exec(s.q(`UPDATE accounts SET last_synced_at = ? WHERE id = ?`), time.Now().Unix(), id)
	return err
}

// DeleteAccount removes an account and everything scoped to it: its
// account-bound webhooks, and any deliveries already queued against it
// (which otherwise keep a deleted tenant's full message payloads around and
// can go on retrying against a hook that no longer applies to anyone). All
// three deletes run in one transaction so a crash mid-way never leaves
// deliveries or webhooks orphaned from a half-deleted account.
func (s *Store) DeleteAccount(id string) error {
	return s.inTx(func(tx *sql.Tx) error {
		if _, err := tx.Exec(s.q(`DELETE FROM webhook_deliveries WHERE account_id = ?`), id); err != nil {
			return err
		}
		if _, err := tx.Exec(s.q(`DELETE FROM webhooks WHERE account_id = ?`), id); err != nil {
			return err
		}
		_, err := tx.Exec(s.q(`DELETE FROM accounts WHERE id = ?`), id)
		return err
	})
}

type scanner interface{ Scan(...any) error }

func scanAccount(r scanner) (model.Account, error) {
	var a model.Account
	var created, updated int64
	var synced sql.NullInt64
	err := r.Scan(&a.ID, &a.DeveloperID, &a.Kind, &a.Provider, &a.Email, &a.Name, &a.Status, &created, &updated, &synced)
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrNotFound
	}
	if err != nil {
		return a, err
	}
	a.CreatedAt = time.Unix(created, 0).UTC()
	a.UpdatedAt = time.Unix(updated, 0).UTC()
	if synced.Valid {
		t := time.Unix(synced.Int64, 0).UTC()
		a.LastSyncedAt = &t
	}
	a.Identifier = a.Email
	return a, nil
}

// ListChatAccounts is UNSCOPED: the chat runtime attaches every live chat
// account at boot.
func (s *Store) ListChatAccounts() ([]model.Account, error) {
	return s.queryAccounts(accountSelect+` WHERE kind = ? AND status = ? ORDER BY created_at`, model.AccountKindChat, model.AccountOK)
}

// ---------- tokens ----------

type TokenRecord struct {
	AccessToken     string
	AccessExpiresAt time.Time
	RefreshTokenEnc string
	Scope           string
}

func (s *Store) SaveTokens(accountID string, t TokenRecord) error {
	_, err := s.db.Exec(s.q(`
		INSERT INTO tokens (account_id, access_token, access_expires_at, refresh_token_enc, scope)
		VALUES (?,?,?,?,?)
		ON CONFLICT(account_id) DO UPDATE SET
		  access_token = excluded.access_token,
		  access_expires_at = excluded.access_expires_at,
		  refresh_token_enc = excluded.refresh_token_enc,
		  scope = excluded.scope`),
		accountID, t.AccessToken, t.AccessExpiresAt.Unix(), t.RefreshTokenEnc, t.Scope)
	return err
}

func (s *Store) GetTokens(accountID string) (TokenRecord, error) {
	var t TokenRecord
	var exp int64
	err := s.db.QueryRow(s.q(`
		SELECT access_token, access_expires_at, refresh_token_enc, scope
		FROM tokens WHERE account_id = ?`), accountID).
		Scan(&t.AccessToken, &exp, &t.RefreshTokenEnc, &t.Scope)
	if errors.Is(err, sql.ErrNoRows) {
		return t, ErrNotFound
	}
	t.AccessExpiresAt = time.Unix(exp, 0).UTC()
	return t, err
}

// ---------- folders ----------

func (s *Store) UpsertFolder(f model.Folder) error {
	_, err := s.db.Exec(s.q(`
		INSERT INTO folders (account_id, id, name, parent_id, role, total_count, unread_count)
		VALUES (?,?,?,?,?,?,?)
		ON CONFLICT(account_id, id) DO UPDATE SET
		  name = excluded.name, parent_id = excluded.parent_id, role = excluded.role,
		  total_count = excluded.total_count, unread_count = excluded.unread_count`),
		f.AccountID, f.ID, f.Name, f.ParentID, f.Role, f.TotalCount, f.UnreadCount)
	return err
}

func (s *Store) ListFolders(accountID string) ([]model.Folder, error) {
	rows, err := s.db.Query(s.q(`
		SELECT account_id, id, name, parent_id, role, total_count, unread_count
		FROM folders WHERE account_id = ? ORDER BY name`), accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Folder{}
	for rows.Next() {
		var f model.Folder
		if err := rows.Scan(&f.AccountID, &f.ID, &f.Name, &f.ParentID, &f.Role,
			&f.TotalCount, &f.UnreadCount); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func (s *Store) DeleteFolder(accountID, folderID string) error {
	_, err := s.db.Exec(s.q(`DELETE FROM folders WHERE account_id = ? AND id = ?`), accountID, folderID)
	return err
}

// ---------- sync cursors ----------

// GetCursor returns the stored cursor for a scope. A missing row is not an
// error: it means "never synced", which is exactly the empty cursor.
func (s *Store) GetCursor(accountID, scopeID string) (string, error) {
	var cursor string
	err := s.db.QueryRow(s.q(`SELECT cursor FROM sync_state WHERE account_id = ? AND scope_id = ?`),
		accountID, scopeID).Scan(&cursor)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return cursor, err
}

func (s *Store) SetCursor(accountID, scopeID, cursor string) error {
	_, err := s.db.Exec(s.q(`
		INSERT INTO sync_state (account_id, scope_id, cursor, updated_at)
		VALUES (?,?,?,?)
		ON CONFLICT(account_id, scope_id) DO UPDATE SET
		  cursor = excluded.cursor, updated_at = excluded.updated_at`),
		accountID, scopeID, cursor, time.Now().Unix())
	return err
}

// ---------- emails ----------

// UpsertEmail is deliberately idempotent: Graph delta replays the same message
// across rounds, so every write path has to tolerate seeing it again.
func (s *Store) UpsertEmail(e model.Email) error {
	defer s.trace("UpsertEmail", time.Now(), "account_id", e.AccountID, "email_id", e.ID)
	to, _ := json.Marshal(e.To)
	cc, _ := json.Marshal(e.Cc)
	bcc, _ := json.Marshal(e.Bcc)
	rt, _ := json.Marshal(e.ReplyTo)
	att, _ := json.Marshal(e.Attachments)
	_, err := s.db.Exec(s.q(`
		INSERT INTO emails (account_id, id, thread_id, folder_id, subject, from_name, from_email,
		  to_json, cc_json, bcc_json, reply_to_json, date, snippet, body, body_type,
		  read, flagged, draft, has_attachments, internet_message_id, attachments_json, stored_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(account_id, id) DO UPDATE SET
		  thread_id = excluded.thread_id, folder_id = excluded.folder_id,
		  -- Content and participants freeze once evicted. A resync, or any
		  -- delta carrying full message properties, would otherwise refill a
		  -- row the tenant's retention policy has already emptied — silently,
		  -- unattended, for every message.
		  subject       = CASE WHEN emails.content_evicted_at IS NOT NULL THEN emails.subject       ELSE excluded.subject END,
		  from_name     = CASE WHEN emails.content_evicted_at IS NOT NULL THEN emails.from_name     ELSE excluded.from_name END,
		  from_email    = CASE WHEN emails.content_evicted_at IS NOT NULL THEN emails.from_email    ELSE excluded.from_email END,
		  to_json       = CASE WHEN emails.content_evicted_at IS NOT NULL THEN emails.to_json       ELSE excluded.to_json END,
		  cc_json       = CASE WHEN emails.content_evicted_at IS NOT NULL THEN emails.cc_json       ELSE excluded.cc_json END,
		  bcc_json      = CASE WHEN emails.content_evicted_at IS NOT NULL THEN emails.bcc_json      ELSE excluded.bcc_json END,
		  reply_to_json = CASE WHEN emails.content_evicted_at IS NOT NULL THEN emails.reply_to_json ELSE excluded.reply_to_json END,
		  snippet       = CASE WHEN emails.content_evicted_at IS NOT NULL THEN emails.snippet       ELSE excluded.snippet END,
		  date = excluded.date,
		  -- a delta "updated" event may carry only changed fields; never blank a
		  -- body we already have just because this page omitted it
		  body = CASE WHEN emails.content_evicted_at IS NOT NULL THEN emails.body
		              WHEN excluded.body = '' THEN emails.body ELSE excluded.body END,
		  body_type = CASE WHEN emails.content_evicted_at IS NOT NULL THEN emails.body_type
		                   WHEN excluded.body_type = '' THEN emails.body_type ELSE excluded.body_type END,
		  read = excluded.read, flagged = excluded.flagged, draft = excluded.draft,
		  has_attachments = excluded.has_attachments,
		  internet_message_id = excluded.internet_message_id,
		  attachments_json = CASE WHEN emails.content_evicted_at IS NOT NULL THEN emails.attachments_json
		                          WHEN excluded.attachments_json = '[]' THEN emails.attachments_json
		                          ELSE excluded.attachments_json END`),
		e.AccountID, e.ID, e.ThreadID, e.FolderID, e.Subject, e.From.Name, e.From.Email,
		string(to), string(cc), string(bcc), string(rt), e.Date.Unix(), e.Snippet, e.Body,
		e.BodyType, b2i(e.Read), b2i(e.Flagged), b2i(e.Draft), b2i(e.HasAttachments),
		e.InternetMessageID, string(att), time.Now().Unix())
	return err
}

// EvictEmailContent blanks a message's content and its participants, keeping
// the identifiers, timestamps and flags that sync depends on — EmailExists in
// particular, which is the only thing stopping a resync from re-firing
// mail_received for a mailbox we have already delivered.
//
// The `content_evicted_at IS NULL` clause makes it idempotent and preserves the
// original eviction time, so a second call cannot quietly extend the audit
// trail forward.
func (s *Store) EvictEmailContent(accountID, id string, at time.Time) error {
	defer s.trace("EvictEmailContent", time.Now(), "account_id", accountID, "email_id", id)
	_, err := s.db.Exec(s.q(`
		UPDATE emails SET
		  subject = '', snippet = '', body = '', body_type = '',
		  from_name = '', from_email = '',
		  to_json = '[]', cc_json = '[]', bcc_json = '[]', reply_to_json = '[]',
		  attachments_json = '[]',
		  content_evicted_at = ?
		WHERE account_id = ? AND id = ? AND content_evicted_at IS NULL`),
		at.Unix(), accountID, id)
	return err
}

// EmailExists tells the sync loop whether a delta hit is genuinely new mail
// (worth a mail_received event) or a replay/update of something we already hold.
func (s *Store) EmailExists(accountID, id string) (bool, error) {
	var one int
	err := s.db.QueryRow(s.q(`SELECT 1 FROM emails WHERE account_id = ? AND id = ?`), accountID, id).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func (s *Store) DeleteEmail(accountID, id string) error {
	_, err := s.db.Exec(s.q(`DELETE FROM emails WHERE account_id = ? AND id = ?`), accountID, id)
	return err
}

func (s *Store) GetEmail(accountID, id string) (model.Email, error) {
	row := s.db.QueryRow(s.q(emailSelect+` WHERE account_id = ? AND id = ?`), accountID, id)
	return scanEmail(row)
}

type EmailQuery struct {
	AccountID string
	FolderID  string
	ThreadID  string
	Search    string
	Unread    *bool
	Limit     int
	Offset    int
}

func (s *Store) ListEmails(q EmailQuery) (result []model.Email, err error) {
	start := time.Now()
	// Search text is a caller-supplied string; only its presence is recorded.
	defer func() {
		s.trace("ListEmails", start, "account_id", q.AccountID, "folder_id", q.FolderID,
			"search", q.Search != "", "rows", len(result))
	}()

	where := []string{"account_id = ?"}
	args := []any{q.AccountID}
	if q.FolderID != "" {
		where = append(where, "folder_id = ?")
		args = append(args, q.FolderID)
	}
	if q.ThreadID != "" {
		where = append(where, "thread_id = ?")
		args = append(args, q.ThreadID)
	}
	if q.Unread != nil {
		where = append(where, "read = ?")
		args = append(args, b2i(!*q.Unread))
	}
	if q.Search != "" {
		where = append(where, "("+s.d.likeCI("subject")+" OR "+s.d.likeCI("snippet")+" OR "+s.d.likeCI("from_email")+")")
		like := "%" + escapeLike(q.Search) + "%"
		args = append(args, like, like, like)
	}
	if q.Limit <= 0 || q.Limit > 200 {
		q.Limit = 50
	}
	args = append(args, q.Limit, q.Offset)

	rows, err := s.db.Query(s.q(emailSelect+" WHERE "+strings.Join(where, " AND ")+
		" ORDER BY date DESC LIMIT ? OFFSET ?"), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Email{}
	for rows.Next() {
		e, err := scanEmail(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) ListThreads(accountID string, limit, offset int) ([]model.Thread, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.Query(s.q(`
		SELECT thread_id, MAX(date) AS last_date, COUNT(*) AS n,
		       SUM(CASE WHEN read = 0 THEN 1 ELSE 0 END) AS unread
		FROM emails WHERE account_id = ? AND thread_id != ''
		GROUP BY thread_id ORDER BY last_date DESC LIMIT ? OFFSET ?`),
		accountID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Thread{}
	for rows.Next() {
		var t model.Thread
		var last int64
		if err := rows.Scan(&t.ID, &last, &t.Count, &t.Unread); err != nil {
			return nil, err
		}
		t.AccountID = accountID
		t.LastDate = time.Unix(last, 0).UTC()
		// Subject of the newest message in the thread reads best as the label.
		_ = s.db.QueryRow(s.q(`SELECT subject FROM emails
			WHERE account_id = ? AND thread_id = ? ORDER BY date DESC LIMIT 1`),
			accountID, t.ID).Scan(&t.Subject)
		out = append(out, t)
	}
	return out, rows.Err()
}

const emailSelect = `
SELECT account_id, id, thread_id, folder_id, subject, from_name, from_email,
       to_json, cc_json, bcc_json, reply_to_json, date, snippet, body, body_type,
       read, flagged, draft, has_attachments, internet_message_id, attachments_json,
       content_evicted_at
FROM emails`

func scanEmail(r scanner) (model.Email, error) {
	var e model.Email
	var toJ, ccJ, bccJ, rtJ, attJ string
	var date int64
	var read, flagged, draft, hasAtt int
	var evictedAt sql.NullInt64
	err := r.Scan(&e.AccountID, &e.ID, &e.ThreadID, &e.FolderID, &e.Subject,
		&e.From.Name, &e.From.Email, &toJ, &ccJ, &bccJ, &rtJ, &date, &e.Snippet,
		&e.Body, &e.BodyType, &read, &flagged, &draft, &hasAtt,
		&e.InternetMessageID, &attJ, &evictedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return e, ErrNotFound
	}
	if err != nil {
		return e, err
	}
	_ = json.Unmarshal([]byte(toJ), &e.To)
	_ = json.Unmarshal([]byte(ccJ), &e.Cc)
	_ = json.Unmarshal([]byte(bccJ), &e.Bcc)
	_ = json.Unmarshal([]byte(rtJ), &e.ReplyTo)
	_ = json.Unmarshal([]byte(attJ), &e.Attachments)
	if e.Body != "" {
		e.BodyPlain = model.PlainText(e.Body, e.BodyType)
	}
	e.Date = time.Unix(date, 0).UTC()
	e.Read, e.Flagged, e.Draft, e.HasAttachments = read == 1, flagged == 1, draft == 1, hasAtt == 1
	e.ContentEvicted = evictedAt.Valid
	return e, nil
}

// escapeLike escapes a caller-supplied search string for safe use inside a
// LIKE pattern with ESCAPE '\': the backslash itself is escaped first so a
// literal one in the input can't be mistaken for an escape sequence, then '%'
// and '_' are escaped so they match themselves instead of acting as
// wildcards. Every LIKE ? built from user input pairs this with an
// `ESCAPE '\'` clause on the query.
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// nullUnix converts an optional time into a value fit for a nullable INTEGER
// column: nil stays nil, a set time becomes its unix seconds.
func nullUnix(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.Unix()
}
