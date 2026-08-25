# Multi-tenancy with Developer Login — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the service multi-tenant: a developer signs up with email + password, mints revocable API keys, and every account/webhook/email they connect is visible only to them — with exhaustive, secret-free debug logging across the codebase.

**Architecture:** `developer_id` on the owning tables (`accounts`, `webhooks`, `oauth_states`) plus new `developers`, `api_keys`, `sessions` tables. One middleware (`withDeveloper`) resolves an API key *or* a session cookie into a `model.Developer` in the request context; every scoped store method takes the developer id in SQL. A new `internal/auth` package owns hashing and credentials; a new `internal/logx` package owns request ids and redaction.

**Tech Stack:** Go 1.26, `net/http` ServeMux pattern routing, `database/sql` + `modernc.org/sqlite` (raw SQL), `log/slog`, `golang.org/x/crypto/bcrypt` (only new dependency).

**Spec:** `docs/superpowers/specs/2026-08-25-multi-tenancy-design.md`

## Global Constraints

- No web framework, no ORM: stdlib `net/http` + raw SQL only (spec "Stack").
- Only new dependency: `golang.org/x/crypto` (bcrypt).
- Fresh start: no `ALTER` migrations for tenancy columns; a pre-tenancy DB is refused at startup with the exact message `database <path> predates multi-tenancy; delete it (and its -wal/-shm files) and reconnect your mailboxes`.
- API key format: `um_` + 40 chars of `[A-Za-z0-9]`; stored as hex SHA-256; `prefix` = first 12 chars.
- bcrypt cost 12; password ≥ 10 chars; login/signup errors uniform.
- Cookie: name `um_session`, `HttpOnly`, `SameSite=Lax`, `Path=/`, `Secure` when TLS or `PUBLIC_BASE_URL` is https; TTL `SESSION_TTL_DAYS` default 30.
- Cross-tenant access → **404**, never 403.
- `POST /api/v1/api-keys` and `DELETE /api/v1/api-keys/{id}` are session-only (API key → 403 `session_required`).
- Never log: passwords, password hashes, session ids, full API keys, key hashes, OAuth codes/verifiers, access/refresh tokens, webhook secrets, `Authorization`/`Cookie` values, Graph `clientState`. Email bodies logged as byte length only.
- TDD: every task starts with a failing test. Run `gofmt -l internal cmd` and `go vet ./...` before every commit; both must be clean.
- Commit after each task with the message given.

## File structure

| File | Responsibility |
|---|---|
| `internal/logx/logx.go` (new) | request-id generation, context logger, `Redact`, test capture handler |
| `internal/logx/logx_test.go` (new) | redaction + context tests |
| `internal/model/model.go` | add `Developer`, `APIKey`; `DeveloperID` on `Account`, `Webhook` |
| `internal/store/schema.go` | new tables, `developer_id` columns, drop old migrations |
| `internal/store/store.go` | pre-tenancy check in `Open`; scoped account queries |
| `internal/store/developers.go` (new) | developers, sessions, api_keys CRUD |
| `internal/store/aux.go` | `developer_id` on webhooks + oauth states |
| `internal/auth/auth.go` (new) | `Service`: signup/login/sessions/keys; sole bcrypt importer |
| `internal/auth/auth_test.go` (new) | |
| `internal/accounts/accounts.go` | `Connect` takes developer id |
| `internal/config/config.go` | drop `APIKey`; add `SessionTTL` |
| `internal/api/api.go` | `withDeveloper`, `withRequestID`, routes |
| `internal/api/context.go` (new) | `developerFrom`, `authKindFrom` context helpers |
| `internal/api/handlers_auth.go` (new) | login/signup/logout pages, `/me`, api-keys |
| `internal/api/handlers_misc.go`, `handlers_mail.go`, `handlers_connect.go` | scoping |
| `internal/api/handlers_ui.go`, `handlers_mail_ui.go` | session gate, API-keys panel |
| `internal/api/isolation_test.go` (new) | table-driven cross-tenant test |
| `cmd/server/main.go` | wiring, startup config log |
| `scripts/smoke.sh`, `README.md`, `.env.example` | tenant flow |

---

### Task 1: `internal/logx` — request ids, context logger, redaction

**Files:**
- Create: `internal/logx/logx.go`, `internal/logx/logx_test.go`

**Interfaces:**
- Produces: `logx.NewRequestID() string`; `logx.With(ctx, *slog.Logger) context.Context`; `logx.From(ctx) *slog.Logger` (falls back to `slog.Default()`); `logx.Redact(v any) any`; `logx.Capture() (*slog.Logger, *Records)` with `Records.Contains(substr string) bool` and `Records.All() []string`.

- [ ] **Step 1: Write the failing tests**

```go
// internal/logx/logx_test.go
package logx

import (
	"context"
	"strings"
	"testing"
)

func TestRedactMasksSecretFields(t *testing.T) {
	in := map[string]any{
		"email":    "a@b.com",
		"password": "hunter22",
		"nested":   map[string]any{"secret": "s3", "token": "t0k", "key": "k", "code": "c0de", "ok": "fine"},
		"list":     []any{map[string]any{"refresh_token": "rt"}},
	}
	out := Redact(in).(map[string]any)
	if out["email"] != "a@b.com" {
		t.Fatalf("non-secret changed: %v", out["email"])
	}
	if out["password"] != "[redacted]" {
		t.Fatalf("password not redacted: %v", out["password"])
	}
	n := out["nested"].(map[string]any)
	for _, k := range []string{"secret", "token", "key", "code"} {
		if n[k] != "[redacted]" {
			t.Fatalf("%s not redacted: %v", k, n[k])
		}
	}
	if n["ok"] != "fine" {
		t.Fatal("non-secret nested value changed")
	}
	if out["list"].([]any)[0].(map[string]any)["refresh_token"] != "[redacted]" {
		t.Fatal("refresh_token in list not redacted")
	}
}

func TestContextLoggerRoundTrip(t *testing.T) {
	log, recs := Capture()
	ctx := With(context.Background(), log.With("request_id", "req_x"))
	From(ctx).Info("hello")
	if !recs.Contains("request_id=req_x") || !recs.Contains("hello") {
		t.Fatalf("record missing fields: %v", recs.All())
	}
	if From(context.Background()) == nil {
		t.Fatal("From must fall back to a default logger")
	}
}

func TestRequestIDShape(t *testing.T) {
	id := NewRequestID()
	if !strings.HasPrefix(id, "req_") || len(id) != 4+16 {
		t.Fatalf("id = %q", id)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/logx/`
Expected: build failure — package does not exist.

- [ ] **Step 3: Implement**

```go
// internal/logx/logx.go
// Package logx carries a per-request logger through context and redacts
// secrets before anything user-supplied is logged.
package logx

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"strings"
	"sync"
)

type ctxKey struct{}

// With attaches log to ctx. Handlers and stores retrieve it with From.
func With(ctx context.Context, log *slog.Logger) context.Context {
	return context.WithValue(ctx, ctxKey{}, log)
}

// From returns the logger attached to ctx, or the default logger.
func From(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(ctxKey{}).(*slog.Logger); ok && l != nil {
		return l
	}
	return slog.Default()
}

// NewRequestID returns "req_" + 16 hex chars.
func NewRequestID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "req_" + hex.EncodeToString(b)
}

// secretKeys are matched as substrings of lower-cased map keys.
var secretKeys = []string{"password", "secret", "token", "key", "code", "verifier", "cookie", "authorization", "client_state", "clientstate"}

// Redact returns a copy of v with secret-looking map values replaced. It
// walks maps and slices produced by encoding/json; other values pass through.
func Redact(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			if isSecret(k) {
				out[k] = "[redacted]"
			} else {
				out[k] = Redact(val)
			}
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = Redact(val)
		}
		return out
	default:
		return v
	}
}

func isSecret(k string) bool {
	lk := strings.ToLower(k)
	for _, s := range secretKeys {
		if strings.Contains(lk, s) {
			return true
		}
	}
	return false
}

// Records collects text log lines for assertions in tests.
type Records struct {
	mu    sync.Mutex
	lines []string
}

func (r *Records) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, string(p))
	return len(p), nil
}

func (r *Records) All() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.lines...)
}

func (r *Records) Contains(sub string) bool {
	for _, l := range r.All() {
		if strings.Contains(l, sub) {
			return true
		}
	}
	return false
}

// Capture returns a DEBUG-level logger whose output is kept in memory.
func Capture() (*slog.Logger, *Records) {
	recs := &Records{}
	return slog.New(slog.NewTextHandler(recs, &slog.HandlerOptions{Level: slog.LevelDebug})), recs
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/logx/`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/logx
git commit -m "feat(logx): request-scoped logger and secret redaction"
```

---

### Task 2: Model types and schema

**Files:**
- Modify: `internal/model/model.go`
- Modify: `internal/store/schema.go`
- Modify: `internal/store/store.go` (`Open`)
- Test: `internal/store/store_test.go`

**Interfaces:**
- Produces: `model.Developer{ID, Email, Name string; CreatedAt time.Time}`; `model.APIKey{ID, Name, Prefix string; CreatedAt time.Time; LastUsedAt, RevokedAt *time.Time}`; `Account.DeveloperID`, `Webhook.DeveloperID` (both `json:"-"`); `store.ErrPreTenancy`.

- [ ] **Step 1: Write the failing test**

```go
// append to internal/store/store_test.go
func TestOpenRefusesPreTenancyDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE accounts (id TEXT PRIMARY KEY, provider TEXT, email TEXT,
		name TEXT, status TEXT, created_at INTEGER, updated_at INTEGER, last_synced_at INTEGER)`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	_, err = Open(path)
	if err == nil {
		t.Fatal("pre-tenancy database was opened")
	}
	want := "database " + path + " predates multi-tenancy; delete it (and its -wal/-shm files) and reconnect your mailboxes"
	if err.Error() != want {
		t.Fatalf("error = %q\nwant  %q", err.Error(), want)
	}
}
```

Also delete `TestOpenMigratesPreWebhookAccountDatabase` from `store_test.go` (its migration path is removed by this task).

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/store/ -run TestOpenRefusesPreTenancyDatabase`
Expected: FAIL — "pre-tenancy database was opened".

- [ ] **Step 3: Add model types**

In `internal/model/model.go`, after the `Account` struct add `DeveloperID`:

```go
type Account struct {
	ID string `json:"id"`
	// DeveloperID is the owner. Never serialised: a caller only ever sees
	// their own accounts, so it carries no information for them.
	DeveloperID string    `json:"-"`
	Provider    string    `json:"provider"`
	Email       string    `json:"email"`
	Name        string    `json:"name"`
	Status      string    `json:"status"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	LastSyncedAt *time.Time `json:"last_synced_at,omitempty"`
}
```

In the `Webhook` struct add, after `ID`:

```go
	DeveloperID string `json:"-"`
```

Append to the file:

```go
// Developer is a tenant: the integrator who signs in, holds API keys, and
// owns connected accounts.
type Developer struct {
	ID        string    `json:"id"`
	Email     string    `json:"email"`
	Name      string    `json:"name,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// APIKey is the listable view of a key. The full key is returned exactly
// once, at creation, and never stored.
type APIKey struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
}
```

- [ ] **Step 4: Rewrite the schema**

Replace the whole of `internal/store/schema.go` with:

```go
package store

const schema = `
PRAGMA journal_mode = WAL;
PRAGMA foreign_keys = ON;

-- A developer is a tenant. Everything below is owned by exactly one.
CREATE TABLE IF NOT EXISTS developers (
  id            TEXT PRIMARY KEY,
  email         TEXT NOT NULL UNIQUE,
  password_hash TEXT NOT NULL,
  name          TEXT NOT NULL DEFAULT '',
  created_at    INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS api_keys (
  id           TEXT PRIMARY KEY,
  developer_id TEXT NOT NULL REFERENCES developers(id) ON DELETE CASCADE,
  name         TEXT NOT NULL,
  prefix       TEXT NOT NULL,
  hash         TEXT NOT NULL UNIQUE,
  created_at   INTEGER NOT NULL,
  last_used_at INTEGER,
  revoked_at   INTEGER
);
CREATE INDEX IF NOT EXISTS api_keys_by_developer ON api_keys(developer_id);

CREATE TABLE IF NOT EXISTS sessions (
  id           TEXT PRIMARY KEY,
  developer_id TEXT NOT NULL REFERENCES developers(id) ON DELETE CASCADE,
  created_at   INTEGER NOT NULL,
  expires_at   INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS sessions_by_developer ON sessions(developer_id);

CREATE TABLE IF NOT EXISTS accounts (
  id            TEXT PRIMARY KEY,
  developer_id  TEXT NOT NULL REFERENCES developers(id) ON DELETE CASCADE,
  provider      TEXT NOT NULL,
  email         TEXT NOT NULL,
  name          TEXT NOT NULL DEFAULT '',
  status        TEXT NOT NULL,
  created_at    INTEGER NOT NULL,
  updated_at    INTEGER NOT NULL,
  last_synced_at INTEGER
);
-- The same mailbox may be connected by two developers as two accounts.
CREATE UNIQUE INDEX IF NOT EXISTS accounts_owner_email ON accounts(developer_id, email);

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
  created_at   INTEGER NOT NULL
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
```

- [ ] **Step 5: Pre-tenancy check in `Open`**

In `internal/store/store.go` replace the body of `Open` with:

```go
var ErrPreTenancy = errors.New("database predates multi-tenancy")

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
		return nil, fmt.Errorf("database %s predates multi-tenancy; delete it (and its -wal/-shm files) and reconnect your mailboxes", path)
	}

	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db}, nil
}

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
```

Delete the `migrations` loop and the `postMigration` exec from `Open` (they no longer exist in `schema.go`). Remove the now-unused `strings` import if `go vet` reports it.

- [ ] **Step 6: Run the store tests**

Run: `go test ./internal/store/`
Expected: `TestOpenRefusesPreTenancyDatabase` PASS. Several other store tests now FAIL with `NOT NULL constraint failed: accounts.developer_id` — expected; Task 3 fixes them.

- [ ] **Step 7: Commit**

```bash
git add internal/model/model.go internal/store/schema.go internal/store/store.go internal/store/store_test.go
git commit -m "feat(store): tenancy schema and pre-tenancy refusal"
```

---

### Task 3: Store — developers, sessions, API keys, and scoped account/webhook queries

**Files:**
- Create: `internal/store/developers.go`
- Modify: `internal/store/store.go` (accounts section), `internal/store/aux.go` (webhooks, oauth states)
- Test: `internal/store/store_test.go`

**Interfaces:**
- Produces (developers.go):
  - `CreateDeveloper(d model.Developer, passwordHash string) error` — `ErrConflict` on duplicate email
  - `DeveloperByEmail(email string) (model.Developer, string, error)` — returns the hash too
  - `GetDeveloper(id string) (model.Developer, error)`
  - `CreateSession(id, developerID string, expiresAt time.Time) error`
  - `SessionDeveloper(id string, now time.Time) (model.Developer, time.Time, error)` — returns expires_at
  - `ExtendSession(id string, expiresAt time.Time) error`
  - `DeleteSession(id string) error`
  - `CreateAPIKey(k model.APIKey, developerID, hash string) error`
  - `DeveloperByKeyHash(hash string) (model.Developer, model.APIKey, error)` — `ErrNotFound` if revoked
  - `TouchAPIKey(id string, at time.Time) error`
  - `ListAPIKeys(developerID string) ([]model.APIKey, error)`
  - `RevokeAPIKey(developerID, id string, at time.Time) error` — `ErrNotFound` if not owned
- Produces (scoped, store.go/aux.go):
  - `GetAccount(developerID, id string)`, `ListAccounts(developerID string)`, `AccountIDByEmail(developerID, email string)`
  - unscoped, explicitly named: `GetAnyAccount(id string)`, `ListAllAccounts()`
  - `ListWebhooks(developerID string)`, `GetWebhook(developerID, id string)`, `DeleteWebhook(developerID, id string)`, `ListAccountWebhooks(developerID, accountID string)`
  - unscoped: `ListWebhooksFor(accountID string)` (unchanged), `GetAnyWebhook(id string)`
  - `OAuthState.DeveloperID string`
- Produces: `store.ErrConflict`

- [ ] **Step 1: Update the test helper and write failing tests**

In `internal/store/store_test.go` replace `seedAccount` with:

```go
func seedDeveloper(t *testing.T, s *Store, id, email string) string {
	t.Helper()
	if err := s.CreateDeveloper(model.Developer{ID: id, Email: email, Name: "Dev"}, "$2a$12$hash"); err != nil {
		t.Fatal(err)
	}
	return id
}

func seedAccount(t *testing.T, s *Store) string {
	t.Helper()
	dev := seedDeveloper(t, s, "dev_1", "dev1@example.com")
	acct := model.Account{
		ID: "acc_1", DeveloperID: dev, Provider: "OUTLOOK", Email: "user@outlook.com",
		Name: "User", Status: model.AccountOK,
	}
	if err := s.UpsertAccount(acct); err != nil {
		t.Fatal(err)
	}
	return acct.ID
}
```

Update existing tests that construct accounts/webhooks inline to include `DeveloperID: "dev_1"` (in `TestListWebhooksForScopesByAccount` the second account `acc_2` also gets `DeveloperID: "dev_1"`; every `model.Webhook{...}` literal gets `DeveloperID: "dev_1"`). Replace calls `s.ListWebhooks()` with `s.ListWebhooks("dev_1")`, `s.GetWebhook("wh_1")` with `s.GetWebhook("dev_1", "wh_1")`, `s.DeleteWebhook("wh_1")` with `s.DeleteWebhook("dev_1", "wh_1")`. In `TestUpsertAccountConflictsOnEmail` (read it first) keep the same developer for both upserts so the conflict still fires.

Append new tests:

```go
func TestDeveloperRoundTripAndUniqueEmail(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateDeveloper(model.Developer{ID: "dev_1", Email: "a@x.com", Name: "A"}, "h1"); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateDeveloper(model.Developer{ID: "dev_2", Email: "a@x.com"}, "h2"); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate email err = %v, want ErrConflict", err)
	}
	d, hash, err := s.DeveloperByEmail("a@x.com")
	if err != nil || d.ID != "dev_1" || hash != "h1" || d.Name != "A" {
		t.Fatalf("DeveloperByEmail = %+v %q %v", d, hash, err)
	}
	if _, _, err := s.DeveloperByEmail("nobody@x.com"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown email err = %v", err)
	}
}

func TestSessionsExpireAndExtend(t *testing.T) {
	s := newTestStore(t)
	seedDeveloper(t, s, "dev_1", "a@x.com")
	now := time.Now().UTC().Truncate(time.Second)
	if err := s.CreateSession("sess1", "dev_1", now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	d, exp, err := s.SessionDeveloper("sess1", now)
	if err != nil || d.ID != "dev_1" || !exp.Equal(now.Add(time.Hour)) {
		t.Fatalf("SessionDeveloper = %+v %v %v", d, exp, err)
	}
	if _, _, err := s.SessionDeveloper("sess1", now.Add(2*time.Hour)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired session err = %v", err)
	}
	if err := s.ExtendSession("sess1", now.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.SessionDeveloper("sess1", now.Add(2*time.Hour)); err != nil {
		t.Fatalf("extended session rejected: %v", err)
	}
	if err := s.DeleteSession("sess1"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.SessionDeveloper("sess1", now); !errors.Is(err, ErrNotFound) {
		t.Fatal("deleted session still resolves")
	}
}

func TestAPIKeysResolveListAndRevoke(t *testing.T) {
	s := newTestStore(t)
	seedDeveloper(t, s, "dev_1", "a@x.com")
	seedDeveloper(t, s, "dev_2", "b@x.com")
	now := time.Now().UTC().Truncate(time.Second)
	if err := s.CreateAPIKey(model.APIKey{ID: "key_1", Name: "prod", Prefix: "um_abcdefghi", CreatedAt: now}, "dev_1", "hash1"); err != nil {
		t.Fatal(err)
	}
	d, k, err := s.DeveloperByKeyHash("hash1")
	if err != nil || d.ID != "dev_1" || k.ID != "key_1" || k.Prefix != "um_abcdefghi" {
		t.Fatalf("DeveloperByKeyHash = %+v %+v %v", d, k, err)
	}
	if err := s.TouchAPIKey("key_1", now); err != nil {
		t.Fatal(err)
	}
	keys, err := s.ListAPIKeys("dev_1")
	if err != nil || len(keys) != 1 || keys[0].LastUsedAt == nil || !keys[0].LastUsedAt.Equal(now) {
		t.Fatalf("ListAPIKeys = %+v %v", keys, err)
	}
	if other, _ := s.ListAPIKeys("dev_2"); len(other) != 0 {
		t.Fatalf("dev_2 sees dev_1's keys: %+v", other)
	}
	if err := s.RevokeAPIKey("dev_2", "key_1", now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-developer revoke err = %v, want ErrNotFound", err)
	}
	if err := s.RevokeAPIKey("dev_1", "key_1", now); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.DeveloperByKeyHash("hash1"); !errors.Is(err, ErrNotFound) {
		t.Fatal("revoked key still resolves")
	}
	keys, _ = s.ListAPIKeys("dev_1")
	if len(keys) != 1 || keys[0].RevokedAt == nil {
		t.Fatalf("revoked key should still be listed with revoked_at: %+v", keys)
	}
}

func TestAccountsAreScopedByDeveloper(t *testing.T) {
	s := newTestStore(t)
	seedDeveloper(t, s, "dev_1", "a@x.com")
	seedDeveloper(t, s, "dev_2", "b@x.com")
	for _, a := range []model.Account{
		{ID: "acc_1", DeveloperID: "dev_1", Provider: "OUTLOOK", Email: "m@outlook.com", Status: model.AccountOK},
		{ID: "acc_2", DeveloperID: "dev_2", Provider: "OUTLOOK", Email: "m@outlook.com", Status: model.AccountOK}, // same mailbox, other tenant
	} {
		if err := s.UpsertAccount(a); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.GetAccount("dev_1", "acc_2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("dev_1 read dev_2's account: %v", err)
	}
	if a, err := s.GetAccount("dev_1", "acc_1"); err != nil || a.DeveloperID != "dev_1" {
		t.Fatalf("GetAccount own = %+v %v", a, err)
	}
	if l, _ := s.ListAccounts("dev_2"); len(l) != 1 || l[0].ID != "acc_2" {
		t.Fatalf("ListAccounts(dev_2) = %+v", l)
	}
	if all, _ := s.ListAllAccounts(); len(all) != 2 {
		t.Fatalf("ListAllAccounts = %d, want 2", len(all))
	}
	if id, _ := s.AccountIDByEmail("dev_2", "m@outlook.com"); id != "acc_2" {
		t.Fatalf("AccountIDByEmail(dev_2) = %q", id)
	}
	if _, err := s.GetAnyAccount("acc_2"); err != nil {
		t.Fatalf("GetAnyAccount: %v", err)
	}
}

func TestDeletingDeveloperCascades(t *testing.T) {
	s := newTestStore(t)
	acct := seedAccount(t, s) // dev_1 / acc_1
	if err := s.CreateAPIKey(model.APIKey{ID: "key_1", Name: "n", Prefix: "p", CreatedAt: time.Now()}, "dev_1", "h"); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSession("sess1", "dev_1", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveWebhook(model.Webhook{ID: "wh_1", DeveloperID: "dev_1", URL: "https://x", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertEmail(model.Email{AccountID: acct, ID: "M1", Subject: "x", Date: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DELETE FROM developers WHERE id = 'dev_1'`); err != nil {
		t.Fatal(err)
	}
	for table, want := range map[string]int{"api_keys": 0, "sessions": 0, "accounts": 0, "emails": 0, "webhooks": 0} {
		var n int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != want {
			t.Fatalf("%s has %d rows after developer delete", table, n)
		}
	}
}

func TestWebhooksAreScopedByDeveloper(t *testing.T) {
	s := newTestStore(t)
	seedDeveloper(t, s, "dev_1", "a@x.com")
	seedDeveloper(t, s, "dev_2", "b@x.com")
	if err := s.SaveWebhook(model.Webhook{ID: "wh_1", DeveloperID: "dev_1", URL: "https://x", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetWebhook("dev_2", "wh_1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-developer GetWebhook err = %v", err)
	}
	if err := s.DeleteWebhook("dev_2", "wh_1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-developer DeleteWebhook err = %v", err)
	}
	if l, _ := s.ListWebhooks("dev_2"); len(l) != 0 {
		t.Fatalf("dev_2 lists dev_1's hooks: %+v", l)
	}
	if err := s.DeleteWebhook("dev_1", "wh_1"); err != nil {
		t.Fatal(err)
	}
}

func TestOAuthStateCarriesDeveloper(t *testing.T) {
	s := newTestStore(t)
	seedDeveloper(t, s, "dev_1", "a@x.com")
	if err := s.SaveOAuthState(OAuthState{State: "st", DeveloperID: "dev_1", Verifier: "v", ExpiresAt: time.Now().Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	got, err := s.TakeOAuthState("st")
	if err != nil || got.DeveloperID != "dev_1" {
		t.Fatalf("TakeOAuthState = %+v %v", got, err)
	}
}
```

Existing OAuth-state tests (`TestOAuthStateIsSingleUse`, `TestExpiredOAuthStateRejected`, `TestOAuthStateCarriesPendingWebhook`) must now seed a developer and set `DeveloperID: "dev_1"` on the state (FK).

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/store/`
Expected: build failure (`CreateDeveloper` undefined, etc.).

- [ ] **Step 3: Implement `developers.go`**

```go
// internal/store/developers.go
package store

import (
	"database/sql"
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
	err := s.db.QueryRow(`
		SELECT id, email, name, password_hash, created_at FROM developers WHERE email = ?`,
		strings.ToLower(strings.TrimSpace(email))).Scan(&d.ID, &d.Email, &d.Name, &hash, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return d, "", ErrNotFound
	}
	if err != nil {
		return d, "", err
	}
	d.CreatedAt = time.Unix(created, 0).UTC()
	return d, hash, nil
}

func (s *Store) GetDeveloper(id string) (model.Developer, error) {
	var d model.Developer
	var created int64
	err := s.db.QueryRow(`SELECT id, email, name, created_at FROM developers WHERE id = ?`, id).
		Scan(&d.ID, &d.Email, &d.Name, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return d, ErrNotFound
	}
	if err != nil {
		return d, err
	}
	d.CreatedAt = time.Unix(created, 0).UTC()
	return d, nil
}

// ---------- sessions ----------

func (s *Store) CreateSession(id, developerID string, expiresAt time.Time) error {
	_, err := s.db.Exec(`
		INSERT INTO sessions (id, developer_id, created_at, expires_at) VALUES (?,?,?,?)`,
		id, developerID, time.Now().Unix(), expiresAt.Unix())
	return err
}

// SessionDeveloper resolves a live session. Expired sessions read as
// ErrNotFound; they are not deleted here so a slow clock skew is recoverable.
func (s *Store) SessionDeveloper(id string, now time.Time) (model.Developer, time.Time, error) {
	var d model.Developer
	var created, exp int64
	err := s.db.QueryRow(`
		SELECT d.id, d.email, d.name, d.created_at, s.expires_at
		FROM sessions s JOIN developers d ON d.id = s.developer_id
		WHERE s.id = ? AND s.expires_at > ?`, id, now.Unix()).
		Scan(&d.ID, &d.Email, &d.Name, &created, &exp)
	if errors.Is(err, sql.ErrNoRows) {
		return d, time.Time{}, ErrNotFound
	}
	if err != nil {
		return d, time.Time{}, err
	}
	d.CreatedAt = time.Unix(created, 0).UTC()
	return d, time.Unix(exp, 0).UTC(), nil
}

func (s *Store) ExtendSession(id string, expiresAt time.Time) error {
	_, err := s.db.Exec(`UPDATE sessions SET expires_at = ? WHERE id = ?`, expiresAt.Unix(), id)
	return err
}

func (s *Store) DeleteSession(id string) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE id = ?`, id)
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
	var last, revoked sql.NullInt64
	err := s.db.QueryRow(`
		SELECT d.id, d.email, d.name, d.created_at, k.id, k.name, k.prefix, k.created_at, k.last_used_at, k.revoked_at
		FROM api_keys k JOIN developers d ON d.id = k.developer_id
		WHERE k.hash = ? AND k.revoked_at IS NULL`, hash).
		Scan(&d.ID, &d.Email, &d.Name, &dCreated, &k.ID, &k.Name, &k.Prefix, &kCreated, &last, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return d, k, ErrNotFound
	}
	if err != nil {
		return d, k, err
	}
	d.CreatedAt = time.Unix(dCreated, 0).UTC()
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
```

- [ ] **Step 4: Scope the account queries in `store.go`**

Replace the `// ---------- accounts ----------` section (from `UpsertAccount` through `ListAccounts`) with:

```go
// ---------- accounts ----------

func (s *Store) UpsertAccount(a model.Account) error {
	now := time.Now().Unix()
	_, err := s.db.Exec(`
		INSERT INTO accounts (id, developer_id, provider, email, name, status, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?)
		ON CONFLICT(developer_id, email) DO UPDATE SET
		  name = excluded.name, status = excluded.status, updated_at = excluded.updated_at`,
		a.ID, a.DeveloperID, a.Provider, a.Email, a.Name, a.Status, now, now)
	return err
}

// AccountIDByEmail lets the OAuth callback reconnect an existing mailbox
// instead of creating a duplicate account row — within one developer.
func (s *Store) AccountIDByEmail(developerID, email string) (string, error) {
	var id string
	err := s.db.QueryRow(`SELECT id FROM accounts WHERE developer_id = ? AND email = ?`,
		developerID, email).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return id, err
}

const accountSelect = `
	SELECT id, developer_id, provider, email, name, status, created_at, updated_at, last_synced_at
	FROM accounts`

// GetAccount is the tenant-scoped read every API handler uses. A row owned
// by another developer is ErrNotFound, not an authorization error, so ids
// cannot be probed across tenants.
func (s *Store) GetAccount(developerID, id string) (model.Account, error) {
	return scanAccount(s.db.QueryRow(accountSelect+` WHERE developer_id = ? AND id = ?`, developerID, id))
}

// GetAnyAccount is UNSCOPED: for internal callers (sync, token custody,
// push) that hold an account id and no tenant. Never call from a handler.
func (s *Store) GetAnyAccount(id string) (model.Account, error) {
	return scanAccount(s.db.QueryRow(accountSelect+` WHERE id = ?`, id))
}

func (s *Store) ListAccounts(developerID string) ([]model.Account, error) {
	return s.queryAccounts(accountSelect+` WHERE developer_id = ? ORDER BY created_at`, developerID)
}

// ListAllAccounts is UNSCOPED: for the poll and subscription loops.
func (s *Store) ListAllAccounts() ([]model.Account, error) {
	return s.queryAccounts(accountSelect + ` ORDER BY created_at`)
}

func (s *Store) queryAccounts(q string, args ...any) ([]model.Account, error) {
	rows, err := s.db.Query(q, args...)
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
```

Update `scanAccount` to scan `developer_id` second:

```go
	err := r.Scan(&a.ID, &a.DeveloperID, &a.Provider, &a.Email, &a.Name, &a.Status, &created, &updated, &synced)
```

- [ ] **Step 5: Scope webhooks and OAuth states in `aux.go`**

Replace the webhook section (`SaveWebhook` through `queryWebhooks`, and `DeleteWebhook`) with:

```go
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
	return s.queryWebhooks(webhookSelect+`
		WHERE account_id = ?
		   OR (account_id = '' AND developer_id = (SELECT developer_id FROM accounts WHERE id = ?))`,
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
```

In `OAuthState` add `DeveloperID string` after `State`. In `SaveOAuthState`, `TakeOAuthState`, `PeekOAuthState` add `developer_id` to the column lists and scans:

```go
func (s *Store) SaveOAuthState(o OAuthState) error {
	_, err := s.db.Exec(`
		INSERT INTO oauth_states (state, developer_id, provider, verifier, success_url, failure_url, notify_url, webhook_json, created_at, expires_at)
		VALUES (?,?,?,?,?,?,?,?,?,?)`,
		o.State, o.DeveloperID, o.Provider, o.Verifier, o.SuccessURL, o.FailureURL, o.NotifyURL,
		encodePendingWebhook(o.Webhook), time.Now().Unix(), o.ExpiresAt.Unix())
	return err
}
```

and in both readers:

```go
		SELECT state, developer_id, provider, verifier, success_url, failure_url, notify_url, webhook_json, expires_at
		FROM oauth_states WHERE state = ?`, state).
		Scan(&o.State, &o.DeveloperID, &o.Provider, &o.Verifier, &o.SuccessURL, &o.FailureURL, &o.NotifyURL, &wh, &exp)
```

- [ ] **Step 6: Run the store tests**

Run: `go test ./internal/store/`
Expected: PASS. (Other packages will not compile yet; that is expected until Tasks 4–6.)

- [ ] **Step 7: Commit**

```bash
git add internal/store
git commit -m "feat(store): developers, sessions, API keys, tenant-scoped queries"
```

---

### Task 4: `internal/auth` — passwords, sessions, API keys

**Files:**
- Create: `internal/auth/auth.go`, `internal/auth/auth_test.go`
- Modify: `go.mod` (via `go get golang.org/x/crypto`)

**Interfaces:**
- Consumes: store methods from Task 3; `logx.From`.
- Produces:
  - `auth.New(s *store.Store, log *slog.Logger, sessionTTL time.Duration) *Service`
  - `(*Service).Signup(ctx, email, password, name string) (model.Developer, error)` — `auth.ErrInvalidInput` (bad email / short password), `auth.ErrEmailTaken`
  - `(*Service).Login(ctx, email, password string) (model.Developer, error)` — `auth.ErrInvalidCredentials` for unknown email *and* wrong password
  - `(*Service).NewSession(ctx, developerID string) (token string, expires time.Time, err error)`
  - `(*Service).SessionDeveloper(ctx, token string) (model.Developer, error)` — slides expiry when >24h consumed
  - `(*Service).DeleteSession(ctx, token string) error`
  - `(*Service).NewAPIKey(ctx, developerID, name string) (fullKey string, k model.APIKey, err error)`
  - `(*Service).KeyDeveloper(ctx, fullKey string) (model.Developer, model.APIKey, error)` — touches `last_used_at` at most once/minute
  - `(*Service).RevokeKey(ctx, developerID, keyID string) error`
  - `auth.HashKey(fullKey string) string` (hex sha256; exported for tests)

- [ ] **Step 1: Add the dependency**

Run: `go get golang.org/x/crypto@latest && go mod tidy`
Expected: `go.mod` gains `golang.org/x/crypto` as a direct requirement.

- [ ] **Step 2: Write the failing tests**

```go
// internal/auth/auth_test.go
package auth

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gauravrautela/unified-messaging/internal/logx"
	"github.com/gauravrautela/unified-messaging/internal/store"
)

func newService(t *testing.T) (*Service, *store.Store, *logx.Records) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	log, recs := logx.Capture()
	return New(db, log, 30*24*time.Hour), db, recs
}

func TestSignupThenLogin(t *testing.T) {
	svc, _, recs := newService(t)
	ctx := context.Background()
	d, err := svc.Signup(ctx, " Dev@Example.com ", "correct horse battery", "Dev")
	if err != nil {
		t.Fatal(err)
	}
	if d.Email != "dev@example.com" || !strings.HasPrefix(d.ID, "dev_") {
		t.Fatalf("developer = %+v", d)
	}
	got, err := svc.Login(ctx, "dev@example.com", "correct horse battery")
	if err != nil || got.ID != d.ID {
		t.Fatalf("login = %+v %v", got, err)
	}
	if recs.Contains("correct horse battery") {
		t.Fatal("password appeared in logs")
	}
}

func TestLoginFailuresAreUniform(t *testing.T) {
	svc, _, _ := newService(t)
	ctx := context.Background()
	if _, err := svc.Signup(ctx, "a@x.com", "longenoughpassword", ""); err != nil {
		t.Fatal(err)
	}
	_, errWrong := svc.Login(ctx, "a@x.com", "wrongpassword!")
	_, errUnknown := svc.Login(ctx, "nobody@x.com", "wrongpassword!")
	if !errors.Is(errWrong, ErrInvalidCredentials) || !errors.Is(errUnknown, ErrInvalidCredentials) {
		t.Fatalf("errors = %v / %v, want both ErrInvalidCredentials", errWrong, errUnknown)
	}
	if errWrong.Error() != errUnknown.Error() {
		t.Fatal("error text differs between unknown email and wrong password")
	}
}

func TestSignupValidation(t *testing.T) {
	svc, _, _ := newService(t)
	ctx := context.Background()
	if _, err := svc.Signup(ctx, "not-an-email", "longenoughpassword", ""); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("bad email err = %v", err)
	}
	if _, err := svc.Signup(ctx, "a@x.com", "short", ""); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("short password err = %v", err)
	}
	if _, err := svc.Signup(ctx, "a@x.com", "longenoughpassword", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Signup(ctx, "A@X.COM", "longenoughpassword", ""); !errors.Is(err, ErrEmailTaken) {
		t.Fatalf("duplicate email err = %v", err)
	}
}

func TestSessionsResolveExpireAndSlide(t *testing.T) {
	svc, db, recs := newService(t)
	ctx := context.Background()
	d, _ := svc.Signup(ctx, "a@x.com", "longenoughpassword", "")
	tok, exp, err := svc.NewSession(ctx, d.ID)
	if err != nil || tok == "" || exp.Before(time.Now().Add(29*24*time.Hour)) {
		t.Fatalf("NewSession = %q %v %v", tok, exp, err)
	}
	if got, err := svc.SessionDeveloper(ctx, tok); err != nil || got.ID != d.ID {
		t.Fatalf("SessionDeveloper = %+v %v", got, err)
	}
	if recs.Contains(tok) {
		t.Fatal("session token appeared in logs")
	}
	// Age the session by two days; the next use must push expiry forward.
	if err := db.ExtendSession(tok, exp.Add(-2*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SessionDeveloper(ctx, tok); err != nil {
		t.Fatal(err)
	}
	if _, newExp, _ := db.SessionDeveloper(tok, time.Now()); !newExp.After(exp.Add(-24*time.Hour)) {
		t.Fatalf("expiry not slid: %v", newExp)
	}
	if err := svc.DeleteSession(ctx, tok); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SessionDeveloper(ctx, tok); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("deleted session err = %v", err)
	}
	if _, err := svc.SessionDeveloper(ctx, "garbage"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("garbage session err = %v", err)
	}
}

func TestAPIKeysLifecycle(t *testing.T) {
	svc, db, recs := newService(t)
	ctx := context.Background()
	d, _ := svc.Signup(ctx, "a@x.com", "longenoughpassword", "")
	full, k, err := svc.NewAPIKey(ctx, d.ID, "prod")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(full, "um_") || len(full) != 43 {
		t.Fatalf("key = %q, want um_ + 40 chars", full)
	}
	if k.Prefix != full[:12] || k.Name != "prod" {
		t.Fatalf("APIKey = %+v", k)
	}
	if recs.Contains(full) || recs.Contains(HashKey(full)) {
		t.Fatal("full key or its hash appeared in logs")
	}
	if _, _, err := db.DeveloperByKeyHash(full); err == nil {
		t.Fatal("key stored in plaintext")
	}
	got, gk, err := svc.KeyDeveloper(ctx, full)
	if err != nil || got.ID != d.ID || gk.ID != k.ID {
		t.Fatalf("KeyDeveloper = %+v %+v %v", got, gk, err)
	}
	if _, _, err := svc.KeyDeveloper(ctx, full[:42]+"X"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("tampered key err = %v", err)
	}
	keys, _ := db.ListAPIKeys(d.ID)
	if len(keys) != 1 || keys[0].LastUsedAt == nil {
		t.Fatalf("last_used_at not touched: %+v", keys)
	}
	if err := svc.RevokeKey(ctx, d.ID, k.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.KeyDeveloper(ctx, full); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("revoked key err = %v", err)
	}
	if err := svc.RevokeKey(ctx, "dev_other", k.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cross-developer revoke err = %v", err)
	}
}
```

- [ ] **Step 3: Run to verify failure**

Run: `go test ./internal/auth/`
Expected: build failure — package does not exist.

- [ ] **Step 4: Implement**

```go
// internal/auth/auth.go
// Package auth owns developer identity: password hashing, browser sessions,
// and API keys. It is the only package that imports bcrypt or knows the key
// format, so the rest of the service handles opaque tokens at most.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/gauravrautela/unified-messaging/internal/accounts"
	"github.com/gauravrautela/unified-messaging/internal/logx"
	"github.com/gauravrautela/unified-messaging/internal/model"
	"github.com/gauravrautela/unified-messaging/internal/store"
)

var (
	ErrInvalidInput       = errors.New("invalid email or password format")
	ErrEmailTaken         = errors.New("could not create account")
	ErrInvalidCredentials = errors.New("invalid email or password")
)

const (
	bcryptCost   = 12
	minPassword  = 10
	keyPrefix    = "um_"
	keyRandom    = 40
	keyAlphabet  = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	prefixLen    = 12
	touchEvery   = time.Minute
	slideAfter   = 24 * time.Hour
)

// dummyHash is compared when the email is unknown so both login failure
// paths cost one bcrypt verification and cannot be told apart by timing.
var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("dummy-password-for-timing"), bcryptCost)

type Service struct {
	store      *store.Store
	log        *slog.Logger
	sessionTTL time.Duration

	mu          sync.Mutex
	lastTouched map[string]time.Time // key id -> last last_used_at write
}

func New(s *store.Store, log *slog.Logger, sessionTTL time.Duration) *Service {
	return &Service{store: s, log: log.With("component", "auth"), sessionTTL: sessionTTL, lastTouched: map[string]time.Time{}}
}

func (a *Service) logger(ctx context.Context) *slog.Logger {
	return logx.From(ctx).With("component", "auth")
}

// ---- developers ----

func (a *Service) Signup(ctx context.Context, email, password, name string) (model.Developer, error) {
	log := a.logger(ctx)
	email = strings.ToLower(strings.TrimSpace(email))
	at := strings.Index(email, "@")
	if at < 1 || !strings.Contains(email[at:], ".") {
		log.Debug("signup rejected", "reason", "malformed email", "email", email)
		return model.Developer{}, ErrInvalidInput
	}
	if len(password) < minPassword {
		log.Debug("signup rejected", "reason", "password too short", "email", email, "len", len(password))
		return model.Developer{}, ErrInvalidInput
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	if err != nil {
		return model.Developer{}, err
	}
	id, err := accounts.NewID("dev")
	if err != nil {
		return model.Developer{}, err
	}
	d := model.Developer{ID: id, Email: email, Name: strings.TrimSpace(name), CreatedAt: time.Now().UTC()}
	if err := a.store.CreateDeveloper(d, string(hash)); err != nil {
		if errors.Is(err, store.ErrConflict) {
			log.Debug("signup rejected", "reason", "email taken", "email", email)
			return model.Developer{}, ErrEmailTaken
		}
		return model.Developer{}, err
	}
	log.Info("developer signed up", "developer_id", d.ID, "email", d.Email)
	return d, nil
}

func (a *Service) Login(ctx context.Context, email, password string) (model.Developer, error) {
	log := a.logger(ctx)
	email = strings.ToLower(strings.TrimSpace(email))
	start := time.Now()
	d, hash, err := a.store.DeveloperByEmail(email)
	if errors.Is(err, store.ErrNotFound) {
		// Burn the same bcrypt cost as a real comparison.
		_ = bcrypt.CompareHashAndPassword(dummyHash, []byte(password))
		log.Debug("login failed", "reason", "unknown email", "email", email, "bcrypt_ms", time.Since(start).Milliseconds())
		return model.Developer{}, ErrInvalidCredentials
	}
	if err != nil {
		return model.Developer{}, err
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)); err != nil {
		log.Debug("login failed", "reason", "wrong password", "developer_id", d.ID, "bcrypt_ms", time.Since(start).Milliseconds())
		return model.Developer{}, ErrInvalidCredentials
	}
	log.Info("developer logged in", "developer_id", d.ID, "bcrypt_ms", time.Since(start).Milliseconds())
	return d, nil
}

// ---- sessions ----

func (a *Service) NewSession(ctx context.Context, developerID string) (string, time.Time, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", time.Time{}, err
	}
	tok := base64.RawURLEncoding.EncodeToString(b)
	exp := time.Now().UTC().Add(a.sessionTTL)
	if err := a.store.CreateSession(tok, developerID, exp); err != nil {
		return "", time.Time{}, err
	}
	a.logger(ctx).Info("session created", "developer_id", developerID, "expires_at", exp)
	return tok, exp, nil
}

// SessionDeveloper resolves a cookie value. Expiry slides forward once more
// than slideAfter of the TTL has been consumed, so an active developer is
// never logged out mid-work.
func (a *Service) SessionDeveloper(ctx context.Context, token string) (model.Developer, error) {
	log := a.logger(ctx)
	if token == "" {
		return model.Developer{}, ErrInvalidCredentials
	}
	now := time.Now().UTC()
	d, exp, err := a.store.SessionDeveloper(token, now)
	if errors.Is(err, store.ErrNotFound) {
		log.Debug("session lookup", "result", "miss or expired")
		return model.Developer{}, ErrInvalidCredentials
	}
	if err != nil {
		return model.Developer{}, err
	}
	if exp.Sub(now) < a.sessionTTL-slideAfter {
		newExp := now.Add(a.sessionTTL)
		if err := a.store.ExtendSession(token, newExp); err != nil {
			log.Warn("extending session", "developer_id", d.ID, "err", err)
		} else {
			log.Debug("session extended", "developer_id", d.ID, "expires_at", newExp)
		}
	}
	log.Debug("session lookup", "result", "hit", "developer_id", d.ID)
	return d, nil
}

func (a *Service) DeleteSession(ctx context.Context, token string) error {
	a.logger(ctx).Info("session deleted")
	return a.store.DeleteSession(token)
}

// ---- API keys ----

// HashKey is how keys are stored and looked up. Keys carry ~238 bits of
// entropy, so a fast hash is sufficient and keeps per-request cost trivial.
func HashKey(full string) string {
	sum := sha256.Sum256([]byte(full))
	return hex.EncodeToString(sum[:])
}

func (a *Service) NewAPIKey(ctx context.Context, developerID, name string) (string, model.APIKey, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", model.APIKey{}, ErrInvalidInput
	}
	full, err := randomKey()
	if err != nil {
		return "", model.APIKey{}, err
	}
	id, err := accounts.NewID("key")
	if err != nil {
		return "", model.APIKey{}, err
	}
	k := model.APIKey{ID: id, Name: name, Prefix: full[:prefixLen], CreatedAt: time.Now().UTC()}
	if err := a.store.CreateAPIKey(k, developerID, HashKey(full)); err != nil {
		return "", model.APIKey{}, err
	}
	a.logger(ctx).Info("api key created", "developer_id", developerID, "key_id", k.ID, "name", k.Name, "prefix", k.Prefix)
	return full, k, nil
}

func randomKey() (string, error) {
	b := make([]byte, keyRandom)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	out := make([]byte, keyRandom)
	for i, v := range b {
		out[i] = keyAlphabet[int(v)%len(keyAlphabet)]
	}
	return keyPrefix + string(out), nil
}

func (a *Service) KeyDeveloper(ctx context.Context, full string) (model.Developer, model.APIKey, error) {
	log := a.logger(ctx)
	if !strings.HasPrefix(full, keyPrefix) || len(full) != len(keyPrefix)+keyRandom {
		log.Debug("api key lookup", "result", "malformed")
		return model.Developer{}, model.APIKey{}, ErrInvalidCredentials
	}
	d, k, err := a.store.DeveloperByKeyHash(HashKey(full))
	if errors.Is(err, store.ErrNotFound) {
		log.Debug("api key lookup", "result", "miss or revoked", "prefix", full[:prefixLen])
		return model.Developer{}, model.APIKey{}, ErrInvalidCredentials
	}
	if err != nil {
		return model.Developer{}, model.APIKey{}, err
	}
	log.Debug("api key lookup", "result", "hit", "developer_id", d.ID, "key_id", k.ID, "prefix", k.Prefix)
	a.touch(ctx, k.ID)
	return d, k, nil
}

// touch records last_used_at at most once per touchEvery per key, so a busy
// integration does not turn every request into a write.
func (a *Service) touch(ctx context.Context, keyID string) {
	now := time.Now().UTC()
	a.mu.Lock()
	last, seen := a.lastTouched[keyID]
	if seen && now.Sub(last) < touchEvery {
		a.mu.Unlock()
		return
	}
	a.lastTouched[keyID] = now
	a.mu.Unlock()
	if err := a.store.TouchAPIKey(keyID, now); err != nil {
		a.logger(ctx).Warn("touching api key", "key_id", keyID, "err", err)
	}
}

func (a *Service) RevokeKey(ctx context.Context, developerID, keyID string) error {
	err := a.store.RevokeAPIKey(developerID, keyID, time.Now().UTC())
	if err == nil {
		a.logger(ctx).Info("api key revoked", "developer_id", developerID, "key_id", keyID)
	}
	return err
}

var _ = fmt.Sprintf // keep fmt available for future error wrapping
```

Remove the trailing `var _ = fmt.Sprintf` line and the `fmt` import if `go vet` reports `fmt` unused.

- [ ] **Step 5: Run tests**

Run: `go test ./internal/auth/`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add go.mod go.sum internal/auth
git commit -m "feat(auth): developer signup/login, sessions, hashed API keys"
```

---

### Task 5: Thread the developer through `accounts.Manager` and fix internal callers

**Files:**
- Modify: `internal/accounts/accounts.go:80-116` (`Connect`), `:146` (`GetAccount` → `GetAnyAccount`)
- Modify: `internal/syncer/syncer.go:158,183`, `internal/syncer/subscriptions.go:43,67,220,255`
- Modify: `internal/events/events.go:115` (unchanged call, but `retryDue` uses `GetWebhook` → `GetAnyWebhook`)
- Test: `internal/syncer/syncer_test.go`, `internal/syncer/provider_contract_test.go`, `internal/events/events_test.go`

**Interfaces:**
- Produces: `(*accounts.Manager).Connect(ctx, developerID, providerName, code, verifier string) (model.Account, error)`

- [ ] **Step 1: Update tests to seed a developer**

In `internal/syncer/syncer_test.go` `TestSyncAccountBackfillThenIncremental`, before `db.UpsertAccount(...)` insert:

```go
	if err := db.CreateDeveloper(model.Developer{ID: "dev_1", Email: "dev@example.com"}, "hash"); err != nil {
		t.Fatal(err)
	}
```

and add `DeveloperID: "dev_1"` to the `model.Account{...}` literal and to the `model.Webhook{...}` literal. Do the same in `TestWakeCollapsesWhileInflight` and in `provider_contract_test.go` wherever an account is upserted (search for `UpsertAccount`).

In `internal/events/events_test.go`, add a helper and use it in every test before saving webhooks:

```go
func seedTenant(t *testing.T, db *store.Store) {
	t.Helper()
	if err := db.CreateDeveloper(model.Developer{ID: "dev_1", Email: "dev@example.com"}, "hash"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"acc_1", "acc_2"} {
		if err := db.UpsertAccount(model.Account{ID: id, DeveloperID: "dev_1", Provider: "OUTLOOK", Email: id + "@x.com", Status: model.AccountOK}); err != nil {
			t.Fatal(err)
		}
	}
}
```

Every `model.Webhook{...}` literal in `events_test.go` gets `DeveloperID: "dev_1"`. Replace `db.ListDeliveries("wh_1")` calls as-is (unscoped, unchanged).

- [ ] **Step 2: Run to verify failure**

Run: `go build ./... && go test ./internal/syncer/ ./internal/events/`
Expected: build errors — `ListAccounts` needs an argument, `GetAccount` needs two, `Connect` signature.

- [ ] **Step 3: Update `accounts.Connect`**

```go
// Connect completes an OAuth handshake and records the account under the
// developer who minted the connect link.
//
// Reconnecting an address that developer already has updates the existing
// record in place rather than creating a duplicate, so callers keep the same
// account_id. The same address under a different developer is a different
// account.
func (m *Manager) Connect(ctx context.Context, developerID, providerName, code, verifier string) (model.Account, error) {
	log := logx.From(ctx).With("component", "accounts", "developer_id", developerID, "provider", providerName)
	p, err := m.registry.Get(providerName)
	if err != nil {
		return model.Account{}, err
	}
	auth := p.Auth()

	log.Debug("exchanging authorization code")
	tok, err := auth.Exchange(ctx, code, verifier)
	if err != nil {
		log.Warn("code exchange failed", "err", err)
		return model.Account{}, err
	}
	if tok.RefreshToken == "" {
		return model.Account{}, errors.New("accounts: no refresh token returned; is offline_access in the requested scopes?")
	}
	log.Debug("code exchanged", "access_expires_at", tok.ExpiresAt, "scope", tok.Scope)

	identity, err := auth.Identify(ctx, tok.AccessToken)
	if err != nil {
		return model.Account{}, fmt.Errorf("identifying account: %w", err)
	}
	if identity.Email == "" {
		return model.Account{}, errors.New("accounts: provider did not report an address")
	}

	id, err := m.store.AccountIDByEmail(developerID, identity.Email)
	reconnect := err == nil
	if errors.Is(err, store.ErrNotFound) {
		id, err = newID("acc")
		if err != nil {
			return model.Account{}, err
		}
	} else if err != nil {
		return model.Account{}, err
	}

	if err := m.store.UpsertAccount(model.Account{
		ID:          id,
		DeveloperID: developerID,
		Provider:    p.Name(),
		Email:       identity.Email,
		Name:        identity.Name,
		Status:      model.AccountOK,
	}); err != nil {
		return model.Account{}, err
	}
	realID, err := m.store.AccountIDByEmail(developerID, identity.Email)
	if err != nil {
		return model.Account{}, err
	}
	if err := m.persist(realID, tok); err != nil {
		return model.Account{}, err
	}
	log.Info("account connected", "account_id", realID, "email", identity.Email, "reconnect", reconnect)
	return m.store.GetAnyAccount(realID)
}
```

Add `"github.com/gauravrautela/unified-messaging/internal/logx"` to the imports. At line ~146 replace `m.store.GetAccount(accountID)` with `m.store.GetAnyAccount(accountID)`.

- [ ] **Step 4: Fix syncer and events callers**

- `internal/syncer/syncer.go:158` and `subscriptions.go:43`: `s.store.ListAccounts()` → `s.store.ListAllAccounts()`.
- `internal/syncer/syncer.go:183`, `subscriptions.go:67,220,255`: `s.store.GetAccount(x)` → `s.store.GetAnyAccount(x)`.
- `internal/events/events.go` in `retryDue`: `d.store.GetWebhook(dl.WebhookID)` → `d.store.GetAnyWebhook(dl.WebhookID)`.

- [ ] **Step 5: Run tests**

Run: `go test ./internal/syncer/ ./internal/events/ ./internal/accounts/ ./internal/store/ ./internal/auth/`
Expected: PASS. (`internal/api` and `cmd/server` still do not compile — Task 6.)

- [ ] **Step 6: Commit**

```bash
git add internal/accounts internal/syncer internal/events
git commit -m "feat: connect accounts under a developer; explicit unscoped store reads"
```

---

### Task 6: API — config, request-id + developer middleware, and tenant scoping of every handler

**Files:**
- Modify: `internal/config/config.go`
- Create: `internal/api/context.go`
- Modify: `internal/api/api.go` (`Server`, `NewServer`, `Routes`, middleware)
- Modify: `internal/api/handlers_mail.go:20-45` (`resolveID`), `internal/api/handlers_misc.go` (accounts + webhooks handlers), `internal/api/handlers_connect.go` (hosted-auth, callback, connect-time webhook)
- Modify: `internal/api/api_test.go` (test helper), `cmd/server/main.go` (wiring)

**Interfaces:**
- Consumes: `auth.Service` (Task 4); scoped store methods (Task 3); `logx` (Task 1).
- Produces:
  - `config.Config` without `APIKey`; with `SessionTTL time.Duration` (env `SESSION_TTL_DAYS`, default 30 days)
  - `api.NewServer(cfg, store, registry, accts, syncer, authSvc *auth.Service, log)`
  - `developerFrom(ctx) (model.Developer, bool)`, `authKindFrom(ctx) string` (`"api_key"` / `"session"` / `""`)
  - `(*Server).withRequestID`, `(*Server).withDeveloper` middleware
  - `(*Server).resolveID(w, r, id string) (model.Account, provider.Mailbox, bool)` — now takes the request to read the developer
  - test helpers in `api_test.go`: `newTestServer(t) (*Server, *store.Store)`, `seedDev(t, s, db, email string) (dev model.Developer, key string)`, `withKey(req, key) *http.Request`, `withSession(t, s, req, devID) *http.Request`

- [ ] **Step 1: Config**

In `internal/config/config.go` remove the `APIKey` field, its comment, its `Load` assignment, and the `API_KEY is required` check. Add:

```go
	// SessionTTL is how long a dashboard login lasts without use.
	SessionTTL time.Duration
```

and in `Load`, after `RedirectURI`:

```go
		SessionTTL:    time.Duration(envInt("SESSION_TTL_DAYS", 30)) * 24 * time.Hour,
```

with helper:

```go
func envInt(k string, def int) int {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return def
	}
	return n
}
```

Add `"strconv"` and `"time"` to the imports.

- [ ] **Step 2: Rewrite the API test helper and write the failing middleware tests**

In `internal/api/api_test.go` replace `newTestServer`, `stubTokens`, `authed`, and `TestAPIKeyRequired` with:

```go
func newTestServerWithLog(t *testing.T) (*Server, *store.Store, *logx.Records) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "api.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	cfg := &config.Config{
		ClientID: "client-123", Tenant: "consumers",
		RedirectURI: "http://localhost:8080/oauth/callback",
		Scopes:      []string{"offline_access", "Mail.Read"},
		SessionTTL:  30 * 24 * time.Hour,
	}
	log, recs := logx.Capture()
	a := outlook.NewAuth(cfg.ClientID, "", cfg.Tenant, cfg.RedirectURI, cfg.Scopes)
	registry := provider.NewRegistry(outlook.New(a, stubTokens{}))
	disp := events.NewDispatcher(db, log)
	sync := syncer.New(db, registry, nil, disp, log, syncer.Options{PollInterval: time.Hour})
	authSvc := auth.New(db, log, cfg.SessionTTL)
	return NewServer(cfg, db, registry, nil, sync, authSvc, log), db, recs
}

func newTestServer(t *testing.T) (*Server, *store.Store) {
	s, db, _ := newTestServerWithLog(t)
	return s, db
}

type stubTokens struct{}

func (stubTokens) AccessToken(context.Context, string, bool) (string, error) {
	return "test-token", nil
}

// seedDev creates a developer and one API key, returning the full key.
func seedDev(t *testing.T, s *Server, email string) (model.Developer, string) {
	t.Helper()
	ctx := context.Background()
	d, err := s.auth.Signup(ctx, email, "longenoughpassword", "Dev")
	if err != nil {
		t.Fatal(err)
	}
	key, _, err := s.auth.NewAPIKey(ctx, d.ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	return d, key
}

func withKey(req *http.Request, key string) *http.Request {
	req.Header.Set("Authorization", "Bearer "+key)
	return req
}

func withSession(t *testing.T, s *Server, req *http.Request, devID string) *http.Request {
	t.Helper()
	tok, _, err := s.auth.NewSession(context.Background(), devID)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: tok})
	return req
}

func TestAPIRequiresDeveloperCredential(t *testing.T) {
	s, _ := newTestServer(t)
	h := s.Routes()
	dev, key := seedDev(t, s, "a@x.com")

	do := func(req *http.Request) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	if rec := do(httptest.NewRequest(http.MethodGet, "/api/v1/accounts", nil)); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no credential: %d, want 401", rec.Code)
	}
	if rec := do(withKey(httptest.NewRequest(http.MethodGet, "/api/v1/accounts", nil), key)); rec.Code != http.StatusOK {
		t.Fatalf("api key: %d, want 200", rec.Code)
	}
	if rec := do(withKey(httptest.NewRequest(http.MethodGet, "/api/v1/accounts", nil), "um_"+strings.Repeat("x", 40))); rec.Code != http.StatusUnauthorized {
		t.Fatalf("bogus key: %d, want 401", rec.Code)
	}
	if rec := do(withSession(t, s, httptest.NewRequest(http.MethodGet, "/api/v1/accounts", nil), dev.ID)); rec.Code != http.StatusOK {
		t.Fatalf("session cookie: %d, want 200", rec.Code)
	}
	if rec := do(httptest.NewRequest(http.MethodGet, "/api/v1/accounts", nil)); rec.Header().Get("X-Request-Id") == "" {
		t.Fatal("responses must carry X-Request-Id")
	}
}

func TestRevokedKeyIsRejected(t *testing.T) {
	s, _ := newTestServer(t)
	dev, key := seedDev(t, s, "a@x.com")
	keys, _ := s.store.ListAPIKeys(dev.ID)
	if err := s.auth.RevokeKey(context.Background(), dev.ID, keys[0].ID); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, withKey(httptest.NewRequest(http.MethodGet, "/api/v1/accounts", nil), key))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("revoked key: %d, want 401", rec.Code)
	}
}
```

Add imports `"github.com/gauravrautela/unified-messaging/internal/auth"` and `"github.com/gauravrautela/unified-messaging/internal/logx"`; keep `model`. Then update every existing test in `api_test.go`:

- Replace every `authed(req)` with `withKey(req, key)` where `key` comes from a `seedDev(t, s, "a@x.com")` call at the top of that test; replace the three literal `req.Header.Set("Authorization", "Bearer test-key")` lines the same way.
- Every `db.UpsertAccount(model.Account{ID: "acc_1", ...})` in tests gets `DeveloperID: dev.ID`.
- Every `db.SaveWebhook(model.Webhook{...})` gets `DeveloperID: dev.ID`.
- `TestDashboardServesWithoutAPIKey`, `TestMailPageServesWithoutAPIKey`, `TestDashboardLinksToMailPage`, `TestDashboardRendersWebhookForm`: these change meaning in Task 8; for now change the request to `withSession(t, s, req, dev.ID)` and rename the first two to `TestDashboardServesWithSession` / `TestMailPageServesWithSession`. Delete the assertion on `id="gate-form"` in the mail-page test (the gate is removed in Task 8); keep the folder/message pane assertions.
- `TestHostedAuthStoresPendingWebhook` / `TestHostedAuthMintsSingleUseConnectLink`: after `db.PeekOAuthState(resp.State)` add `if pending.DeveloperID != dev.ID { t.Fatalf("pending state owner = %q", pending.DeveloperID) }` (declare `pending, err :=` where currently `_` is used).

- [ ] **Step 3: Run to verify failure**

Run: `go test ./internal/api/`
Expected: build failure (`NewServer` arity, `s.auth`, `sessionCookie`, `developerFrom` undefined).

- [ ] **Step 4: Context helpers**

```go
// internal/api/context.go
package api

import (
	"context"

	"github.com/gauravrautela/unified-messaging/internal/model"
)

type ctxKey int

const (
	ctxDeveloper ctxKey = iota
	ctxAuthKind
)

const (
	authKindAPIKey  = "api_key"
	authKindSession = "session"
)

func withDeveloperCtx(ctx context.Context, d model.Developer, kind string) context.Context {
	ctx = context.WithValue(ctx, ctxDeveloper, d)
	return context.WithValue(ctx, ctxAuthKind, kind)
}

// developerFrom returns the caller resolved by withDeveloper. Handlers under
// /api/v1 can rely on ok being true; it is false only outside that tree.
func developerFrom(ctx context.Context) (model.Developer, bool) {
	d, ok := ctx.Value(ctxDeveloper).(model.Developer)
	return d, ok
}

func authKindFrom(ctx context.Context) string {
	k, _ := ctx.Value(ctxAuthKind).(string)
	return k
}
```

- [ ] **Step 5: Server struct, middleware, routes**

In `internal/api/api.go`:

```go
type Server struct {
	cfg      *config.Config
	store    *store.Store
	registry *provider.Registry
	accts    *accounts.Manager
	syncer   *syncer.Syncer
	auth     *auth.Service
	log      *slog.Logger
}

func NewServer(cfg *config.Config, s *store.Store, reg *provider.Registry,
	a *accounts.Manager, sy *syncer.Syncer, au *auth.Service, log *slog.Logger) *Server {
	return &Server{cfg: cfg, store: s, registry: reg, accts: a, syncer: sy, auth: au, log: log}
}

const sessionCookie = "um_session"
```

Replace `requireAPIKey` and `withLogging` with:

```go
// withRequestID gives every request an id, a request-scoped logger, and the
// X-Request-Id response header, then logs one summary line at the end.
func (s *Server) withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if id == "" || len(id) > 64 {
			id = logx.NewRequestID()
		}
		log := s.log.With("component", "api", "request_id", id)
		w.Header().Set("X-Request-Id", id)
		ctx := logx.With(r.Context(), log)

		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		log.Debug("request received",
			"method", r.Method, "path", r.URL.Path, "query", r.URL.RawQuery,
			"content_type", r.Header.Get("Content-Type"), "content_length", r.ContentLength,
			"has_authorization", r.Header.Get("Authorization") != "" || r.Header.Get("X-API-Key") != "",
			"has_session_cookie", hasCookie(r, sessionCookie),
			"remote", r.RemoteAddr, "user_agent", r.UserAgent())

		next.ServeHTTP(rec, r.WithContext(ctx))

		dev, _ := developerFrom(rec.ctx)
		log.Info("http",
			"method", r.Method, "path", r.URL.Path, "status", rec.status,
			"bytes", rec.bytes, "dur", time.Since(start).Round(time.Millisecond),
			"developer_id", dev.ID, "auth", authKindFrom(rec.ctx))
	})
}

func hasCookie(r *http.Request, name string) bool {
	_, err := r.Cookie(name)
	return err == nil
}

// withDeveloper resolves the caller from an API key or a session cookie, in
// that order, and rejects the request when neither is valid.
func (s *Server) withDeveloper(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		log := logx.From(ctx)

		bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if bearer == "" {
			bearer = r.Header.Get("X-API-Key")
		}
		if bearer != "" {
			log.Debug("auth: bearer present, resolving api key")
			dev, key, err := s.auth.KeyDeveloper(ctx, bearer)
			if err != nil {
				log.Debug("auth: api key rejected", "err", err)
				writeError(w, http.StatusUnauthorized, "unauthorized", "missing or invalid API key")
				return
			}
			log.Debug("auth: resolved", "developer_id", dev.ID, "key_id", key.ID, "prefix", key.Prefix)
			s.serveAs(w, r, next, dev, authKindAPIKey)
			return
		}

		if c, err := r.Cookie(sessionCookie); err == nil {
			log.Debug("auth: no bearer, session cookie present, resolving")
			dev, err := s.auth.SessionDeveloper(ctx, c.Value)
			if err != nil {
				log.Debug("auth: session rejected", "err", err)
				writeError(w, http.StatusUnauthorized, "unauthorized", "session expired; sign in again")
				return
			}
			log.Debug("auth: resolved", "developer_id", dev.ID, "via", "session")
			s.serveAs(w, r, next, dev, authKindSession)
			return
		}

		log.Debug("auth: no credential presented")
		writeError(w, http.StatusUnauthorized, "unauthorized", "missing or invalid API key")
	})
}

// logBody logs a JSON request body at DEBUG with secrets masked, then hands
// the bytes back to the handler. Bodies over 64 KB are logged by size only.
func logBody(r *http.Request) {
	log := logx.From(r.Context())
	if r.Body == nil || r.ContentLength == 0 || !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		return
	}
	if r.ContentLength > 64<<10 {
		log.Debug("request body", "bytes", r.ContentLength, "logged", false)
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err != nil {
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(raw))
	var v any
	if json.Unmarshal(raw, &v) != nil {
		log.Debug("request body", "bytes", len(raw), "json", false)
		return
	}
	log.Debug("request body", "bytes", len(raw), "body", logx.Redact(v))
}

// serveAs runs next with the developer in context and the logger enriched,
// and records the context on the status recorder so the summary line can
// name the developer.
func (s *Server) serveAs(w http.ResponseWriter, r *http.Request, next http.Handler, dev model.Developer, kind string) {
	ctx := withDeveloperCtx(r.Context(), dev, kind)
	ctx = logx.With(ctx, logx.From(ctx).With("developer_id", dev.ID, "auth", kind))
	if rec, ok := w.(*statusRecorder); ok {
		rec.ctx = ctx
	}
	r = r.WithContext(ctx)
	logBody(r)
	next.ServeHTTP(w, r)
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
	ctx    context.Context
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(p []byte) (int, error) {
	n, err := r.ResponseWriter.Write(p)
	r.bytes += n
	return n, err
}
```

In `Routes()`: `mux.Handle("/api/v1/", s.requireAPIKey(api))` → `mux.Handle("/api/v1/", s.withDeveloper(api))`; `return s.withLogging(mux)` → `return s.withRequestID(mux)`. Initialise `rec.ctx = ctx` right after creating `rec` in `withRequestID` so unauthenticated requests still have a context. Add imports `auth`, `logx`, `bytes`, `io`.

Add to the middleware test in Step 2 (inside `TestAPIRequiresDeveloperCredential`, using `newTestServerWithLog`): a `POST /api/v1/webhooks` with body `{"url":"https://x","secret":"hush"}` must produce a `request body` log line containing `url=https://x` and `secret:[redacted]` and never `hush`.

- [ ] **Step 6: Scope every handler**

`internal/api/handlers_mail.go` — `resolve`/`resolveID`:

```go
func (s *Server) resolve(w http.ResponseWriter, r *http.Request) (model.Account, provider.Mailbox, bool) {
	return s.resolveID(w, r, r.URL.Query().Get("account_id"))
}

func (s *Server) resolveID(w http.ResponseWriter, r *http.Request, id string) (model.Account, provider.Mailbox, bool) {
	log := logx.From(r.Context())
	if id == "" {
		writeError(w, http.StatusBadRequest, "missing_account_id", "account_id is required")
		return model.Account{}, nil, false
	}
	dev, _ := developerFrom(r.Context())
	acct, err := s.store.GetAccount(dev.ID, id)
	if errors.Is(err, store.ErrNotFound) {
		log.Debug("ownership check", "account_id", id, "result", "not owned or unknown")
		writeError(w, http.StatusNotFound, "account_not_found", "no such account: "+id)
		return model.Account{}, nil, false
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return model.Account{}, nil, false
	}
	log.Debug("ownership check", "account_id", id, "result", "ok", "provider", acct.Provider, "status", acct.Status)
	mailbox, err := s.mailboxFor(acct)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "unknown_provider", err.Error())
		return model.Account{}, nil, false
	}
	return acct, mailbox, true
}
```

Update the one caller `s.resolveID(w, p.AccountID)` in `handleSendEmail` (and any other `resolveID` caller — grep) to pass `r`.

`internal/api/handlers_misc.go` — replace each store call:

| Handler | Old | New |
|---|---|---|
| `handleListAccounts` | `s.store.ListAccounts()` | `dev, _ := developerFrom(r.Context()); s.store.ListAccounts(dev.ID)` |
| `handleGetAccount` | `s.store.GetAccount(r.PathValue("id"))` | `s.store.GetAccount(dev.ID, r.PathValue("id"))` |
| `handleDeleteAccount` | `s.store.GetAccount(id)` | `s.store.GetAccount(dev.ID, id)` |
| `handleResync` | `s.store.GetAccount(id)` | `s.store.GetAccount(dev.ID, id)` |
| `handleListWebhookDeliveries` | `s.store.GetWebhook(id)` | `s.store.GetWebhook(dev.ID, id)` |
| `handleCreateAccountWebhook` | `s.store.GetAccount(accountID)` | `s.store.GetAccount(dev.ID, accountID)` |
| `handleListAccountWebhooks` | `s.store.GetAccount(accountID)`; `ListAccountWebhooks(accountID)` | `GetAccount(dev.ID, accountID)`; `ListAccountWebhooks(dev.ID, accountID)` |
| `handleDeleteAccountWebhook` | `s.store.GetWebhook(wid)`; `DeleteWebhook(hook.ID)` | `GetWebhook(dev.ID, wid)`; `DeleteWebhook(dev.ID, hook.ID)` |
| `handleListWebhooks` | `s.store.ListWebhooks()` | `s.store.ListWebhooks(dev.ID)` |
| `handleDeleteWebhook` | `s.store.DeleteWebhook(id)` (any error → 500) | `err := s.store.DeleteWebhook(dev.ID, id)`; `errors.Is(err, store.ErrNotFound)` → 404 `not_found`; other error → 500 |
| `newWebhook(accountID, req)` | — | `newWebhook(developerID, accountID string, req webhookRequest)` sets `DeveloperID` |
| `createAccountWebhook(accountID, req)` | — | `createAccountWebhook(developerID, accountID string, req)` |
| `handleCreateWebhook` | `newWebhook("", req)` | `newWebhook(dev.ID, "", req)` |

In each handler add `dev, _ := developerFrom(r.Context())` as the first line. Add a debug log after each ownership decision, e.g. in `handleDeleteAccount`:

```go
	logx.From(r.Context()).Info("deleting account", "account_id", id)
```

`internal/api/handlers_connect.go`:
- `handleHostedAuth`: `dev, _ := developerFrom(r.Context())`; set `DeveloperID: dev.ID` in the `store.OAuthState{...}` literal; log `Info("connect link minted", "state_prefix", state[:6], "provider", p.Name(), "expires_at", expiresAt, "has_webhook", pendingHook != nil)`.
- `handleOAuthCallback`: `s.accts.Connect(r.Context(), pending.DeveloperID, pending.Provider, code, pending.Verifier)`; the connect-time webhook call becomes `s.createAccountWebhook(pending.DeveloperID, acct.ID, webhookRequest{...})`. Log `Info("oauth callback", "state_prefix", state[:6], "has_code", code != "", "error", errCode)` at the top (never the code itself).

- [ ] **Step 7: Wire `main.go`**

In `cmd/server/main.go`: `authSvc := auth.New(db, log, cfg.SessionTTL)` after `acctMgr`; pass it: `api.NewServer(cfg, db, registry, acctMgr, sync, authSvc, log)`. Add the import. Replace the `log.Info("listening", ...)` with one that also logs the effective config (no secrets):

```go
		log.Info("listening",
			"addr", cfg.ListenAddr,
			"db", cfg.DBPath,
			"providers", registry.Names(),
			"push_notifications", cfg.PushEnabled(),
			"public_base_url", cfg.PublicBaseURL,
			"tenant", cfg.Tenant,
			"redirect_uri", cfg.RedirectURI,
			"scopes", cfg.Scopes,
			"client_secret_set", cfg.ClientSecret != "",
			"session_ttl", cfg.SessionTTL,
			"backfill", opts.BackfillWindow,
			"poll_every", opts.PollInterval,
			"debug", os.Getenv("DEBUG") != "")
```

- [ ] **Step 8: Run everything**

Run: `gofmt -l internal cmd; go vet ./... && go test ./...`
Expected: all packages PASS.

- [ ] **Step 9: Commit**

```bash
git add internal/config internal/api cmd/server
git commit -m "feat(api): developer middleware with request ids; scope every handler to its tenant"
```

---

### Task 7: Auth pages, `/api/v1/me`, and API-key endpoints

**Files:**
- Create: `internal/api/handlers_auth.go`
- Modify: `internal/api/api.go` (routes)
- Test: `internal/api/api_test.go`

**Interfaces:**
- Consumes: `auth.Service`, `developerFrom`, `authKindFrom`, `sessionCookie`.
- Produces routes: `GET/POST /signup`, `GET/POST /login`, `POST /logout`, `GET /api/v1/me`, `GET/POST /api/v1/api-keys`, `DELETE /api/v1/api-keys/{id}`; helper `(*Server).setSessionCookie(w, r, token string, expires time.Time)`, `(*Server).clearSessionCookie(w, r)`, `(*Server).sessionDeveloper(r) (model.Developer, bool)`.

- [ ] **Step 1: Write the failing tests**

```go
// append to internal/api/api_test.go
func postForm(h http.Handler, path string, form url.Values) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestSignupSetsSessionCookieAndRedirects(t *testing.T) {
	s, db := newTestServer(t)
	rec := postForm(s.Routes(), "/signup", url.Values{
		"email": {"new@x.com"}, "password": {"longenoughpassword"}, "name": {"New"},
	})
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/dashboard" {
		t.Fatalf("status = %d location = %q", rec.Code, rec.Header().Get("Location"))
	}
	var cookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			cookie = c
		}
	}
	if cookie == nil || cookie.Value == "" || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode || cookie.Path != "/" {
		t.Fatalf("session cookie = %+v", cookie)
	}
	if _, _, err := db.SessionDeveloper(cookie.Value, time.Now()); err != nil {
		t.Fatalf("cookie does not resolve to a session: %v", err)
	}
	if _, _, err := db.DeveloperByEmail("new@x.com"); err != nil {
		t.Fatalf("developer not created: %v", err)
	}
}

func TestSignupAndLoginRejectBadInputInline(t *testing.T) {
	s, _ := newTestServer(t)
	h := s.Routes()
	rec := postForm(h, "/signup", url.Values{"email": {"bad"}, "password": {"short"}})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid email or password") {
		t.Fatalf("signup bad input: %d %s", rec.Code, rec.Body.String())
	}
	seedDev(t, s, "a@x.com")
	rec = postForm(h, "/login", url.Values{"email": {"a@x.com"}, "password": {"wrongpassword!"}})
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "invalid email or password") {
		t.Fatalf("login wrong password: %d %s", rec.Code, rec.Body.String())
	}
	rec = postForm(h, "/login", url.Values{"email": {"a@x.com"}, "password": {"longenoughpassword"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("login ok: %d", rec.Code)
	}
}

func TestLoginHonoursSameOriginNext(t *testing.T) {
	s, _ := newTestServer(t)
	seedDev(t, s, "a@x.com")
	rec := postForm(s.Routes(), "/login?next=/mail?account_id=x", url.Values{"email": {"a@x.com"}, "password": {"longenoughpassword"}})
	if loc := rec.Header().Get("Location"); loc != "/mail?account_id=x" {
		t.Fatalf("location = %q", loc)
	}
	rec = postForm(s.Routes(), "/login?next=https://evil.example.com/", url.Values{"email": {"a@x.com"}, "password": {"longenoughpassword"}})
	if loc := rec.Header().Get("Location"); loc != "/dashboard" {
		t.Fatalf("open redirect: %q", loc)
	}
}

func TestLogoutClearsSession(t *testing.T) {
	s, db := newTestServer(t)
	dev, _ := seedDev(t, s, "a@x.com")
	req := withSession(t, s, httptest.NewRequest(http.MethodPost, "/logout", nil), dev.ID)
	tok, _ := req.Cookie(sessionCookie)
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
		t.Fatalf("status = %d location = %q", rec.Code, rec.Header().Get("Location"))
	}
	if _, _, err := db.SessionDeveloper(tok.Value, time.Now()); err == nil {
		t.Fatal("session survived logout")
	}
}

func TestMeReportsAuthKind(t *testing.T) {
	s, _ := newTestServer(t)
	dev, key := seedDev(t, s, "a@x.com")
	for _, tc := range []struct {
		req  *http.Request
		kind string
	}{
		{withKey(httptest.NewRequest(http.MethodGet, "/api/v1/me", nil), key), "api_key"},
		{withSession(t, s, httptest.NewRequest(http.MethodGet, "/api/v1/me", nil), dev.ID), "session"},
	} {
		rec := httptest.NewRecorder()
		s.Routes().ServeHTTP(rec, tc.req)
		var body struct {
			ID    string `json:"id"`
			Email string `json:"email"`
			Auth  string `json:"auth"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body.ID != dev.ID || body.Email != "a@x.com" || body.Auth != tc.kind {
			t.Fatalf("me = %+v, want auth %q", body, tc.kind)
		}
	}
}

func TestAPIKeyEndpoints(t *testing.T) {
	s, _ := newTestServer(t)
	h := s.Routes()
	dev, key := seedDev(t, s, "a@x.com")

	// Minting requires a session.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, withKey(httptest.NewRequest(http.MethodPost, "/api/v1/api-keys", strings.NewReader(`{"name":"x"}`)), key))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("mint with api key: %d, want 403", rec.Code)
	}

	req := withSession(t, s, httptest.NewRequest(http.MethodPost, "/api/v1/api-keys", strings.NewReader(`{"name":"prod"}`)), dev.ID)
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("mint: %d %s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Prefix string `json:"prefix"`
		Key    string `json:"key"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(created.Key, "um_") || created.Prefix != created.Key[:12] || created.Name != "prod" {
		t.Fatalf("created = %+v", created)
	}

	// The new key works, and the list never shows it in full.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, withKey(httptest.NewRequest(http.MethodGet, "/api/v1/api-keys", nil), created.Key))
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), created.Key) || !strings.Contains(rec.Body.String(), created.Prefix) {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}

	// Revoke: session only, and the key dies.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, withKey(httptest.NewRequest(http.MethodDelete, "/api/v1/api-keys/"+created.ID, nil), key))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("revoke with api key: %d, want 403", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, withSession(t, s, httptest.NewRequest(http.MethodDelete, "/api/v1/api-keys/"+created.ID, nil), dev.ID))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("revoke: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, withKey(httptest.NewRequest(http.MethodGet, "/api/v1/me", nil), created.Key))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("revoked key still works: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, withSession(t, s, httptest.NewRequest(http.MethodDelete, "/api/v1/api-keys/key_nope", nil), dev.ID))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("revoke unknown: %d", rec.Code)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/api/ -run 'Signup|Login|Logout|TestMe|APIKeyEndpoints'`
Expected: FAIL — 404s (routes missing).

- [ ] **Step 3: Implement `handlers_auth.go`**

```go
// internal/api/handlers_auth.go
package api

import (
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gauravrautela/unified-messaging/internal/auth"
	"github.com/gauravrautela/unified-messaging/internal/logx"
	"github.com/gauravrautela/unified-messaging/internal/model"
	"github.com/gauravrautela/unified-messaging/internal/store"
)

// ---- cookies ----

func (s *Server) secureCookies(r *http.Request) bool {
	return r.TLS != nil || strings.HasPrefix(s.cfg.PublicBaseURL, "https://")
}

func (s *Server) setSessionCookie(w http.ResponseWriter, r *http.Request, token string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: token, Path: "/", Expires: expires,
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: s.secureCookies(r),
	})
}

func (s *Server) clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: s.secureCookies(r),
	})
}

// sessionDeveloper resolves the browser session for page handlers, which
// sit outside the /api/v1 middleware.
func (s *Server) sessionDeveloper(r *http.Request) (model.Developer, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return model.Developer{}, false
	}
	d, err := s.auth.SessionDeveloper(r.Context(), c.Value)
	return d, err == nil
}

// safeNext keeps ?next= on this origin: a path starting with a single "/".
func safeNext(raw string) string {
	if strings.HasPrefix(raw, "/") && !strings.HasPrefix(raw, "//") {
		return raw
	}
	return "/dashboard"
}

// ---- pages ----

var authTmpl = template.Must(template.New("auth").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}}</title>
<style>
:root{--bg:#f7f7f8;--card:#fff;--text:#1a1a1a;--muted:#6b6b76;--border:#e6e6ea;--accent:#2563eb;--accent-text:#fff;--danger:#dc2626;--danger-bg:#fef2f2}
@media (prefers-color-scheme:dark){:root{--bg:#0f1115;--card:#171a21;--text:#f0f0f2;--muted:#9a9aa5;--border:#2a2d36;--danger-bg:#2a1414}}
body{margin:0;font-family:system-ui,sans-serif;background:var(--bg);color:var(--text)}
.card{max-width:24rem;margin:4rem auto;background:var(--card);border:1px solid var(--border);border-radius:16px;padding:2rem}
h1{font-size:1.35rem;margin:0 0 .25rem}
p{color:var(--muted);font-size:.9rem;margin:.25rem 0 1rem}
form{display:flex;flex-direction:column;gap:.75rem}
input{font:inherit;padding:.6rem .75rem;border:1px solid var(--border);border-radius:8px;background:var(--bg);color:var(--text)}
button{font:inherit;cursor:pointer;padding:.6rem .9rem;border-radius:8px;border:1px solid var(--accent);background:var(--accent);color:var(--accent-text)}
.err{color:var(--danger);background:var(--danger-bg);border-radius:8px;padding:.6rem .8rem;font-size:.85rem}
a{color:var(--accent)}
</style></head><body>
<div class="card">
  <h1>{{.Title}}</h1>
  <p>{{.Lead}}</p>
  {{if .Error}}<p class="err">{{.Error}}</p>{{end}}
  <form method="post" action="{{.Action}}">
    <input name="email" type="email" placeholder="Email" value="{{.Email}}" required autofocus>
    <input name="password" type="password" placeholder="Password{{if .Signup}} (10+ characters){{end}}" required minlength="{{if .Signup}}10{{else}}1{{end}}">
    {{if .Signup}}<input name="name" type="text" placeholder="Your name (optional)">{{end}}
    <button type="submit">{{.Button}}</button>
  </form>
  <p>{{.AltLead}} <a href="{{.AltHref}}">{{.AltText}}</a></p>
</div>
</body></html>`))

type authPage struct {
	Title, Lead, Action, Button, AltLead, AltHref, AltText, Email, Error string
	Signup                                                              bool
}

func loginPage(next, email, errMsg string) authPage {
	action := "/login"
	if next != "" {
		action += "?next=" + url.QueryEscape(next)
	}
	return authPage{Title: "Sign in", Lead: "Sign in to manage connected accounts and API keys.",
		Action: action, Button: "Sign in", AltLead: "New here?", AltHref: "/signup", AltText: "Create an account",
		Email: email, Error: errMsg}
}

func signupPage(email, errMsg string) authPage {
	return authPage{Title: "Create your account", Lead: "One account per developer. You will get API keys next.",
		Action: "/signup", Button: "Create account", AltLead: "Already have one?", AltHref: "/login", AltText: "Sign in",
		Email: email, Error: errMsg, Signup: true}
}

func renderAuth(w http.ResponseWriter, status int, p authPage) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_ = authTmpl.Execute(w, p)
}

func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.sessionDeveloper(r); ok {
		http.Redirect(w, r, safeNext(r.URL.Query().Get("next")), http.StatusFound)
		return
	}
	renderAuth(w, http.StatusOK, loginPage(r.URL.Query().Get("next"), "", ""))
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	log := logx.From(r.Context())
	if err := r.ParseForm(); err != nil {
		renderAuth(w, http.StatusBadRequest, loginPage("", "", "bad form"))
		return
	}
	email := r.PostForm.Get("email")
	next := r.URL.Query().Get("next")
	log.Debug("login attempt", "email", strings.ToLower(strings.TrimSpace(email)), "next", next)
	dev, err := s.auth.Login(r.Context(), email, r.PostForm.Get("password"))
	if err != nil {
		if errors.Is(err, auth.ErrInvalidCredentials) {
			renderAuth(w, http.StatusUnauthorized, loginPage(next, email, err.Error()))
			return
		}
		log.Error("login", "err", err)
		renderAuth(w, http.StatusInternalServerError, loginPage(next, email, "something went wrong"))
		return
	}
	s.startSession(w, r, dev, safeNext(next))
}

func (s *Server) handleSignupPage(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.sessionDeveloper(r); ok {
		http.Redirect(w, r, "/dashboard", http.StatusFound)
		return
	}
	renderAuth(w, http.StatusOK, signupPage("", ""))
}

func (s *Server) handleSignup(w http.ResponseWriter, r *http.Request) {
	log := logx.From(r.Context())
	if err := r.ParseForm(); err != nil {
		renderAuth(w, http.StatusBadRequest, signupPage("", "bad form"))
		return
	}
	email := r.PostForm.Get("email")
	log.Debug("signup attempt", "email", strings.ToLower(strings.TrimSpace(email)))
	dev, err := s.auth.Signup(r.Context(), email, r.PostForm.Get("password"), r.PostForm.Get("name"))
	switch {
	case errors.Is(err, auth.ErrInvalidInput):
		renderAuth(w, http.StatusBadRequest, signupPage(email, "invalid email or password (10+ characters)"))
		return
	case errors.Is(err, auth.ErrEmailTaken):
		renderAuth(w, http.StatusConflict, signupPage(email, "could not create account — try signing in"))
		return
	case err != nil:
		log.Error("signup", "err", err)
		renderAuth(w, http.StatusInternalServerError, signupPage(email, "something went wrong"))
		return
	}
	s.startSession(w, r, dev, "/dashboard")
}

func (s *Server) startSession(w http.ResponseWriter, r *http.Request, dev model.Developer, next string) {
	tok, exp, err := s.auth.NewSession(r.Context(), dev.ID)
	if err != nil {
		logx.From(r.Context()).Error("creating session", "developer_id", dev.ID, "err", err)
		renderAuth(w, http.StatusInternalServerError, loginPage("", dev.Email, "something went wrong"))
		return
	}
	s.setSessionCookie(w, r, tok, exp)
	logx.From(r.Context()).Info("session started", "developer_id", dev.ID, "redirect", next)
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		_ = s.auth.DeleteSession(r.Context(), c.Value)
	}
	s.clearSessionCookie(w, r)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// ---- /api/v1/me and API keys ----

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	dev, _ := developerFrom(r.Context())
	writeJSON(w, http.StatusOK, struct {
		model.Developer
		Auth string `json:"auth"`
	}{dev, authKindFrom(r.Context())})
}

// requireSession is for actions that must not be reachable with an API key
// alone, so a leaked key cannot mint or revoke keys.
func (s *Server) requireSession(w http.ResponseWriter, r *http.Request) bool {
	if authKindFrom(r.Context()) != authKindSession {
		logx.From(r.Context()).Debug("session-only endpoint refused api key")
		writeError(w, http.StatusForbidden, "session_required", "sign in to the dashboard to manage API keys")
		return false
	}
	return true
}

func (s *Server) handleListAPIKeys(w http.ResponseWriter, r *http.Request) {
	dev, _ := developerFrom(r.Context())
	keys, err := s.store.ListAPIKeys(dev.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, listResponse[model.APIKey]{Items: keys})
}

type createKeyRequest struct {
	Name string `json:"name"`
}

type createKeyResponse struct {
	model.APIKey
	Key string `json:"key"`
}

func (s *Server) handleCreateAPIKey(w http.ResponseWriter, r *http.Request) {
	if !s.requireSession(w, r) {
		return
	}
	dev, _ := developerFrom(r.Context())
	var req createKeyRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	full, k, err := s.auth.NewAPIKey(r.Context(), dev.ID, req.Name)
	if errors.Is(err, auth.ErrInvalidInput) {
		writeError(w, http.StatusBadRequest, "missing_name", "name is required")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	// The only time the full key is ever returned.
	writeJSON(w, http.StatusCreated, createKeyResponse{APIKey: k, Key: full})
}

func (s *Server) handleRevokeAPIKey(w http.ResponseWriter, r *http.Request) {
	if !s.requireSession(w, r) {
		return
	}
	dev, _ := developerFrom(r.Context())
	err := s.auth.RevokeKey(r.Context(), dev.ID, r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "no such key")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
```

- [ ] **Step 4: Routes**

In `Routes()` add before the dashboard routes:

```go
	// --- developer sign-in (form posts, cookie session) ---
	mux.HandleFunc("GET /login", s.handleLoginPage)
	mux.HandleFunc("POST /login", s.handleLogin)
	mux.HandleFunc("GET /signup", s.handleSignupPage)
	mux.HandleFunc("POST /signup", s.handleSignup)
	mux.HandleFunc("POST /logout", s.handleLogout)
```

and inside the `api` mux, right after `hosted-auth`:

```go
	api.HandleFunc("GET /api/v1/me", s.handleMe)
	api.HandleFunc("GET /api/v1/api-keys", s.handleListAPIKeys)
	api.HandleFunc("POST /api/v1/api-keys", s.handleCreateAPIKey)
	api.HandleFunc("DELETE /api/v1/api-keys/{id}", s.handleRevokeAPIKey)
```

- [ ] **Step 5: Run tests**

Run: `go test ./internal/api/`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/api
git commit -m "feat(api): login/signup pages, /me, and session-only API key management"
```

---

### Task 8: Dashboard and mail viewer behind the session; API-keys panel

**Files:**
- Modify: `internal/api/handlers_ui.go`, `internal/api/handlers_mail_ui.go`
- Test: `internal/api/api_test.go`

**Interfaces:**
- Consumes: `sessionDeveloper(r)`.
- Produces: `handleDashboard`/`handleMailPage` redirect to `/login?next=<path>` without a session; dashboard HTML contains `id="keys"`, `data-action="create-key"`, `data-action="revoke-key"`, `id="logout-form"`, and the developer's email.

- [ ] **Step 1: Write the failing tests**

```go
// append to internal/api/api_test.go
func TestPagesRedirectToLoginWithoutSession(t *testing.T) {
	s, _ := newTestServer(t)
	for _, path := range []string{"/dashboard", "/mail?account_id=acc_1"} {
		rec := httptest.NewRecorder()
		s.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusFound {
			t.Fatalf("%s: status = %d, want 302", path, rec.Code)
		}
		loc, _ := url.Parse(rec.Header().Get("Location"))
		if loc.Path != "/login" || loc.Query().Get("next") != path {
			t.Fatalf("%s: location = %q", path, rec.Header().Get("Location"))
		}
	}
}

func TestDashboardShowsDeveloperAndKeysPanel(t *testing.T) {
	s, _ := newTestServer(t)
	dev, _ := seedDev(t, s, "dev@x.com")
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, withSession(t, s, httptest.NewRequest(http.MethodGet, "/dashboard", nil), dev.ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"dev@x.com", `id="keys"`, `data-action="create-key"`, `id="logout-form"`, `/api/v1/api-keys`} {
		if !strings.Contains(body, want) {
			t.Fatalf("dashboard missing %q", want)
		}
	}
	if strings.Contains(body, "um_api_key") || strings.Contains(body, `id="gate-form"`) {
		t.Fatal("dashboard still has the localStorage API-key gate")
	}
}
```

Also update the earlier renamed `TestDashboardServesWithSession` / `TestMailPageServesWithSession` to assert 200 with `withSession` (they already do after Task 6) and delete any remaining `gate-form` assertion.

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/api/ -run 'PagesRedirect|DashboardShows'`
Expected: FAIL — 200 instead of 302; missing `id="keys"`.

- [ ] **Step 3: Gate the pages and render the developer**

`handlers_ui.go`: make the dashboard a template with the developer's email injected.

```go
// handleDashboard requires a browser session. The page's own fetches then
// ride the same cookie, so account data stays gated by the API middleware.
func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	dev, ok := s.sessionDeveloper(r)
	if !ok {
		http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = dashboardTmpl.Execute(w, struct{ Email string }{dev.Email})
}

var dashboardTmpl = template.Must(template.New("dashboard").Parse(dashboardHTML))
```

Add imports `html/template` and `net/url`. In `dashboardHTML`:

1. Delete the whole `<div id="gate" class="gate">…</div>` block and the `.gate` CSS rules.
2. Change `<div id="app" class="hidden">` to `<div id="app">`.
3. Replace the header's right-hand controls with:

```html
      <div style="display:flex;gap:.75rem;align-items:center">
        <span class="sub">{{.Email}}</span>
        <button id="connect-btn" class="primary">+ Connect account</button>
        <form id="logout-form" method="post" action="/logout" style="margin:0"><button class="signout" type="submit">Log out</button></form>
      </div>
```

4. After the accounts `<div class="card">…</div>` add the keys panel:

```html
    <h2 style="font-size:1.05rem;margin:2rem 0 .5rem">API keys</h2>
    <p class="sub" style="margin-bottom:.75rem">Use a key as <code>Authorization: Bearer &lt;key&gt;</code>. Keys are shown once.</p>
    <div class="card">
      <div id="new-key" class="hidden" style="margin-bottom:1rem">
        <p class="sub">Copy this key now — it will not be shown again.</p>
        <code id="new-key-value" style="display:block;padding:.6rem;border:1px dashed var(--border);border-radius:8px;word-break:break-all"></code>
      </div>
      <form id="key-form" style="display:flex;gap:.5rem;margin-bottom:1rem">
        <input id="key-name" placeholder="Key name, e.g. production" required style="flex:1;font:inherit;padding:.5rem .7rem;border:1px solid var(--border);border-radius:8px;background:var(--bg);color:var(--text)">
        <button class="primary" data-action="create-key" type="submit">Create key</button>
      </form>
      <div id="keys"></div>
    </div>
```

5. In the script: delete `KEY_STORAGE`, `apiKey()`, and `signOut()`. Replace `api()` with:

```js
async function api(path, opts) {
  const res = await fetch(path, Object.assign({ credentials: "same-origin" }, opts));
  if (res.status === 401) { location.href = "/login?next=" + encodeURIComponent(location.pathname + location.search); throw new Error("unauthorized"); }
  if (!res.ok) {
    let msg = res.statusText;
    try { msg = (await res.json()).error.message; } catch (e) {}
    throw new Error(msg);
  }
  if (res.status === 204) return null;
  return res.json();
}
```

Delete the `signout-btn` and `gate-form` listeners. Replace `enter()` and `init()` with:

```js
async function loadKeys() {
  const data = await api("/api/v1/api-keys");
  const items = data.items || [];
  if (!items.length) { $("keys").innerHTML = '<div class="empty">No API keys yet.</div>'; return; }
  $("keys").innerHTML = items.map((k) =>
    '<div class="row" data-kid="' + k.id + '">' +
      '<div class="who"><div class="email">' + escapeHtml(k.name) + '</div>' +
      '<div class="meta"><code>' + escapeHtml(k.prefix) + '…</code> &middot; created ' + new Date(k.created_at).toLocaleDateString() +
      (k.last_used_at ? ' &middot; last used ' + new Date(k.last_used_at).toLocaleString() : ' &middot; never used') + '</div></div>' +
      (k.revoked_at ? '<span class="status bad">Revoked</span>' :
        '<div class="actions"><button data-action="revoke-key" class="danger">Revoke</button></div>') +
    '</div>').join("");
}

$("key-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  const name = $("key-name").value.trim();
  if (!name) return;
  try {
    const k = await api("/api/v1/api-keys", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ name }) });
    $("new-key-value").textContent = k.key;
    $("new-key").classList.remove("hidden");
    $("key-name").value = "";
    loadKeys();
  } catch (err) { alert("Could not create key: " + err.message); }
});

$("keys").addEventListener("click", async (e) => {
  const btn = e.target.closest('button[data-action="revoke-key"]');
  if (!btn) return;
  if (!confirm("Revoke this key? Anything using it stops working immediately.")) return;
  btn.disabled = true;
  try { await api("/api/v1/api-keys/" + btn.closest(".row").dataset.kid, { method: "DELETE" }); loadKeys(); }
  catch (err) { alert("Could not revoke: " + err.message); btn.disabled = false; }
});

(async function init() {
  if (new URLSearchParams(location.search).get("connected")) {
    $("banner").textContent = "Account connected.";
    $("banner").classList.remove("hidden");
    history.replaceState(null, "", location.pathname);
  }
  await loadProviders();
  await Promise.all([loadAccounts(), loadKeys()]);
})();
```

Because the HTML is now a Go template, any literal `{{` in the JS must be avoided (there is none today; keep it that way).

`handlers_mail_ui.go`: same gating in `handleMailPage` (redirect with `next`), delete the gate `<div>`, the `.gate` CSS, `KEY_STORAGE`, `apiKey()`, `signOut()`, the `gate-form` listener; `api()` becomes the `credentials: "same-origin"` version above but returning `res` (the mail page uses `apiJSON`); `init()` calls `enter()` directly. Add a **Log out** form next to the existing `<a href="/dashboard">Accounts</a>` link. The mail page has no developer-specific content, so it stays a plain string (no template).

- [ ] **Step 4: Run tests**

Run: `go test ./internal/api/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/api/handlers_ui.go internal/api/handlers_mail_ui.go internal/api/api_test.go
git commit -m "feat(ui): session-gated dashboard and mail viewer with API key management"
```

---

### Task 9: Table-driven cross-tenant isolation test

**Files:**
- Create: `internal/api/isolation_test.go`

**Interfaces:**
- Consumes: test helpers from Task 6.

- [ ] **Step 1: Write the test**

```go
// internal/api/isolation_test.go
package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gauravrautela/unified-messaging/internal/model"
	"github.com/gauravrautela/unified-messaging/internal/store"
)

// Developer B must not be able to see or touch anything of developer A's,
// and must learn nothing from the attempt: every route answers 404 (or 400
// when the id is carried in a body that fails before ownership). Adding a
// route to the server without adding it here fails the test, so scoping is
// a decision made per route, never an accident.
func TestCrossTenantAccessIs404(t *testing.T) {
	s, db := newTestServer(t)
	h := s.Routes()
	devA, _ := seedDev(t, s, "a@x.com")
	_, keyB := seedDev(t, s, "b@x.com")

	now := time.Now()
	if err := db.UpsertAccount(model.Account{ID: "acc_A", DeveloperID: devA.ID, Provider: "OUTLOOK", Email: "a@outlook.com", Status: model.AccountOK}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertFolder(model.Folder{AccountID: "acc_A", ID: "F1", Name: "Inbox", Role: "inbox"}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertEmail(model.Email{AccountID: "acc_A", ID: "M1", FolderID: "F1", Subject: "secret", Date: now}); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveWebhook(model.Webhook{ID: "wh_A", DeveloperID: devA.ID, URL: "https://a.example.com", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveWebhook(model.Webhook{ID: "wh_A_acc", DeveloperID: devA.ID, AccountID: "acc_A", URL: "https://a.example.com", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveDelivery(store.Delivery{ID: "dl_A", WebhookID: "wh_A", EventType: "mail_received", Payload: []byte(`{}`), NextAttemptAt: now, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	keysA, _ := db.ListAPIKeys(devA.ID)

	body := func(s string) *strings.Reader { return strings.NewReader(s) }
	cases := []struct {
		route        string // pattern from apiRoutes this case covers
		method, path string
		body         *strings.Reader
		want         int
	}{
		{"GET /api/v1/accounts/{id}", "GET", "/api/v1/accounts/acc_A", nil, 404},
		{"DELETE /api/v1/accounts/{id}", "DELETE", "/api/v1/accounts/acc_A", nil, 404},
		{"POST /api/v1/accounts/{id}/resync", "POST", "/api/v1/accounts/acc_A/resync", nil, 404},
		{"GET /api/v1/accounts/{id}/webhooks", "GET", "/api/v1/accounts/acc_A/webhooks", nil, 404},
		{"POST /api/v1/accounts/{id}/webhooks", "POST", "/api/v1/accounts/acc_A/webhooks", body(`{"url":"https://b.example.com"}`), 404},
		{"DELETE /api/v1/accounts/{id}/webhooks/{wid}", "DELETE", "/api/v1/accounts/acc_A/webhooks/wh_A_acc", nil, 404},
		{"GET /api/v1/folders", "GET", "/api/v1/folders?account_id=acc_A", nil, 404},
		{"GET /api/v1/threads", "GET", "/api/v1/threads?account_id=acc_A", nil, 404},
		{"GET /api/v1/emails", "GET", "/api/v1/emails?account_id=acc_A", nil, 404},
		{"GET /api/v1/emails/{id}", "GET", "/api/v1/emails/M1?account_id=acc_A", nil, 404},
		{"PATCH /api/v1/emails/{id}", "PATCH", "/api/v1/emails/M1?account_id=acc_A", body(`{"read":true}`), 404},
		{"POST /api/v1/emails", "POST", "/api/v1/emails", body(`{"account_id":"acc_A","to":[{"email":"x@y.com"}],"subject":"s","body":"b"}`), 404},
		{"POST /api/v1/emails/{id}/reply", "POST", "/api/v1/emails/M1/reply?account_id=acc_A", body(`{"body":"b"}`), 404},
		{"POST /api/v1/emails/{id}/forward", "POST", "/api/v1/emails/M1/forward?account_id=acc_A", body(`{"to":[{"email":"x@y.com"}],"body":"b"}`), 404},
		{"GET /api/v1/emails/{id}/attachments", "GET", "/api/v1/emails/M1/attachments?account_id=acc_A", nil, 404},
		{"GET /api/v1/emails/{id}/attachments/{aid}", "GET", "/api/v1/emails/M1/attachments/A1?account_id=acc_A", nil, 404},
		{"POST /api/v1/drafts", "POST", "/api/v1/drafts", body(`{"account_id":"acc_A","to":[{"email":"x@y.com"}],"subject":"s","body":"b"}`), 404},
		{"POST /api/v1/drafts/{id}/send", "POST", "/api/v1/drafts/D1/send?account_id=acc_A", nil, 404},
		{"DELETE /api/v1/webhooks/{id}", "DELETE", "/api/v1/webhooks/wh_A", nil, 404},
		{"GET /api/v1/webhooks/{id}/deliveries", "GET", "/api/v1/webhooks/wh_A/deliveries", nil, 404},
		// Session-only endpoint refuses the key before any lookup.
		{"DELETE /api/v1/api-keys/{id}", "DELETE", "/api/v1/api-keys/" + keysA[0].ID, nil, 403},
	}
	for _, tc := range cases {
		var req *http.Request
		if tc.body != nil {
			req = httptest.NewRequest(tc.method, tc.path, tc.body)
			req.Header.Set("Content-Type", "application/json")
		} else {
			req = httptest.NewRequest(tc.method, tc.path, nil)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, withKey(req, keyB))
		if rec.Code != tc.want {
			t.Errorf("%s %s as B: status = %d, want %d (body %s)", tc.method, tc.path, rec.Code, tc.want, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "secret") || strings.Contains(rec.Body.String(), "a@outlook.com") {
			t.Errorf("%s %s leaked A's data: %s", tc.method, tc.path, rec.Body.String())
		}
	}

	// Lists as B are empty, not A's.
	for _, path := range []string{"/api/v1/accounts", "/api/v1/webhooks"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, withKey(httptest.NewRequest(http.MethodGet, path, nil), keyB))
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"items":[]`) {
			t.Errorf("%s as B: %d %s", path, rec.Code, rec.Body.String())
		}
	}

	// Every registered /api/v1 route must be covered above, or be listed here
	// as carrying no foreign id to probe. A new route fails until placed.
	covered := map[string]bool{
		"GET /api/v1/accounts": true, "GET /api/v1/webhooks": true, // list checks above
		"GET /api/v1/providers": true, "GET /api/v1/me": true,
		"GET /api/v1/api-keys": true, "POST /api/v1/api-keys": true,
		"POST /api/v1/hosted-auth": true, "POST /api/v1/webhooks": true,
	}
	for _, tc := range cases {
		covered[tc.route] = true
	}
	for _, route := range apiRoutes {
		if !covered[route] {
			t.Errorf("route %q has no isolation case; add one", route)
		}
	}
}
```

- [ ] **Step 2: Expose the route list**

In `internal/api/api.go`, declare the API routes as data so the test can enumerate them:

```go
// apiRoutes is every pattern registered under the developer middleware. It
// is a package-level list so the isolation test can prove each one is
// tenant-scoped.
var apiRoutes = []string{
	"POST /api/v1/hosted-auth",
	"GET /api/v1/me",
	"GET /api/v1/api-keys",
	"POST /api/v1/api-keys",
	"DELETE /api/v1/api-keys/{id}",
	"GET /api/v1/providers",
	"GET /api/v1/accounts",
	"GET /api/v1/accounts/{id}",
	"DELETE /api/v1/accounts/{id}",
	"POST /api/v1/accounts/{id}/resync",
	"GET /api/v1/accounts/{id}/webhooks",
	"POST /api/v1/accounts/{id}/webhooks",
	"DELETE /api/v1/accounts/{id}/webhooks/{wid}",
	"GET /api/v1/folders",
	"GET /api/v1/threads",
	"GET /api/v1/emails",
	"POST /api/v1/emails",
	"GET /api/v1/emails/{id}",
	"PATCH /api/v1/emails/{id}",
	"POST /api/v1/emails/{id}/reply",
	"POST /api/v1/emails/{id}/forward",
	"GET /api/v1/emails/{id}/attachments",
	"GET /api/v1/emails/{id}/attachments/{aid}",
	"POST /api/v1/drafts",
	"POST /api/v1/drafts/{id}/send",
	"GET /api/v1/webhooks",
	"POST /api/v1/webhooks",
	"DELETE /api/v1/webhooks/{id}",
	"GET /api/v1/webhooks/{id}/deliveries",
}
```

and register handlers from a map keyed by these strings:

```go
	handlers := map[string]http.HandlerFunc{
		"POST /api/v1/hosted-auth": s.handleHostedAuth,
		"GET /api/v1/me":           s.handleMe,
		// ... one entry per apiRoutes element, same handler names as today ...
	}
	for _, pattern := range apiRoutes {
		h, ok := handlers[pattern]
		if !ok {
			panic("no handler for " + pattern)
		}
		api.HandleFunc(pattern, h)
	}
```

- [ ] **Step 3: Run the test and fix what it finds**

Run: `go test ./internal/api/ -run TestCrossTenantAccessIs404 -v`
Expected: PASS once Task 6's scoping is complete. If any row reports 200/500, that handler bypasses `resolveID`/`GetAccount(dev.ID, …)` — fix the handler, not the test.

- [ ] **Step 4: Commit**

```bash
git add internal/api/isolation_test.go internal/api/api.go
git commit -m "test(api): prove every route is tenant-scoped"
```

---

### Task 10: Exhaustive debug logging across syncer, events, outlook, store

**Files:**
- Modify: `internal/syncer/syncer.go`, `internal/syncer/subscriptions.go`, `internal/events/events.go`, `internal/provider/outlook/client.go`, `internal/provider/outlook/auth.go`, `internal/provider/outlook/messages.go`, `internal/store/store.go`
- Test: `internal/syncer/syncer_test.go`, `internal/events/events_test.go`, `internal/api/api_test.go`

Rules from the spec §5: every line has `component`; per-run ids; INFO stays quiet; DEBUG reconstructs input → decision → output; never a secret.

- [ ] **Step 1: Write the failing tests**

`internal/syncer/syncer_test.go` — in `TestSyncAccountBackfillThenIncremental` replace the discard logger with `log, recs := logx.Capture()` and append at the end:

```go
	for _, want := range []string{
		"component=syncer", "run_id=run_", "sync run started", "scope decision", "message decision",
		"decision=new", "event=mail_received", "sync run finished",
	} {
		if !recs.Contains(want) {
			t.Errorf("sync log missing %q", want)
		}
	}
	if recs.Contains("test-token") {
		t.Error("access token leaked into sync log")
	}
```

`internal/events/events_test.go` — `TestFailedDeliveryIsQueuedAndRetried`: capture the logger and assert:

```go
	for _, want := range []string{"component=events", "delivery attempt", "attempt=1", "decision=\"scheduled retry\"", "decision=delivered"} {
		if !recs.Contains(want) {
			t.Errorf("events log missing %q", want)
		}
	}
```

and in the test that saves a webhook with `Secret: "s3"` (add one to `TestDeliveryIsScopedToAccount`: `Secret: "topsecret"`) assert `!recs.Contains("topsecret")`.

`internal/api/api_test.go` — `newTestServerWithLog` already exists (Task 6). Then in `TestSignupSetsSessionCookieAndRedirects` use `s, db, recs := newTestServerWithLog(t)` and assert after the request:

```go
	if recs.Contains("longenoughpassword") || recs.Contains(cookie.Value) {
		t.Fatal("password or session id leaked into logs")
	}
	if !recs.Contains("request_id=req_") || !recs.Contains("developer signed up") {
		t.Fatalf("expected request-scoped signup logs: %v", recs.All())
	}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/syncer/ ./internal/events/ ./internal/api/ -run 'Backfill|QueuedAndRetried|SignupSets'`
Expected: FAIL on the missing log strings.

- [ ] **Step 3: Syncer logging**

In `SyncAccount` (`syncer.go`), create a run logger at the top and use it everywhere in the run:

```go
	runID := "run_" + strings.TrimPrefix(logx.NewRequestID(), "req_")
	log := s.log.With("component", "syncer", "run_id", runID, "account_id", accountID)
	ctx = logx.With(ctx, log)
	start := time.Now()
	log.Info("sync run started")
	defer func() { log.Info("sync run finished", "dur", time.Since(start).Round(time.Millisecond)) }()
```

Add `"strings"` import. In `syncScope`, after computing `cursor`/`initial`:

```go
	log := logx.From(ctx).With("scope", scope.Name, "scope_id", scope.ID, "role", scope.Role)
	log.Debug("scope decision", "cursor_present", cursor != "", "initial", initial, "first_run", firstRun, "since", since)
```

After `SyncMessages` returns: `log.Debug("scope changes", "changed", len(changes.Changed), "removed", len(changes.Removed), "cursor_out_present", changes.Cursor != "")`. In the per-message loop, before the `switch`:

```go
		decision := "updated"
		switch {
		case quiet:
			decision = "suppressed"
		case !existed && scope.Role == "sentitems":
			decision = "new-sent"
		case !existed && !e.Draft:
			decision = "new"
		case !existed:
			decision = "new-draft"
		}
		log.Debug("message decision", "email_id", e.ID, "existed", existed, "draft", e.Draft,
			"has_attachments", e.HasAttachments, "body_bytes", len(e.Body), "decision", decision)
```

and log `"event", model.EventMailReceived` (etc.) next to each `Emit`. Wrap the existing `Emit` calls so each logs `log.Debug("event emitted", "event", ev.Type, "email_id", e.ID)`. In `attachAttachments` log the fetch and its count. In `pollLoop` log `Debug("poll tick", "accounts", len(accts), "ok", n)`. In `subscriptions.go` log each create/renew/delete decision with `subscription_id`, `expires_at`, and `Debug("subscription decision", ...)` — never `client_state`.

- [ ] **Step 4: Events logging**

In `events.go` give the dispatcher `log.With("component", "events")` in `NewDispatcher`. In `post`, log before the request and after:

```go
	log := d.log.With("delivery_id", dl.ID, "webhook_id", h.ID, "event", dl.EventType, "attempt", attempt)
	log.Debug("delivery attempt", "url", h.URL, "payload_bytes", len(dl.Payload), "signed", h.Secret != "")
	...
	log.Debug("delivery response", "status", resp.StatusCode, "dur", time.Since(start).Round(time.Millisecond))
```

In `deliver`, after the hook list: `d.log.Debug("dispatching", "event", ev.Type, "account_id", ev.AccountID, "hooks", len(hooks))`, and per skipped hook `Debug("hook skipped", "webhook_id", h.ID, "reason", "event filter")`. In `schedule`: `Debug("delivery decision", "decision", "scheduled retry", "next_attempt_at", dl.NextAttemptAt, "attempts", dl.Attempts)` or `"decision", "dead"`. In `retryDue`: on success `Debug("delivery decision", "decision", "delivered")`; each tick `Debug("retry tick", "due", len(due))` only when `len(due) > 0`.

- [ ] **Step 5: Outlook client logging**

In `client.go` `do()`: accept the logger from `logx.From(ctx).With("component", "outlook", "account_id", accountID)`; log `Debug("graph request", "method", req.method, "url", req.url)` before and `Debug("graph response", "status", status, "bytes", n, "dur", …)` after; on Graph error include `"graph_code"`. Never log the `Authorization` header. In `auth.go` token refresh: `Debug("token decision", "decision", "cached", "expires_in", …)` vs `"refreshing"`, and `Info("token refreshed", "expires_at", …)` — never the token values. In `messages.go` delta walk: `Debug("delta page", "page", n, "items", len(page.Value), "next", page.NextLink != "", "delta", page.DeltaLink != "")`.

- [ ] **Step 6: Store logging**

Add to `Store` a `log *slog.Logger` set via `store.Open(path)` → keep signature; add `func (s *Store) SetLogger(l *slog.Logger)` called from `main.go` with `log.With("component", "store")`. Add a helper used by the hot paths (`UpsertEmail`, `ListEmails`, `GetAccount`, `DueDeliveries`, `SaveDelivery`):

```go
func (s *Store) trace(op string, start time.Time, kv ...any) {
	if s.log == nil {
		return
	}
	s.log.Debug("query", append([]any{"op", op, "dur", time.Since(start).Round(time.Microsecond)}, kv...)...)
}
```

called as `defer s.trace("UpsertEmail", time.Now(), "account_id", e.AccountID, "email_id", e.ID)`. Never pass token/secret/hash columns.

- [ ] **Step 7: Run all tests with the race detector**

Run: `gofmt -l internal cmd; go vet ./... && go test -race ./...`
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add internal
git commit -m "feat(logging): request/run-scoped debug logs across sync, delivery, provider, store"
```

---

### Task 11: Startup wiring, docs, smoke script, end-to-end verification

**Files:**
- Modify: `cmd/server/main.go`, `README.md`, `.env.example`, `scripts/smoke.sh`, `.gitignore`

- [ ] **Step 1: `main.go` and `.env.example`**

Ensure `main.go` calls `db.SetLogger(log.With("component", "store"))` and passes `authSvc` (Task 6). Remove `API_KEY` from `.env.example`; add:

```
# Dashboard login sessions last this many days without use.
SESSION_TTL_DAYS=30

# Set to any value for exhaustive debug logging (inputs, decisions, outputs;
# secrets are always redacted).
DEBUG=
```

Add `.idea/` to `.gitignore`.

- [ ] **Step 2: README**

- "Setup → 2. Configure": drop the `API_KEY` line.
- "4. Connect a mailbox": becomes "Open `http://localhost:8080/signup`, create your developer account, then on the dashboard create an API key (shown once) and click **Connect account**." The manual `curl` uses `$API_KEY` as the key you created.
- New section **"Developers, sessions and API keys"** under the endpoint tables listing `/signup`, `/login`, `/logout`, `GET /api/v1/me`, `GET/POST /api/v1/api-keys`, `DELETE /api/v1/api-keys/{id}`, with the session-only note.
- "Webhooks": "global" → "developer-wide".
- New section **"Logging"**: `DEBUG=1`, request ids (`X-Request-Id` in and out), what is redacted.
- Limitations: add "Open signup, no password reset, no login rate limiting", "Pre-tenancy databases are refused; there is no migration".
- Status paragraph: multi-tenancy and API-key management are no longer "deliberately absent".

- [ ] **Step 3: `scripts/smoke.sh` bootstrap**

Replace the `: "${API_KEY:?...}"` line and the account lookup with:

```bash
SMOKE_EMAIL="${SMOKE_EMAIL:-smoke@example.com}"
SMOKE_PASSWORD="${SMOKE_PASSWORD:-smoke-test-password}"
JAR=$(mktemp)
# Sign up, or log in if this developer already exists.
code=$(curl -s -o /dev/null -w '%{http_code}' -c "$JAR" -X POST "$BASE/signup" \
  --data-urlencode "email=$SMOKE_EMAIL" --data-urlencode "password=$SMOKE_PASSWORD")
if [ "$code" != "303" ]; then
  curl -s -o /dev/null -c "$JAR" -X POST "$BASE/login" \
    --data-urlencode "email=$SMOKE_EMAIL" --data-urlencode "password=$SMOKE_PASSWORD"
fi
API_KEY=$(curl -s -b "$JAR" -H 'Content-Type: application/json' -d '{"name":"smoke"}' "$BASE/api/v1/api-keys" | python3 -c 'import sys,json; print(json.load(sys.stdin)["key"])')
[ -n "$API_KEY" ] || { echo "could not mint an API key"; exit 1; }
AUTH="Authorization: Bearer $API_KEY"
```

Keep the existing steps. Append an isolation step at the end (before Summary):

```bash
JAR2=$(mktemp)
curl -s -o /dev/null -c "$JAR2" -X POST "$BASE/signup" --data-urlencode "email=other-$RANDOM@example.com" --data-urlencode "password=$SMOKE_PASSWORD"
OTHER_KEY=$(curl -s -b "$JAR2" -H 'Content-Type: application/json' -d '{"name":"smoke-other"}' "$BASE/api/v1/api-keys" | python3 -c 'import sys,json; print(json.load(sys.stdin)["key"])')
AUTH="Authorization: Bearer $OTHER_KEY"
step "GET /api/v1/accounts/{id} — as a different developer" 404 "$BASE/api/v1/accounts/$ACC"
```

Note: the smoke test needs a connected mailbox under `$SMOKE_EMAIL`; the README tells the operator to log in as that developer and connect one first (or pass `SMOKE_EMAIL`/`SMOKE_PASSWORD` for their own developer).

- [ ] **Step 4: End-to-end verification (verifying-services-end-to-end skill)**

1. Stop any running server; move the old DB aside: `mv unified-messaging.db unified-messaging.db.pre-tenancy` (and its `-wal`/`-shm`). Do **not** delete — the operator decides.
2. Start with `DEBUG=1`, logs to a file. Confirm the startup line shows `session_ttl=720h0m0s debug=true` and no `API_KEY`.
3. Prove the refusal: start once against `unified-messaging.db.pre-tenancy` via `DB_PATH` and confirm the exact "predates multi-tenancy" message and non-zero exit.
4. In the browser: `/signup` → dashboard → create key → **Connect account** → Microsoft consent → back on dashboard with the account listed. Set a webhook on it.
5. Run `SMOKE_EMAIL=<that email> SMOKE_PASSWORD=<pw> scripts/smoke.sh`; all steps PASS including the final 404 as another developer.
6. Read-only DB checks: `SELECT id, email FROM developers; SELECT id, developer_id, email FROM accounts; SELECT id, prefix, revoked_at FROM api_keys;` — hashes and sessions never printed.
7. Grep the server log for the password, the full key, the session cookie value, and `Authorization:` — all must be absent; grep for `request_id=` and `decision=` — both present.

- [ ] **Step 5: Commit**

```bash
git add cmd/server README.md .env.example scripts/smoke.sh .gitignore
git commit -m "docs+ops: tenant setup flow, debug logging guide, smoke bootstrap"
```
