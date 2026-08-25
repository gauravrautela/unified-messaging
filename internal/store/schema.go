package store

const schema = `
PRAGMA journal_mode = WAL;
PRAGMA foreign_keys = ON;

CREATE TABLE IF NOT EXISTS accounts (
  id            TEXT PRIMARY KEY,
  provider      TEXT NOT NULL,
  email         TEXT NOT NULL,
  name          TEXT NOT NULL DEFAULT '',
  status        TEXT NOT NULL,
  created_at    INTEGER NOT NULL,
  updated_at    INTEGER NOT NULL,
  last_synced_at INTEGER
);
CREATE UNIQUE INDEX IF NOT EXISTS accounts_email ON accounts(email);

-- Refresh tokens are stored sealed (AES-GCM); access tokens are short-lived and
-- kept only to avoid a refresh round-trip on every call.
CREATE TABLE IF NOT EXISTS tokens (
  account_id        TEXT PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
  access_token      TEXT NOT NULL,
  access_expires_at INTEGER NOT NULL,
  refresh_token_enc TEXT NOT NULL,
  scope             TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS folders (
  account_id   TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  id           TEXT NOT NULL,
  name         TEXT NOT NULL,
  parent_id    TEXT NOT NULL DEFAULT '',
  role         TEXT NOT NULL DEFAULT '',
  total_count  INTEGER NOT NULL DEFAULT 0,
  unread_count INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (account_id, id)
);

-- One cursor per sync scope. What a scope is depends on the provider: Outlook
-- exposes message delta only per mail folder, so scopes are folders there,
-- while a provider with a single mailbox-wide cursor uses exactly one row.
CREATE TABLE IF NOT EXISTS sync_state (
  account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  scope_id   TEXT NOT NULL,
  cursor     TEXT NOT NULL DEFAULT '',
  updated_at INTEGER NOT NULL,
  PRIMARY KEY (account_id, scope_id)
);

CREATE TABLE IF NOT EXISTS emails (
  account_id          TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  id                  TEXT NOT NULL,
  thread_id           TEXT NOT NULL DEFAULT '',
  folder_id           TEXT NOT NULL DEFAULT '',
  subject             TEXT NOT NULL DEFAULT '',
  from_name           TEXT NOT NULL DEFAULT '',
  from_email          TEXT NOT NULL DEFAULT '',
  to_json             TEXT NOT NULL DEFAULT '[]',
  cc_json             TEXT NOT NULL DEFAULT '[]',
  bcc_json            TEXT NOT NULL DEFAULT '[]',
  reply_to_json       TEXT NOT NULL DEFAULT '[]',
  date                INTEGER NOT NULL DEFAULT 0,
  snippet             TEXT NOT NULL DEFAULT '',
  body                TEXT NOT NULL DEFAULT '',
  body_type           TEXT NOT NULL DEFAULT '',
  read                INTEGER NOT NULL DEFAULT 0,
  flagged             INTEGER NOT NULL DEFAULT 0,
  draft               INTEGER NOT NULL DEFAULT 0,
  has_attachments     INTEGER NOT NULL DEFAULT 0,
  internet_message_id TEXT NOT NULL DEFAULT '',
  attachments_json    TEXT NOT NULL DEFAULT '[]',
  PRIMARY KEY (account_id, id)
);
CREATE INDEX IF NOT EXISTS emails_by_date   ON emails(account_id, date DESC);
CREATE INDEX IF NOT EXISTS emails_by_folder ON emails(account_id, folder_id, date DESC);
CREATE INDEX IF NOT EXISTS emails_by_thread ON emails(account_id, thread_id, date DESC);

CREATE TABLE IF NOT EXISTS subscriptions (
  id           TEXT PRIMARY KEY,
  account_id   TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  resource     TEXT NOT NULL,
  client_state TEXT NOT NULL,
  expires_at   INTEGER NOT NULL,
  created_at   INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS subs_by_account ON subscriptions(account_id);

-- account_id '' means global. Account-scoped rows are removed by hand in
-- DeleteAccount rather than by FK cascade, since '' cannot reference a row.
CREATE TABLE IF NOT EXISTS webhooks (
  id          TEXT PRIMARY KEY,
  account_id  TEXT NOT NULL DEFAULT '',
  name        TEXT NOT NULL DEFAULT '',
  url         TEXT NOT NULL,
  secret      TEXT NOT NULL DEFAULT '',
  events_json TEXT NOT NULL DEFAULT '[]',
  created_at  INTEGER NOT NULL
);

-- Short-lived PKCE state for the connect flow.
CREATE TABLE IF NOT EXISTS oauth_states (
  state          TEXT PRIMARY KEY,
  provider       TEXT NOT NULL DEFAULT '',
  verifier       TEXT NOT NULL,
  success_url    TEXT NOT NULL DEFAULT '',
  failure_url    TEXT NOT NULL DEFAULT '',
  notify_url     TEXT NOT NULL DEFAULT '',
  webhook_json   TEXT NOT NULL DEFAULT '',
  created_at     INTEGER NOT NULL,
  expires_at     INTEGER NOT NULL
);
-- Failed webhook deliveries waiting for a retry. A row is removed on success,
-- rescheduled on failure, and kept with dead = 1 once the schedule is used up
-- so the caller can see what never arrived.
CREATE TABLE IF NOT EXISTS webhook_deliveries (
  id              TEXT PRIMARY KEY,
  webhook_id      TEXT NOT NULL REFERENCES webhooks(id) ON DELETE CASCADE,
  account_id      TEXT NOT NULL DEFAULT '',
  event_type      TEXT NOT NULL,
  payload         BLOB NOT NULL,
  attempts        INTEGER NOT NULL DEFAULT 0,
  next_attempt_at INTEGER NOT NULL,
  last_error      TEXT NOT NULL DEFAULT '',
  dead            INTEGER NOT NULL DEFAULT 0,
  created_at      INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS deliveries_due ON webhook_deliveries(dead, next_attempt_at);
CREATE INDEX IF NOT EXISTS deliveries_by_webhook ON webhook_deliveries(webhook_id);
`

// migrations are additive column changes for databases created before the
// column existed. Each is safe to re-run: "duplicate column" is ignored.
// Indexes on migrated columns live in postMigration, not schema, because
// schema runs first and the column may not exist yet.
var migrations = []string{
	`ALTER TABLE webhooks ADD COLUMN account_id TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE oauth_states ADD COLUMN webhook_json TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE webhooks ADD COLUMN name TEXT NOT NULL DEFAULT ''`,
}

const postMigration = `
CREATE INDEX IF NOT EXISTS webhooks_by_account ON webhooks(account_id);
`
