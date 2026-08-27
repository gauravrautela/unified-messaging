package store

import (
	"os"
	"strings"
	"testing"

	"github.com/gauravrautela/unified-messaging/internal/model"
)

func TestRebindPostgres(t *testing.T) {
	d := postgresDialect()
	cases := map[string]string{
		`SELECT 1`:                                    `SELECT 1`,
		`INSERT INTO t (a, b) VALUES (?, ?)`:          `INSERT INTO t (a, b) VALUES ($1, $2)`,
		`UPDATE t SET a = ? WHERE b = ? AND c = ?`:    `UPDATE t SET a = $1 WHERE b = $2 AND c = $3`,
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
		if strings.HasPrefix(m, "ALTER TABLE") && !strings.Contains(m, "IF NOT EXISTS") {
			t.Errorf("postgres migration must be idempotent: %s", m)
		}
	}
}

func TestDialectNames(t *testing.T) {
	s := OpenForTest(t)
	switch os.Getenv("TEST_DATABASE_URL") {
	case "":
		if s.DriverName() != "sqlite" || s.Dialect() != "sqlite3" {
			t.Fatalf("driver = %q, dialect = %q", s.DriverName(), s.Dialect())
		}
	default:
		if s.DriverName() != "postgres" || s.Dialect() != "postgres" {
			t.Fatalf("driver = %q, dialect = %q", s.DriverName(), s.Dialect())
		}
	}
}

func TestOpenRejectsUnknownDriver(t *testing.T) {
	if _, err := Open("mysql", "whatever"); err == nil {
		t.Fatal("unknown driver was accepted")
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
