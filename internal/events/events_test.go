package events

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

	"github.com/gauravrautela/unified-messaging/internal/logx"
	"github.com/gauravrautela/unified-messaging/internal/model"
	"github.com/gauravrautela/unified-messaging/internal/notify"
	"github.com/gauravrautela/unified-messaging/internal/safehttp"
	"github.com/gauravrautela/unified-messaging/internal/store"
)

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	// Every test in this file that builds a dispatcher with a nil registry
	// (or notify.NewRegistry(nil)) delivers through safehttp.Client, whose
	// dial guard refuses loopback by default; httptest servers are always
	// loopback.
	safehttp.AllowLoopbackForTests(t)
	s, err := store.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	s.SetSealKey([]byte("0123456789abcdef0123456789abcdef"))
	return s
}

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

// receiver is an HTTP endpoint that records what it was sent.
type receiver struct {
	*httptest.Server
	mu     sync.Mutex
	hits   []string // X-Outlook-Event header of each delivery
	bodies [][]byte
	code   int
}

func newReceiver(t *testing.T, code int) *receiver {
	t.Helper()
	r := &receiver{code: code}
	r.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		r.hits = append(r.hits, req.Header.Get("X-Outlook-Event"))
		r.bodies = append(r.bodies, body)
		r.mu.Unlock()
		w.WriteHeader(r.code)
	}))
	t.Cleanup(r.Close)
	return r
}

func (r *receiver) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.hits)
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}

func TestDeliveryIsScopedToAccount(t *testing.T) {
	db := newTestStore(t)
	seedTenant(t, db)
	mine := newReceiver(t, http.StatusOK)
	theirs := newReceiver(t, http.StatusOK)
	global := newReceiver(t, http.StatusOK)

	now := time.Now()
	for _, w := range []model.Webhook{
		{ID: "wh_mine", DeveloperID: "dev_1", AccountID: "acc_1", URL: mine.URL, Secret: "topsecret", CreatedAt: now},
		{ID: "wh_theirs", DeveloperID: "dev_1", AccountID: "acc_2", URL: theirs.URL, CreatedAt: now},
		{ID: "wh_global", DeveloperID: "dev_1", URL: global.URL, CreatedAt: now},
	} {
		if err := db.SaveWebhook(w); err != nil {
			t.Fatal(err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	log, recs := logx.Capture()
	d := NewDispatcher(db, nil, log)
	d.Start(ctx)

	d.Emit(model.Event{Type: model.EventMailReceived, AccountID: "acc_1", Email: &model.Email{ID: "M1"}})

	waitFor(t, func() bool { return mine.count() == 1 && global.count() == 1 })
	// Give a stray delivery a moment to show up before asserting it did not.
	time.Sleep(50 * time.Millisecond)
	if theirs.count() != 0 {
		t.Fatalf("acc_2's webhook received acc_1's mail (%d deliveries)", theirs.count())
	}
	if recs.Contains("topsecret") {
		t.Fatalf("webhook signing secret leaked into logs: %v", recs.All())
	}
}

func (r *receiver) setCode(code int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.code = code
}

func newFastDispatcher(t *testing.T, db *store.Store, schedule ...time.Duration) *Dispatcher {
	t.Helper()
	return newFastDispatcherLog(t, db, slog.New(slog.NewTextHandler(io.Discard, nil)), schedule...)
}

// newFastDispatcherLog is newFastDispatcher with a caller-supplied logger, so a
// test can assert on what the delivery path logged.
func newFastDispatcherLog(t *testing.T, db *store.Store, log *slog.Logger, schedule ...time.Duration) *Dispatcher {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	d := NewDispatcher(db, nil, log)
	d.RetrySchedule = schedule
	d.RetryPoll = 20 * time.Millisecond
	d.Start(ctx)
	return d
}

// A non-2xx does not lose the event: it is parked in the store and re-sent on
// the schedule once the subscriber recovers, surviving anything in between.
func TestFailedDeliveryIsQueuedAndRetried(t *testing.T) {
	db := newTestStore(t)
	seedTenant(t, db)
	rcv := newReceiver(t, http.StatusInternalServerError)
	if err := db.SaveWebhook(model.Webhook{ID: "wh_1", DeveloperID: "dev_1", URL: rcv.URL, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	log, recs := logx.Capture()
	d := newFastDispatcherLog(t, db, log, 10*time.Millisecond, time.Hour)

	d.Emit(model.Event{Type: model.EventMailReceived, AccountID: "acc_1", Email: &model.Email{ID: "M1"}})

	waitFor(t, func() bool {
		q, _ := db.ListDeliveries("wh_1")
		return len(q) == 1 && q[0].Attempts == 1 && q[0].LastError != "" && !q[0].Dead
	})

	rcv.setCode(http.StatusOK)
	waitFor(t, func() bool {
		q, _ := db.ListDeliveries("wh_1")
		return len(q) == 0
	})
	if rcv.count() < 2 {
		t.Fatalf("expected a redelivery, got %d hits", rcv.count())
	}

	for _, want := range []string{"component=events", "delivery attempt", "attempt=1", "decision=\"scheduled retry\"", "decision=delivered"} {
		if !recs.Contains(want) {
			t.Errorf("events log missing %q", want)
		}
	}

	// Every delivery line, including the failures, carries the correlation set.
	attempt := lineWith(recs, "delivery attempt")
	if !strings.Contains(attempt, "account_id=acc_1") || !strings.Contains(attempt, "developer_id=dev_1") {
		t.Errorf("delivery attempt line missing tenant ids: %q", attempt)
	}
	failed := lineWith(recs, "webhook delivery failed")
	if !strings.Contains(failed, "delivery_id=") {
		t.Errorf("failed delivery line missing delivery_id: %q", failed)
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

func TestDeliveryIsDeadAfterScheduleExhausted(t *testing.T) {
	db := newTestStore(t)
	seedTenant(t, db)
	rcv := newReceiver(t, http.StatusBadGateway)
	if err := db.SaveWebhook(model.Webhook{ID: "wh_1", DeveloperID: "dev_1", URL: rcv.URL, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	d := newFastDispatcher(t, db, 10*time.Millisecond, 10*time.Millisecond)

	d.Emit(model.Event{Type: model.EventMailReceived, AccountID: "acc_1", Email: &model.Email{ID: "M1"}})

	waitFor(t, func() bool {
		q, _ := db.ListDeliveries("wh_1")
		return len(q) == 1 && q[0].Dead
	})
	q, _ := db.ListDeliveries("wh_1")
	// first attempt + one per schedule entry
	if q[0].Attempts != 3 || rcv.count() != 3 {
		t.Fatalf("attempts = %d, hits = %d, want 3 each", q[0].Attempts, rcv.count())
	}
}

// Each subscriber is told which of its hooks fired, so one endpoint serving
// several accounts can tell the deliveries apart.
func TestPayloadIdentifiesWebhook(t *testing.T) {
	db := newTestStore(t)
	seedTenant(t, db)
	rcv := newReceiver(t, http.StatusOK)
	if err := db.SaveWebhook(model.Webhook{ID: "wh_1", DeveloperID: "dev_1", Name: "crm-sync", AccountID: "acc_1", URL: rcv.URL, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	newFastDispatcher(t, db).Emit(model.Event{Type: model.EventMailReceived, AccountID: "acc_1", Email: &model.Email{ID: "M1"}})

	waitFor(t, func() bool { return rcv.count() == 1 })
	var got model.Event
	rcv.mu.Lock()
	err := json.Unmarshal(rcv.bodies[0], &got)
	rcv.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if got.Webhook == nil || got.Webhook.ID != "wh_1" || got.Webhook.Name != "crm-sync" {
		t.Fatalf("payload webhook = %+v, want {wh_1 crm-sync}", got.Webhook)
	}
}

// --- back-pressure (I1) ---

// A full queue must push back on the emitter rather than silently discarding
// the event: the actor that produced it is the one place that can slow down,
// and its handlers are idempotent, so blocking there is safe. Only after the
// bound elapses may an event be dropped, and a dropped event must be counted.
func TestEmitBlocksWhileQueueIsFullThenDropsAndCounts(t *testing.T) {
	db := newTestStore(t)
	seedTenant(t, db)
	log, recs := logx.Capture()
	d := NewDispatcher(db, nil, log)
	d.EmitBlock = 80 * time.Millisecond

	ev := model.Event{Type: model.EventMailReceived, AccountID: "acc_1"}
	for i := 0; i < cap(d.queue); i++ {
		d.Emit(ev)
	}
	if d.Dropped() != 0 {
		t.Fatalf("filling the queue dropped %d events", d.Dropped())
	}

	start := time.Now()
	d.Emit(ev)
	waited := time.Since(start)
	if waited < 60*time.Millisecond {
		t.Fatalf("Emit returned after %v on a full queue; it must block for EmitBlock first", waited)
	}
	if got := d.Dropped(); got != 1 {
		t.Fatalf("Dropped() = %d, want 1", got)
	}
	if !recs.Contains("dropped_total") {
		t.Fatalf("drop was not logged with a running total: %v", recs.All())
	}
}

// The blocking fallback is a bound, not a sleep: as soon as the consumer takes
// one event the emitter proceeds, and nothing is dropped.
func TestEmitProceedsWhenQueueDrainsWithinBound(t *testing.T) {
	db := newTestStore(t)
	seedTenant(t, db)
	log, _ := logx.Capture()
	d := NewDispatcher(db, nil, log)
	d.EmitBlock = 2 * time.Second

	ev := model.Event{Type: model.EventMailReceived, AccountID: "acc_1"}
	for i := 0; i < cap(d.queue); i++ {
		d.Emit(ev)
	}
	go func() {
		time.Sleep(30 * time.Millisecond)
		<-d.queue
	}()
	start := time.Now()
	d.Emit(ev)
	if waited := time.Since(start); waited > time.Second {
		t.Fatalf("Emit waited %v after the queue drained", waited)
	}
	if got := d.Dropped(); got != 0 {
		t.Fatalf("Dropped() = %d, want 0", got)
	}
}

// --- drain on shutdown (I2) ---

// Cancelling the dispatcher's context must not throw away events that are
// already queued: Wait() is documented as a drain, and until this landed it
// only looked like one.
func TestDeliveryWorkerDrainsQueuedEventsOnShutdown(t *testing.T) {
	db := newTestStore(t)
	seedTenant(t, db)
	rcv := newReceiver(t, http.StatusOK)
	if err := db.SaveWebhook(model.Webhook{ID: "wh_1", DeveloperID: "dev_1", AccountID: "acc_1",
		URL: rcv.URL, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}

	log, _ := logx.Capture()
	d := NewDispatcher(db, nil, log)
	for i := 0; i < 3; i++ {
		d.Emit(model.Event{Type: model.EventMailReceived, AccountID: "acc_1", Email: &model.Email{ID: "M1"}})
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already shutting down when the worker starts
	d.Start(ctx)
	d.Wait()

	if rcv.count() != 3 {
		t.Fatalf("delivered %d of 3 queued events on shutdown", rcv.count())
	}
}

// One event fanned out to a webhook, discord and telegram hook must reach
// each in its own shape: raw JSON, Markdown, and HTML respectively.
func TestOneEventReachesEachKindInItsOwnShape(t *testing.T) {
	db := newTestStore(t)
	db.SetSealKey([]byte("0123456789abcdef0123456789abcdef"))
	seedTenant(t, db)
	wh := newReceiver(t, 200)
	discord := newReceiver(t, 204)
	telegram := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		// Decode first: encoding/json HTML-escapes "<"/">" on the wire (valid
		// JSON either way), so comparing raw bytes against literal "<b>" tags
		// would fail on a correct encoder. Every other consumer of this body
		// (Telegram included) decodes JSON before reading text, so this test
		// does too.
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		text, _ := m["text"].(string)
		discord.mu.Lock() // reuse a mutex; the assertion below reads bodies under it
		discord.bodies = append(discord.bodies, append([]byte("TG "+r.URL.Path+" "), text...))
		discord.mu.Unlock()
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(telegram.Close)
	now := time.Now().UTC()
	for _, h := range []model.Webhook{
		{ID: "wh_a", DeveloperID: "dev_1", Kind: "webhook", URL: wh.URL, CreatedAt: now},
		{ID: "wh_b", DeveloperID: "dev_1", Kind: "discord", URL: discord.URL + "/api/webhooks/1/t", CreatedAt: now},
		{ID: "wh_c", DeveloperID: "dev_1", Kind: "telegram", Telegram: &model.TelegramTarget{BotToken: "1:A", ChatID: "-5"}, CreatedAt: now},
	} {
		if err := db.SaveWebhook(h); err != nil {
			t.Fatal(err)
		}
	}
	reg := notify.NewRegistry(nil)
	reg.SetTelegramBase(telegram.URL)
	d := NewDispatcher(db, reg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.Start(ctx)
	d.Emit(model.Event{Type: model.EventChatReceived, AccountID: "acc_1",
		Chat: &model.Chat{Name: "Team"}, Message: &model.ChatMessage{Sender: model.Attendee{Name: "Ada"}, Text: "hi"}})
	waitFor(t, func() bool { return wh.count() == 1 && discord.count() >= 1 && len(discord.bodies) >= 2 })
	discord.mu.Lock()
	defer discord.mu.Unlock()
	var sawJSON, sawMarkdown, sawHTML bool
	for _, b := range discord.bodies {
		s := string(b)
		switch {
		case strings.HasPrefix(s, "TG /bot1:A/sendMessage") && strings.Contains(s, "<b>Ada</b>"):
			sawHTML = true
		case strings.Contains(s, "**Ada**") && strings.Contains(s, "allowed_mentions"):
			sawMarkdown = true
		}
	}
	if strings.Contains(string(wh.bodies[0]), `"type":"chat_received"`) {
		sawJSON = true
	}
	if !sawJSON || !sawMarkdown || !sawHTML {
		t.Fatalf("json=%v markdown=%v html=%v bodies=%q", sawJSON, sawMarkdown, sawHTML, discord.bodies)
	}
}

func TestFailedDeliveryStoresScrubbedError(t *testing.T) {
	db := newTestStore(t)
	db.SetSealKey([]byte("0123456789abcdef0123456789abcdef"))
	seedTenant(t, db)
	bad := newReceiver(t, 500)
	if err := db.SaveWebhook(model.Webhook{ID: "wh_d", DeveloperID: "dev_1", Kind: "discord",
		URL: bad.URL + "/api/webhooks/9/topsecret", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	d := NewDispatcher(db, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.Start(ctx)
	d.Emit(model.Event{Type: model.EventMailReceived, AccountID: "acc_1", Email: &model.Email{Subject: "s"}})
	waitFor(t, func() bool {
		dls, _ := db.ListDeliveries("wh_d")
		return len(dls) == 1
	})
	dls, _ := db.ListDeliveries("wh_d")
	if strings.Contains(dls[0].LastError, "topsecret") || !strings.Contains(dls[0].LastError, "500") {
		t.Fatalf("last_error = %q", dls[0].LastError)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// The sibling test above points at a 127.0.0.1 httptest URL, which the
// host-anchored scrubber correctly declines to mask — so it never actually
// exercises masking. This one uses a real discord.com URL (saved straight into
// the store, since the API now rejects anything but a plain discord.com host)
// and a transport that fails with the URL in the error, which is exactly the
// shape url.Error produces in production.
func TestFailedDiscordDeliveryMasksTokenInLastError(t *testing.T) {
	db := newTestStore(t)
	seedTenant(t, db)
	const hookURL = "https://discord.com/api/webhooks/9/topsecret"
	if err := db.SaveWebhook(model.Webhook{ID: "wh_dm", DeveloperID: "dev_1", Kind: "discord",
		URL: hookURL, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("dial tcp: no route to %s", r.URL.String())
	})}
	d := NewDispatcher(db, notify.NewRegistry(client), slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.Start(ctx)
	d.Emit(model.Event{Type: model.EventMailReceived, AccountID: "acc_1", Email: &model.Email{Subject: "s"}})
	waitFor(t, func() bool {
		dls, _ := db.ListDeliveries("wh_dm")
		return len(dls) == 1 && dls[0].LastError != ""
	})
	dls, _ := db.ListDeliveries("wh_dm")
	if strings.Contains(dls[0].LastError, "topsecret") || !strings.Contains(dls[0].LastError, "/api/webhooks/9/•••") {
		t.Fatalf("last_error = %q", dls[0].LastError)
	}
}

// A target that flaps between 404 and 429 has to be triageable from the log
// without parsing an error string, so the failing "delivery response" line
// carries the numeric status the way the succeeding one carries "ok".
func TestFailedDeliveryLogsNumericStatus(t *testing.T) {
	db := newTestStore(t)
	seedTenant(t, db)
	bad := newReceiver(t, 503)
	if err := db.SaveWebhook(model.Webhook{ID: "wh_st", DeveloperID: "dev_1",
		URL: bad.URL, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	log, recs := logx.Capture()
	d := NewDispatcher(db, nil, log)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.Start(ctx)
	d.Emit(model.Event{Type: model.EventMailReceived, AccountID: "acc_1", Email: &model.Email{Subject: "s"}})
	waitFor(t, func() bool {
		dls, _ := db.ListDeliveries("wh_st")
		return len(dls) == 1
	})
	found := false
	for _, l := range recs.All() {
		if strings.Contains(l, `msg="delivery response"`) && strings.Contains(l, "status=503") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no delivery response line with status=503: %v", recs.All())
	}
}
