package store

import "time"

// EvictExpiredContent blanks message content that has outlived the owning
// developer's retention policy, and reports how many rows it touched.
//
// This is the backstop, not the main event: content whose webhooks all
// accepted it is evicted immediately by the dispatcher. What lands here is
// content that was never forwarded (no webhook configured), or whose delivery
// is still retrying or has died. Running hourly is therefore fine.
//
// The EXISTS subquery — rather than a JOIN — keeps the statement portable
// across SQLite and Postgres, neither of which supports UPDATE ... JOIN in the
// same syntax.
func (s *Store) EvictExpiredContent(now time.Time) (int64, error) {
	defer s.trace("EvictExpiredContent", time.Now())
	var total int64

	res, err := s.db.Exec(s.q(`
		UPDATE emails SET
		  subject = '', snippet = '', body = '', body_type = '',
		  from_name = '', from_email = '',
		  to_json = '[]', cc_json = '[]', bcc_json = '[]', reply_to_json = '[]',
		  attachments_json = '[]',
		  content_evicted_at = ?
		WHERE content_evicted_at IS NULL AND EXISTS (
		  SELECT 1 FROM accounts a JOIN developers d ON d.id = a.developer_id
		  WHERE a.id = emails.account_id
		    AND d.retention_max_age_secs > 0
		    AND ? - emails.stored_at > d.retention_max_age_secs)`),
		now.Unix(), now.Unix())
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	total += n

	res, err = s.db.Exec(s.q(`
		UPDATE chat_messages SET text = '', content_evicted_at = ?
		WHERE content_evicted_at IS NULL AND EXISTS (
		  SELECT 1 FROM accounts a JOIN developers d ON d.id = a.developer_id
		  WHERE a.id = chat_messages.account_id
		    AND d.retention_max_age_secs > 0
		    AND ? - chat_messages.stored_at > d.retention_max_age_secs)`),
		now.Unix(), now.Unix())
	if err != nil {
		return total, err
	}
	n, _ = res.RowsAffected()
	return total + n, nil
}
