# Postgres support — design

**Status:** approved in conversation, 2026-08-27
**Goal:** Run the service on Postgres (Supabase) for the public deployment while keeping SQLite as the zero-setup driver for local development and tests. Migrate the current SQLite data into Postgres once so nobody reconnects.

Decisions taken:

| Question | Decision |
|---|---|
| One driver or two | Both. `DB_DRIVER=sqlite` (default) or `postgres`; one store package, one SQL dialect layer. |
| Existing data | A one-off `cmd/migrate` copies every table (ours and whatsmeow's) from the SQLite file into Postgres. |
| Driver | `github.com/jackc/pgx/v5` through `database/sql` (`pgx/v5/stdlib`). The only new dependency. |
| Supabase | Direct host, port 5432, `sslmode=require`. Pooler (6543) also works; documented. |

## 1. Configuration

```
DB_DRIVER=sqlite|postgres        # default sqlite
DB_PATH=./unified-messaging.db   # sqlite only (unchanged)
DATABASE_URL=postgres://user:pass@host:5432/db?sslmode=require   # postgres only
DB_MAX_OPEN_CONNS=10             # postgres only; default 10
```

`config.Load()` fails when `DB_DRIVER=postgres` and `DATABASE_URL` is empty. `DATABASE_URL` is a secret: never logged (the startup line logs `db_driver` and, for postgres, host and database name only).

## 2. Store package

`store.Open(driver, dsn string) (*Store, error)` replaces `Open(path)`. The `Store` gains a `dialect` value:

```go
type dialect struct {
    name      string                 // "sqlite" | "postgres"
    rebind    func(q string) string  // "?" → "$1,$2,…" on postgres; identity on sqlite
    schema    string                 // the CREATE TABLE script for this dialect
    migrate   []string               // additive column migrations for this dialect
    blob      string                 // "BLOB" | "BYTEA" (used when composing schema)
    bigint    string                 // "INTEGER" | "BIGINT" for epoch columns
    likeCI    func(col string) string// case-insensitive LIKE: sqlite "col LIKE ?"; postgres "col ILIKE ?"
}
```

- Every `s.db.Exec/Query/QueryRow` call goes through `s.q(query)` = `dialect.rebind(query)`. SQL text stays written with `?`.
- The schema is one template with `{{BLOB}}`, `{{BIGINT}}` tokens rendered per dialect; the `PRAGMA` lines are sqlite-only; Postgres uses `BYTEA`, `BIGINT` for epoch columns, and `ALTER TABLE … ADD COLUMN IF NOT EXISTS` for migrations (sqlite keeps the "duplicate column" tolerance).
- Booleans stay `INTEGER 0/1` in both dialects (database/sql converts int64 → bool); no `BOOLEAN` columns, so the migration copies values verbatim.
- `LIKE` search (`ListEmails`, `ListChats`, `ListAttendees`) uses `dialect.likeCI` so behaviour is case-insensitive on both; the existing `escapeLike` + `ESCAPE '\'` stays (Postgres needs `ESCAPE '\'` written as `ESCAPE E'\\'` or with `standard_conforming_strings` on — use `ESCAPE '\'` which is valid when `standard_conforming_strings=on`, the Postgres default since 9.1).
- `ON CONFLICT (…) DO UPDATE SET …` statements are already Postgres-compatible; the two `INSERT … ON CONFLICT DO NOTHING` too. Any `excluded.col` references are valid in both.
- `inTx` unchanged. `SetMaxOpenConns(1)` applies to sqlite only; postgres uses `DB_MAX_OPEN_CONNS` with `SetMaxIdleConns(2)` and `SetConnMaxLifetime(30m)`.
- `Store.DB()` still returns the `*sql.DB` for whatsmeow; `Store.Dialect()` returns `"sqlite3"` / `"postgres"` in whatsmeow's naming for `sqlstore.NewWithDB`.
- The `preTenancy` check (`PRAGMA table_info`) becomes dialect-aware (sqlite only; postgres never has a pre-tenancy DB).
- File mode 0600 logic applies to sqlite only.

## 3. Tests

- Default: SQLite in a temp dir, exactly as today (`store.OpenForTest(t)` helper replaces the `store.Open(filepath.Join(t.TempDir(), …))` pattern in every test helper).
- When `TEST_DATABASE_URL` is set, `OpenForTest` opens Postgres, creates a private schema `t_<12 hex>` and sets `search_path` on the connection string, runs the schema, and drops the schema in `t.Cleanup`. Each test is isolated; nothing touches `public`. The store and api suites are run once in each mode before merge (`go test ./... ` and `TEST_DATABASE_URL=… go test ./internal/store/ ./internal/api/ ./internal/events/`).
- `go test ./...` without the variable never needs a network.

## 4. whatsmeow

`whatsapp.New(db.DB(), db.Dialect(), …)` — `sqlstore.NewWithDB(db, dialect, …)` with `"postgres"` on Postgres. whatsmeow creates and migrates its own tables in the same database (schema `public`).

## 5. Migration tool

`cmd/migrate`: `go run ./cmd/migrate -from ./unified-messaging.db -to "$DATABASE_URL"`.

1. Opens the SQLite file read-only and Postgres with the store's Postgres schema (creating our tables) and whatsmeow's `Upgrade` (creating its tables).
2. Refuses to run unless every destination table is empty (`-force` overrides by truncating destination tables first, in dependency order).
3. Copies table by table in FK order (developers, api_keys, sessions, accounts, tokens, folders, emails, cursors, subscriptions, webhooks, webhook_deliveries, oauth_states, chats, attendees, chat_members, chat_messages, chat_sessions, idempotency_keys, then every `whatsmeow_*` table) using `SELECT *` → column-name-mapped `INSERT`, in transactions of 500 rows, with a per-table row count printed and verified against the source.
4. Sealed values (refresh tokens, Telegram config, pending hooks) are copied byte-for-byte — same `TOKEN_ENCRYPTION_KEY` on both sides, nothing is re-sealed.
5. Exit non-zero on any count mismatch. Never modifies the SQLite file.

## 6. Operations

- `main.go`: `store.Open(cfg.DBDriver, cfg.DBDSN)`; startup log line adds `db_driver`, and for postgres `db_host`/`db_name` (never the URL). `/healthz` adds `"db":"ok"|"error"` from a 2-second `PingContext` (still unauthenticated; the value is a single word).
- Purge tickers, delivery pool, and everything above the store are unchanged.
- README: a "Postgres / Supabase" setup section (env, sslmode, pooler note, migration command, the fact that SQLite remains for dev), and the audit's "single-connection SQLite" limitation marked closed for Postgres deployments.

## 7. Out of scope

Connection-level row security, read replicas, schema-per-tenant, Supabase Auth/PostgREST (the service keeps its own auth), automatic SQLite→Postgres sync (the migration is one-off).
