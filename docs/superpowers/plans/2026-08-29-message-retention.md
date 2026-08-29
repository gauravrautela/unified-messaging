# Message Content Retention Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let a developer set a retention policy so message content is blanked from the local mirror once it has been forwarded to their webhooks, and in no case kept longer than their configured maximum.

**Architecture:** Two eviction triggers write to the same store primitives. The fast one lives in `events.Dispatcher.deliver()`, which already knows in-process whether every subscribing hook accepted the event; the backstop is a SQL sweep added to the hourly purge loop that already runs in `cmd/server/main.go`. Eviction blanks columns in place — it never deletes rows — because `store.EmailExists` and `store.GetChatMessage` are load-bearing for sync correctness. Three write paths would otherwise refill an evicted row and are guarded.

**Tech Stack:** Go 1.x, `database/sql` over SQLite *and* Postgres (every query goes through `s.q()` for placeholder rebinding, and every schema change lands in **both** `sqliteMigrations` and `postgresMigrations`), `go test ./...`.

**Spec:** `docs/superpowers/specs/2026-08-29-message-retention-design.md`

## Global Constraints

- **Dual dialect.** Every schema change goes in both `sqliteMigrations` and `postgresMigrations` (`internal/store/schema.go:252`, `:271`) *and* in `schemaTemplate` for fresh databases. SQLite spells the type `INTEGER`; Postgres spells it `BIGINT` and uses `ADD COLUMN IF NOT EXISTS`. The template uses the `{{BIGINT}}` token.
- **Every query is wrapped in `s.q(...)`.** That is what rebinds `?` to `$1` on Postgres. A query without it works on SQLite and fails on Postgres — and the test suite only catches it when `TEST_DATABASE_URL` is set.
- **Never log content.** Log counts, ids and byte lengths only, matching `internal/store/aux.go:274` ("payload is a byte count, never content").
- **`0` means retention off.** `retention_max_age_secs = 0` is the default for every existing and new developer and disables both triggers. No behaviour changes on deploy.
- **Eviction is best-effort.** It must never fail a webhook delivery or an API request. Log the error and let the hourly sweep pick the message up.
- **Run `go test ./...` from the repo root.** `make test` is the same thing.

---

### Task 1: Schema — retention columns and ingestion timestamps

**Files:**
- Modify: `internal/store/schema.go` (`schemaTemplate` developers/emails/chat_messages blocks; `sqliteMigrations:252`; `postgresMigrations:271`)
- Modify: `internal/store/store.go:448-481` (`UpsertEmail`)
- Modify: `internal/store/chat.go:315-331` (`UpsertChatMessage`)
- Test: `internal/store/store_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: columns `developers.retention_max_age_secs`, `emails.stored_at`, `emails.content_evicted_at`, `chat_messages.stored_at`, `chat_messages.content_evicted_at`. `UpsertEmail` and `UpsertChatMessage` stamp `stored_at` on insert only.

- [ ] **Step 1: Write the failing test**

Add to `internal/store/store_test.go`:

```go
// stored_at is ingestion time, not the provider's timestamp: a backfill of an
// old mailbox must not look ancient to the retention sweep.
func TestUpsertEmailStampsStoredAtOnInsertOnly(t *testing.T) {
	s := newTestStore(t)
	acct := seedAccount(t, s)
	old := time.Now().Add(-3 * 365 * 24 * time.Hour).UTC().Truncate(time.Second)

	if err := s.UpsertEmail(model.Email{
		AccountID: acct, ID: "m1", Subject: "hello", Body: "body", Date: old,
	}); err != nil {
		t.Fatal(err)
	}

	var first int64
	if err := s.DB().QueryRow(s.Q(`SELECT stored_at FROM emails WHERE account_id = ? AND id = ?`), acct, "m1").Scan(&first); err != nil {
		t.Fatal(err)
	}
	if first < time.Now().Add(-time.Minute).Unix() {
		t.Fatalf("stored_at = %d, want a recent timestamp, not the message date %d", first, old.Unix())
	}

	// A later update must not restamp it, or nothing would ever age out.
	if err := s.UpsertEmail(model.Email{
		AccountID: acct, ID: "m1", Subject: "hello again", Body: "body", Date: old,
	}); err != nil {
		t.Fatal(err)
	}
	var second int64
	if err := s.DB().QueryRow(s.Q(`SELECT stored_at FROM emails WHERE account_id = ? AND id = ?`), acct, "m1").Scan(&second); err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Fatalf("stored_at changed on update: %d -> %d", first, second)
	}
}

func TestUpsertChatMessageStampsStoredAt(t *testing.T) {
	s := newTestStore(t)
	acct := seedChatAccount(t, s)
	if err := s.UpsertChat(model.Chat{AccountID: acct, ID: "c1", Kind: "dm"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertChatMessage(model.ChatMessage{
		AccountID: acct, ID: "cm1", ChatID: "c1", Kind: "text", Text: "hi",
		SentAt: time.Now().Add(-48 * time.Hour).UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	var storedAt int64
	if err := s.DB().QueryRow(s.Q(`SELECT stored_at FROM chat_messages WHERE account_id = ? AND id = ?`), acct, "cm1").Scan(&storedAt); err != nil {
		t.Fatal(err)
	}
	if storedAt < time.Now().Add(-time.Minute).Unix() {
		t.Fatalf("stored_at = %d, want a recent timestamp", storedAt)
	}
}

func TestDevelopersHaveRetentionColumnDefaultingToZero(t *testing.T) {
	s := newTestStore(t)
	dev := seedDeveloper(t, s, "dev_1", "dev1@example.com")
	var secs int64
	if err := s.DB().QueryRow(s.Q(`SELECT retention_max_age_secs FROM developers WHERE id = ?`), dev).Scan(&secs); err != nil {
		t.Fatal(err)
	}
	if secs != 0 {
		t.Fatalf("retention_max_age_secs = %d, want 0 (retention off by default)", secs)
	}
}
```

These tests need raw DB access from the test package. `store_test.go` is in `package store`, so add two unexported-but-test-visible accessors to `internal/store/testing.go` rather than exporting the pool:

```go
// DB and Q give tests raw access to the pool and the dialect's rebinder, so a
// test can assert on a column the public API does not expose.
func (s *Store) DB() *sql.DB { return s.db }
func (s *Store) Q(q string) string { return s.q(q) }
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/store/ -run 'StampsStoredAt|RetentionColumn' -v`
Expected: FAIL — `no such column: stored_at` / `no such column: retention_max_age_secs`.

- [ ] **Step 3: Add the columns to `schemaTemplate`**

In `internal/store/schema.go`, add to the `developers` block:

```sql
  retention_max_age_secs {{BIGINT}} NOT NULL DEFAULT 0
```

to the `emails` block:

```sql
  stored_at           {{BIGINT}} NOT NULL DEFAULT 0,
  content_evicted_at  {{BIGINT}}
```

and to the `chat_messages` block:

```sql
  stored_at          {{BIGINT}} NOT NULL DEFAULT 0,
  content_evicted_at {{BIGINT}}
```

Put each before the `PRIMARY KEY` line of its table.

- [ ] **Step 4: Add the migrations to both dialect lists**

Append to `sqliteMigrations`:

```go
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
```

Append to `postgresMigrations`:

```go
	`ALTER TABLE developers ADD COLUMN IF NOT EXISTS retention_max_age_secs BIGINT NOT NULL DEFAULT 0`,
	`ALTER TABLE emails ADD COLUMN IF NOT EXISTS stored_at BIGINT NOT NULL DEFAULT 0`,
	`ALTER TABLE emails ADD COLUMN IF NOT EXISTS content_evicted_at BIGINT`,
	`ALTER TABLE chat_messages ADD COLUMN IF NOT EXISTS stored_at BIGINT NOT NULL DEFAULT 0`,
	`ALTER TABLE chat_messages ADD COLUMN IF NOT EXISTS content_evicted_at BIGINT`,
	// See the sqliteMigrations note: pre-existing rows start their retention
	// clock at the upgrade rather than at the epoch.
	`UPDATE emails SET stored_at = EXTRACT(EPOCH FROM now())::bigint WHERE stored_at = 0`,
	`UPDATE chat_messages SET stored_at = EXTRACT(EPOCH FROM now())::bigint WHERE stored_at = 0`,
```

Order matters: each `UPDATE` must come after the `ALTER` that creates its column.

- [ ] **Step 5: Stamp `stored_at` in `UpsertEmail`**

In `internal/store/store.go:455`, add the column to the INSERT list and one more `?`. Do **not** add it to the `DO UPDATE SET` clause — that is what makes it insert-only:

```go
	_, err := s.db.Exec(s.q(`
		INSERT INTO emails (account_id, id, thread_id, folder_id, subject, from_name, from_email,
		  to_json, cc_json, bcc_json, reply_to_json, date, snippet, body, body_type,
		  read, flagged, draft, has_attachments, internet_message_id, attachments_json, stored_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(account_id, id) DO UPDATE SET
		  ...unchanged for now...`),
		e.AccountID, e.ID, e.ThreadID, e.FolderID, e.Subject, e.From.Name, e.From.Email,
		string(to), string(cc), string(bcc), string(rt), e.Date.Unix(), e.Snippet, e.Body,
		e.BodyType, b2i(e.Read), b2i(e.Flagged), b2i(e.Draft), b2i(e.HasAttachments),
		e.InternetMessageID, string(att), time.Now().Unix())
```

- [ ] **Step 6: Stamp `stored_at` in `UpsertChatMessage`**

In `internal/store/chat.go:320`:

```go
	res, err := s.db.Exec(s.q(`
		INSERT INTO chat_messages (account_id, id, chat_id, sender_id, is_from_me, kind, text, quoted_id, sent_at, edited_at, deleted, status, reactions_json, stored_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(account_id, id) DO NOTHING`),
		m.AccountID, m.ID, m.ChatID, m.Sender.ID, b2i(m.IsFromMe), m.Kind, m.Text, m.QuotedMessageID, m.SentAt.Unix(),
		nullUnix(m.EditedAt), b2i(m.Deleted), m.Status, string(rj), time.Now().Unix())
```

`DO NOTHING` already makes this insert-only.

- [ ] **Step 7: Run the tests to verify they pass**

Run: `go test ./internal/store/ -run 'StampsStoredAt|RetentionColumn' -v`
Expected: PASS.

- [ ] **Step 8: Run the full suite**

Run: `go test ./...`
Expected: PASS. Watch for tests asserting on exact column counts or `SELECT *`.

- [ ] **Step 9: Commit**

```bash
git add internal/store/schema.go internal/store/store.go internal/store/chat.go internal/store/testing.go internal/store/store_test.go
git commit -m "feat(store): retention columns and ingestion timestamps"
```

---

### Task 2: Store — eviction primitives and the sync re-upsert guard

**Files:**
- Modify: `internal/store/store.go` (add `EvictEmailContent`; modify `UpsertEmail`'s `ON CONFLICT` clause at `:460-475`)
- Modify: `internal/store/chat.go` (add `EvictChatMessageContent`)
- Test: `internal/store/store_test.go`

**Interfaces:**
- Consumes: the columns from Task 1.
- Produces:
  - `func (s *Store) EvictEmailContent(accountID, id string, at time.Time) error`
  - `func (s *Store) EvictChatMessageContent(accountID, id string, at time.Time) error`

  Both are idempotent — a second call on an evicted row is a no-op and returns nil.

- [ ] **Step 1: Write the failing test**

```go
// Eviction blanks content and participants but keeps every column sync depends
// on. EmailExists in particular is the only thing stopping a resync from
// re-firing mail_received for the whole mailbox.
func TestEvictEmailContentBlanksContentAndKeepsEnvelope(t *testing.T) {
	s := newTestStore(t)
	acct := seedAccount(t, s)
	if err := s.UpsertEmail(model.Email{
		AccountID: acct, ID: "m1", ThreadID: "t1", FolderID: "f1",
		Subject: "quarterly numbers", Snippet: "here they are",
		From: model.Recipient{Name: "Alice", Email: "alice@example.com"},
		To:   []model.Recipient{{Name: "Bob", Email: "bob@example.com"}},
		Body: "<p>secret</p>", BodyType: "html", Date: time.Now().UTC(),
		Read: true, HasAttachments: true, InternetMessageID: "<abc@example.com>",
		Attachments: []model.Attachment{{ID: "a1", Name: "payroll.xlsx"}},
	}); err != nil {
		t.Fatal(err)
	}

	if err := s.EvictEmailContent(acct, "m1", time.Now()); err != nil {
		t.Fatal(err)
	}

	got, err := s.GetEmail(acct, "m1")
	if err != nil {
		t.Fatal(err)
	}
	for name, v := range map[string]string{
		"subject": got.Subject, "snippet": got.Snippet, "body": got.Body,
		"body_type": got.BodyType, "from.name": got.From.Name, "from.email": got.From.Email,
	} {
		if v != "" {
			t.Errorf("%s = %q after eviction, want empty", name, v)
		}
	}
	if len(got.To) != 0 || len(got.Attachments) != 0 {
		t.Errorf("to=%v attachments=%v after eviction, want both empty", got.To, got.Attachments)
	}
	if got.ThreadID != "t1" || got.FolderID != "f1" || got.InternetMessageID != "<abc@example.com>" {
		t.Errorf("envelope lost: thread=%q folder=%q imid=%q", got.ThreadID, got.FolderID, got.InternetMessageID)
	}
	if !got.Read || !got.HasAttachments {
		t.Errorf("flags lost: read=%v has_attachments=%v", got.Read, got.HasAttachments)
	}
	// The invariant that keeps a resync from replaying the mailbox.
	if ok, err := s.EmailExists(acct, "m1"); err != nil || !ok {
		t.Fatalf("EmailExists = %v, %v after eviction; want true (a resync would re-fire mail_received)", ok, err)
	}
}

func TestEvictEmailContentIsIdempotent(t *testing.T) {
	s := newTestStore(t)
	acct := seedAccount(t, s)
	if err := s.UpsertEmail(model.Email{AccountID: acct, ID: "m1", Subject: "x", Body: "y", Date: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	first := time.Now().Add(-time.Hour).UTC()
	if err := s.EvictEmailContent(acct, "m1", first); err != nil {
		t.Fatal(err)
	}
	if err := s.EvictEmailContent(acct, "m1", time.Now()); err != nil {
		t.Fatal(err)
	}
	var at int64
	if err := s.DB().QueryRow(s.Q(`SELECT content_evicted_at FROM emails WHERE account_id = ? AND id = ?`), acct, "m1").Scan(&at); err != nil {
		t.Fatal(err)
	}
	if at != first.Unix() {
		t.Fatalf("content_evicted_at = %d, want the first eviction %d", at, first.Unix())
	}
}

// The guard that matters most: sync runs unattended, for every message,
// forever. Without it a resync refills every evicted row.
func TestUpsertEmailDoesNotRefillEvictedRow(t *testing.T) {
	s := newTestStore(t)
	acct := seedAccount(t, s)
	full := model.Email{
		AccountID: acct, ID: "m1", Subject: "quarterly numbers",
		From: model.Recipient{Email: "alice@example.com"},
		To:   []model.Recipient{{Email: "bob@example.com"}},
		Snippet: "here they are", Body: "<p>secret</p>", BodyType: "html",
		Date: time.Now().UTC(), Attachments: []model.Attachment{{ID: "a1", Name: "payroll.xlsx"}},
	}
	if err := s.UpsertEmail(full); err != nil {
		t.Fatal(err)
	}
	if err := s.EvictEmailContent(acct, "m1", time.Now()); err != nil {
		t.Fatal(err)
	}

	// A resync hands us the whole message again.
	full.Read = true
	if err := s.UpsertEmail(full); err != nil {
		t.Fatal(err)
	}

	got, err := s.GetEmail(acct, "m1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Body != "" || got.Subject != "" || got.From.Email != "" || len(got.Attachments) != 0 {
		t.Fatalf("resync refilled an evicted row: subject=%q body=%q from=%q atts=%d",
			got.Subject, got.Body, got.From.Email, len(got.Attachments))
	}
	// Flags are not content and must still track the provider.
	if !got.Read {
		t.Error("read flag did not update on an evicted row; flags are not content")
	}
}

func TestEvictChatMessageContentBlanksTextOnly(t *testing.T) {
	s := newTestStore(t)
	acct := seedChatAccount(t, s)
	if err := s.UpsertChat(model.Chat{AccountID: acct, ID: "c1", Kind: "dm"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertChatMessage(model.ChatMessage{
		AccountID: acct, ID: "cm1", ChatID: "c1", Kind: "text",
		Text: "meet me at six", SentAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.EvictChatMessageContent(acct, "cm1", time.Now()); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetChatMessage(acct, "cm1")
	if err != nil {
		t.Fatalf("GetChatMessage after eviction: %v (a late reaction must still resolve)", err)
	}
	if got.Text != "" {
		t.Errorf("text = %q after eviction, want empty", got.Text)
	}
	if got.ChatID != "c1" || got.Kind != "text" {
		t.Errorf("envelope lost: chat=%q kind=%q", got.ChatID, got.Kind)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/store/ -run 'Evict|DoesNotRefill' -v`
Expected: FAIL — `s.EvictEmailContent undefined`.

- [ ] **Step 3: Add `EvictEmailContent`**

Add to `internal/store/store.go`, next to `UpsertEmail`:

```go
// EvictEmailContent blanks a message's content and its participants, keeping
// the identifiers, timestamps and flags that sync depends on — EmailExists in
// particular, which is the only thing stopping a resync from re-firing
// mail_received for a mailbox we have already delivered.
//
// The `content_evicted_at IS NULL` clause makes it idempotent and preserves the
// original eviction time, so a second call cannot quietly extend the audit
// trail forward.
func (s *Store) EvictEmailContent(accountID, id string, at time.Time) error {
	defer s.trace("EvictEmailContent", time.Now(), "account_id", accountID, "email_id", id)
	_, err := s.db.Exec(s.q(`
		UPDATE emails SET
		  subject = '', snippet = '', body = '', body_type = '',
		  from_name = '', from_email = '',
		  to_json = '[]', cc_json = '[]', bcc_json = '[]', reply_to_json = '[]',
		  attachments_json = '[]',
		  content_evicted_at = ?
		WHERE account_id = ? AND id = ? AND content_evicted_at IS NULL`),
		at.Unix(), accountID, id)
	return err
}
```

- [ ] **Step 4: Add `EvictChatMessageContent`**

Add to `internal/store/chat.go`, next to `RevokeChatMessage`:

```go
// EvictChatMessageContent blanks a chat message's text. Unlike mail, the
// participants are not on this row — they live in `attendees`, which is
// address-book state and out of scope. The row itself survives so a late
// reaction or receipt still resolves through GetChatMessage.
func (s *Store) EvictChatMessageContent(accountID, id string, at time.Time) error {
	_, err := s.db.Exec(s.q(`
		UPDATE chat_messages SET text = '', content_evicted_at = ?
		WHERE account_id = ? AND id = ? AND content_evicted_at IS NULL`),
		at.Unix(), accountID, id)
	return err
}
```

- [ ] **Step 5: Guard `UpsertEmail`'s conflict clause**

Replace the `ON CONFLICT` body in `internal/store/store.go` with this. Every content and participant column now checks `content_evicted_at` first; the flag columns deliberately do not, because read/flagged/draft state is not content and must keep tracking the provider:

```go
		ON CONFLICT(account_id, id) DO UPDATE SET
		  thread_id = excluded.thread_id, folder_id = excluded.folder_id,
		  -- Content and participants freeze once evicted. A resync, or any
		  -- delta carrying full message properties, would otherwise refill a
		  -- row the tenant's retention policy has already emptied — silently,
		  -- unattended, for every message.
		  subject       = CASE WHEN emails.content_evicted_at IS NOT NULL THEN emails.subject       ELSE excluded.subject END,
		  from_name     = CASE WHEN emails.content_evicted_at IS NOT NULL THEN emails.from_name     ELSE excluded.from_name END,
		  from_email    = CASE WHEN emails.content_evicted_at IS NOT NULL THEN emails.from_email    ELSE excluded.from_email END,
		  to_json       = CASE WHEN emails.content_evicted_at IS NOT NULL THEN emails.to_json       ELSE excluded.to_json END,
		  cc_json       = CASE WHEN emails.content_evicted_at IS NOT NULL THEN emails.cc_json       ELSE excluded.cc_json END,
		  bcc_json      = CASE WHEN emails.content_evicted_at IS NOT NULL THEN emails.bcc_json      ELSE excluded.bcc_json END,
		  reply_to_json = CASE WHEN emails.content_evicted_at IS NOT NULL THEN emails.reply_to_json ELSE excluded.reply_to_json END,
		  snippet       = CASE WHEN emails.content_evicted_at IS NOT NULL THEN emails.snippet       ELSE excluded.snippet END,
		  date = excluded.date,
		  -- a delta "updated" event may carry only changed fields; never blank a
		  -- body we already have just because this page omitted it
		  body = CASE WHEN emails.content_evicted_at IS NOT NULL THEN emails.body
		              WHEN excluded.body = '' THEN emails.body ELSE excluded.body END,
		  body_type = CASE WHEN emails.content_evicted_at IS NOT NULL THEN emails.body_type
		                   WHEN excluded.body_type = '' THEN emails.body_type ELSE excluded.body_type END,
		  read = excluded.read, flagged = excluded.flagged, draft = excluded.draft,
		  has_attachments = excluded.has_attachments,
		  internet_message_id = excluded.internet_message_id,
		  attachments_json = CASE WHEN emails.content_evicted_at IS NOT NULL THEN emails.attachments_json
		                          WHEN excluded.attachments_json = '[]' THEN emails.attachments_json
		                          ELSE excluded.attachments_json END`),
```

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test ./internal/store/ -run 'Evict|DoesNotRefill' -v`
Expected: PASS.

- [ ] **Step 7: Run the full suite**

Run: `go test ./...`
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/store/store.go internal/store/chat.go internal/store/store_test.go
git commit -m "feat(store): content eviction primitives, guarded against sync refill"
```

---

### Task 3: Model and scanners — surface `content_evicted`

**Files:**
- Modify: `internal/model/model.go:63-100` (`Email`)
- Modify: `internal/model/chat.go:54-68` (`ChatMessage`)
- Modify: `internal/store/store.go:595-599` (`emailSelect`), `:601-626` (`scanEmail`)
- Modify: `internal/store/chat.go:306-311` (`chatMessageSelect`), `:463-492` (`scanChatMessage`)
- Test: `internal/store/store_test.go`

**Interfaces:**
- Consumes: `EvictEmailContent`, `EvictChatMessageContent` from Task 2.
- Produces: `model.Email.ContentEvicted bool` and `model.ChatMessage.ContentEvicted bool`, both JSON `content_evicted`, set by the two scanners from whether `content_evicted_at` is non-NULL.

**The flag is negative on purpose.** `model.Email` is built in the Outlook adapter, in the syncer and in API handlers, none of which know anything about retention. A positive `ContentAvailable bool` would default to `false` at every one of those sites, so every webhook would announce that the content it was carrying had been destroyed. `ContentEvicted` is correct at its zero value everywhere.

- [ ] **Step 1: Write the failing test**

```go
func TestGetEmailReportsContentEvicted(t *testing.T) {
	s := newTestStore(t)
	acct := seedAccount(t, s)
	if err := s.UpsertEmail(model.Email{AccountID: acct, ID: "m1", Subject: "x", Body: "y", Date: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetEmail(acct, "m1")
	if err != nil {
		t.Fatal(err)
	}
	if got.ContentEvicted {
		t.Error("ContentEvicted = true on a fresh message, want false")
	}
	if err := s.EvictEmailContent(acct, "m1", time.Now()); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetEmail(acct, "m1")
	if err != nil {
		t.Fatal(err)
	}
	if !got.ContentEvicted {
		t.Error("ContentEvicted = false after eviction, want true")
	}
}

// The list form needs the flag too: list responses already omit the body, so
// without it a client cannot tell "not included here" from "destroyed".
func TestListEmailsReportsContentEvicted(t *testing.T) {
	s := newTestStore(t)
	acct := seedAccount(t, s)
	if err := s.UpsertEmail(model.Email{AccountID: acct, ID: "m1", Subject: "x", Body: "y", Date: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if err := s.EvictEmailContent(acct, "m1", time.Now()); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListEmails(EmailQuery{AccountID: acct})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !got[0].ContentEvicted {
		t.Fatalf("ListEmails = %+v, want one row with ContentEvicted true", got)
	}
}

// Eviction blanks subject, snippet and from_email — the three columns
// ListEmails searches (store.go:537) — so an evicted message can never be a
// search hit again. That is correct (there is nothing left to match) but
// surprising, so it is pinned here rather than discovered in production.
func TestSearchDoesNotMatchEvictedMail(t *testing.T) {
	s := newTestStore(t)
	acct := seedAccount(t, s)
	if err := s.UpsertEmail(model.Email{
		AccountID: acct, ID: "m1", Subject: "quarterly numbers",
		Snippet: "here they are", From: model.Recipient{Email: "alice@example.com"},
		Body: "<p>secret</p>", Date: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListEmails(EmailQuery{AccountID: acct, Search: "quarterly"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("baseline search returned %d rows, want 1", len(got))
	}

	if err := s.EvictEmailContent(acct, "m1", time.Now()); err != nil {
		t.Fatal(err)
	}
	got, err = s.ListEmails(EmailQuery{AccountID: acct, Search: "quarterly"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("search returned %d rows for an evicted message, want 0", len(got))
	}
}

func TestGetChatMessageReportsContentEvicted(t *testing.T) {
	s := newTestStore(t)
	acct := seedChatAccount(t, s)
	if err := s.UpsertChat(model.Chat{AccountID: acct, ID: "c1", Kind: "dm"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertChatMessage(model.ChatMessage{
		AccountID: acct, ID: "cm1", ChatID: "c1", Kind: "text", Text: "hi", SentAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.EvictChatMessageContent(acct, "cm1", time.Now()); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetChatMessage(acct, "cm1")
	if err != nil {
		t.Fatal(err)
	}
	if !got.ContentEvicted {
		t.Error("ContentEvicted = false after eviction, want true")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/store/ -run 'ReportsContentEvicted|SearchDoesNotMatch' -v`
Expected: FAIL — `got.ContentEvicted undefined`.

- [ ] **Step 3: Add the model fields**

In `internal/model/model.go`, in `Email`, after `HasAttachments`:

```go
	// ContentEvicted reports that this message's content and participants were
	// removed by the owning developer's retention policy. The flag is negative
	// so its zero value is correct at the many sites that build an Email
	// without going through the store — a provider adapter, the syncer, an API
	// handler — none of which know about retention.
	ContentEvicted bool `json:"content_evicted"`
```

In `internal/model/chat.go`, in `ChatMessage`, after `Status`:

```go
	// ContentEvicted reports that this message's text was removed by the
	// owning developer's retention policy. See model.Email.ContentEvicted for
	// why the flag is negative.
	ContentEvicted bool `json:"content_evicted"`
```

- [ ] **Step 4: Thread it through the mail scanner**

`internal/store/store.go`, `emailSelect` gains the column as the last one:

```go
const emailSelect = `
SELECT account_id, id, thread_id, folder_id, subject, from_name, from_email,
       to_json, cc_json, bcc_json, reply_to_json, date, snippet, body, body_type,
       read, flagged, draft, has_attachments, internet_message_id, attachments_json,
       content_evicted_at
FROM emails`
```

and `scanEmail` scans it:

```go
func scanEmail(r scanner) (model.Email, error) {
	var e model.Email
	var toJ, ccJ, bccJ, rtJ, attJ string
	var date int64
	var read, flagged, draft, hasAtt int
	var evictedAt sql.NullInt64
	err := r.Scan(&e.AccountID, &e.ID, &e.ThreadID, &e.FolderID, &e.Subject,
		&e.From.Name, &e.From.Email, &toJ, &ccJ, &bccJ, &rtJ, &date, &e.Snippet,
		&e.Body, &e.BodyType, &read, &flagged, &draft, &hasAtt,
		&e.InternetMessageID, &attJ, &evictedAt)
	// ...unchanged through the json.Unmarshal block and BodyPlain...
	e.Read, e.Flagged, e.Draft, e.HasAttachments = read == 1, flagged == 1, draft == 1, hasAtt == 1
	e.ContentEvicted = evictedAt.Valid
	return e, nil
}
```

- [ ] **Step 5: Thread it through the chat scanner**

`internal/store/chat.go`, `chatMessageSelect` gains `m.content_evicted_at` as the last column:

```go
const chatMessageSelect = `
	SELECT m.account_id, m.id, m.chat_id, m.sender_id, COALESCE(a.phone, ''), COALESCE(a.name, ''),
	       COALESCE(a.is_self, 0), m.is_from_me, m.kind, m.text, m.quoted_id, m.sent_at, m.edited_at,
	       m.deleted, m.status, m.reactions_json, m.content_evicted_at
	FROM chat_messages m
	LEFT JOIN attendees a ON a.account_id = m.account_id AND a.id = m.sender_id`
```

and `scanChatMessage`:

```go
	var evictedAt sql.NullInt64
	err := r.Scan(&m.AccountID, &m.ID, &m.ChatID, &m.Sender.ID, &m.Sender.Phone, &m.Sender.Name, &senderIsSelf,
		&isFromMe, &m.Kind, &m.Text, &m.QuotedMessageID,
		&sentAt, &editedAt, &deleted, &m.Status, &reactionsJSON, &evictedAt)
```

and before `return m, nil`:

```go
	m.ContentEvicted = evictedAt.Valid
```

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test ./internal/store/ -run 'ReportsContentEvicted|SearchDoesNotMatch' -v`
Expected: PASS.

- [ ] **Step 7: Run the full suite**

Run: `go test ./...`
Expected: PASS. Any test comparing a whole `model.Email` with `==` or `reflect.DeepEqual` against a literal now needs the new field; it defaults to `false`, which is correct.

- [ ] **Step 8: Commit**

```bash
git add internal/model/ internal/store/store.go internal/store/chat.go internal/store/store_test.go
git commit -m "feat(model): content_evicted on Email and ChatMessage"
```

---

### Task 4: Store — read and write the developer's retention policy

**Files:**
- Modify: `internal/model/model.go:220-231` (`Developer`)
- Modify: `internal/store/developers.go:56`, `:73`, `:180`, `:253` (the four developer SELECTs), and add two functions
- Test: `internal/store/store_test.go`

**Interfaces:**
- Consumes: `developers.retention_max_age_secs` from Task 1.
- Produces:
  - `model.Developer.RetentionMaxAgeSecs int64` (JSON `retention_max_age_secs`)
  - `func (s *Store) SetRetentionMaxAge(developerID string, secs int64) error`
  - `func (s *Store) RetentionMaxAge(developerID string) (time.Duration, error)` — returns `0` when retention is off; used by Task 7 on the delivery hot path.

- [ ] **Step 1: Write the failing test**

```go
func TestSetAndReadRetentionMaxAge(t *testing.T) {
	s := newTestStore(t)
	dev := seedDeveloper(t, s, "dev_1", "dev1@example.com")

	d, err := s.RetentionMaxAge(dev)
	if err != nil {
		t.Fatal(err)
	}
	if d != 0 {
		t.Fatalf("RetentionMaxAge = %v on a new developer, want 0 (retention off)", d)
	}

	if err := s.SetRetentionMaxAge(dev, 3600); err != nil {
		t.Fatal(err)
	}
	d, err = s.RetentionMaxAge(dev)
	if err != nil {
		t.Fatal(err)
	}
	if d != time.Hour {
		t.Fatalf("RetentionMaxAge = %v, want 1h", d)
	}

	got, err := s.GetDeveloper(dev)
	if err != nil {
		t.Fatal(err)
	}
	if got.RetentionMaxAgeSecs != 3600 {
		t.Fatalf("GetDeveloper().RetentionMaxAgeSecs = %d, want 3600", got.RetentionMaxAgeSecs)
	}
}

func TestRetentionMaxAgeUnknownDeveloper(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.RetentionMaxAge("dev_missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("RetentionMaxAge for an unknown developer = %v, want ErrNotFound", err)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/store/ -run 'RetentionMaxAge' -v`
Expected: FAIL — `s.RetentionMaxAge undefined`.

- [ ] **Step 3: Add the model field**

In `internal/model/model.go`, in `Developer`, after `RedirectDomains`:

```go
	// RetentionMaxAgeSecs is how long message content may sit in the local
	// mirror. 0 means keep forever, which is the default and today's
	// behaviour; any positive value also turns on eviction-on-delivery.
	RetentionMaxAgeSecs int64 `json:"retention_max_age_secs"`
```

- [ ] **Step 4: Add the column to all four developer SELECTs**

In `internal/store/developers.go`, append `retention_max_age_secs` to the column list of each query below and add `&d.RetentionMaxAgeSecs` (or the local equivalent) in the matching `Scan`:

- `:56` — lookup by email (`SELECT id, email, name, password_hash, created_at, redirect_domains_json FROM developers WHERE email = ?`)
- `:73` — `GetDeveloper`
- `:180` — `SessionDeveloper` (qualify it as `d.retention_max_age_secs`); this is the one the dashboard reads, so missing it makes the setting invisible in the UI
- `:253` — the API-key join (qualify as `d.retention_max_age_secs`); this is the one API-key requests read

Each is `SELECT`-then-`Scan`, so the column must be added in the same position in both.

- [ ] **Step 5: Add the two accessors**

Add to `internal/store/developers.go`:

```go
// SetRetentionMaxAge sets how long message content may be retained for this
// developer's accounts. 0 turns retention off.
func (s *Store) SetRetentionMaxAge(developerID string, secs int64) error {
	res, err := s.db.Exec(s.q(`UPDATE developers SET retention_max_age_secs = ? WHERE id = ?`), secs, developerID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// RetentionMaxAge is the single-column read on the delivery hot path: the
// dispatcher asks it once per delivered event to decide whether to evict.
// A zero duration means retention is off.
func (s *Store) RetentionMaxAge(developerID string) (time.Duration, error) {
	var secs int64
	err := s.db.QueryRow(s.q(`SELECT retention_max_age_secs FROM developers WHERE id = ?`), developerID).Scan(&secs)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	return time.Duration(secs) * time.Second, nil
}
```

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test ./internal/store/ -run 'RetentionMaxAge' -v`
Expected: PASS.

- [ ] **Step 7: Run the full suite**

Run: `go test ./...`
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/model/model.go internal/store/developers.go internal/store/store_test.go
git commit -m "feat(store): per-developer retention policy accessors"
```

---

### Task 5: The max-age sweep

**Files:**
- Create: `internal/store/retention.go`
- Modify: `cmd/server/main.go:83-100` (the hourly purge loop)
- Test: `internal/store/retention_test.go`

**Interfaces:**
- Consumes: everything from Tasks 1–4.
- Produces: `func (s *Store) EvictExpiredContent(now time.Time) (int64, error)` — returns the number of rows evicted across both tables.

- [ ] **Step 1: Write the failing test**

Create `internal/store/retention_test.go`:

```go
package store

import (
	"testing"
	"time"

	"github.com/gauravrautela/unified-messaging/internal/model"
)

// storeEmailAgedDays writes a message and backdates its stored_at, which is the
// only clock the sweep looks at. The message's own Date is deliberately recent,
// so a sweep that keyed on the provider timestamp instead would fail this test.
func storeEmailAgedDays(t *testing.T, s *Store, acct, id string, days int) {
	t.Helper()
	if err := s.UpsertEmail(model.Email{
		AccountID: acct, ID: id, Subject: "subject " + id, Body: "body " + id,
		From: model.Recipient{Email: "alice@example.com"}, Date: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	aged := time.Now().Add(-time.Duration(days) * 24 * time.Hour).Unix()
	if _, err := s.DB().Exec(s.Q(`UPDATE emails SET stored_at = ? WHERE account_id = ? AND id = ?`), aged, acct, id); err != nil {
		t.Fatal(err)
	}
}

func TestEvictExpiredContentRespectsThePolicyWindow(t *testing.T) {
	s := newTestStore(t)
	acct := seedAccount(t, s)
	storeEmailAgedDays(t, s, acct, "old", 10)
	storeEmailAgedDays(t, s, acct, "new", 1)

	if err := s.SetRetentionMaxAge("dev_1", int64((7 * 24 * time.Hour).Seconds())); err != nil {
		t.Fatal(err)
	}
	n, err := s.EvictExpiredContent(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("EvictExpiredContent evicted %d rows, want 1 (only the 10-day-old one)", n)
	}

	old, err := s.GetEmail(acct, "old")
	if err != nil {
		t.Fatal(err)
	}
	if !old.ContentEvicted || old.Body != "" {
		t.Errorf("old message not evicted: evicted=%v body=%q", old.ContentEvicted, old.Body)
	}
	fresh, err := s.GetEmail(acct, "new")
	if err != nil {
		t.Fatal(err)
	}
	if fresh.ContentEvicted || fresh.Body == "" {
		t.Errorf("message inside the window was evicted: evicted=%v body=%q", fresh.ContentEvicted, fresh.Body)
	}
}

func TestEvictExpiredContentIsANoOpWhenRetentionIsOff(t *testing.T) {
	s := newTestStore(t)
	acct := seedAccount(t, s)
	storeEmailAgedDays(t, s, acct, "ancient", 4000)

	n, err := s.EvictExpiredContent(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("evicted %d rows with retention off, want 0", n)
	}
	got, err := s.GetEmail(acct, "ancient")
	if err != nil {
		t.Fatal(err)
	}
	if got.Body == "" {
		t.Error("content evicted although the developer has no retention policy")
	}
}

// The sweep is the only trigger a developer with no webhooks ever gets, and it
// must stay inside its own tenant.
func TestEvictExpiredContentIsScopedToTheDeveloper(t *testing.T) {
	s := newTestStore(t)
	acctA := seedAccount(t, s) // dev_1 / acc_1
	seedDeveloper(t, s, "dev_2", "dev2@example.com")
	if err := s.UpsertAccount(model.Account{
		ID: "acc_2", DeveloperID: "dev_2", Provider: "OUTLOOK",
		Email: "other@outlook.com", Status: model.AccountOK,
	}); err != nil {
		t.Fatal(err)
	}
	storeEmailAgedDays(t, s, acctA, "a", 10)
	storeEmailAgedDays(t, s, "acc_2", "b", 10)

	if err := s.SetRetentionMaxAge("dev_1", 3600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EvictExpiredContent(time.Now()); err != nil {
		t.Fatal(err)
	}

	other, err := s.GetEmail("acc_2", "b")
	if err != nil {
		t.Fatal(err)
	}
	if other.ContentEvicted {
		t.Error("evicted another developer's message")
	}
}

func TestEvictExpiredContentSweepsChatMessages(t *testing.T) {
	s := newTestStore(t)
	acct := seedChatAccount(t, s)
	if err := s.UpsertChat(model.Chat{AccountID: acct, ID: "c1", Kind: "dm"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertChatMessage(model.ChatMessage{
		AccountID: acct, ID: "cm1", ChatID: "c1", Kind: "text", Text: "old news", SentAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	aged := time.Now().Add(-10 * 24 * time.Hour).Unix()
	if _, err := s.DB().Exec(s.Q(`UPDATE chat_messages SET stored_at = ? WHERE account_id = ? AND id = ?`), aged, acct, "cm1"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetRetentionMaxAge("dev_1", int64((24 * time.Hour).Seconds())); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EvictExpiredContent(time.Now()); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetChatMessage(acct, "cm1")
	if err != nil {
		t.Fatal(err)
	}
	if !got.ContentEvicted || got.Text != "" {
		t.Fatalf("chat message not evicted: evicted=%v text=%q", got.ContentEvicted, got.Text)
	}
}
```

`seedChatAccount` (`store_test.go:776`) seeds developer `dev_1`, the same tenant as `seedAccount`.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/store/ -run 'EvictExpiredContent' -v`
Expected: FAIL — `s.EvictExpiredContent undefined`.

- [ ] **Step 3: Write the sweep**

Create `internal/store/retention.go`:

```go
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
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/store/ -run 'EvictExpiredContent' -v`
Expected: PASS.

- [ ] **Step 5: Wire the sweep into the hourly loop**

In `cmd/server/main.go`, inside the `case <-t.C:` block, after the `PurgeDeadDeliveries` logging:

```go
				evicted, err := db.EvictExpiredContent(time.Now())
				if err != nil {
					log.Error("evicting expired content", "err", err)
					continue
				}
				log.Info("retention sweep", "evicted", evicted)
```

Do **not** add it to the boot-time purge block at `:69-70`. The sweep is not urgent, and leaving it out of startup keeps boot fast and avoids a surprise mass eviction in the first second after an operator sets a policy.

- [ ] **Step 6: Verify it builds and the suite passes**

Run: `go build ./... && go test ./...`
Expected: both succeed.

- [ ] **Step 7: Commit**

```bash
git add internal/store/retention.go internal/store/retention_test.go cmd/server/main.go
git commit -m "feat(store): hourly max-age content sweep"
```

---

### Task 6: Cap dead deliveries at the tenant's policy

**Files:**
- Modify: `internal/store/aux.go:313-325` (`PurgeDeadDeliveries`)
- Modify: `cmd/server/main.go:70`, `:93` (both call sites)
- Modify: `internal/store/store_test.go:1301` (the existing test's call)
- Test: `internal/store/retention_test.go`

**Interfaces:**
- Consumes: `developers.retention_max_age_secs`.
- Produces: `func (s *Store) PurgeDeadDeliveries(now time.Time, global time.Duration) (int64, error)` — **signature change**. A row is deleted when it is older than `global`, or older than its own developer's retention policy when that policy is shorter.

A dead delivery holds the full serialized event, body included (`aux.go:279`). Without this, a tenant whose policy says one hour still has bodies on disk for the global seven days, and the policy is a false claim.

- [ ] **Step 1: Write the failing test**

Add to `internal/store/retention_test.go`:

```go
func seedDeadDelivery(t *testing.T, s *Store, id, webhookID, accountID string, ageDays int) {
	t.Helper()
	if err := s.SaveDelivery(Delivery{
		ID: id, WebhookID: webhookID, AccountID: accountID, EventType: "mail_received",
		Payload: []byte(`{"type":"mail_received"}`), Attempts: 8, Dead: true,
		CreatedAt: time.Now().Add(-time.Duration(ageDays) * 24 * time.Hour).UTC(),
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPurgeDeadDeliveriesHonoursAShorterTenantPolicy(t *testing.T) {
	s := newTestStore(t)
	acct := seedAccount(t, s)
	hook := model.Webhook{ID: "wh_1", DeveloperID: "dev_1", AccountID: acct, URL: "https://example.com/hook", Events: []string{"*"}}
	if err := s.SaveWebhook(hook); err != nil {
		t.Fatal(err)
	}
	seedDeadDelivery(t, s, "dl_1", "wh_1", acct, 2)

	// Global cutoff is 7 days: on its own this row survives.
	n, err := s.PurgeDeadDeliveries(time.Now(), 7*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("purged %d rows under the global cutoff alone, want 0", n)
	}

	// The tenant says one hour. The two-day-old dead delivery must go.
	if err := s.SetRetentionMaxAge("dev_1", 3600); err != nil {
		t.Fatal(err)
	}
	n, err = s.PurgeDeadDeliveries(time.Now(), 7*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("purged %d rows under a 1h tenant policy, want 1", n)
	}
}

// A live delivery is still retrying and must never be purged, whatever the
// policy says — the payload is the only copy the retry has.
func TestPurgeDeadDeliveriesLeavesLiveRowsAlone(t *testing.T) {
	s := newTestStore(t)
	acct := seedAccount(t, s)
	if err := s.SaveWebhook(model.Webhook{ID: "wh_1", DeveloperID: "dev_1", AccountID: acct, URL: "https://example.com/hook", Events: []string{"*"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveDelivery(Delivery{
		ID: "dl_live", WebhookID: "wh_1", AccountID: acct, EventType: "mail_received",
		Payload: []byte(`{}`), Attempts: 2, Dead: false,
		CreatedAt: time.Now().Add(-30 * 24 * time.Hour).UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetRetentionMaxAge("dev_1", 60); err != nil {
		t.Fatal(err)
	}
	n, err := s.PurgeDeadDeliveries(time.Now(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("purged %d live deliveries, want 0", n)
	}
}
```

Check the exact field names of `store.Delivery` (`aux.go:260-270`) and `model.Webhook` before running, and adjust the literals if they differ.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/store/ -run 'PurgeDeadDeliveries' -v`
Expected: FAIL — too many arguments to `PurgeDeadDeliveries`.

- [ ] **Step 3: Rewrite `PurgeDeadDeliveries`**

Replace it in `internal/store/aux.go`:

```go
// PurgeDeadDeliveries removes abandoned deliveries. A dead delivery keeps the
// full message payload it failed to send, so leaving them forever is an
// unbounded (and increasingly sensitive) amount of retained mail/chat content.
// Live deliveries — still retrying — are never touched: that payload is the
// only copy the retry has.
//
// Two cutoffs apply, whichever is shorter: the deployment-wide `global`
// window, and the owning developer's own retention policy. Without the second,
// a tenant who asked us to hold content for an hour would still have bodies on
// disk a week later, inside a dead delivery, and their policy would be a false
// claim.
func (s *Store) PurgeDeadDeliveries(now time.Time, global time.Duration) (int64, error) {
	res, err := s.db.Exec(s.q(`
		DELETE FROM webhook_deliveries
		WHERE dead = 1 AND (
		  created_at < ?
		  OR EXISTS (
		    SELECT 1 FROM webhooks w JOIN developers d ON d.id = w.developer_id
		    WHERE w.id = webhook_deliveries.webhook_id
		      AND d.retention_max_age_secs > 0
		      AND ? - webhook_deliveries.created_at > d.retention_max_age_secs))`),
		now.Add(-global).Unix(), now.Unix())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
```

- [ ] **Step 4: Update the three call sites**

`cmd/server/main.go:70`:

```go
	db.PurgeDeadDeliveries(time.Now(), cfg.DeliveryRetention)
```

`cmd/server/main.go:93`:

```go
				n, err := db.PurgeDeadDeliveries(time.Now(), cfg.DeliveryRetention)
```

`internal/store/store_test.go:1301`:

```go
	n, err := s.PurgeDeadDeliveries(now, 7*24*time.Hour)
```

That existing test builds rows relative to a fixed `now`; passing the same `now` with a 7-day window preserves its meaning exactly.

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/store/ -run 'PurgeDeadDeliveries' -v`
Expected: PASS, including the pre-existing `TestPurgeDeadDeliveriesRemovesRowsOlderThanCutoff`.

- [ ] **Step 6: Build and run the full suite**

Run: `go build ./... && go test ./...`
Expected: both succeed.

- [ ] **Step 7: Commit**

```bash
git add internal/store/aux.go internal/store/retention_test.go internal/store/store_test.go cmd/server/main.go
git commit -m "feat(store): cap dead deliveries at the tenant's retention policy"
```

---

### Task 7: Evict on delivery

**Files:**
- Modify: `internal/events/events.go:225-284` (`deliver`), and add `evictDelivered`
- Test: `internal/events/events_test.go`

**Interfaces:**
- Consumes: `store.RetentionMaxAge`, `store.EvictEmailContent`, `store.EvictChatMessageContent`.
- Produces: no new exported API. `deliver` evicts the event's message when every subscribing hook accepted it on the first attempt.

The three cases, all from one condition — see spec §4:

| Situation | Result |
|---|---|
| Every hook accepted first try | Evicted immediately |
| Any hook enqueued a retry | Not evicted; the max-age sweep handles it |
| No hook matched | Not evicted; max-age only |

That third row is the reason this lives in `deliver` rather than being reconstructed later from the delivery table. A developer with no webhooks has never forwarded anything, and inferring "delivered" from the *absence* of a delivery row would destroy content they never received — irreversibly, since WhatsApp has no history API.

- [ ] **Step 1: Write the failing test**

Add to `internal/events/events_test.go`. Mirror the fixture style already in the file (`seedTenant`, `newReceiver`); a webhook is registered with `db.SaveWebhook`, and a dispatcher is built the way the neighbouring tests build one.

```go
// seedEmail puts a message in the mirror so eviction has something to blank.
func seedEmail(t *testing.T, db *store.Store, accountID, id string) {
	t.Helper()
	if err := db.UpsertEmail(model.Email{
		AccountID: accountID, ID: id, Subject: "quarterly numbers",
		From: model.Recipient{Email: "alice@example.com"},
		Body: "<p>secret</p>", BodyType: "html", Date: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
}

func TestDeliverEvictsContentOnceEveryHookAccepts(t *testing.T) {
	db := newTestStore(t)
	seedTenant(t, db)
	seedEmail(t, db, "acc_1", "m1")
	if err := db.SetRetentionMaxAge("dev_1", 3600); err != nil {
		t.Fatal(err)
	}
	rcv := newReceiver(t, http.StatusOK)
	if err := db.SaveWebhook(model.Webhook{
		ID: "wh_1", DeveloperID: "dev_1", AccountID: "acc_1",
		URL: rcv.URL, Events: []string{"*"},
	}); err != nil {
		t.Fatal(err)
	}

	log, _ := logx.Capture()
	d := NewDispatcher(db, nil, log)
	email, err := db.GetEmail("acc_1", "m1")
	if err != nil {
		t.Fatal(err)
	}
	d.deliver(context.Background(), model.Event{
		Type: model.EventMailReceived, AccountID: "acc_1", Email: &email,
	})

	got, err := db.GetEmail("acc_1", "m1")
	if err != nil {
		t.Fatal(err)
	}
	if !got.ContentEvicted || got.Body != "" || got.From.Email != "" {
		t.Fatalf("not evicted after a clean delivery: evicted=%v body=%q from=%q",
			got.ContentEvicted, got.Body, got.From.Email)
	}
}

// A failed hook means the content may still be needed. The retry has its own
// payload snapshot, but the mirror stays until max-age.
func TestDeliverDoesNotEvictWhenAHookFails(t *testing.T) {
	db := newTestStore(t)
	seedTenant(t, db)
	seedEmail(t, db, "acc_1", "m1")
	if err := db.SetRetentionMaxAge("dev_1", 3600); err != nil {
		t.Fatal(err)
	}
	rcv := newReceiver(t, http.StatusInternalServerError)
	if err := db.SaveWebhook(model.Webhook{
		ID: "wh_1", DeveloperID: "dev_1", AccountID: "acc_1",
		URL: rcv.URL, Events: []string{"*"},
	}); err != nil {
		t.Fatal(err)
	}

	log, _ := logx.Capture()
	d := NewDispatcher(db, nil, log)
	email, err := db.GetEmail("acc_1", "m1")
	if err != nil {
		t.Fatal(err)
	}
	d.deliver(context.Background(), model.Event{
		Type: model.EventMailReceived, AccountID: "acc_1", Email: &email,
	})

	got, err := db.GetEmail("acc_1", "m1")
	if err != nil {
		t.Fatal(err)
	}
	if got.ContentEvicted {
		t.Fatal("evicted although the hook failed; the mirror must survive to max-age")
	}
}

// The zero-webhook trap: nothing was forwarded, so nothing may be destroyed.
func TestDeliverDoesNotEvictWithNoWebhooks(t *testing.T) {
	db := newTestStore(t)
	seedTenant(t, db)
	seedEmail(t, db, "acc_1", "m1")
	if err := db.SetRetentionMaxAge("dev_1", 3600); err != nil {
		t.Fatal(err)
	}

	log, _ := logx.Capture()
	d := NewDispatcher(db, nil, log)
	email, err := db.GetEmail("acc_1", "m1")
	if err != nil {
		t.Fatal(err)
	}
	d.deliver(context.Background(), model.Event{
		Type: model.EventMailReceived, AccountID: "acc_1", Email: &email,
	})

	got, err := db.GetEmail("acc_1", "m1")
	if err != nil {
		t.Fatal(err)
	}
	if got.ContentEvicted {
		t.Fatal("evicted content that was never forwarded anywhere")
	}
}

func TestDeliverDoesNotEvictWhenRetentionIsOff(t *testing.T) {
	db := newTestStore(t)
	seedTenant(t, db)
	seedEmail(t, db, "acc_1", "m1")
	rcv := newReceiver(t, http.StatusOK)
	if err := db.SaveWebhook(model.Webhook{
		ID: "wh_1", DeveloperID: "dev_1", AccountID: "acc_1",
		URL: rcv.URL, Events: []string{"*"},
	}); err != nil {
		t.Fatal(err)
	}

	log, _ := logx.Capture()
	d := NewDispatcher(db, nil, log)
	email, err := db.GetEmail("acc_1", "m1")
	if err != nil {
		t.Fatal(err)
	}
	d.deliver(context.Background(), model.Event{
		Type: model.EventMailReceived, AccountID: "acc_1", Email: &email,
	})

	got, err := db.GetEmail("acc_1", "m1")
	if err != nil {
		t.Fatal(err)
	}
	if got.ContentEvicted {
		t.Fatal("evicted although the developer has no retention policy")
	}
}
```

`logx.Capture()` returns `(*slog.Logger, *logx.Records)` and is what every other test in this file uses (`events_test.go:117`).

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/events/ -run 'DeliverEvicts|DeliverDoesNotEvict' -v`
Expected: FAIL — the first test fails because nothing evicts; the others pass trivially.

- [ ] **Step 3: Track match and failure in `deliver`**

In `internal/events/events.go`, inside `deliver`, declare the two locals just after `var wg sync.WaitGroup` / the deferred `wg.Wait()`:

```go
	// matched and failed are what makes eviction-on-delivery possible without
	// a schema change: at the end of this function we know both how many hooks
	// this event was actually sent to and whether every one of them accepted
	// it. Neither fact is recoverable afterwards — a hook that succeeds on the
	// first attempt never writes a delivery row at all.
	var matched int
	var failed atomic.Bool
	var developerID string
```

Increment `matched` and capture `developerID` immediately after the `subscribes` filter:

```go
		if !subscribes(h, ev.Type) {
			d.log.Debug("hook skipped", "webhook_id", h.ID, "account_id", ev.AccountID,
				"developer_id", h.DeveloperID, "reason", "event filter")
			continue
		}
		matched++
		developerID = h.DeveloperID
```

Set `failed` in the send-failure branch of the per-hook goroutine:

```go
			if err := d.send(ctx, h, dl, 1); err != nil {
				failed.Store(true)
				d.enqueue(dl, err)
				return
			}
```

- [ ] **Step 4: Wait explicitly, then evict**

At the very end of `deliver`, after the `for` loop closes:

```go
	// Explicit, in addition to the deferred Wait that covers the early returns
	// above: the eviction decision below needs every hook to have finished.
	wg.Wait()
	if matched > 0 && !failed.Load() {
		d.evictDelivered(ev, developerID)
	}
```

Calling `wg.Wait()` twice is safe — the deferred one returns immediately.

- [ ] **Step 5: Add `evictDelivered`**

Add below `deliver`:

```go
// evictDelivered drops a message's content once every subscribing hook has
// accepted it, if the owning developer has asked us not to retain it. The
// subscriber's own copy is now the copy of record.
//
// Only the events that *carry* a message evict: chat_updated, chat_reaction
// and chat_deleted reference a message whose own event already governed it.
//
// Best effort throughout. A failure here must never fail a delivery — the
// hourly sweep picks the message up on the max-age path instead.
func (d *Dispatcher) evictDelivered(ev model.Event, developerID string) {
	if developerID == "" {
		return
	}
	maxAge, err := d.store.RetentionMaxAge(developerID)
	if err != nil {
		d.log.Error("reading retention policy", "developer_id", developerID, "err", err)
		return
	}
	if maxAge <= 0 {
		return
	}
	now := time.Now().UTC()
	switch ev.Type {
	case model.EventMailReceived, model.EventMailSent:
		if ev.Email == nil || ev.Email.ID == "" {
			return
		}
		err = d.store.EvictEmailContent(ev.AccountID, ev.Email.ID, now)
	case model.EventChatReceived, model.EventChatSent:
		if ev.Message == nil || ev.Message.ID == "" {
			return
		}
		err = d.store.EvictChatMessageContent(ev.AccountID, ev.Message.ID, now)
	default:
		return
	}
	if err != nil {
		d.log.Error("evicting delivered content", "account_id", ev.AccountID, "event", ev.Type, "err", err)
	}
}
```

`sync/atomic` and `time` are already imported by this file.

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test ./internal/events/ -run 'DeliverEvicts|DeliverDoesNotEvict' -v`
Expected: PASS.

- [ ] **Step 7: Run the full suite**

Run: `go test ./...`
Expected: PASS. Run `go test ./internal/events/ -race -count=2` as well — `deliver` is concurrent and this task adds shared state to it.

- [ ] **Step 8: Commit**

```bash
git add internal/events/events.go internal/events/events_test.go
git commit -m "feat(events): evict message content once every hook accepts it"
```

---

### Task 8: Settings API

**Files:**
- Modify: `internal/api/api.go:89` (route list), `:260` (handler map)
- Modify: `internal/api/handlers_auth.go` (add `setRetentionRequest`, `maxRetentionSecs`, `handleSetRetention`)
- Test: `internal/api/` — add to whichever `_test.go` covers `handleSetRedirectDomains`

**Interfaces:**
- Consumes: `store.SetRetentionMaxAge`, `model.Developer.RetentionMaxAgeSecs`.
- Produces: `PUT /api/v1/me/retention`, body `{"retention_max_age_secs": <int>}`, response `{"retention_max_age_secs": <int>}`. `GET /api/v1/me` already returns the embedded `model.Developer`, so the field appears there for free once Task 4 lands.

- [ ] **Step 1: Write the failing test**

Add to `internal/api/api_test.go`. `newTestServer`, `seedDev`, `withSession` and `withKey` are the file's existing helpers (`api_test.go:94`, `:283`, `:296`, `:302`).

```go
func TestSetRetention(t *testing.T) {
	s, db := newTestServer(t)
	h := s.Routes()
	dev, key := seedDev(t, s, "a@x.com")

	put := func(t *testing.T, body string, mod func(*http.Request) *http.Request) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPut, "/api/v1/me/retention", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, mod(req))
		return rec
	}
	session := func(req *http.Request) *http.Request { return withSession(t, s, req, dev.ID) }

	for _, tc := range []struct {
		name string
		body string
		want int
	}{
		{"an hour", `{"retention_max_age_secs":3600}`, http.StatusOK},
		{"zero disables", `{"retention_max_age_secs":0}`, http.StatusOK},
		{"negative rejected", `{"retention_max_age_secs":-1}`, http.StatusBadRequest},
		{"over a year rejected", `{"retention_max_age_secs":31536001}`, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if rec := put(t, tc.body, session); rec.Code != tc.want {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, tc.want, rec.Body.String())
			}
		})
	}

	// The value actually persists, and GET /api/v1/me reports it back.
	if rec := put(t, `{"retention_max_age_secs":3600}`, session); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got, err := db.RetentionMaxAge(dev.ID); err != nil || got != time.Hour {
		t.Fatalf("RetentionMaxAge = %v, %v; want 1h", got, err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, withKey(httptest.NewRequest(http.MethodGet, "/api/v1/me", nil), key))
	if !strings.Contains(rec.Body.String(), `"retention_max_age_secs":3600`) {
		t.Fatalf("GET /api/v1/me does not report the policy: %s", rec.Body.String())
	}
}

// An API key must not be able to change how long its developer's content is
// kept — in either direction. Shortening it destroys content; lengthening it
// defeats the policy. Same rule as the other account settings.
func TestSetRetentionIsSessionOnly(t *testing.T) {
	s, _ := newTestServer(t)
	_, key := seedDev(t, s, "a@x.com")
	req := httptest.NewRequest(http.MethodPut, "/api/v1/me/retention", strings.NewReader(`{"retention_max_age_secs":3600}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, withKey(req, key))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/api/ -run 'SetRetention' -v`
Expected: FAIL — 404, the route does not exist.

- [ ] **Step 3: Add the handler**

In `internal/api/handlers_auth.go`, next to `handleSetRedirectDomains`:

```go
type setRetentionRequest struct {
	MaxAgeSecs int64 `json:"retention_max_age_secs"`
}

// maxRetentionSecs is a year. Anything longer is indistinguishable from "keep
// forever", which is what 0 already means, and a bound keeps a typo from
// setting a policy that silently never fires.
const maxRetentionSecs = 365 * 24 * 60 * 60

// handleSetRetention is session-only, like the other account-settings
// mutations: a leaked API key must not be able to change how long its
// developer's message content is kept — in either direction. Shortening it
// destroys content; lengthening it defeats the policy.
func (s *Server) handleSetRetention(w http.ResponseWriter, r *http.Request) {
	if !s.requireSession(w, r) {
		return
	}
	dev, _ := developerFrom(r.Context())
	var req setRetentionRequest
	if err := decodeJSON(r, &req); err != nil {
		writeDecodeError(w, err)
		return
	}
	if req.MaxAgeSecs < 0 || req.MaxAgeSecs > maxRetentionSecs {
		writeError(w, http.StatusBadRequest, "invalid_body",
			"retention_max_age_secs must be between 0 (keep forever) and 31536000 (one year)")
		return
	}
	if err := s.store.SetRetentionMaxAge(dev.ID, req.MaxAgeSecs); err != nil {
		logx.From(r.Context()).Error("setting retention", "developer_id", dev.ID, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not save the retention policy")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"retention_max_age_secs": req.MaxAgeSecs})
}
```

- [ ] **Step 4: Register the route**

In `internal/api/api.go`, add to the route list after `"PUT /api/v1/me/redirect-domains",`:

```go
	"PUT /api/v1/me/retention",
```

and to the handler map after the `handleSetRedirectDomains` entry:

```go
		"PUT /api/v1/me/retention": s.handleSetRetention,
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/api/ -run 'SetRetention' -v`
Expected: PASS.

- [ ] **Step 6: Run the full suite**

Run: `go test ./...`
Expected: PASS. If a test asserts the exact route list, add the new route there.

- [ ] **Step 7: Commit**

```bash
git add internal/api/api.go internal/api/handlers_auth.go internal/api/*_test.go
git commit -m "feat(api): PUT /api/v1/me/retention"
```

---

### Task 9: The two remaining resurrection guards

**Files:**
- Modify: `internal/api/handlers_mail.go:250-259` (`Server.complete`)
- Modify: `internal/chatsync/sink.go:167-190` (the edit path)
- Test: `internal/api/` and `internal/chatsync/`

**Interfaces:**
- Consumes: `model.Email.ContentEvicted`, `model.ChatMessage.ContentEvicted` from Task 3.
- Produces: no new API.

Task 2's `UpsertEmail` guard already stops both of these from *writing* content back. What remains is a response that leaks filenames, and an event that fires twice.

- [ ] **Step 1: Give the fake mailbox an attachment fixture and a call counter**

`internal/provider/providertest/fakemail.go` currently returns `provider.ErrNotFound` from `ListAttachments` and counts nothing, so a test cannot tell whether it was called. Both changes are additive and leave existing callers behaving identically (the zero value of `Attachments` is nil → still `ErrNotFound`):

```go
	// Attachments is what ListAttachments reports. Nil keeps the old
	// behaviour: provider.ErrNotFound, as if the message had none.
	Attachments []model.Attachment
	// AttachmentCalls counts ListAttachments calls, so a test can assert that
	// a read of an evicted message never reached the provider at all.
	AttachmentCalls atomic.Int64
```

```go
func (f *FakeMail) ListAttachments(ctx context.Context, accountID, messageID string) ([]model.Attachment, error) {
	f.AttachmentCalls.Add(1)
	if f.Attachments == nil {
		return nil, provider.ErrNotFound
	}
	return f.Attachments, nil
}
```

- [ ] **Step 2: Write the failing tests**

Add to `internal/api/api_test.go`. `seedFakeMailAccount` is the existing helper used by `TestMirrorMissIsNegativelyCached` (`api_test.go:3760`):

```go
// Server.complete lazily fetches attachment metadata from the provider and
// caches it on the row. On an evicted message the store write is already
// refused by UpsertEmail's guard, but the response would still carry the
// filenames — and a filename is as revealing as a subject line.
func TestGetEmailDoesNotFetchAttachmentsForEvictedMessage(t *testing.T) {
	fm := providertest.NewFakeMail("FAKEMAIL")
	fm.Attachments = []model.Attachment{{ID: "a1", Name: "payroll.xlsx"}}
	s, db := newTestServerWithProviders(t, fm)
	_, key, acctID := seedFakeMailAccount(t, s, db)

	if err := db.UpsertEmail(model.Email{
		AccountID: acctID, ID: "M1", Subject: "quarterly numbers",
		From: model.Recipient{Email: "alice@example.com"},
		Body: "<p>secret</p>", BodyType: "html", Date: time.Now().UTC(),
		HasAttachments: true,
	}); err != nil {
		t.Fatal(err)
	}

	// A normal read fetches and caches the metadata: the baseline that proves
	// the assertion below is about eviction and not about a broken fixture.
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, withKey(httptest.NewRequest(http.MethodGet,
		"/api/v1/emails/M1?account_id="+acctID, nil), key))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "payroll.xlsx") {
		t.Fatalf("baseline read did not return attachment metadata: %s", rec.Body.String())
	}

	if err := db.EvictEmailContent(acctID, "M1", time.Now()); err != nil {
		t.Fatal(err)
	}
	before := fm.AttachmentCalls.Load()

	rec = httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, withKey(httptest.NewRequest(http.MethodGet,
		"/api/v1/emails/M1?account_id="+acctID, nil), key))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "payroll.xlsx") {
		t.Errorf("evicted message returned attachment filenames: %s", body)
	}
	if !strings.Contains(body, `"content_evicted":true`) {
		t.Errorf("evicted message not flagged: %s", body)
	}
	if got := fm.AttachmentCalls.Load(); got != before {
		t.Errorf("ListAttachments called %d extra times for an evicted message, want 0", got-before)
	}
}
```

Add to `internal/chatsync/runtime_test.go`, using the file's existing `newHarness`, `link`, `waitFor` and `events` helpers:

```go
// The replay guard in sink.Edited compares prev.Text to the incoming text. On
// an evicted row prev.Text is "", so a replayed edit would no longer match,
// would re-apply, and would emit a second chat_updated. Eviction must not
// introduce a duplicate-event bug.
func TestEditOnEvictedMessageIsIgnored(t *testing.T) {
	h := newHarness(t)
	acc := h.link(t, "111")
	waitFor(t, func() bool { return h.fake.Sink(acc) != nil })

	sent := time.Now()
	h.fake.Sink(acc).Message(acc, model.ChatMessage{
		ID: "M1", ChatID: "c1", Kind: "text", Text: "meet me at six",
		SentAt: sent, Sender: model.Attendee{ID: "peer"},
	}, model.Chat{ID: "c1", Kind: "direct"}, model.Attendee{ID: "peer"})
	waitFor(t, func() bool {
		m, err := h.db.GetChatMessage(acc, "M1")
		return err == nil && m.Text == "meet me at six"
	})

	if err := h.db.EvictChatMessageContent(acc, "M1", time.Now()); err != nil {
		t.Fatal(err)
	}
	before := len(h.events())

	h.fake.Sink(acc).Edited(acc, "c1", "M1", "meet me at seven", time.Now())

	// Give the sink's queue a chance to process it before asserting absence.
	waitFor(t, func() bool {
		return h.recs.Contains("content-evicted")
	})
	if got := len(h.events()); got != before {
		t.Fatalf("edit on an evicted message emitted %d new event(s), want 0", got-before)
	}
	m, err := h.db.GetChatMessage(acc, "M1")
	if err != nil {
		t.Fatal(err)
	}
	if m.Text != "" {
		t.Fatalf("edit resurrected evicted text: %q", m.Text)
	}
}
```

`h.recs` is the captured log (`logx.Records`); use whatever substring-matching method it exposes — if it has none, assert on `len(h.events())` after a short `waitFor` on an unrelated observable instead, rather than adding a sleep.

- [ ] **Step 3: Run the tests to verify they fail**

Run: `go test ./internal/api/ ./internal/chatsync/ -run 'EvictedMessage|ForEvictedMessage' -v`
Expected: FAIL — the attachment fetch still happens and the filenames come back; the edit still emits `chat_updated`.

- [ ] **Step 4: Guard `complete`**

`internal/api/handlers_mail.go`, change the condition:

```go
	// An evicted message has no attachment metadata by policy. Without this
	// the lazy cache-fill would fetch the filenames from the provider and put
	// them on the response — the store write is already refused, but the
	// response would still hand back what the tenant asked us to forget.
	if e.HasAttachments && len(e.Attachments) == 0 && !e.ContentEvicted {
```

- [ ] **Step 5: Guard the chat edit path**

`internal/chatsync/sink.go`, immediately after the `GetChatMessage` error check in the edit handler:

```go
		if prev.ContentEvicted {
			// The replay guard below compares prev.Text, which eviction blanked.
			// A replayed edit would no longer match, would re-apply, and would
			// emit a second chat_updated. There is also nothing legitimate to
			// forward: WhatsApp's edit window is 15 minutes, far inside any
			// retention policy, so a real edit cannot reach an evicted message.
			log.Debug("chat event", "kind", "edit", "decision", "content-evicted")
			return
		}
```

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test ./internal/api/ ./internal/chatsync/ -run 'EvictedMessage|ForEvictedMessage' -v`
Expected: PASS.

- [ ] **Step 7: Run the full suite**

Run: `go test ./...`
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/provider/providertest/fakemail.go internal/api/handlers_mail.go internal/chatsync/sink.go internal/api/api_test.go internal/chatsync/runtime_test.go
git commit -m "fix: stop evicted content resurfacing through reads and chat edits"
```

---

### Task 10: Dashboard, viewers and docs

**Files:**
- Modify: `internal/web/templates/dashboard.html` (retention control)
- Modify: `internal/web/templates/mail.html`, `internal/web/templates/chat.html` (evicted state)
- Modify: `internal/api/handlers_llms.go:86-129` (the `Email` shape and the endpoint list)
- Modify: `internal/web/docs/docs.go` and/or `internal/web/docs/snippets.go` (retention section)
- Modify: `README.md` (the feature table's Events/Read rows)
- Test: `internal/api/handlers_auth_ui_test.go`

**Interfaces:**
- Consumes: `PUT /api/v1/me/retention` and `GET /api/v1/me` from Task 8; `content_evicted` from Task 3.
- Produces: no new API.

- [ ] **Step 1: Write the failing test**

Add to `internal/api/handlers_auth_ui_test.go` — `/dashboard` is served by the api package's router:

```go
// The dashboard must actually render the control, or the setting exists only
// for API callers and the "user option" this feature exists for is missing.
func TestDashboardRendersRetentionControl(t *testing.T) {
	s, db := newTestServer(t)
	dev, _ := seedDev(t, s, "a@x.com")
	if err := db.SetRetentionMaxAge(dev.ID, 3600); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, withSession(t, s, httptest.NewRequest(http.MethodGet, "/dashboard", nil), dev.ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"retention", "3600"} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard does not render %q", want)
		}
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/api/ -run 'RetentionControl' -v`
Expected: FAIL — the string is absent.

- [ ] **Step 3: Add the dashboard control**

A number input plus a `PUT /api/v1/me/retention` submit, alongside the redirect-domains control. The copy must state the three things a developer cannot infer from a number field:

- content is dropped as soon as every webhook has accepted it, and in no case kept longer than the value set;
- eviction is permanent — **for WhatsApp there is no way to recover it**, because there is no history API to re-fetch from, so your webhook endpoint becomes the only copy;
- `0` means keep forever.

- [ ] **Step 4: Add the evicted state to both viewers**

In `mail.html` and `chat.html`, where a message body is rendered, branch on `content_evicted` and show an explicit "content removed by your retention policy" state. Without this an evicted message renders blank with no sender and reads as a bug.

- [ ] **Step 5: Update the API docs**

In `internal/api/handlers_llms.go`, add `content_evicted` to the `Email` shape at `:86-92` and to the chat message shape, and add `PUT /api/v1/me/retention` to the endpoint list at `:129`. Add a short retention section to the `/docs` page covering the two triggers and these three consequences:

- search stops matching evicted mail — `ListEmails` matches `Search` against `subject`, `snippet` and `from_email` (`store/store.go:537`), all of which eviction blanks, so results shrink as content ages out;
- a WhatsApp edit arriving for an already-evicted message is ignored;
- for chat, the message text is removed but the conversation's participants are not — they live in `attendees`, which is address-book state.

- [ ] **Step 6: Update the README**

In the feature table, note that message content retention is configurable per developer and off by default. Remove retention from any "not included" list it appears in.

- [ ] **Step 7: Run the tests to verify they pass**

Run: `go test ./internal/api/ -run 'RetentionControl' -v`
Expected: PASS.

- [ ] **Step 8: Run the full suite and build**

Run: `go build ./... && go test ./...`
Expected: both succeed.

- [ ] **Step 9: Manual smoke test**

Use the `verifying-services-end-to-end` skill. Runtime behaviour is the whole point of this feature and none of the above proves it end to end:

1. `make run` with a connected Outlook account and a webhook pointing at a local receiver.
2. Set retention to 3600 in the dashboard.
3. Send yourself mail. Confirm the receiver got the full body.
4. `GET /api/v1/emails/{id}` — expect `content_evicted: true`, empty body, empty `from`, and a truthful `has_attachments`.
5. Confirm the mail viewer shows the evicted state rather than a blank message.
6. Query the DB directly: `SELECT subject, body, from_email, content_evicted_at FROM emails WHERE id = '...'` — content columns empty, `content_evicted_at` set.
7. Stop the receiver, send another message, confirm that one is **not** evicted (delivery failed) and that a `webhook_deliveries` row exists holding the payload.

- [ ] **Step 10: Commit**

```bash
git add internal/web/ internal/api/handlers_llms.go internal/api/handlers_auth_ui_test.go README.md
git commit -m "feat(web): retention control, evicted state in the viewers, docs"
```

---

## Notes for the implementer

**The three guards are the whole risk.** Everything else either works or fails loudly. The guards fail *silently* — content quietly comes back and the tenant's policy becomes a false claim without anything logging or erroring. Task 2 step 5, Task 9 step 3 and Task 9 step 4 are the three; if you cut a corner anywhere, do not cut it there.

**Postgres is not optional.** The suite runs against SQLite by default and against Postgres when `TEST_DATABASE_URL` is set. Before the final commit, run it both ways — the `EXISTS` subqueries and the `CASE WHEN` conflict clause are the statements most likely to behave differently, and `s.q()` is easy to forget on a new query.

```bash
go test ./...
TEST_DATABASE_URL='postgres://...' go test ./...
```

**Do not add eviction to the boot-time purge in `main.go:69-70`.** An operator who sets a policy and restarts would get a mass eviction in the first second, before they could reconsider. The hourly tick is soon enough for a backstop.
