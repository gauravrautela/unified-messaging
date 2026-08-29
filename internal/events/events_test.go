package events

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
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
	s := store.OpenForTest(t)
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

// waitForTimeout is generous enough to cover the delivery retry pipeline it
// polls even when every store read/write in it is a real network round trip
// (running against TEST_DATABASE_URL) rather than SQLite's effectively-free
// local ones. It only bounds how long a test waits before failing — the
// fast (SQLite) path still returns as soon as cond() is true.
const waitForTimeout = 20 * time.Second

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitForTimeout)
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
		q, _ := db.ListDeliveries("wh_1", 200, 0)
		return len(q) == 1 && q[0].Attempts == 1 && q[0].LastError != "" && !q[0].Dead
	})

	rcv.setCode(http.StatusOK)
	waitFor(t, func() bool {
		q, _ := db.ListDeliveries("wh_1", 200, 0)
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
		q, _ := db.ListDeliveries("wh_1", 200, 0)
		return len(q) == 1 && q[0].Dead
	})
	q, _ := db.ListDeliveries("wh_1", 200, 0)
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
		dls, _ := db.ListDeliveries("wh_d", 200, 0)
		return len(dls) == 1
	})
	dls, _ := db.ListDeliveries("wh_d", 200, 0)
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
		dls, _ := db.ListDeliveries("wh_dm", 200, 0)
		return len(dls) == 1 && dls[0].LastError != ""
	})
	dls, _ := db.ListDeliveries("wh_dm", 200, 0)
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
		dls, _ := db.ListDeliveries("wh_st", 200, 0)
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

// --- concurrent hook delivery (I4) ---

// One slow webhook target must not stall every other subscriber's deliveries:
// the dispatcher fans hooks out under a worker pool rather than sending them
// one at a time on the single delivery goroutine.
func TestSlowHookDoesNotBlockOtherHooks(t *testing.T) {
	db := newTestStore(t)
	seedTenant(t, db)
	safehttp.AllowLoopbackForTests(t)
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { time.Sleep(3 * time.Second) }))
	t.Cleanup(slow.Close)
	fast := newReceiver(t, 200)
	for _, h := range []model.Webhook{
		{ID: "wh_slow", DeveloperID: "dev_1", URL: slow.URL, CreatedAt: time.Now()},
		{ID: "wh_fast", DeveloperID: "dev_1", URL: fast.URL, CreatedAt: time.Now()},
	} {
		if err := db.SaveWebhook(h); err != nil {
			t.Fatal(err)
		}
	}
	d := NewDispatcher(db, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	d.DeliveryWorkers = 4
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.Start(ctx)
	start := time.Now()
	d.Emit(model.Event{Type: model.EventMailReceived, AccountID: "acc_1", Email: &model.Email{Subject: "x"}})
	waitFor(t, func() bool { return fast.count() == 1 })
	if time.Since(start) > 2*time.Second {
		t.Fatal("fast hook waited behind the slow one")
	}
}

// deliver's fan-out must not return before every hook goroutine it spawned
// has finished: an early return partway through the loop (the encoding-
// failure branch used to do this) would let the caller — the delivery loop,
// or drain on shutdown — move on while a send was still in flight.
func TestDeliverWaitsForSpawnedHookBeforeReturning(t *testing.T) {
	db := newTestStore(t)
	seedTenant(t, db)
	safehttp.AllowLoopbackForTests(t)
	var hit atomic.Bool
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		hit.Store(true)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(slow.Close)
	if err := db.SaveWebhook(model.Webhook{ID: "wh_slow", DeveloperID: "dev_1", AccountID: "acc_1", URL: slow.URL, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	d := NewDispatcher(db, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	// Calling deliver directly (same package) rather than through Start/Emit
	// isolates exactly what is under test: this call's own duration. sem is
	// normally built by Start; build it here since deliver needs it.
	d.sem = make(chan struct{}, 4)

	start := time.Now()
	d.deliver(context.Background(), model.Event{Type: model.EventMailReceived, AccountID: "acc_1", Email: &model.Email{Subject: "x"}})
	if elapsed := time.Since(start); elapsed < 200*time.Millisecond {
		t.Fatalf("deliver returned after %v, want >= 200ms (it must wait for the hook it spawned)", elapsed)
	}
	if !hit.Load() {
		t.Fatal("the slow hook was never actually invoked")
	}
}

// A payload that fails to encode (a Timestamp outside the range
// time.Time.MarshalJSON accepts) must not derail the rest of the dispatcher:
// the failing hook is skipped, not the whole fan-out, and the very next
// event still delivers normally.
func TestDeliverSkipsOnlyTheHookThatFailsToEncode(t *testing.T) {
	db := newTestStore(t)
	seedTenant(t, db)
	rcv := newReceiver(t, http.StatusOK)
	if err := db.SaveWebhook(model.Webhook{ID: "wh_1", DeveloperID: "dev_1", AccountID: "acc_1", URL: rcv.URL, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	d := NewDispatcher(db, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.Start(ctx)

	// time.Time.MarshalJSON rejects a year outside [0, 9999], so this event
	// fails to encode for every hook it reaches.
	bad := model.Event{Type: model.EventMailReceived, AccountID: "acc_1",
		Timestamp: time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC), Email: &model.Email{Subject: "bad"}}
	d.Emit(bad)
	d.Emit(model.Event{Type: model.EventMailReceived, AccountID: "acc_1", Email: &model.Email{Subject: "good"}})

	waitFor(t, func() bool { return rcv.count() == 1 })
	time.Sleep(50 * time.Millisecond)
	if rcv.count() != 1 {
		t.Fatalf("hits = %d, want exactly 1 (the bad event must not double-fire or hang the queue)", rcv.count())
	}
}

// --- retries must not starve fresh dispatch (I-2) ---

// A tenant whose endpoint accepts and never answers generates 100 due
// retries per tick, each holding a worker for the client's full timeout.
// While retries and fresh dispatch shared one 8-slot pool that was
// continuous full occupancy: the fresh-delivery goroutine blocked on the
// semaphore inside deliver, the event queue filled behind it, and Emit — which
// some API request paths call inline — pushed back on every other tenant.
// Retries get their own pool of the same size, so a fresh event still goes
// out promptly no matter how deep the retry backlog is.
func TestRetryBacklogDoesNotStarveFreshDelivery(t *testing.T) {
	db := newTestStore(t)
	seedTenant(t, db)
	safehttp.AllowLoopbackForTests(t)

	// Never answers within the test's lifetime: every retry that reaches it
	// holds its worker for as long as the test runs.
	var inflight atomic.Int64
	release := make(chan struct{})
	stuck := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inflight.Add(1)
		<-release
	}))
	// Released before Close, or Close would block for as long as the handlers
	// do — the point of the endpoint is that it never answers on its own.
	t.Cleanup(func() { close(release); stuck.Close() })
	fast := newReceiver(t, http.StatusOK)

	// The backlog is on acc_2's hook; the fresh event goes to acc_1's, so
	// nothing but the worker pool connects the two.
	for _, h := range []model.Webhook{
		{ID: "wh_stuck", DeveloperID: "dev_1", AccountID: "acc_2", URL: stuck.URL, CreatedAt: time.Now()},
		{ID: "wh_fast", DeveloperID: "dev_1", AccountID: "acc_1", URL: fast.URL, CreatedAt: time.Now()},
	} {
		if err := db.SaveWebhook(h); err != nil {
			t.Fatal(err)
		}
	}
	payload, err := json.Marshal(model.Event{Type: model.EventMailReceived, AccountID: "acc_2",
		Email: &model.Email{ID: "M1"}, Timestamp: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Minute).UTC()
	for i := range 100 {
		if err := db.SaveDelivery(store.Delivery{
			ID: fmt.Sprintf("dl_%03d", i), WebhookID: "wh_stuck", AccountID: "acc_2",
			EventType: model.EventMailReceived, Payload: payload, Attempts: 1,
			NextAttemptAt: past, CreatedAt: past,
		}); err != nil {
			t.Fatal(err)
		}
	}

	d := NewDispatcher(db, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	d.RetryPoll = 20 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.Start(ctx)

	// Let the retry ticks take every worker they can before the fresh event
	// is emitted — that is the state the starvation happens in. The pool is
	// DeliveryWorkers wide, so that many in-flight retries means saturated.
	waitFor(t, func() bool { return inflight.Load() >= int64(d.DeliveryWorkers) })

	start := time.Now()
	d.Emit(model.Event{Type: model.EventMailReceived, AccountID: "acc_1", Email: &model.Email{ID: "M2"}})
	waitFor(t, func() bool { return fast.count() == 1 })
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("fresh delivery took %v behind the retry backlog, want < 2s", elapsed)
	}
}

// The shutdown drain gives whatever is still queued one attempt, but must
// give up at drainDeadline. deliver's semaphore acquire used to be an
// unbounded block, so a pool held by someone else kept drain — and with it
// the whole process shutdown — waiting for as long as that took, however
// short the deadline was.
func TestDrainHonoursItsDeadlineWhenThePoolIsBusy(t *testing.T) {
	db := newTestStore(t)
	seedTenant(t, db)
	rcv := newReceiver(t, http.StatusOK)
	if err := db.SaveWebhook(model.Webhook{ID: "wh_1", DeveloperID: "dev_1", AccountID: "acc_1",
		URL: rcv.URL, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	d := NewDispatcher(db, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	// A one-slot pool, already taken: drain can never get a worker.
	d.sem = make(chan struct{}, 1)
	d.sem <- struct{}{}
	d.queue <- model.Event{Type: model.EventMailReceived, AccountID: "acc_1", Email: &model.Email{ID: "M1"}}

	done := make(chan struct{})
	go func() { d.drain(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(drainDeadline + 2*time.Second):
		t.Fatalf("drain did not return within %v of its %v deadline", 2*time.Second, drainDeadline)
	}
}

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
	// deliver is called directly (same package) rather than through Start/Emit;
	// sem is normally built by Start, so build it here since deliver needs it.
	d.sem = make(chan struct{}, 4)
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
	// deliver is called directly (same package) rather than through Start/Emit;
	// sem is normally built by Start, so build it here since deliver needs it.
	d.sem = make(chan struct{}, 4)
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
	// deliver is called directly (same package) rather than through Start/Emit;
	// sem is normally built by Start, so build it here since deliver needs it.
	d.sem = make(chan struct{}, 4)
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
	// deliver is called directly (same package) rather than through Start/Emit;
	// sem is normally built by Start, so build it here since deliver needs it.
	d.sem = make(chan struct{}, 4)
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
