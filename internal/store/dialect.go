package store

import (
	"strconv"
	"strings"
)

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
	// forUpdate suffixes a SELECT that reads a row in order to read-modify-
	// write it inside a transaction. On SQLite this is the empty string: the
	// connection pool is pinned to one connection (see Open), so a
	// transaction already holds the database's only connection for its
	// whole lifetime and no concurrent transaction can interleave with it.
	// Postgres has no such accidental serialization — its pool routinely
	// hands out several connections at once — so the same read there must
	// take a row lock (SELECT ... FOR UPDATE) or two concurrent
	// transactions can each read the same starting value and one's write
	// clobbers the other's.
	forUpdate string
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
		schema:    render(schemaTemplate, "BYTEA", "BIGINT"),
		migrate:   postgresMigrations,
		likeCI:    func(col string) string { return col + " ILIKE ? ESCAPE '\\'" },
		rebind:    rebindDollar,
		forUpdate: " FOR UPDATE",
	}
}

func render(tmpl, blob, bigint string) string {
	return strings.NewReplacer("{{BLOB}}", blob, "{{BIGINT}}", bigint).Replace(tmpl)
}

// rebindDollar turns ? placeholders into $1, $2, … skipping quoted strings and
// -- line comments (mirroring splitStatements' comment handling: the schema
// and hand-written queries alike may carry a comment with a literal ? or ;
// in it, and neither is a placeholder).
func rebindDollar(q string) string {
	var b strings.Builder
	b.Grow(len(q) + 8)
	n := 0
	inStr := false
	inLine := false
	for i := 0; i < len(q); i++ {
		c := q[i]
		switch {
		case inLine:
			if c == '\n' {
				inLine = false
			}
		case inStr:
			if c == '\'' {
				inStr = false
			}
		case c == '\'':
			inStr = true
		case c == '-' && i+1 < len(q) && q[i+1] == '-':
			inLine = true
		case c == '?':
			n++
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n))
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

// splitStatements breaks a schema script into single statements. Postgres'
// extended protocol refuses a multi-statement Exec, and splitting is safe for
// SQLite too, so Open runs the schema statement by statement on both.
//
// A semicolon only ends a statement outside a '…' literal and outside a --
// line comment; the schema has both, including a comment that contains a
// semicolon of its own.
func splitStatements(script string) []string {
	var (
		out    []string
		cur    strings.Builder
		inStr  bool
		inLine bool
	)
	flush := func() {
		if s := strings.TrimSpace(cur.String()); s != "" {
			out = append(out, s)
		}
		cur.Reset()
	}
	for i := 0; i < len(script); i++ {
		c := script[i]
		switch {
		case inLine:
			if c == '\n' {
				inLine = false
			}
		case inStr:
			if c == '\'' {
				inStr = false
			}
		case c == '\'':
			inStr = true
		case c == '-' && i+1 < len(script) && script[i+1] == '-':
			inLine = true
		case c == ';':
			flush()
			continue
		}
		cur.WriteByte(c)
	}
	flush()
	return out
}
