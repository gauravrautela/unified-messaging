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
// database, created now and dropped when the test ends. Every test that only
// needs "a store" goes through here, so the whole suite runs against either
// engine without a single test knowing which one it got.
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

	// One schema per store, so two stores opened by the same test (or by two
	// tests running in parallel) never see each other's rows.
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	schema := "t_" + hex.EncodeToString(b[:])
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(context.Background(), `CREATE SCHEMA "`+schema+`"`); err != nil {
		admin.Close()
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
		_, _ = admin.ExecContext(context.Background(), `DROP SCHEMA "`+schema+`" CASCADE`)
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		s.Close()
		_, _ = admin.ExecContext(context.Background(), `DROP SCHEMA "`+schema+`" CASCADE`)
		admin.Close()
	})
	return s
}
