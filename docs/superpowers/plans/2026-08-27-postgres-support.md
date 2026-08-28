# Postgres Support Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Run the service on Postgres (Supabase) with SQLite kept as the default dev/test driver, and migrate existing SQLite data once.

**Architecture:** A small dialect layer inside `internal/store` (placeholder rebinding, per-dialect schema/migrations, case-insensitive LIKE, pool settings). All SQL stays written with `?`. `store.Open(driver, dsn)`; `OpenForTest(t)` isolates Postgres tests in a throwaway schema. whatsmeow gets the matching dialect name. `cmd/migrate` copies tables SQLite → Postgres with count verification.

**Tech Stack:** Go 1.26, `database/sql`, `modernc.org/sqlite` (existing), `github.com/jackc/pgx/v5/stdlib` (new, the only added dependency).

**Spec:** `docs/superpowers/specs/2026-08-27-postgres-support-design.md`

## Global Constraints

- One new dependency only: `github.com/jackc/pgx/v5`. `gofmt -l internal cmd` empty; `go vet ./...`; `go test ./...` green **without** any database env var (SQLite path).
- With `TEST_DATABASE_URL` set, `go test ./internal/store/ ./internal/api/ ./internal/events/ ./internal/auth/` must be green on Postgres; tests create and drop a private schema and never touch `public`.
- `DATABASE_URL` / `TEST_DATABASE_URL` are secrets: never logged, never printed in a report; the startup line logs `db_driver`, `db_host`, `db_name` only.
- SQL text keeps `?` placeholders; every execution goes through `s.q()`. No `$1` literals in handwritten SQL.
- Booleans stay `INTEGER 0/1`; epoch columns are `BIGINT` on Postgres, `INTEGER` on SQLite; `BLOB` ↔ `BYTEA`.
- SQLite behaviour is byte-for-byte unchanged for existing deployments (file mode 0600, PRAGMAs, single writer, migrations tolerant of "duplicate column").
- Nothing in the repo is deleted; the real `unified-messaging.db` is never opened for writing by any task (the migration tool opens it read-only; verification uses copies and a throwaway Postgres schema).
- Commit trailers on every commit:
  `Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>` and
  `Claude-Session: https://claude.ai/code/session_01RwMaDW9KNcu6BjtbMkU8mo`

---

## File structure

| File | Responsibility |
|---|---|
| `internal/store/dialect.go` (+test) | `dialect` type, `sqliteDialect`, `postgresDialect`, `rebind`, `renderSchema` |
| `internal/store/schema.go` | schema as a template with `{{BLOB}}`/`{{BIGINT}}`; PRAGMA lines moved to the sqlite dialect; postgres migrations use `ADD COLUMN IF NOT EXISTS` |
| `internal/store/store.go` | `Open(driver, dsn)`, `Dialect()`, `s.q()`, pool settings, sqlite-only file mode and pre-tenancy check |
| `internal/store/testing.go` | `OpenForTest(t)` (sqlite temp file, or Postgres throwaway schema when `TEST_DATABASE_URL` is set) |
| `internal/store/{store,aux,chat,developers}.go` | every `s.db.Exec/Query/QueryRow` wrapped with `s.q`; `likeCI` at the three search sites |
| `internal/config/config.go` | `DBDriver`, `DBDSN`, `DBMaxOpenConns` |
| `internal/provider/whatsapp/whatsapp.go` | `New(db, dialect, deviceName, log)` |
| `cmd/server/main.go` | wiring, startup log, `/healthz` db ping (via `Store.Ping`) |
| `cmd/migrate/main.go` (+test) | SQLite → Postgres copier |
| all `*_test.go` that call `store.Open` | switch to `store.OpenForTest(t)` |
| `README.md`, `.env.example`, `/docs` §12 | Postgres/Supabase setup, migration, limits update |

---

### Task 1: Dialect layer, `Open(driver, dsn)`, config, `OpenForTest`

**Files:**
- Create: `internal/store/dialect.go`, `internal/store/dialect_test.go`, `internal/store/testing.go`
- Modify: `internal/store/schema.go`, `internal/store/store.go` (`Open`, struct, `preTenancy`, file-mode block), `internal/config/config.go`, `.env.example`, `go.mod`/`go.sum` (`go get github.com/jackc/pgx/v5@latest`), `cmd/server/main.go:57`, and the nine test files that call `store.Open(` (`internal/auth/auth_test.go`, `internal/chatsync/soak_test.go`, `internal/chatsync/runtime_test.go`, `internal/accounts/accounts_test.go`, `internal/api/api_test.go`, `internal/syncer/provider_contract_test.go`, `internal/syncer/syncer_test.go`, `internal/events/events_test.go`, `internal/store/webhooks_test.go`) plus `internal/store/store_test.go` if it opens stores directly.

**Interfaces:**
- `store.Open(driver, dsn string) (*Store, error)`; `driver` ∈ `"sqlite"`, `"postgres"`.
- `(*Store).Dialect() string` → `"sqlite3"` | `"postgres"` (whatsmeow's names); `(*Store).DriverName() string` → `"sqlite"` | `"postgres"`.
- `(*Store).q(query string) string` (unexported) — placeholder rebinding.
- `store.OpenForTest(t testing.TB) *Store`.
- `config.Config{DBDriver string; DBDSN string; DBMaxOpenConns int}`; `DBDSN` is `DB_PATH` for sqlite and `DATABASE_URL` for postgres.

- [ ] **Step 1: Failing tests** — `internal/store/dialect_test.go`:

```go
package store

import "testing"

func TestRebindPostgres(t *testing.T) {
	d := postgresDialect()
	cases := map[string]string{
		`SELECT 1`:                                   `SELECT 1`,
		`INSERT INTO t (a, b) VALUES (?, ?)`:         `INSERT INTO t (a, b) VALUES ($1, $2)`,
		`UPDATE t SET a = ? WHERE b = ? AND c = ?`:   `UPDATE t SET a = $1 WHERE b = $2 AND c = $3`,
		`SELECT '?' AS literal, a FROM t WHERE b = ?`: `SELECT '?' AS literal, a FROM t WHERE b = $1`, // ? inside a quoted string is untouched
	}
	for in, want := range cases {
		if got := d.rebind(in); got != want {
			t.Errorf("rebind(%q) = %q, want %q", in, got, want)
		}
	}
	if s := sqliteDialect().rebind(`a = ? AND b = ?`); s != `a = ? AND b = ?` {
		t.Fatalf("sqlite rebind must be identity, got %q", s)
	}
}

func TestSchemaRendersPerDialect(t *testing.T) {
	pg := postgresDialect().schema
	sq := sqliteDialect().schema
	for _, bad := range []string{"{{BLOB}}", "{{BIGINT}}", "PRAGMA"} {
		if strings.Contains(pg, bad) {
			t.Errorf("postgres schema still contains %q", bad)
		}
	}
	if !strings.Contains(pg, "payload         BYTEA") || !strings.Contains(pg, "created_at    BIGINT") {
		t.Errorf("postgres schema types not rendered")
	}
	if !strings.Contains(sq, "PRAGMA journal_mode = WAL") || !strings.Contains(sq, "payload         BLOB") {
		t.Errorf("sqlite schema changed")
	}
	for _, m := range postgresDialect().migrate {
		if !strings.Contains(m, "IF NOT EXISTS") {
			t.Errorf("postgres migration must be idempotent: %s", m)
		}
	}
}

func TestOpenForTestIsolatesSchemasOnPostgres(t *testing.T) {
	if os.Getenv("TEST_DATABASE_URL") == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	a := OpenForTest(t)
	b := OpenForTest(t)
	if err := a.CreateDeveloper(model.Developer{ID: "dev_1", Email: "x@y.z"}, "h"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.DeveloperByEmail("x@y.z"); err == nil {
		t.Fatal("stores must not share a schema")
	}
}
```

Check the exact column spacing in `schema.go` for the `Contains` strings (adjust to the real text; keep the assertions meaningful).

- [ ] **Step 2: Run** `go test ./internal/store/ -run 'TestRebind|TestSchemaRenders|TestOpenForTest'` → compile failure.

- [ ] **Step 3: Implement**

`internal/store/dialect.go`:

```go
package store

import "strings"

// dialect is everything that differs between SQLite and Postgres. SQL in
// this package is written once, with ? placeholders and a few tokens in
// the schema; the dialect renders it for the engine in use.
type dialect struct {
	name    string   // "sqlite" | "postgres"
	wmName  string   // whatsmeow's name for the same engine
	schema  string   // rendered CREATE TABLE script
	migrate []string // additive column migrations
	likeCI  func(col string) string
	rebind  func(q string) string
}

func sqliteDialect() *dialect {
	return &dialect{
		name: "sqlite", wmName: "sqlite3",
		schema:  sqlitePragmas + render(schemaTemplate, "BLOB", "INTEGER"),
		migrate: sqliteMigrations,
		likeCI:  func(col string) string { return col + " LIKE ? ESCAPE '\\'" },
		rebind:  func(q string) string { return q },
	}
}

func postgresDialect() *dialect {
	return &dialect{
		name: "postgres", wmName: "postgres",
		schema:  render(schemaTemplate, "BYTEA", "BIGINT"),
		migrate: postgresMigrations,
		likeCI:  func(col string) string { return col + " ILIKE ? ESCAPE '\\'" },
		rebind:  rebindDollar,
	}
}

func render(tmpl, blob, bigint string) string {
	return strings.NewReplacer("{{BLOB}}", blob, "{{BIGINT}}", bigint).Replace(tmpl)
}

// rebindDollar turns ? placeholders into $1, $2, … skipping quoted strings.
func rebindDollar(q string) string {
	var b strings.Builder
	b.Grow(len(q) + 8)
	n := 0
	inStr := false
	for i := 0; i < len(q); i++ {
		c := q[i]
		switch {
		case c == '\'':
			inStr = !inStr
			b.WriteByte(c)
		case c == '?' && !inStr:
			n++
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n))
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}
```

`schema.go`: rename the const to `schemaTemplate`, replace `BLOB` with `{{BLOB}}` (2 sites) and every epoch/`INTEGER NOT NULL` timestamp column (`created_at`, `updated_at`, `expires_at`, `last_used_at`, `revoked_at`, `last_synced_at`, `sent_at`, `edited_at`, `next_attempt_at`, `consented_at`, `date`, `last_message_at`, `at` … — grep `INTEGER` and decide per column: epochs → `{{BIGINT}}`, flags/counters stay `INTEGER`) with `{{BIGINT}}`; move the two `PRAGMA` lines into `const sqlitePragmas`; keep `sqliteMigrations` as is and add `postgresMigrations` with the same columns as `ALTER TABLE x ADD COLUMN IF NOT EXISTS …`; the `DELETE FROM sessions WHERE length(id) <> 64` line is valid in both.

`store.go`:

```go
type Store struct {
	db      *sql.DB
	d       *dialect
	// … existing fields
}

func Open(driver, dsn string) (*Store, error) {
	var d *dialect
	var db *sql.DB
	var err error
	switch driver {
	case "sqlite", "":
		d = sqliteDialect()
		if err := ensureSQLiteFile(dsn); err != nil { // the existing 0600 logic
			return nil, err
		}
		db, err = sql.Open("sqlite", dsn+"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
		if err == nil {
			db.SetMaxOpenConns(1)
		}
	case "postgres":
		d = postgresDialect()
		db, err = sql.Open("pgx", dsn)
		if err == nil {
			db.SetMaxOpenConns(10) // overridden by SetMaxOpenConns below
			db.SetMaxIdleConns(2)
			db.SetConnMaxLifetime(30 * time.Minute)
		}
	default:
		return nil, fmt.Errorf("store: unknown driver %q", driver)
	}
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(d.schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: schema: %w", err)
	}
	for _, m := range d.migrate {
		if _, err := db.Exec(m); err != nil && !(d.name == "sqlite" && strings.Contains(err.Error(), "duplicate column")) {
			db.Close()
			return nil, fmt.Errorf("store: migration: %w", err)
		}
	}
	s := &Store{db: db, d: d}
	if d.name == "sqlite" {
		// preTenancy check as today
	}
	return s, nil
}

func (s *Store) q(query string) string { return s.d.rebind(query) }
func (s *Store) Dialect() string       { return s.d.wmName }
func (s *Store) DriverName() string    { return s.d.name }
func (s *Store) SetMaxOpenConns(n int) { if s.d.name == "postgres" && n > 0 { s.db.SetMaxOpenConns(n) } }
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }
```

Postgres executes the schema as one multi-statement `Exec` — pgx's stdlib supports multiple statements in a simple-protocol `Exec` without arguments; if it does not, split on `;\n` and execute each (do that unconditionally: it is safe for both).

`internal/store/testing.go`:

```go
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

// OpenForTest opens a throwaway store: a SQLite file in t.TempDir(), or —
// when TEST_DATABASE_URL is set — a private schema in that Postgres
// database, created now and dropped when the test ends.
func OpenForTest(t testing.TB) *Store {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		s, err := Open("sqlite", filepath.Join(t.TempDir(), "test.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Close() })
		return s
	}
	var b [6]byte
	_, _ = rand.Read(b[:])
	schema := "t_" + hex.EncodeToString(b[:])
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(context.Background(), `CREATE SCHEMA "`+schema+`"`); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	qv := u.Query()
	qv.Set("search_path", schema)
	u.RawQuery = qv.Encode()
	s, err := Open("postgres", u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		s.Close()
		_, _ = admin.ExecContext(context.Background(), `DROP SCHEMA "`+schema+`" CASCADE`)
		admin.Close()
	})
	return s
}
```

pgx accepts `search_path` as a URL parameter (it maps to the `search_path` runtime parameter). Verify with the isolation test; if not honoured, use `options=-c search_path=<schema>` instead.

`config.go`: `DBDriver: env("DB_DRIVER", "sqlite")`, `DBDSN` = `DB_PATH` (sqlite) or `DATABASE_URL` (postgres; error if empty), `DBMaxOpenConns: envInt("DB_MAX_OPEN_CONNS", 10)`; `.env.example` documents the three plus a commented Supabase example URL (`postgresql://postgres:<password>@db.<ref>.supabase.co:5432/postgres?sslmode=require`).

`main.go`: `store.Open(cfg.DBDriver, cfg.DBDSN)`; `db.SetMaxOpenConns(cfg.DBMaxOpenConns)`; startup log adds `"db_driver", cfg.DBDriver` and for postgres `"db_host", u.Hostname(), "db_name", strings.TrimPrefix(u.Path, "/")` (parse once; never the URL). Test helpers: replace `store.Open(filepath.Join(t.TempDir(), "…"))` with `store.OpenForTest(t)` everywhere (drop the now-unused `filepath` imports and the manual `t.Cleanup(s.Close)`).

- [ ] **Step 4: Run** `go test ./...` (sqlite) → PASS. Then, if `TEST_DATABASE_URL` is available in the environment (set from `.env`'s `DATABASE_URL` — read it via `set -a; source .env; set +a; export TEST_DATABASE_URL="$DATABASE_URL"` inside the shell; never echo it), `go test ./internal/store/ -run 'TestOpenForTest|TestRebind'` → PASS.
- [ ] **Step 5: Commit** `feat(store): dialect layer, Open(driver, dsn), postgres driver, OpenForTest`

---

### Task 2: Route every statement through the dialect; make the store suite pass on Postgres

**Files:**
- Modify: `internal/store/store.go`, `aux.go`, `chat.go`, `developers.go` — wrap every `s.db.Exec(`, `s.db.Query(`, `s.db.QueryRow(`, `tx.Exec(`, `tx.QueryRow(` first argument in `s.q(...)` (inside `inTx` closures the store is in scope); the three `LIKE … ESCAPE` sites use `s.d.likeCI(col)`.
- Test: the existing store suite under `TEST_DATABASE_URL`; add `internal/store/postgres_test.go` with a smoke test that round-trips one row through every table's insert/select path (or simply relies on the suite).

**Interfaces:** none new.

- [ ] **Step 1: Failing run** — `export TEST_DATABASE_URL=…; go test ./internal/store/` → failures (`syntax error at or near "?"`, type errors).
- [ ] **Step 2: Implement** the wrapping mechanically (a `grep -n "s.db\.\(Exec\|Query\|QueryRow\)(" internal/store/*.go` list; every hit gets `s.q(`). Fix what Postgres reports: `INTEGER` columns scanned into `time.Time` via `time.Unix` are fine; `BYTEA` scans into `[]byte` fine; `RowsAffected` fine; `ON CONFLICT` targets must name a unique index/PK — confirm every conflict target has a PK/UNIQUE constraint in the schema (`subscriptions(id)`, `webhooks(id)`, `chats(account_id,id)`, `attendees(account_id,id)`, `chat_messages(account_id,id)`, `chat_sessions(account_id)`, `idempotency_keys(developer_id,key)`, `accounts(developer_id,email)` → check `UNIQUE(developer_id, email)` exists, `tokens(account_id)`, `emails(account_id,id)`, `cursors(account_id,scope_id)`, `folders(account_id,id)`); `excluded.` references are the same in Postgres; boolean scans (`dead`, `read`, `flagged`, `is_group`, `archived`, `muted`, `is_self`, `deleted`, `is_from_me`, `draft`, `has_attachments`, `is_inline`) come back as `int64` → Go `bool` via database/sql conversion — confirm; if pgx returns `int32` for `INTEGER`, database/sql still converts. `LIMIT ? OFFSET ?` fine. `length(id) <> 64` fine. `GROUP BY`/`ORDER BY` unchanged.
- [ ] **Step 3: Run** `TEST_DATABASE_URL=… go test ./internal/store/ ./internal/auth/ ./internal/events/ ./internal/api/` → PASS; and `go test ./...` without the variable → PASS.
- [ ] **Step 4: Commit** `feat(store): all queries dialect-rebound; store suite green on postgres`

---

### Task 3: whatsmeow dialect, wiring, health, docs

**Files:**
- Modify: `internal/provider/whatsapp/whatsapp.go:55` (`New(db *sql.DB, dialect, deviceName string, log)`), its tests, `cmd/server/main.go` (pass `db.Dialect()`; `/healthz` gets `"db"` via `Store.Ping` with a 2 s context — the handler lives in `internal/api/api.go` `browserHandlers`), `internal/api/api_test.go` (healthz test asserts `"db":"ok"`), `README.md` (new `### 7. Optional: Postgres / Supabase` under Setup; `## Known gaps` single-connection note marked "SQLite only"), `.env.example`, `internal/api/handlers_docs.go` §12 and `handlers_llms.go` Limits (single-connection note scoped to SQLite).

- [ ] **Step 1: Failing tests** — `TestHealthzReportsDB` (200 with `"db":"ok"`); whatsapp `New` signature test compiles with the dialect arg.
- [ ] **Step 2: Implement.** `sqlstore.NewWithDB(db, dialect, waLog.Noop)`; `main.go` passes `db.Dialect()`; healthz `{"status":"ok","db":"ok","dropped_events":n}` (503 with `"db":"error"` on ping failure — keep the body free of error text).
- [ ] **Step 3: Run** `go test ./...`; commit `feat: whatsmeow on the store dialect; db health; postgres docs`.

---

### Task 4: `cmd/migrate` — SQLite → Postgres copier

**Files:**
- Create: `cmd/migrate/main.go`, `cmd/migrate/migrate.go` (the logic, testable), `cmd/migrate/migrate_test.go`

**Interfaces:** `func Migrate(ctx context.Context, src *sql.DB, dst *sql.DB, force bool, log func(string, ...any)) (map[string]int64, error)` — returns per-table counts copied; `main` opens the SQLite file read-only (`?mode=ro` — verify the modernc DSN form, else `immutable=1`), opens Postgres via `store.Open("postgres", dsn)` (creates our schema) and runs whatsmeow's `sqlstore.NewWithDB(dst, "postgres", …).Upgrade(ctx)` (creates its tables), then `Migrate`.

- [ ] **Step 1: Failing test** — `TestMigrateCopiesEveryTable` (skips without `TEST_DATABASE_URL`): build a SQLite store in a temp dir with one row in each of our tables (developer, api key, session, account, tokens row, folder, email, cursor, subscription, webhook, delivery, oauth_state, chat, attendee, member, message, chat_session, idempotency key) via the store API, plus one row in a `whatsmeow_device`-shaped table if whatsmeow's schema can be created on SQLite in the test (call its `Upgrade` on the source too); run `Migrate` into a throwaway Postgres schema (reuse `store.OpenForTest` for the destination and pass its `DB()`), assert every table's count matches and a sealed token round-trips byte-for-byte; a second run without `-force` errors "destination not empty"; with `force` it succeeds.
- [ ] **Step 2: Implement.** Table list in FK order (spec §5), discover columns with `PRAGMA table_info` on the source and build `INSERT INTO t (cols…) VALUES (…)` for the destination with `$n` placeholders; copy in transactions of 500 rows; `whatsmeow_*` tables discovered from `sqlite_master` (`name LIKE 'whatsmeow_%'`); Postgres `BYTEA` ← SQLite `BLOB` works via `[]byte`; integers/`int64`; text. Emptiness check: `SELECT COUNT(*)` per destination table; `-force` truncates in reverse FK order (`TRUNCATE … CASCADE`). Print a per-table summary; exit 1 on mismatch.
- [ ] **Step 3: Run** the test on Postgres; `go vet ./...`; commit `feat(migrate): one-off sqlite→postgres copier with count verification`.

---

### Task 5: Verification against Supabase (throwaway schema) and hand-off

- [ ] **Step 1:** With `.env` sourced (never echo `DATABASE_URL`): run the whole suite on Postgres — `TEST_DATABASE_URL="$DATABASE_URL" go test ./internal/... ./cmd/...` — record pass/fail per package.
- [ ] **Step 2: Dry-run migration** into a throwaway schema: create `migrate_check_<hex>` on Supabase, run `go run ./cmd/migrate -from <copy of ./unified-messaging.db in the scratch dir> -to "$DATABASE_URL&search_path=migrate_check_…"` (or `?options=-c search_path=…`), confirm the per-table counts match the SQLite copy's counts (`SELECT COUNT(*)` on both sides for every table), then `DROP SCHEMA … CASCADE`. Report the counts table.
- [ ] **Step 3: E2E on Postgres** in the same throwaway-schema style: run the server with `DB_DRIVER=postgres DATABASE_URL=<url with search_path=e2e_<hex>> LISTEN_ADDR=:8099` on an empty schema; sign up a developer, create a key, `GET /healthz` → `db:ok`, `GET /api/v1/me`, create a webhook, list it, delete it; check the startup log shows `db_driver=postgres db_host=db.….supabase.co db_name=postgres` and never the URL; stop; drop the schema. Leak grep on the log for the password.
- [ ] **Step 4:** README "Cut-over" subsection with the exact three commands for the operator: stop the server → `go run ./cmd/migrate -from ./unified-messaging.db -to "$DATABASE_URL"` → start with `DB_DRIVER=postgres`. State that the operator runs the real migration (this plan does not touch `public`).
- [ ] **Step 5:** `gofmt -l internal cmd; go vet ./...; go test ./...`; commit `docs: postgres cut-over; verification notes`.

---

## Self-review

- **Spec coverage:** §1 → T1; §2 → T1+T2; §3 → T1 (`OpenForTest`) + T2 (suite on Postgres); §4 → T3; §5 → T4; §6 → T3 + T5; §7 n/a.
- **Placeholders:** the T2/T4 steps describe exact mechanical edits and the exact test inputs; code for the dialect layer and `OpenForTest` is given in full.
- **Type consistency:** `Open(driver, dsn)`, `Dialect()`, `DriverName()`, `q()`, `OpenForTest(t)`, `SetMaxOpenConns`, `Ping` used identically in T1–T5; whatsapp `New(db, dialect, deviceName, log)` in T3 and T4; `Migrate` signature in T4/T5.
