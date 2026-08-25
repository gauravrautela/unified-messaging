package syncer

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gauravrautela/unified-messaging/internal/events"
	"github.com/gauravrautela/unified-messaging/internal/model"
	"github.com/gauravrautela/unified-messaging/internal/provider"
	"github.com/gauravrautela/unified-messaging/internal/provider/outlook"
	"github.com/gauravrautela/unified-messaging/internal/store"
)

type fakeTokens struct{}

func (fakeTokens) AccessToken(context.Context, string, bool) (string, error) {
	return "test-token", nil
}

// fakeGraph is a stand-in Microsoft Graph that serves a scripted two-round
// delta conversation, including pagination and a removal.
type fakeGraph struct {
	t   *testing.T
	url string

	mu    sync.Mutex
	calls []string
}

func (f *fakeGraph) record(path string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, path)
}

func (f *fakeGraph) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.record(r.URL.Path + "?" + r.URL.RawQuery)
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.URL.Path == "/me/mailFolders/inbox":
			writeJSON(w, folderJSON("F_INBOX", "Inbox"))

		case r.URL.Path == "/me/mailFolders/sentitems":
			writeJSON(w, folderJSON("F_SENT", "Sent Items"))

		// Folders this mailbox does not have. Consumer accounts commonly lack
		// Archive, and the sync must treat that as normal, not fatal.
		case strings.HasPrefix(r.URL.Path, "/me/mailFolders/") &&
			!strings.Contains(r.URL.Path, "/messages") &&
			!strings.HasSuffix(r.URL.Path, "/delta"):
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"error":{"code":"ErrorItemNotFound","message":"not found"}}`)

		case r.URL.Path == "/me/mailFolders/delta":
			writeJSON(w, fmt.Sprintf(`{"value":[%s,%s],"@odata.deltaLink":%q}`,
				folderJSON("F_INBOX", "Inbox"), folderJSON("F_SENT", "Sent Items"),
				f.url+"/me/mailFolders/delta?$deltatoken=FD1"))

		case r.URL.Path == "/me/mailFolders/F_INBOX/messages/delta":
			f.serveInboxDelta(w, r)

		// M3 arrives with an attachment; the webhook payload should list it
		// without the subscriber making a second call.
		case r.URL.Path == "/me/messages/M3/attachments":
			writeJSON(w, `{"value":[{"id":"A1","name":"q3.pdf","contentType":"application/pdf","size":1234,"isInline":false}]}`)

		case r.URL.Path == "/me/mailFolders/F_SENT/messages/delta":
			writeJSON(w, fmt.Sprintf(`{"value":[],"@odata.deltaLink":%q}`,
				f.url+"/me/mailFolders/F_SENT/messages/delta?$deltatoken=SD1"))

		default:
			f.t.Errorf("unexpected Graph call: %s", r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

func (f *fakeGraph) serveInboxDelta(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	base := f.url + "/me/mailFolders/F_INBOX/messages/delta"

	switch {
	case q.Get("$skiptoken") == "S1":
		// Second page of the initial backfill.
		writeJSON(w, fmt.Sprintf(`{"value":[%s],"@odata.deltaLink":%q}`,
			messageJSON("M2", "Second", "2026-08-20T10:00:00Z"), base+"?$deltatoken=D1"))

	case q.Get("$deltatoken") == "D1":
		// Incremental round: one arrival, one departure.
		writeJSON(w, fmt.Sprintf(`{"value":[%s,{"id":"M1","@removed":{"reason":"deleted"}}],"@odata.deltaLink":%q}`,
			strings.Replace(messageJSON("M3", "Third", "2026-08-20T11:00:00Z"),
				`"hasAttachments":false`, `"hasAttachments":true`, 1),
			base+"?$deltatoken=D2"))

	case q.Get("$deltatoken") == "D2":
		writeJSON(w, fmt.Sprintf(`{"value":[],"@odata.deltaLink":%q}`, base+"?$deltatoken=D2"))

	default:
		// First page of the initial backfill.
		writeJSON(w, fmt.Sprintf(`{"value":[%s],"@odata.nextLink":%q}`,
			messageJSON("M1", "First", "2026-08-20T09:00:00Z"), base+"?$skiptoken=S1"))
	}
}

func writeJSON(w http.ResponseWriter, body string) {
	io.WriteString(w, body)
}

func folderJSON(id, name string) string {
	return fmt.Sprintf(`{"id":%q,"displayName":%q,"parentFolderId":"ROOT","totalItemCount":2,"unreadItemCount":1}`, id, name)
}

func messageJSON(id, subject, received string) string {
	return fmt.Sprintf(`{
	  "id":%q,"conversationId":"C1","parentFolderId":"F_INBOX","subject":%q,
	  "bodyPreview":"preview","body":{"contentType":"html","content":"<p>body</p>"},
	  "from":{"emailAddress":{"name":"Ada","address":"ada@example.com"}},
	  "toRecipients":[{"emailAddress":{"address":"me@outlook.com"}}],
	  "receivedDateTime":%q,"isRead":false,"isDraft":false,"hasAttachments":false,
	  "internetMessageId":"<%s@example.com>"}`, id, subject, received, id)
}

// eventSink is a webhook endpoint that collects deliveries.
type eventSink struct {
	mu     sync.Mutex
	events []model.Event
}

func (s *eventSink) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var ev model.Event
		if err := json.NewDecoder(r.Body).Decode(&ev); err == nil {
			s.mu.Lock()
			s.events = append(s.events, ev)
			s.mu.Unlock()
		}
		w.WriteHeader(http.StatusOK)
	})
}

func (s *eventSink) snapshot() []model.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]model.Event(nil), s.events...)
}

// waitFor polls until cond holds or the deadline passes, so the async webhook
// dispatcher does not make the test flaky.
func waitFor(t *testing.T, d time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return cond()
}

func TestSyncAccountBackfillThenIncremental(t *testing.T) {
	fg := &fakeGraph{t: t}
	srv := httptest.NewServer(fg.handler())
	defer srv.Close()
	fg.url = srv.URL

	prev := outlook.BaseURL
	outlook.BaseURL = srv.URL
	defer func() { outlook.BaseURL = prev }()

	db, err := store.Open(filepath.Join(t.TempDir(), "sync.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if err := db.CreateDeveloper(model.Developer{ID: "dev_1", Email: "dev@example.com"}, "hash"); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertAccount(model.Account{
		ID: "acc_1", DeveloperID: "dev_1", Provider: outlook.Name, Email: "me@outlook.com", Status: model.AccountOK,
	}); err != nil {
		t.Fatal(err)
	}

	sink := &eventSink{}
	sinkSrv := httptest.NewServer(sink.handler())
	defer sinkSrv.Close()
	if err := db.SaveWebhook(model.Webhook{
		ID: "wh_1", DeveloperID: "dev_1", URL: sinkSrv.URL, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	disp := events.NewDispatcher(db, log)
	disp.Start(ctx)

	registry := provider.NewRegistry(outlook.New(nil, fakeTokens{}))
	s := New(db, registry, nil, disp, log, Options{PollInterval: time.Hour})

	// --- round one: initial backfill ---
	if err := s.SyncAccount(ctx, "acc_1"); err != nil {
		t.Fatalf("first sync: %v", err)
	}

	mails, err := db.ListEmails(store.EmailQuery{AccountID: "acc_1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(mails) != 2 {
		t.Fatalf("after backfill got %d messages, want 2 (pagination across nextLink)", len(mails))
	}

	folders, err := db.ListFolders("acc_1")
	if err != nil {
		t.Fatal(err)
	}
	if len(folders) != 2 {
		t.Fatalf("got %d folders, want 2", len(folders))
	}
	roles := map[string]string{}
	for _, f := range folders {
		roles[f.ID] = f.Role
	}
	if roles["F_INBOX"] != "inbox" || roles["F_SENT"] != "sentitems" {
		t.Fatalf("well-known roles not tagged: %+v", roles)
	}

	// A first sync must stay silent, or connecting a mailbox would fire an event
	// for every message already in it.
	time.Sleep(150 * time.Millisecond)
	if got := sink.snapshot(); len(got) != 0 {
		t.Fatalf("backfill emitted %d events, want 0: %+v", len(got), got)
	}

	// --- round two: incremental ---
	if err := s.SyncAccount(ctx, "acc_1"); err != nil {
		t.Fatalf("second sync: %v", err)
	}

	if _, err := db.GetEmail("acc_1", "M1"); err == nil {
		t.Fatal("M1 was removed upstream but is still stored")
	}
	if _, err := db.GetEmail("acc_1", "M3"); err != nil {
		t.Fatalf("M3 was not synced: %v", err)
	}

	ok := waitFor(t, 3*time.Second, func() bool { return len(sink.snapshot()) >= 2 })
	got := sink.snapshot()
	if !ok {
		t.Fatalf("expected 2 events, got %d: %+v", len(got), got)
	}

	seen := map[string]string{}
	var received *model.Email
	for _, ev := range got {
		switch {
		case ev.Email != nil:
			seen[ev.Type] = ev.Email.ID
			if ev.Type == model.EventMailReceived {
				received = ev.Email
			}
		default:
			seen[ev.Type] = ev.EmailID
		}
	}
	if seen[model.EventMailReceived] != "M3" {
		t.Fatalf("expected mail_received for M3, got %+v", seen)
	}
	// The event carries what a subscriber needs to act without calling back:
	// which well-known folder it landed in, and the attachment list.
	if received.Role != "inbox" {
		t.Fatalf("mail_received role = %q, want inbox", received.Role)
	}
	if len(received.Attachments) != 1 || received.Attachments[0].Name != "q3.pdf" {
		t.Fatalf("mail_received attachments = %+v, want [q3.pdf]", received.Attachments)
	}
	if seen[model.EventMailDeleted] != "M1" {
		t.Fatalf("expected mail_deleted for M1, got %+v", seen)
	}
}

// A second wakeup while a sync is running must collapse into one follow-up run
// rather than piling up duplicate work.
func TestWakeCollapsesWhileInflight(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "wake.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := New(db, provider.NewRegistry(), nil, events.NewDispatcher(db, log), log, Options{})

	s.mu.Lock()
	s.inflight["acc_1"] = true
	s.mu.Unlock()

	s.Wake("acc_1")
	s.Wake("acc_1")

	if len(s.wake) != 0 {
		t.Fatalf("queued %d wakeups for an in-flight account, want 0", len(s.wake))
	}
	s.mu.Lock()
	pending := s.pending["acc_1"]
	s.mu.Unlock()
	if !pending {
		t.Fatal("follow-up run was not recorded")
	}
}
