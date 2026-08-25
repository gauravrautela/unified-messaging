package events

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

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
	mine := newReceiver(t, http.StatusOK)
	theirs := newReceiver(t, http.StatusOK)
	global := newReceiver(t, http.StatusOK)

	now := time.Now()
	for _, w := range []model.Webhook{
		{ID: "wh_mine", AccountID: "acc_1", URL: mine.URL, CreatedAt: now},
		{ID: "wh_theirs", AccountID: "acc_2", URL: theirs.URL, CreatedAt: now},
		{ID: "wh_global", URL: global.URL, CreatedAt: now},
	} {
		if err := db.SaveWebhook(w); err != nil {
			t.Fatal(err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := NewDispatcher(db, slog.New(slog.NewTextHandler(io.Discard, nil)))
	d.Start(ctx)

	d.Emit(model.Event{Type: model.EventMailReceived, AccountID: "acc_1", Email: &model.Email{ID: "M1"}})

	waitFor(t, func() bool { return mine.count() == 1 && global.count() == 1 })
	// Give a stray delivery a moment to show up before asserting it did not.
	time.Sleep(50 * time.Millisecond)
	if theirs.count() != 0 {
		t.Fatalf("acc_2's webhook received acc_1's mail (%d deliveries)", theirs.count())
	}
}

func (r *receiver) setCode(code int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.code = code
}

func newFastDispatcher(t *testing.T, db *store.Store, schedule ...time.Duration) *Dispatcher {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	d := NewDispatcher(db, slog.New(slog.NewTextHandler(io.Discard, nil)))
	d.RetrySchedule = schedule
	d.RetryPoll = 20 * time.Millisecond
	d.Start(ctx)
	return d
}

// A non-2xx does not lose the event: it is parked in the store and re-sent on
// the schedule once the subscriber recovers, surviving anything in between.
func TestFailedDeliveryIsQueuedAndRetried(t *testing.T) {
	db := newTestStore(t)
	rcv := newReceiver(t, http.StatusInternalServerError)
	if err := db.SaveWebhook(model.Webhook{ID: "wh_1", URL: rcv.URL, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	d := newFastDispatcher(t, db, 10*time.Millisecond, time.Hour)

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
}

func TestDeliveryIsDeadAfterScheduleExhausted(t *testing.T) {
	db := newTestStore(t)
	rcv := newReceiver(t, http.StatusBadGateway)
	if err := db.SaveWebhook(model.Webhook{ID: "wh_1", URL: rcv.URL, CreatedAt: time.Now()}); err != nil {
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
	rcv := newReceiver(t, http.StatusOK)
	if err := db.SaveWebhook(model.Webhook{ID: "wh_1", Name: "crm-sync", AccountID: "acc_1", URL: rcv.URL, CreatedAt: time.Now()}); err != nil {
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
