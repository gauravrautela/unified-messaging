package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/gauravrautela/unified-messaging/internal/model"
)

// inTx runs fn inside a transaction: commits on success, rolls back on any
// error (and re-panics after rolling back on a panic). The pool is capped at
// one connection (see Open), so a transaction holds the pool's only
// connection for its whole lifetime — a concurrent caller's db.Exec/Query
// blocks waiting for a connection rather than interleaving with fn's reads
// and writes. That is what makes a multi-statement read-modify-write (or a
// delete-then-insert loop) atomic here without any SQLite-specific locking.
func (s *Store) inTx(fn func(*sql.Tx) error) (err error) {
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
	}()
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// ---------- chats ----------

const chatSelect = `
	SELECT account_id, id, kind, name, unread_count, last_message_at, archived, muted
	FROM chats`

// UpsertChat creates or renames a chat. Runtime fields (unread_count,
// last_message_at, archived, muted) are owned by BumpChat/ClearUnread/
// SetChatFlags and are left untouched here so a metadata refresh from the
// provider never clobbers state the local reader has accumulated.
func (s *Store) UpsertChat(c model.Chat) error {
	_, err := s.db.Exec(s.q(`
		INSERT INTO chats (account_id, id, kind, name, unread_count, last_message_at, archived, muted)
		VALUES (?,?,?,?,?,?,?,?)
		ON CONFLICT(account_id, id) DO UPDATE SET
		  kind = excluded.kind, name = excluded.name`),
		c.AccountID, c.ID, c.Kind, c.Name, c.UnreadCount, nullUnix(c.LastMessageAt), b2i(c.Archived), b2i(c.Muted))
	return err
}

// GetChat returns a chat with its members resolved to attendees. A member
// with no attendee row yet still appears, keyed by its bare attendee id.
func (s *Store) GetChat(accountID, id string) (model.Chat, error) {
	c, err := scanChat(s.db.QueryRow(s.q(chatSelect+` WHERE account_id = ? AND id = ?`), accountID, id))
	if err != nil {
		return c, err
	}
	members, err := s.chatMembers(accountID, id)
	if err != nil {
		return c, err
	}
	c.Members = members
	return c, nil
}

func (s *Store) chatMembers(accountID, chatID string) ([]model.Attendee, error) {
	rows, err := s.db.Query(s.q(`
		SELECT cm.attendee_id, COALESCE(a.phone, ''), COALESCE(a.name, ''), COALESCE(a.is_self, 0)
		FROM chat_members cm
		LEFT JOIN attendees a ON a.account_id = cm.account_id AND a.id = cm.attendee_id
		WHERE cm.account_id = ? AND cm.chat_id = ?
		ORDER BY cm.attendee_id`), accountID, chatID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Attendee{}
	for rows.Next() {
		var a model.Attendee
		var isSelf int
		if err := rows.Scan(&a.ID, &a.Phone, &a.Name, &isSelf); err != nil {
			return nil, err
		}
		a.IsSelf = isSelf == 1
		out = append(out, a)
	}
	return out, rows.Err()
}

// ChatQuery filters ListChats. Zero values mean "no filter" except Limit,
// which defaults to 50.
type ChatQuery struct {
	AccountID string
	Kind      string
	Unread    *bool
	Search    string
	Limit     int
	Offset    int
}

// ListChats orders most-recently-active first; chats with no messages yet
// (last_message_at IS NULL) sort last.
func (s *Store) ListChats(q ChatQuery) ([]model.Chat, error) {
	where := []string{"account_id = ?"}
	args := []any{q.AccountID}
	if q.Kind != "" {
		where = append(where, "kind = ?")
		args = append(args, q.Kind)
	}
	if q.Unread != nil {
		if *q.Unread {
			where = append(where, "unread_count > 0")
		} else {
			where = append(where, "unread_count = 0")
		}
	}
	if q.Search != "" {
		where = append(where, s.d.likeCI("name"))
		args = append(args, "%"+escapeLike(q.Search)+"%")
	}
	limit := q.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	args = append(args, limit, q.Offset)

	rows, err := s.db.Query(s.q(chatSelect+" WHERE "+strings.Join(where, " AND ")+
		" ORDER BY last_message_at IS NULL, last_message_at DESC, id LIMIT ? OFFSET ?"), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Chat{}
	for rows.Next() {
		c, err := scanChat(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SetChatFlags updates archived/muted independently: a nil pointer leaves
// that flag untouched.
func (s *Store) SetChatFlags(accountID, id string, archived, muted *bool) error {
	sets := []string{}
	args := []any{}
	if archived != nil {
		sets = append(sets, "archived = ?")
		args = append(args, b2i(*archived))
	}
	if muted != nil {
		sets = append(sets, "muted = ?")
		args = append(args, b2i(*muted))
	}
	if len(sets) == 0 {
		return nil
	}
	args = append(args, accountID, id)
	_, err := s.db.Exec(s.q(`UPDATE chats SET `+strings.Join(sets, ", ")+` WHERE account_id = ? AND id = ?`), args...)
	return err
}

// BumpChat advances a chat's activity clock and unread count in one write,
// so a batch of incoming messages never leaves the two inconsistent.
func (s *Store) BumpChat(accountID, id string, at time.Time, unreadDelta int) error {
	_, err := s.db.Exec(s.q(`
		UPDATE chats SET last_message_at = ?, unread_count = unread_count + ?
		WHERE account_id = ? AND id = ?`), at.Unix(), unreadDelta, accountID, id)
	return err
}

func (s *Store) ClearUnread(accountID, id string) error {
	_, err := s.db.Exec(s.q(`UPDATE chats SET unread_count = 0 WHERE account_id = ? AND id = ?`), accountID, id)
	return err
}

func scanChat(r scanner) (model.Chat, error) {
	var c model.Chat
	var unread, archived, muted int
	var lastMsg sql.NullInt64
	err := r.Scan(&c.AccountID, &c.ID, &c.Kind, &c.Name, &unread, &lastMsg, &archived, &muted)
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrNotFound
	}
	if err != nil {
		return c, err
	}
	c.UnreadCount = unread
	if lastMsg.Valid {
		t := time.Unix(lastMsg.Int64, 0).UTC()
		c.LastMessageAt = &t
	}
	c.Archived = archived == 1
	c.Muted = muted == 1
	return c, nil
}

// ---------- attendees ----------

// UpsertAttendee inserts or refreshes an attendee's profile. IsSelf is
// caller-controlled: providers report exactly one self attendee per account.
func (s *Store) UpsertAttendee(a model.Attendee, accountID string) error {
	_, err := s.db.Exec(s.q(`
		INSERT INTO attendees (account_id, id, lid, phone, name, is_self)
		VALUES (?,?,?,?,?,?)
		ON CONFLICT(account_id, id) DO UPDATE SET
		  phone = excluded.phone, name = excluded.name, is_self = excluded.is_self`),
		accountID, a.ID, "", a.Phone, a.Name, b2i(a.IsSelf))
	return err
}

func (s *Store) GetAttendee(accountID, id string) (model.Attendee, error) {
	return scanAttendee(s.db.QueryRow(s.q(`
		SELECT id, phone, name, is_self FROM attendees WHERE account_id = ? AND id = ?`), accountID, id))
}

// SelfAttendee returns the account's own attendee row (is_self = 1). Callers
// use it to tag outgoing messages with a stable sender id.
func (s *Store) SelfAttendee(accountID string) (model.Attendee, error) {
	return scanAttendee(s.db.QueryRow(s.q(`
		SELECT id, phone, name, is_self FROM attendees WHERE account_id = ? AND is_self = 1 LIMIT 1`), accountID))
}

func (s *Store) ListAttendees(accountID, search string, limit, offset int) ([]model.Attendee, error) {
	where := "account_id = ?"
	args := []any{accountID}
	if search != "" {
		where += " AND " + s.d.likeCI("name")
		args = append(args, "%"+escapeLike(search)+"%")
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	args = append(args, limit, offset)
	rows, err := s.db.Query(s.q(`SELECT id, phone, name, is_self FROM attendees WHERE `+where+
		` ORDER BY name LIMIT ? OFFSET ?`), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Attendee{}
	for rows.Next() {
		a, err := scanAttendee(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func scanAttendee(r scanner) (model.Attendee, error) {
	var a model.Attendee
	var isSelf int
	err := r.Scan(&a.ID, &a.Phone, &a.Name, &isSelf)
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrNotFound
	}
	if err != nil {
		return a, err
	}
	a.IsSelf = isSelf == 1
	return a, nil
}

// ---------- chat members ----------

// ReplaceChatMembers overwrites a chat's roster wholesale: simpler and safer
// than diffing against whatever a provider's group-metadata event reports.
// The delete and the insert loop run in one transaction, so a mid-loop
// failure (e.g. a duplicate attendee id violating the primary key) leaves
// the previous roster intact rather than a half-replaced one.
func (s *Store) ReplaceChatMembers(accountID, chatID string, members []model.ChatMember) error {
	return s.inTx(func(tx *sql.Tx) error {
		if _, err := tx.Exec(s.q(`DELETE FROM chat_members WHERE account_id = ? AND chat_id = ?`), accountID, chatID); err != nil {
			return err
		}
		for _, m := range members {
			if _, err := tx.Exec(s.q(`
				INSERT INTO chat_members (account_id, chat_id, attendee_id, role) VALUES (?,?,?,?)`),
				accountID, chatID, m.AttendeeID, m.Role); err != nil {
				return err
			}
		}
		return nil
	})
}

// ---------- chat messages ----------

// chatMessageSelect resolves the sender to a full Attendee, the same way
// chatMembers does. A LEFT JOIN, not an inner one: a message can arrive from a
// participant the roster has not caught up with yet, and it must still be
// readable — it just carries a bare id. Every column is qualified because the
// join makes account_id and id ambiguous.
const chatMessageSelect = `
	SELECT m.account_id, m.id, m.chat_id, m.sender_id, COALESCE(a.phone, ''), COALESCE(a.name, ''),
	       COALESCE(a.is_self, 0), m.is_from_me, m.kind, m.text, m.quoted_id, m.sent_at, m.edited_at,
	       m.deleted, m.status, m.reactions_json
	FROM chat_messages m
	LEFT JOIN attendees a ON a.account_id = m.account_id AND a.id = m.sender_id`

// UpsertChatMessage inserts a message and reports whether it was new. A
// replayed id (reconnect replay, own-message echo) is a no-op.
func (s *Store) UpsertChatMessage(m model.ChatMessage) (bool, error) {
	if m.Reactions == nil {
		m.Reactions = []model.Reaction{}
	}
	rj, _ := json.Marshal(m.Reactions)
	res, err := s.db.Exec(s.q(`
		INSERT INTO chat_messages (account_id, id, chat_id, sender_id, is_from_me, kind, text, quoted_id, sent_at, edited_at, deleted, status, reactions_json, stored_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(account_id, id) DO NOTHING`),
		m.AccountID, m.ID, m.ChatID, m.Sender.ID, b2i(m.IsFromMe), m.Kind, m.Text, m.QuotedMessageID, m.SentAt.Unix(),
		nullUnix(m.EditedAt), b2i(m.Deleted), m.Status, string(rj), time.Now().Unix())
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

func (s *Store) GetChatMessage(accountID, id string) (model.ChatMessage, error) {
	return scanChatMessage(s.db.QueryRow(s.q(chatMessageSelect+` WHERE m.account_id = ? AND m.id = ?`), accountID, id))
}

// ListChatMessages pages newest-first with a keyset cursor: `before` is the
// id of the last message of the previous page (its (sent_at, id) is the bound).
func (s *Store) ListChatMessages(accountID, chatID, before string, limit int) ([]model.ChatMessage, string, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	args := []any{accountID, chatID}
	where := `m.account_id = ? AND m.chat_id = ?`
	if before != "" {
		var sentAt int64
		err := s.db.QueryRow(s.q(`SELECT sent_at FROM chat_messages WHERE account_id = ? AND id = ?`), accountID, before).Scan(&sentAt)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, "", ErrNotFound
		} else if err != nil {
			return nil, "", err
		}
		where += ` AND (m.sent_at < ? OR (m.sent_at = ? AND m.id < ?))`
		args = append(args, sentAt, sentAt, before)
	}
	args = append(args, limit+1)
	rows, err := s.db.Query(s.q(chatMessageSelect+` WHERE `+where+` ORDER BY m.sent_at DESC, m.id DESC LIMIT ?`), args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	out := []model.ChatMessage{}
	for rows.Next() {
		m, err := scanChatMessage(rows)
		if err != nil {
			return nil, "", err
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	next := ""
	if len(out) > limit {
		out = out[:limit]
		next = out[limit-1].ID
	}
	return out, next, nil
}

// RenameChatMessage swaps a locally-minted temp id for the id the provider
// assigns once it accepts the send. ErrNotFound if oldID is unknown.
func (s *Store) RenameChatMessage(accountID, oldID, newID string) error {
	res, err := s.db.Exec(s.q(`UPDATE chat_messages SET id = ? WHERE account_id = ? AND id = ?`), newID, accountID, oldID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) DeleteChatMessageRow(accountID, id string) error {
	_, err := s.db.Exec(s.q(`DELETE FROM chat_messages WHERE account_id = ? AND id = ?`), accountID, id)
	return err
}

// SetMessageStatus advances the own-message delivery status (sending → sent
// → delivered → read) for a batch of ids in one statement.
func (s *Store) SetMessageStatus(accountID string, ids []string, status string) error {
	if len(ids) == 0 {
		return nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, 0, len(ids)+2)
	args = append(args, status, accountID)
	for _, id := range ids {
		args = append(args, id)
	}
	_, err := s.db.Exec(s.q(`UPDATE chat_messages SET status = ? WHERE account_id = ? AND id IN (`+placeholders+`)`), args...)
	return err
}

// ApplyReaction replaces the given attendee's earlier reaction to a message
// (if any) with r. An empty emoji removes that attendee's reaction outright.
// The read of reactions_json and the write back run in one transaction, and
// the read takes a row lock (see dialect.forUpdate), so two concurrent
// callers merging into the same message (a chat-runtime actor applying an
// inbound reaction, an API handler applying a local one) cannot interleave
// and lose one of the two updates: the second caller's SELECT blocks until
// the first's transaction commits, and then sees its write.
func (s *Store) ApplyReaction(accountID, id string, r model.Reaction) error {
	return s.inTx(func(tx *sql.Tx) error {
		var reactionsJSON string
		err := tx.QueryRow(s.q(`SELECT reactions_json FROM chat_messages WHERE account_id = ? AND id = ?`+s.d.forUpdate), accountID, id).Scan(&reactionsJSON)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		var reactions []model.Reaction
		_ = json.Unmarshal([]byte(reactionsJSON), &reactions)
		filtered := make([]model.Reaction, 0, len(reactions)+1)
		for _, x := range reactions {
			if x.AttendeeID != r.AttendeeID {
				filtered = append(filtered, x)
			}
		}
		if r.Emoji != "" {
			filtered = append(filtered, r)
		}
		rj, _ := json.Marshal(filtered)
		_, err = tx.Exec(s.q(`UPDATE chat_messages SET reactions_json = ? WHERE account_id = ? AND id = ?`), string(rj), accountID, id)
		return err
	})
}

func (s *Store) EditChatMessage(accountID, id, text string, at time.Time) error {
	_, err := s.db.Exec(s.q(`UPDATE chat_messages SET text = ?, edited_at = ? WHERE account_id = ? AND id = ?`),
		text, at.Unix(), accountID, id)
	return err
}

// RevokeChatMessage handles a "delete for everyone": the text is discarded,
// not merely hidden, since we have no right to retain it once revoked.
func (s *Store) RevokeChatMessage(accountID, id string) error {
	_, err := s.db.Exec(s.q(`UPDATE chat_messages SET deleted = 1, text = '' WHERE account_id = ? AND id = ?`), accountID, id)
	return err
}

func scanChatMessage(r scanner) (model.ChatMessage, error) {
	var m model.ChatMessage
	var isFromMe, deleted int
	var sentAt int64
	var editedAt sql.NullInt64
	var reactionsJSON string
	var senderIsSelf int
	err := r.Scan(&m.AccountID, &m.ID, &m.ChatID, &m.Sender.ID, &m.Sender.Phone, &m.Sender.Name, &senderIsSelf,
		&isFromMe, &m.Kind, &m.Text, &m.QuotedMessageID,
		&sentAt, &editedAt, &deleted, &m.Status, &reactionsJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return m, ErrNotFound
	}
	if err != nil {
		return m, err
	}
	m.Sender.IsSelf = senderIsSelf == 1
	m.IsFromMe = isFromMe == 1
	m.Deleted = deleted == 1
	m.SentAt = time.Unix(sentAt, 0).UTC()
	if editedAt.Valid {
		t := time.Unix(editedAt.Int64, 0).UTC()
		m.EditedAt = &t
	}
	_ = json.Unmarshal([]byte(reactionsJSON), &m.Reactions)
	if m.Reactions == nil {
		m.Reactions = []model.Reaction{}
	}
	return m, nil
}

// ---------- chat sessions ----------

// SaveChatSession persists the linked-device identity so the runtime can
// reconnect without re-scanning a QR code.
func (s *Store) SaveChatSession(accountID, provider, deviceJID string) error {
	_, err := s.db.Exec(s.q(`
		INSERT INTO chat_sessions (account_id, provider, device_jid, updated_at)
		VALUES (?,?,?,?)
		ON CONFLICT(account_id) DO UPDATE SET
		  provider = excluded.provider, device_jid = excluded.device_jid, updated_at = excluded.updated_at`),
		accountID, provider, deviceJID, time.Now().Unix())
	return err
}

func (s *Store) ChatSession(accountID string) (string, error) {
	var jid string
	err := s.db.QueryRow(s.q(`SELECT device_jid FROM chat_sessions WHERE account_id = ?`), accountID).Scan(&jid)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return jid, err
}

func (s *Store) DeleteChatSession(accountID string) error {
	_, err := s.db.Exec(s.q(`DELETE FROM chat_sessions WHERE account_id = ?`), accountID)
	return err
}

// ---------- idempotency keys ----------

// PutIdempotency stores a response body under a developer-scoped key so a
// retried request with the same Idempotency-Key header replays the original
// response instead of re-executing a side effect.
func (s *Store) PutIdempotency(developerID, key string, response []byte) error {
	_, err := s.db.Exec(s.q(`
		INSERT INTO idempotency_keys (developer_id, key, response, created_at)
		VALUES (?,?,?,?)
		ON CONFLICT(developer_id, key) DO UPDATE SET
		  response = excluded.response, created_at = excluded.created_at`),
		developerID, key, response, time.Now().Unix())
	return err
}

func (s *Store) GetIdempotency(developerID, key string) ([]byte, error) {
	var b []byte
	err := s.db.QueryRow(s.q(`SELECT response FROM idempotency_keys WHERE developer_id = ? AND key = ?`),
		developerID, key).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return b, err
}

// ReserveIdempotency atomically claims (developerID, key) by inserting an
// empty-response placeholder row, reporting whether this call was the one
// that created it. A concurrent second caller for the same key loses (won =
// false) and must not run the operation again — it should instead wait for
// or read whatever the winner eventually stores via PutIdempotency, or the
// key's removal via DeleteIdempotency if the winner's attempt failed.
func (s *Store) ReserveIdempotency(developerID, key string) (bool, error) {
	res, err := s.db.Exec(s.q(`
		INSERT INTO idempotency_keys (developer_id, key, response, created_at)
		VALUES (?,?,?,?)
		ON CONFLICT(developer_id, key) DO NOTHING`),
		developerID, key, []byte{}, time.Now().Unix())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// DeleteIdempotency removes one key outright. Used to release a reservation
// after its operation failed, so a retry with the same key is free to try
// again instead of being permanently told "in progress".
func (s *Store) DeleteIdempotency(developerID, key string) error {
	_, err := s.db.Exec(s.q(`DELETE FROM idempotency_keys WHERE developer_id = ? AND key = ?`), developerID, key)
	return err
}

// PurgeIdempotency drops keys older than olderThan. Best-effort background
// hygiene: a failure here is not worth surfacing to a caller.
func (s *Store) PurgeIdempotency(olderThan time.Time) {
	_, _ = s.db.Exec(s.q(`DELETE FROM idempotency_keys WHERE created_at < ?`), olderThan.Unix())
}
