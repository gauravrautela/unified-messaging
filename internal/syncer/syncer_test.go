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
	"github.com/gauravrautela/unified-messaging/internal/logx"
	"github.com/gauravrautela/unified-messaging/internal/model"
	"github.com/gauravrautela/unified-messaging/internal/provider"
	"github.com/gauravrautela/unified-messaging/internal/provider/outlook"
	"github.com/gauravrautela/unified-messaging/internal/provider/providertest"
	"github.com/gauravrautela/unified-messaging/internal/safehttp"
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
	// The dispatcher's default registry delivers through safehttp.Client,
	// whose dial guard refuses loopback by default; the webhook sink below
	// is an httptest server (loopback).
	safehttp.AllowLoopbackForTests(t)
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

	log, recs := logx.Capture()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	disp := events.NewDispatcher(db, nil, log)
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

	// A failing run must be traceable to the run it belongs to.
	s.runOnce(ctx, "acc_missing")
	failed := ""
	for _, l := range recs.All() {
		if strings.Contains(l, "sync failed") {
			failed = l
		}
	}
	if failed == "" || !strings.Contains(failed, "run_id=") {
		t.Errorf("sync failed line missing run_id: %q", failed)
	}
	if started := lineWith(recs, "sync run started"); !strings.Contains(started, "run_id=") {
		t.Errorf("sync run started line missing run_id: %q", started)
	}

	// One line, one component: the context logger carries ids only, so a Graph
	// or store line inside a sync must not print component twice.
	for _, l := range recs.All() {
		if n := strings.Count(l, "component="); n != 1 {
			t.Errorf("line has %d component attributes, want 1: %s", n, strings.TrimSpace(l))
		}
		if n := strings.Count(l, "account_id="); n > 1 {
			t.Errorf("line has %d account_id attributes, want at most 1: %s", n, strings.TrimSpace(l))
		}
	}
}

// lineWith returns the last captured line containing sub, or "".
func lineWith(recs *logx.Records, sub string) string {
	out := ""
	for _, l := range recs.All() {
		if strings.Contains(l, sub) {
			out = l
		}
	}
	return out
}

// The poll loop must not try to delta-sync a chat account: it has no Mailbox.
func TestPollSkipsChatAccounts(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "sync.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_ = db.CreateDeveloper(model.Developer{ID: "dev_1", Email: "dev@example.com"}, "hash")
	_ = db.UpsertAccount(model.Account{ID: "acc_wa", DeveloperID: "dev_1", Provider: "FAKECHAT", Kind: model.AccountKindChat, Email: "+91", Status: model.AccountOK})
	fake := providertest.NewFakeChat("FAKECHAT")
	log, recs := logx.Capture()
	s := New(db, provider.NewRegistry(fake), nil, events.NewDispatcher(db, nil, log), log, Options{PollInterval: time.Hour})
	s.pollOnce(context.Background())
	if recs.Contains("sync run started") {
		t.Fatalf("chat account was polled: %v", recs.All())
	}
	if !recs.Contains("skipping non-mail account") {
		t.Fatalf("expected skip decision: %v", recs.All())
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
	s := New(db, provider.NewRegistry(), nil, events.NewDispatcher(db, nil, log), log, Options{})

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

// A chat account's provider has no Mailbox at all. SyncAccount reaching one —
// a stray Wake against a chat account, say — must fail cleanly rather than
// dereference the nil Mailbox and panic the worker goroutine.
func TestSyncAccountOnChatAccountReturnsError(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "chat.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_ = db.CreateDeveloper(model.Developer{ID: "dev_1", Email: "dev@example.com"}, "hash")
	_ = db.UpsertAccount(model.Account{ID: "acc_wa", DeveloperID: "dev_1", Provider: "FAKECHAT", Kind: model.AccountKindChat, Email: "+91", Status: model.AccountOK})
	fake := providertest.NewFakeChat("FAKECHAT")
	log, _ := logx.Capture()
	s := New(db, provider.NewRegistry(fake), nil, events.NewDispatcher(db, nil, log), log, Options{PollInterval: time.Hour})

	if err := s.SyncAccount(context.Background(), "acc_wa"); err == nil {
		t.Fatal("SyncAccount on a chat account = nil error, want one")
	}
}

// A subscription the provider reports as already existing (typically because
// our own record of it was lost) must never be adopted with its original,
// unknown clientState: that leaves HandleNotifications trusting the
// subscription ID alone, which anyone who learns or guesses it can then use
// to force a sync. EnsureSubscription must delete the pre-existing
// subscription and create a fresh one of its own instead.
func TestAdoptedSubscriptionIsReplacedNotTrusted(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "adopt.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.CreateDeveloper(model.Developer{ID: "dev_1", Email: "dev@example.com"}, "hash"); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertAccount(model.Account{
		ID: "acc_1", DeveloperID: "dev_1", Provider: "FAKEMAIL", Email: "me@example.com", Status: model.AccountOK,
	}); err != nil {
		t.Fatal(err)
	}

	fm := providertest.NewFakeMail("FAKEMAIL")
	fm.DuplicateOnce = true
	fm.ListSubs = []provider.Subscription{
		{ID: "REMOTE1", Resource: "me/mailFolders/inbox/messages", ExpiresAt: time.Now().Add(24 * time.Hour)},
	}

	log, recs := logx.Capture()
	s := New(db, provider.NewRegistry(fm), nil, events.NewDispatcher(db, nil, log), log,
		Options{PollInterval: time.Hour, PublicBaseURL: "https://example.com"})

	// Mirrors how reconcileSubscriptions attaches the base logger before
	// calling EnsureSubscription; a bare context.Background() would fall back
	// to slog.Default() and the log assertion below would see nothing.
	ctx := logx.With(context.Background(), log)
	if err := s.EnsureSubscription(ctx, "acc_1"); err != nil {
		t.Fatalf("EnsureSubscription: %v", err)
	}

	if got := fm.Deleted(); len(got) != 1 || got[0] != "REMOTE1" {
		t.Fatalf("Delete calls = %v, want exactly [REMOTE1]", got)
	}
	subs, err := db.SubscriptionsForAccount("acc_1")
	if err != nil {
		t.Fatal(err)
	}
	if len(subs) != 1 {
		t.Fatalf("stored subscriptions = %+v, want 1", subs)
	}
	if subs[0].ID == "REMOTE1" {
		t.Fatal("stored subscription is still the adopted remote one, want a freshly created one")
	}
	if subs[0].ClientState == "" {
		t.Fatal("stored subscription has no clientState: it is still being trusted blind")
	}
	if !recs.Contains("replaced pre-existing subscription") {
		t.Errorf("missing replace decision log: %v", recs.All())
	}
}

// A provider that keeps reporting a duplicate even after every subscription
// it listed has been deleted — Delete silently failing to take effect
// upstream, or the provider simply lying — must not send EnsureSubscription
// into unbounded recursion. One retry (delete-everything-then-create-again)
// is allowed; a second ErrSubscriptionExists after that must be a hard error,
// not another adopt attempt.
func TestCreateSubscriptionStopsRetryingAfterOneAdopt(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "alwaysdup.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.CreateDeveloper(model.Developer{ID: "dev_1", Email: "dev@example.com"}, "hash"); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertAccount(model.Account{
		ID: "acc_1", DeveloperID: "dev_1", Provider: "FAKEMAIL", Email: "me@example.com", Status: model.AccountOK,
	}); err != nil {
		t.Fatal(err)
	}

	fm := providertest.NewFakeMail("FAKEMAIL")
	fm.AlwaysDuplicate = true
	fm.ListSubs = []provider.Subscription{
		{ID: "REMOTE1", Resource: "me/mailFolders/inbox/messages", ExpiresAt: time.Now().Add(24 * time.Hour)},
	}

	log, recs := logx.Capture()
	s := New(db, provider.NewRegistry(fm), nil, events.NewDispatcher(db, nil, log), log,
		Options{PollInterval: time.Hour, PublicBaseURL: "https://example.com"})

	ctx := logx.With(context.Background(), log)
	err = s.EnsureSubscription(ctx, "acc_1")
	if err == nil {
		t.Fatal("EnsureSubscription = nil error, want the provider-still-duplicate error")
	}
	if !strings.Contains(err.Error(), "still reports an existing subscription after replacing it") {
		t.Fatalf("err = %v, want the bounded-retry error", err)
	}

	// Exactly one retry: Create ran twice (the original attempt plus the one
	// retry after adopting), List and Delete ran exactly once each (one
	// adopt attempt, not one per Create).
	if n := fm.CreateCalls(); n != 2 {
		t.Fatalf("Create calls = %d, want 2 (original + one retry)", n)
	}
	if n := fm.ListCalls(); n != 1 {
		t.Fatalf("List calls = %d, want 1 (exactly one adopt attempt)", n)
	}
	if got := fm.Deleted(); len(got) != 1 || got[0] != "REMOTE1" {
		t.Fatalf("Delete calls = %v, want exactly [REMOTE1]", got)
	}
	if !recs.Contains("provider still reports an existing subscription after replacing it") {
		t.Errorf("missing bounded-retry error log: %v", recs.All())
	}
	if subs, _ := db.SubscriptionsForAccount("acc_1"); len(subs) != 0 {
		t.Fatalf("a subscription was stored despite the hard failure: %+v", subs)
	}
}

// HandleNotifications must reject anything but an exact clientState match: an
// empty one (the shape an adopted-and-trusted subscription used to have), and
// a near-miss that only differs by case, must both be treated as forged.
func TestNotificationWithoutMatchingClientStateIsRejected(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "cstate.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.CreateDeveloper(model.Developer{ID: "dev_1", Email: "dev@example.com"}, "hash"); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertAccount(model.Account{
		ID: "acc_1", DeveloperID: "dev_1", Provider: "FAKEMAIL", Email: "me@example.com", Status: model.AccountOK,
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveSubscription(store.Subscription{
		ID: "SUB1", AccountID: "acc_1", Resource: "me/mailFolders/inbox/messages",
		ClientState: "s", ExpiresAt: time.Now().Add(24 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	fm := providertest.NewFakeMail("FAKEMAIL")
	fm.SetNotifications([]provider.Notification{{SubscriptionID: "SUB1", ClientState: ""}})
	log, _ := logx.Capture()
	s := New(db, provider.NewRegistry(fm), nil, events.NewDispatcher(db, nil, log), log, Options{})

	if err := s.HandleNotifications(context.Background(), "FAKEMAIL", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if drainWake(s) {
		t.Fatal("empty clientState woke the account; want it rejected")
	}

	fm.SetNotifications([]provider.Notification{{SubscriptionID: "SUB1", ClientState: "S"}})
	if err := s.HandleNotifications(context.Background(), "FAKEMAIL", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if drainWake(s) {
		t.Fatal("near-miss clientState woke the account; want it rejected")
	}

	fm.SetNotifications([]provider.Notification{{SubscriptionID: "SUB1", ClientState: "s"}})
	if err := s.HandleNotifications(context.Background(), "FAKEMAIL", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if !drainWake(s) {
		t.Fatal("matching clientState did not wake the account")
	}
}

// drainWake reports whether a Wake was queued (and consumes it), without
// requiring the worker goroutine to be running: the syncer under test here is
// never Start-ed, so s.wake is a plain channel this test can inspect directly.
func drainWake(s *Syncer) bool {
	select {
	case <-s.wake:
		return true
	default:
		return false
	}
}
