// Package store is the POC's persistence layer: connected accounts, sealed
// tokens, per-folder delta state, and the locally synced mail cache.
package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gauravrautela/unified-messaging/internal/model"
	_ "modernc.org/sqlite"
)

var ErrNotFound = errors.New("not found")

var ErrPreTenancy = errors.New("database predates multi-tenancy")

type Store struct{ db *sql.DB }

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	// modernc's driver serializes fine, but a single writer avoids SQLITE_BUSY
	// churn between the sync loop and API handlers.
	db.SetMaxOpenConns(1)

	// Refuse a database from before tenancy rather than failing on the first
	// query. We never delete on the operator's behalf.
	if old, err := preTenancy(db); err != nil {
		return nil, err
	} else if old {
		db.Close()
		return nil, &preTenancyError{path: path}
	}

	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db}, nil
}

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
	_, err := s.db.Exec(`
		INSERT INTO accounts (id, provider, email, name, status, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?)
		ON CONFLICT(email) DO UPDATE SET
		  name = excluded.name, status = excluded.status, updated_at = excluded.updated_at`,
		a.ID, a.Provider, a.Email, a.Name, a.Status, now, now)
	return err
}

// AccountIDByEmail lets the OAuth callback reconnect an existing mailbox
// instead of creating a duplicate account row.
func (s *Store) AccountIDByEmail(email string) (string, error) {
	var id string
	err := s.db.QueryRow(`SELECT id FROM accounts WHERE email = ?`, email).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return id, err
}

func (s *Store) GetAccount(id string) (model.Account, error) {
	row := s.db.QueryRow(`
		SELECT id, provider, email, name, status, created_at, updated_at, last_synced_at
		FROM accounts WHERE id = ?`, id)
	return scanAccount(row)
}

func (s *Store) ListAccounts() ([]model.Account, error) {
	rows, err := s.db.Query(`
		SELECT id, provider, email, name, status, created_at, updated_at, last_synced_at
		FROM accounts ORDER BY created_at`)
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
	_, err := s.db.Exec(`UPDATE accounts SET status = ?, updated_at = ? WHERE id = ?`,
		status, time.Now().Unix(), id)
	return err
}

func (s *Store) MarkSynced(id string) error {
	_, err := s.db.Exec(`UPDATE accounts SET last_synced_at = ? WHERE id = ?`, time.Now().Unix(), id)
	return err
}

func (s *Store) DeleteAccount(id string) error {
	if _, err := s.db.Exec(`DELETE FROM webhooks WHERE account_id = ?`, id); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM accounts WHERE id = ?`, id)
	return err
}

type scanner interface{ Scan(...any) error }

func scanAccount(r scanner) (model.Account, error) {
	var a model.Account
	var created, updated int64
	var synced sql.NullInt64
	err := r.Scan(&a.ID, &a.Provider, &a.Email, &a.Name, &a.Status, &created, &updated, &synced)
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
	return a, nil
}

// ---------- tokens ----------

type TokenRecord struct {
	AccessToken     string
	AccessExpiresAt time.Time
	RefreshTokenEnc string
	Scope           string
}

func (s *Store) SaveTokens(accountID string, t TokenRecord) error {
	_, err := s.db.Exec(`
		INSERT INTO tokens (account_id, access_token, access_expires_at, refresh_token_enc, scope)
		VALUES (?,?,?,?,?)
		ON CONFLICT(account_id) DO UPDATE SET
		  access_token = excluded.access_token,
		  access_expires_at = excluded.access_expires_at,
		  refresh_token_enc = excluded.refresh_token_enc,
		  scope = excluded.scope`,
		accountID, t.AccessToken, t.AccessExpiresAt.Unix(), t.RefreshTokenEnc, t.Scope)
	return err
}

func (s *Store) GetTokens(accountID string) (TokenRecord, error) {
	var t TokenRecord
	var exp int64
	err := s.db.QueryRow(`
		SELECT access_token, access_expires_at, refresh_token_enc, scope
		FROM tokens WHERE account_id = ?`, accountID).
		Scan(&t.AccessToken, &exp, &t.RefreshTokenEnc, &t.Scope)
	if errors.Is(err, sql.ErrNoRows) {
		return t, ErrNotFound
	}
	t.AccessExpiresAt = time.Unix(exp, 0).UTC()
	return t, err
}

// ---------- folders ----------

func (s *Store) UpsertFolder(f model.Folder) error {
	_, err := s.db.Exec(`
		INSERT INTO folders (account_id, id, name, parent_id, role, total_count, unread_count)
		VALUES (?,?,?,?,?,?,?)
		ON CONFLICT(account_id, id) DO UPDATE SET
		  name = excluded.name, parent_id = excluded.parent_id, role = excluded.role,
		  total_count = excluded.total_count, unread_count = excluded.unread_count`,
		f.AccountID, f.ID, f.Name, f.ParentID, f.Role, f.TotalCount, f.UnreadCount)
	return err
}

func (s *Store) ListFolders(accountID string) ([]model.Folder, error) {
	rows, err := s.db.Query(`
		SELECT account_id, id, name, parent_id, role, total_count, unread_count
		FROM folders WHERE account_id = ? ORDER BY name`, accountID)
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
	_, err := s.db.Exec(`DELETE FROM folders WHERE account_id = ? AND id = ?`, accountID, folderID)
	return err
}

// ---------- sync cursors ----------

// GetCursor returns the stored cursor for a scope. A missing row is not an
// error: it means "never synced", which is exactly the empty cursor.
func (s *Store) GetCursor(accountID, scopeID string) (string, error) {
	var cursor string
	err := s.db.QueryRow(`SELECT cursor FROM sync_state WHERE account_id = ? AND scope_id = ?`,
		accountID, scopeID).Scan(&cursor)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return cursor, err
}

func (s *Store) SetCursor(accountID, scopeID, cursor string) error {
	_, err := s.db.Exec(`
		INSERT INTO sync_state (account_id, scope_id, cursor, updated_at)
		VALUES (?,?,?,?)
		ON CONFLICT(account_id, scope_id) DO UPDATE SET
		  cursor = excluded.cursor, updated_at = excluded.updated_at`,
		accountID, scopeID, cursor, time.Now().Unix())
	return err
}

// ---------- emails ----------

// UpsertEmail is deliberately idempotent: Graph delta replays the same message
// across rounds, so every write path has to tolerate seeing it again.
func (s *Store) UpsertEmail(e model.Email) error {
	to, _ := json.Marshal(e.To)
	cc, _ := json.Marshal(e.Cc)
	bcc, _ := json.Marshal(e.Bcc)
	rt, _ := json.Marshal(e.ReplyTo)
	att, _ := json.Marshal(e.Attachments)
	_, err := s.db.Exec(`
		INSERT INTO emails (account_id, id, thread_id, folder_id, subject, from_name, from_email,
		  to_json, cc_json, bcc_json, reply_to_json, date, snippet, body, body_type,
		  read, flagged, draft, has_attachments, internet_message_id, attachments_json)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(account_id, id) DO UPDATE SET
		  thread_id = excluded.thread_id, folder_id = excluded.folder_id,
		  subject = excluded.subject, from_name = excluded.from_name,
		  from_email = excluded.from_email, to_json = excluded.to_json,
		  cc_json = excluded.cc_json, bcc_json = excluded.bcc_json,
		  reply_to_json = excluded.reply_to_json, date = excluded.date,
		  snippet = excluded.snippet,
		  -- a delta "updated" event may carry only changed fields; never blank a
		  -- body we already have just because this page omitted it
		  body = CASE WHEN excluded.body = '' THEN emails.body ELSE excluded.body END,
		  body_type = CASE WHEN excluded.body_type = '' THEN emails.body_type ELSE excluded.body_type END,
		  read = excluded.read, flagged = excluded.flagged, draft = excluded.draft,
		  has_attachments = excluded.has_attachments,
		  internet_message_id = excluded.internet_message_id,
		  attachments_json = CASE WHEN excluded.attachments_json = '[]'
		    THEN emails.attachments_json ELSE excluded.attachments_json END`,
		e.AccountID, e.ID, e.ThreadID, e.FolderID, e.Subject, e.From.Name, e.From.Email,
		string(to), string(cc), string(bcc), string(rt), e.Date.Unix(), e.Snippet, e.Body,
		e.BodyType, b2i(e.Read), b2i(e.Flagged), b2i(e.Draft), b2i(e.HasAttachments),
		e.InternetMessageID, string(att))
	return err
}

// EmailExists tells the sync loop whether a delta hit is genuinely new mail
// (worth a mail_received event) or a replay/update of something we already hold.
func (s *Store) EmailExists(accountID, id string) (bool, error) {
	var one int
	err := s.db.QueryRow(`SELECT 1 FROM emails WHERE account_id = ? AND id = ?`, accountID, id).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func (s *Store) DeleteEmail(accountID, id string) error {
	_, err := s.db.Exec(`DELETE FROM emails WHERE account_id = ? AND id = ?`, accountID, id)
	return err
}

func (s *Store) GetEmail(accountID, id string) (model.Email, error) {
	row := s.db.QueryRow(emailSelect+` WHERE account_id = ? AND id = ?`, accountID, id)
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

func (s *Store) ListEmails(q EmailQuery) ([]model.Email, error) {
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
		where = append(where, "(subject LIKE ? OR snippet LIKE ? OR from_email LIKE ?)")
		like := "%" + q.Search + "%"
		args = append(args, like, like, like)
	}
	if q.Limit <= 0 || q.Limit > 200 {
		q.Limit = 50
	}
	args = append(args, q.Limit, q.Offset)

	rows, err := s.db.Query(emailSelect+" WHERE "+strings.Join(where, " AND ")+
		" ORDER BY date DESC LIMIT ? OFFSET ?", args...)
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
	rows, err := s.db.Query(`
		SELECT thread_id, MAX(date) AS last_date, COUNT(*) AS n,
		       SUM(CASE WHEN read = 0 THEN 1 ELSE 0 END) AS unread
		FROM emails WHERE account_id = ? AND thread_id != ''
		GROUP BY thread_id ORDER BY last_date DESC LIMIT ? OFFSET ?`,
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
		_ = s.db.QueryRow(`SELECT subject FROM emails
			WHERE account_id = ? AND thread_id = ? ORDER BY date DESC LIMIT 1`,
			accountID, t.ID).Scan(&t.Subject)
		out = append(out, t)
	}
	return out, rows.Err()
}

const emailSelect = `
SELECT account_id, id, thread_id, folder_id, subject, from_name, from_email,
       to_json, cc_json, bcc_json, reply_to_json, date, snippet, body, body_type,
       read, flagged, draft, has_attachments, internet_message_id, attachments_json
FROM emails`

func scanEmail(r scanner) (model.Email, error) {
	var e model.Email
	var toJ, ccJ, bccJ, rtJ, attJ string
	var date int64
	var read, flagged, draft, hasAtt int
	err := r.Scan(&e.AccountID, &e.ID, &e.ThreadID, &e.FolderID, &e.Subject,
		&e.From.Name, &e.From.Email, &toJ, &ccJ, &bccJ, &rtJ, &date, &e.Snippet,
		&e.Body, &e.BodyType, &read, &flagged, &draft, &hasAtt,
		&e.InternetMessageID, &attJ)
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
	return e, nil
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
