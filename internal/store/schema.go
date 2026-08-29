package store

// sqlitePragmas are prepended to the SQLite rendering of the schema. They are
// connection/file settings with no Postgres equivalent, so they live outside
// the shared template.
const sqlitePragmas = `
PRAGMA journal_mode = WAL;
PRAGMA foreign_keys = ON;
`

// schemaTemplate is the schema both engines share. Two tokens are rendered per
// dialect: {{BLOB}} (BLOB / BYTEA) and {{BIGINT}} for the epoch-second columns,
// which overflow a Postgres INTEGER. Flags and counters stay INTEGER.
const schemaTemplate = `
-- A developer is a tenant. Everything below is owned by exactly one.
CREATE TABLE IF NOT EXISTS developers (
  id                    TEXT PRIMARY KEY,
  email                 TEXT NOT NULL UNIQUE,
  password_hash         TEXT NOT NULL,
  name                  TEXT NOT NULL DEFAULT '',
  created_at            {{BIGINT}} NOT NULL,
  redirect_domains_json TEXT NOT NULL DEFAULT '[]',
  retention_max_age_secs {{BIGINT}} NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS api_keys (
  id           TEXT PRIMARY KEY,
  developer_id TEXT NOT NULL REFERENCES developers(id) ON DELETE CASCADE,
  name         TEXT NOT NULL,
  prefix       TEXT NOT NULL,
  hash         TEXT NOT NULL UNIQUE,
  created_at   {{BIGINT}} NOT NULL,
  last_used_at {{BIGINT}},
  revoked_at   {{BIGINT}}
);
CREATE INDEX IF NOT EXISTS api_keys_by_developer ON api_keys(developer_id);

CREATE TABLE IF NOT EXISTS sessions (
  id           TEXT PRIMARY KEY,
  developer_id TEXT NOT NULL REFERENCES developers(id) ON DELETE CASCADE,
  created_at   {{BIGINT}} NOT NULL,
  expires_at   {{BIGINT}} NOT NULL
);
CREATE INDEX IF NOT EXISTS sessions_by_developer ON sessions(developer_id);

CREATE TABLE IF NOT EXISTS accounts (
  id            TEXT PRIMARY KEY,
  developer_id  TEXT NOT NULL REFERENCES developers(id) ON DELETE CASCADE,
  kind          TEXT NOT NULL DEFAULT 'mail',
  provider      TEXT NOT NULL,
  email         TEXT NOT NULL,
  name          TEXT NOT NULL DEFAULT '',
  status        TEXT NOT NULL,
  created_at    {{BIGINT}} NOT NULL,
  updated_at    {{BIGINT}} NOT NULL,
  last_synced_at {{BIGINT}}
);
-- The same mailbox may be connected by two developers as two accounts.
CREATE UNIQUE INDEX IF NOT EXISTS accounts_owner_email ON accounts(developer_id, email);

-- Refresh tokens are stored sealed (AES-GCM); access tokens are short-lived and
-- kept only to avoid a refresh round-trip on every call.
CREATE TABLE IF NOT EXISTS tokens (
  account_id        TEXT PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
  access_token      TEXT NOT NULL,
  access_expires_at {{BIGINT}} NOT NULL,
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
  updated_at {{BIGINT}} NOT NULL,
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
  date                {{BIGINT}} NOT NULL DEFAULT 0,
  snippet             TEXT NOT NULL DEFAULT '',
  body                TEXT NOT NULL DEFAULT '',
  body_type           TEXT NOT NULL DEFAULT '',
  read                INTEGER NOT NULL DEFAULT 0,
  flagged             INTEGER NOT NULL DEFAULT 0,
  draft               INTEGER NOT NULL DEFAULT 0,
  has_attachments     INTEGER NOT NULL DEFAULT 0,
  internet_message_id TEXT NOT NULL DEFAULT '',
  attachments_json    TEXT NOT NULL DEFAULT '[]',
  stored_at           {{BIGINT}} NOT NULL DEFAULT 0,
  content_evicted_at  {{BIGINT}},
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
  expires_at   {{BIGINT}} NOT NULL,
  created_at   {{BIGINT}} NOT NULL
);
CREATE INDEX IF NOT EXISTS subs_by_account ON subscriptions(account_id);

-- account_id '' means every account of this developer. Account-scoped rows
-- are removed by hand in DeleteAccount, since '' cannot reference a row.
CREATE TABLE IF NOT EXISTS webhooks (
  id           TEXT PRIMARY KEY,
  developer_id TEXT NOT NULL REFERENCES developers(id) ON DELETE CASCADE,
  account_id   TEXT NOT NULL DEFAULT '',
  name         TEXT NOT NULL DEFAULT '',
  url          TEXT NOT NULL,
  secret       TEXT NOT NULL DEFAULT '',
  events_json  TEXT NOT NULL DEFAULT '[]',
  kind         TEXT NOT NULL DEFAULT 'webhook',
  config       TEXT NOT NULL DEFAULT '',
  created_at   {{BIGINT}} NOT NULL
);
CREATE INDEX IF NOT EXISTS webhooks_by_developer ON webhooks(developer_id);
CREATE INDEX IF NOT EXISTS webhooks_by_account   ON webhooks(account_id);

-- Short-lived PKCE state for the connect flow, minted by a developer.
CREATE TABLE IF NOT EXISTS oauth_states (
  state          TEXT PRIMARY KEY,
  developer_id   TEXT NOT NULL REFERENCES developers(id) ON DELETE CASCADE,
  provider       TEXT NOT NULL DEFAULT '',
  verifier       TEXT NOT NULL,
  success_url    TEXT NOT NULL DEFAULT '',
  failure_url    TEXT NOT NULL DEFAULT '',
  notify_url     TEXT NOT NULL DEFAULT '',
  webhook_json   TEXT NOT NULL DEFAULT '',
  created_at     {{BIGINT}} NOT NULL,
  expires_at     {{BIGINT}} NOT NULL,
  consented_at   {{BIGINT}},
  -- browser_hash is the sha256 of the um_link cookie belonging to whichever
  -- browser first claimed this connect state (consent, normally, or a /qr
  -- retry after a previous pairing attempt failed). Empty means unclaimed.
  browser_hash   TEXT NOT NULL DEFAULT ''
);

-- Failed webhook deliveries waiting for a retry. A row is removed on success,
-- rescheduled on failure, and kept with dead = 1 once the schedule is used up
-- so the caller can see what never arrived.
CREATE TABLE IF NOT EXISTS webhook_deliveries (
  id              TEXT PRIMARY KEY,
  webhook_id      TEXT NOT NULL REFERENCES webhooks(id) ON DELETE CASCADE,
  account_id      TEXT NOT NULL DEFAULT '',
  event_type      TEXT NOT NULL,
  payload         {{BLOB}} NOT NULL,
  attempts        INTEGER NOT NULL DEFAULT 0,
  next_attempt_at {{BIGINT}} NOT NULL,
  last_error      TEXT NOT NULL DEFAULT '',
  dead            INTEGER NOT NULL DEFAULT 0,
  created_at      {{BIGINT}} NOT NULL
);
CREATE INDEX IF NOT EXISTS deliveries_due ON webhook_deliveries(dead, next_attempt_at);
CREATE INDEX IF NOT EXISTS deliveries_by_webhook ON webhook_deliveries(webhook_id);

-- ---- chat providers (WhatsApp) ----
CREATE TABLE IF NOT EXISTS chats (
  account_id      TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  id              TEXT NOT NULL,
  kind            TEXT NOT NULL,
  name            TEXT NOT NULL DEFAULT '',
  unread_count    INTEGER NOT NULL DEFAULT 0,
  last_message_at {{BIGINT}},
  archived        INTEGER NOT NULL DEFAULT 0,
  muted           INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (account_id, id)
);
CREATE INDEX IF NOT EXISTS chats_by_activity ON chats(account_id, last_message_at DESC);

CREATE TABLE IF NOT EXISTS attendees (
  account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  id         TEXT NOT NULL,
  lid        TEXT NOT NULL DEFAULT '',
  phone      TEXT NOT NULL DEFAULT '',
  name       TEXT NOT NULL DEFAULT '',
  is_self    INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (account_id, id)
);

CREATE TABLE IF NOT EXISTS chat_members (
  account_id  TEXT NOT NULL,
  chat_id     TEXT NOT NULL,
  attendee_id TEXT NOT NULL,
  role        TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (account_id, chat_id, attendee_id),
  FOREIGN KEY (account_id, chat_id) REFERENCES chats(account_id, id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS chat_messages (
  account_id     TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  id             TEXT NOT NULL,
  chat_id        TEXT NOT NULL,
  sender_id      TEXT NOT NULL,
  is_from_me     INTEGER NOT NULL DEFAULT 0,
  kind           TEXT NOT NULL,
  text           TEXT NOT NULL DEFAULT '',
  quoted_id      TEXT NOT NULL DEFAULT '',
  sent_at        {{BIGINT}} NOT NULL,
  edited_at      {{BIGINT}},
  deleted        INTEGER NOT NULL DEFAULT 0,
  status         TEXT NOT NULL DEFAULT '',
  reactions_json TEXT NOT NULL DEFAULT '[]',
  stored_at          {{BIGINT}} NOT NULL DEFAULT 0,
  content_evicted_at {{BIGINT}},
  PRIMARY KEY (account_id, id)
);
CREATE INDEX IF NOT EXISTS chat_messages_by_chat ON chat_messages(account_id, chat_id, sent_at DESC, id DESC);

CREATE TABLE IF NOT EXISTS chat_sessions (
  account_id TEXT PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
  provider   TEXT NOT NULL,
  device_jid TEXT NOT NULL,
  updated_at {{BIGINT}} NOT NULL
);

CREATE TABLE IF NOT EXISTS idempotency_keys (
  developer_id TEXT NOT NULL REFERENCES developers(id) ON DELETE CASCADE,
  key          TEXT NOT NULL,
  response     {{BLOB}} NOT NULL,
  created_at   {{BIGINT}} NOT NULL,
  PRIMARY KEY (developer_id, key)
);
`

// sqliteMigrations are additive column changes for databases created before
// the column existed. Each is safe to re-run: "duplicate column" is ignored.
var sqliteMigrations = []string{
	`ALTER TABLE accounts ADD COLUMN kind TEXT NOT NULL DEFAULT 'mail'`,
	`ALTER TABLE oauth_states ADD COLUMN consented_at INTEGER`,
	`ALTER TABLE webhooks ADD COLUMN kind TEXT NOT NULL DEFAULT 'webhook'`,
	`ALTER TABLE webhooks ADD COLUMN config TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE developers ADD COLUMN redirect_domains_json TEXT NOT NULL DEFAULT '[]'`,
	`ALTER TABLE oauth_states ADD COLUMN browser_hash TEXT NOT NULL DEFAULT ''`,
	// Sessions are now keyed by sha256 of the token — 64 hex characters. Rows
	// written before that cut-over hold the raw token as the primary key (43
	// characters: 32 bytes of unpadded base64url). They are inert, since every
	// lookup hashes first, but they are precisely the "a DB read yields every
	// live session" artefact the hashing removed, and they sit in every backup
	// taken before the upgrade. Keyed on length rather than a blanket DELETE so
	// an upgrade does not sign out every developer who is currently signed in.
	`DELETE FROM sessions WHERE length(id) <> 64`,
	`ALTER TABLE developers ADD COLUMN retention_max_age_secs INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE emails ADD COLUMN stored_at INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE emails ADD COLUMN content_evicted_at INTEGER`,
	`ALTER TABLE chat_messages ADD COLUMN stored_at INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE chat_messages ADD COLUMN content_evicted_at INTEGER`,
	// Rows written before stored_at existed start their retention clock at the
	// upgrade, not at the epoch — otherwise the first sweep after enabling a
	// policy would evict the entire existing mirror at once. Idempotent: once
	// stamped, no row matches stored_at = 0 again, because every insert path
	// now sets it.
	`UPDATE emails SET stored_at = CAST(strftime('%s','now') AS INTEGER) WHERE stored_at = 0`,
	`UPDATE chat_messages SET stored_at = CAST(strftime('%s','now') AS INTEGER) WHERE stored_at = 0`,
}

// postgresMigrations are the same additive changes for Postgres, where
// ADD COLUMN IF NOT EXISTS makes each one idempotent on its own rather than
// relying on a "duplicate column" error being swallowed.
var postgresMigrations = []string{
	`ALTER TABLE accounts ADD COLUMN IF NOT EXISTS kind TEXT NOT NULL DEFAULT 'mail'`,
	`ALTER TABLE oauth_states ADD COLUMN IF NOT EXISTS consented_at BIGINT`,
	`ALTER TABLE webhooks ADD COLUMN IF NOT EXISTS kind TEXT NOT NULL DEFAULT 'webhook'`,
	`ALTER TABLE webhooks ADD COLUMN IF NOT EXISTS config TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE developers ADD COLUMN IF NOT EXISTS redirect_domains_json TEXT NOT NULL DEFAULT '[]'`,
	`ALTER TABLE oauth_states ADD COLUMN IF NOT EXISTS browser_hash TEXT NOT NULL DEFAULT ''`,
	// See the sqliteMigrations note: pre-hash session rows hold the raw token
	// as the primary key (43 characters) instead of its sha256 (64).
	`DELETE FROM sessions WHERE length(id) <> 64`,
	`ALTER TABLE developers ADD COLUMN IF NOT EXISTS retention_max_age_secs BIGINT NOT NULL DEFAULT 0`,
	`ALTER TABLE emails ADD COLUMN IF NOT EXISTS stored_at BIGINT NOT NULL DEFAULT 0`,
	`ALTER TABLE emails ADD COLUMN IF NOT EXISTS content_evicted_at BIGINT`,
	`ALTER TABLE chat_messages ADD COLUMN IF NOT EXISTS stored_at BIGINT NOT NULL DEFAULT 0`,
	`ALTER TABLE chat_messages ADD COLUMN IF NOT EXISTS content_evicted_at BIGINT`,
	// See the sqliteMigrations note: pre-existing rows start their retention
	// clock at the upgrade rather than at the epoch.
	`UPDATE emails SET stored_at = EXTRACT(EPOCH FROM now())::bigint WHERE stored_at = 0`,
	`UPDATE chat_messages SET stored_at = EXTRACT(EPOCH FROM now())::bigint WHERE stored_at = 0`,
}
