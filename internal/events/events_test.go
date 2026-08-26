package events

import (
	"context"
	"encoding/json"
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
	"github.com/gauravrautela/unified-messaging/internal/store"
)

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
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
	d := NewDispatcher(db, log)
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
	d := NewDispatcher(db, log)
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
	d := NewDispatcher(db, log)
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
	d := NewDispatcher(db, log)
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
	d := NewDispatcher(db, log)
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
