package chatsync

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gauravrautela/unified-messaging/internal/accounts"
	"github.com/gauravrautela/unified-messaging/internal/events"
	"github.com/gauravrautela/unified-messaging/internal/logx"
	"github.com/gauravrautela/unified-messaging/internal/model"
	"github.com/gauravrautela/unified-messaging/internal/provider"
	"github.com/gauravrautela/unified-messaging/internal/provider/providertest"
	"github.com/gauravrautela/unified-messaging/internal/safehttp"
	"github.com/gauravrautela/unified-messaging/internal/store"
)

type harness struct {
	db     *store.Store
	fake   *providertest.FakeChat
	mgr    *accounts.Manager
	rt     *Runtime
	recs   *logx.Records
	sleeps []time.Duration
	mu     sync.Mutex
	hook   *httptest.Server
	got    []model.Event
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	// The dispatcher's default registry delivers through safehttp.Client,
	// whose dial guard refuses loopback by default; h.hook below is an
	// httptest server (loopback).
	safehttp.AllowLoopbackForTests(t)
	db, err := store.Open(filepath.Join(t.TempDir(), "cs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	_ = db.CreateDeveloper(model.Developer{ID: "dev_1", Email: "d@x.com"}, "h")
	h := &harness{db: db, fake: providertest.NewFakeChat("FAKECHAT")}
	h.hook = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var ev model.Event
		_ = json.NewDecoder(r.Body).Decode(&ev)
		h.mu.Lock()
		h.got = append(h.got, ev)
		h.mu.Unlock()
	}))
	t.Cleanup(h.hook.Close)
	_ = db.SaveWebhook(model.Webhook{ID: "wh_1", DeveloperID: "dev_1", URL: h.hook.URL, CreatedAt: time.Now()})
	log, recs := logx.Capture()
	h.recs = recs
	h.mgr = accounts.NewManager(db, make([]byte, 32), log)
	reg := provider.NewRegistry(h.fake)
	h.mgr.SetRegistry(reg)
	disp := events.NewDispatcher(db, nil, log)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	disp.Start(ctx)
	h.rt = New(db, reg, h.mgr, disp, log, Options{MaxAccounts: 2,
		Sleep: func(_ context.Context, d time.Duration) { h.mu.Lock(); h.sleeps = append(h.sleeps, d); h.mu.Unlock() }})
	h.rt.Start(ctx)
	return h
}

func (h *harness) link(t *testing.T, id string) string {
	t.Helper()
	a, err := h.mgr.ConnectLinked(context.Background(), "dev_1", "FAKECHAT", provider.Identity{Identifier: "+91" + id}, "j"+id)
	if err != nil {
		t.Fatal(err)
	}
	return a.ID
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatal("condition not met")
}

func (h *harness) events() []model.Event {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]model.Event(nil), h.got...)
}

func TestAttachConnectsAndReceivesMessage(t *testing.T) {
	h := newHarness(t)
	acc := h.link(t, "1")
	if err := h.rt.Attach(acc); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return h.fake.Sink(acc) != nil })
	waitFor(t, func() bool { c, ok := h.rt.HealthFor(acc); return ok && c.State == "connected" })
	if c, ok := h.rt.HealthFor(acc); !ok || c.State != "connected" {
		t.Fatalf("health = %+v %v", c, ok)
	}
	m := model.ChatMessage{ID: "M1", ChatID: "c1", Kind: "text", Text: "hello", SentAt: time.Now(), Sender: model.Attendee{ID: "a1", Phone: "+9199", Name: "Ada"}}
	h.fake.Sink(acc).Message(acc, m, model.Chat{ID: "c1", Kind: "direct", Name: "Ada"}, m.Sender)
	waitFor(t, func() bool { return len(h.events()) == 1 })
	ev := h.events()[0]
	if ev.Type != model.EventChatReceived || ev.Message == nil || ev.Message.ID != "M1" || ev.Chat == nil || ev.AccountID != acc {
		t.Fatalf("event = %+v", ev)
	}
	// Replay: no second event, store unchanged.
	h.fake.Sink(acc).Message(acc, m, model.Chat{ID: "c1", Kind: "direct"}, m.Sender)
	time.Sleep(100 * time.Millisecond)
	if len(h.events()) != 1 {
		t.Fatalf("replay emitted: %d events", len(h.events()))
	}
	c, _ := h.db.GetChat(acc, "c1")
	if c.UnreadCount != 1 {
		t.Fatalf("unread = %d", c.UnreadCount)
	}
	if h.recs.Contains("hello") || h.recs.Contains("+9199") {
		t.Fatal("message text or phone leaked into logs")
	}
	if !h.recs.Contains("component=chatsync") || !h.recs.Contains("conn_id=") || !h.recs.Contains("decision=new") || !h.recs.Contains("decision=replay") {
		t.Fatalf("expected decision logs: %v", h.recs.All())
	}
}

func TestOwnEchoAfterAPISendEmitsNothing(t *testing.T) {
	h := newHarness(t)
	acc := h.link(t, "1")
	_ = h.rt.Attach(acc)
	waitFor(t, func() bool { return h.fake.Sink(acc) != nil })
	// The API layer inserts the row before the echo arrives (Task 8); simulate that.
	_, _ = h.db.UpsertChatMessage(model.ChatMessage{AccountID: acc, ID: "S1", ChatID: "c1", IsFromMe: true, Kind: "text", Text: "x", SentAt: time.Now(), Status: "sent", Sender: model.Attendee{ID: "self"}})
	h.fake.Sink(acc).Message(acc, model.ChatMessage{ID: "S1", ChatID: "c1", IsFromMe: true, Kind: "text", Text: "x", SentAt: time.Now(), Sender: model.Attendee{ID: "self"}}, model.Chat{ID: "c1"}, model.Attendee{ID: "self"})
	time.Sleep(100 * time.Millisecond)
	if len(h.events()) != 0 {
		t.Fatalf("own echo emitted %d events", len(h.events()))
	}
	// A phone-originated own message (not in store) does emit chat_sent.
	h.fake.Sink(acc).Message(acc, model.ChatMessage{ID: "P1", ChatID: "c1", IsFromMe: true, Kind: "text", Text: "from phone", SentAt: time.Now(), Sender: model.Attendee{ID: "self"}}, model.Chat{ID: "c1"}, model.Attendee{ID: "self"})
	waitFor(t, func() bool { return len(h.events()) == 1 && h.events()[0].Type == model.EventChatSent })
}

func TestReceiptReactionEditDeleteMapping(t *testing.T) {
	h := newHarness(t)
	acc := h.link(t, "1")
	_ = h.rt.Attach(acc)
	waitFor(t, func() bool { return h.fake.Sink(acc) != nil })
	s := h.fake.Sink(acc)
	s.Message(acc, model.ChatMessage{ID: "M1", ChatID: "c1", Kind: "text", Text: "a", SentAt: time.Now(), Sender: model.Attendee{ID: "a1"}}, model.Chat{ID: "c1", Kind: "direct"}, model.Attendee{ID: "a1"})
	_, _ = h.db.UpsertChatMessage(model.ChatMessage{AccountID: acc, ID: "S1", ChatID: "c1", IsFromMe: true, Kind: "text", Text: "x", SentAt: time.Now(), Status: "sent", Sender: model.Attendee{ID: "self"}})
	s.Receipt(acc, "c1", []string{"S1"}, "read")
	s.Reaction(acc, "c1", "M1", model.Reaction{AttendeeID: "a1", Emoji: "👍", At: time.Now()})
	s.Edited(acc, "c1", "M1", "a!", time.Now())
	s.Deleted(acc, "c1", "M1")
	waitFor(t, func() bool { return len(h.events()) == 5 })
	types := []string{}
	for _, e := range h.events() {
		types = append(types, e.Type)
	}
	want := []string{model.EventChatReceived, model.EventChatUpdated, model.EventChatReaction, model.EventChatUpdated, model.EventChatDeleted}
	for i := range want {
		if types[i] != want[i] {
			t.Fatalf("events = %v want %v", types, want)
		}
	}
	m, _ := h.db.GetChatMessage(acc, "S1")
	if m.Status != "read" {
		t.Fatalf("receipt not applied: %+v", m)
	}
	m, _ = h.db.GetChatMessage(acc, "M1")
	if !m.Deleted || m.EditedAt == nil || len(m.Reactions) != 1 {
		t.Fatalf("mutations not applied: %+v", m)
	}
	if h.events()[3].Change != "edited" || h.events()[1].Status != "read" {
		t.Fatalf("payload fields: %+v %+v", h.events()[1], h.events()[3])
	}
}

func TestLogoutIsTerminalAndForgetsDevice(t *testing.T) {
	h := newHarness(t)
	acc := h.link(t, "1")
	_ = h.rt.Attach(acc)
	waitFor(t, func() bool { return h.fake.Sink(acc) != nil })
	h.fake.Disconnect(acc, "device removed", true)
	waitFor(t, func() bool { a, _ := h.db.GetAnyAccount(acc); return a.Status == model.AccountCredentials })
	waitFor(t, func() bool {
		for _, e := range h.events() {
			if e.Type == model.EventAccountError {
				return true
			}
		}
		return false
	})
	if len(h.fake.Forgotten()) != 1 {
		t.Fatal("device not forgotten")
	}
	if c, ok := h.rt.HealthFor(acc); ok && c.State != "stopped" {
		t.Fatalf("actor still alive: %+v", c)
	}
}

// A provider goroutine must never be parked forever on an inbox that stopped
// being drained. The actor's context is the sink's only escape from a full
// queue, so every terminal exit has to cancel it, not just Detach.
func TestSinkDoesNotBlockOnceTheActorHasStopped(t *testing.T) {
	h := newHarness(t)
	acc := h.link(t, "1")
	_ = h.rt.Attach(acc)
	waitFor(t, func() bool { return h.fake.Sink(acc) != nil })
	s := h.fake.Sink(acc)
	h.fake.Disconnect(acc, "device removed", true)
	waitFor(t, func() bool { a, _ := h.db.GetAnyAccount(acc); return a.Status == model.AccountCredentials })

	done := make(chan struct{})
	go func() {
		defer close(done)
		// One more than the inbox holds: the last call has to fall through to
		// the blocking send, and must come back out of it.
		for i := 0; i <= 1024; i++ {
			s.Message(acc, model.ChatMessage{ID: "F" + strconv.Itoa(i), ChatID: "c1", Kind: "text",
				SentAt: time.Now(), Sender: model.Attendee{ID: "a1"}},
				model.Chat{ID: "c1", Kind: "direct"}, model.Attendee{ID: "a1"})
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("sink still blocked on the inbox after the actor stopped")
	}
}

// The window between "the actor has given the account up" and "the supervisor
// has dropped its map entry" spans a store write and an emit. A relink landing
// in it must still get a connection.
func TestAttachReplacesATerminatingActor(t *testing.T) {
	h := newHarness(t)
	acc := h.link(t, "1")
	if err := h.rt.Attach(acc); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return h.fake.Sink(acc) != nil })

	h.rt.mu.Lock()
	old := h.rt.actors[acc]
	h.rt.mu.Unlock()
	old.terminate() // as markLoggedOut does, before its store work

	if n := h.rt.Count(); n != 0 {
		t.Fatalf("terminating actor counted as live: %d", n)
	}
	if err := h.rt.Attach(acc); err != nil {
		t.Fatal(err)
	}
	h.rt.mu.Lock()
	fresh := h.rt.actors[acc]
	h.rt.mu.Unlock()
	if fresh == old {
		t.Fatal("Attach reused the terminating actor")
	}
	waitFor(t, func() bool { c, ok := h.rt.HealthFor(acc); return ok && c.State == "connected" })

	// The old actor finishing must not evict its replacement.
	old.stop()
	time.Sleep(100 * time.Millisecond)
	h.rt.mu.Lock()
	still := h.rt.actors[acc]
	h.rt.mu.Unlock()
	if still != fresh {
		t.Fatalf("replacement evicted by the old actor's removal: %v", still)
	}
}

// The same window, end to end: logout, relink, attach, and a live socket again.
func TestRelinkAfterLogoutReattaches(t *testing.T) {
	h := newHarness(t)
	acc := h.link(t, "1")
	_ = h.rt.Attach(acc)
	waitFor(t, func() bool { return h.fake.Sink(acc) != nil })
	h.fake.Disconnect(acc, "device removed", true)
	waitFor(t, func() bool { a, _ := h.db.GetAnyAccount(acc); return a.Status == model.AccountCredentials })

	if got := h.link(t, "1"); got != acc {
		t.Fatalf("relink minted a new account: %s != %s", got, acc)
	}
	if err := h.rt.Attach(acc); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return h.fake.Sink(acc) != nil })
	waitFor(t, func() bool { c, ok := h.rt.HealthFor(acc); return ok && c.State == "connected" })
}

func TestTransientDisconnectBacksOffAndReconnects(t *testing.T) {
	h := newHarness(t)
	acc := h.link(t, "1")
	_ = h.rt.Attach(acc)
	waitFor(t, func() bool { return h.fake.Sink(acc) != nil })
	h.fake.Disconnect(acc, "network", false)
	waitFor(t, func() bool { h.mu.Lock(); defer h.mu.Unlock(); return len(h.sleeps) >= 1 })
	waitFor(t, func() bool { c, _ := h.rt.HealthFor(acc); return c.State == "connected" && c.Reconnects == 1 })
	h.mu.Lock()
	if h.sleeps[0] < 800*time.Millisecond || h.sleeps[0] > 1200*time.Millisecond {
		t.Fatalf("first backoff = %v, want ~1s ±20%%", h.sleeps[0])
	}
	h.mu.Unlock()
}

func TestConnectFailuresExhaustToCredentials(t *testing.T) {
	h := newHarness(t)
	acc := h.link(t, "1")
	h.fake.ConnectErr = errors.New("boom")
	_ = h.rt.Attach(acc)
	waitFor(t, func() bool { a, _ := h.db.GetAnyAccount(acc); return a.Status == model.AccountCredentials })
	h.mu.Lock()
	n := len(h.sleeps)
	h.mu.Unlock()
	if n != maxFailures-1 && n != maxFailures {
		t.Fatalf("sleeps = %d, want ~%d", n, maxFailures)
	}
}

// The last thing said about an account being given up must not be "backing
// off": the drop that reaches the cap decides to stop, so that line belongs
// below the decision, not above it. This is the disconnect path — the
// connect-failure path above already had the order right.
func TestExhaustionDoesNotLogBackingOffOnTheTerminalDrop(t *testing.T) {
	h := newHarness(t)
	acc := h.link(t, "1")
	_ = h.rt.Attach(acc)
	for i := 0; i < maxFailures; i++ {
		// Wait for this attempt's own connection before dropping it: dropping a
		// sink the actor has already abandoned is a no-op and would silently
		// shorten the run.
		waitFor(t, func() bool {
			c, ok := h.rt.HealthFor(acc)
			return ok && c.State == "connected" && c.Reconnects == i
		})
		h.fake.Disconnect(acc, "network", false)
	}
	waitFor(t, func() bool { a, _ := h.db.GetAnyAccount(acc); return a.Status == model.AccountCredentials })

	var backingOff, gaveUp, lastGiveUp, lastBackOff int
	lastGiveUp, lastBackOff = -1, -1
	for i, line := range h.recs.All() {
		if strings.Contains(line, "disconnected, backing off") {
			backingOff++
			lastBackOff = i
		}
		if strings.Contains(line, "giving up on chat account") {
			gaveUp++
			lastGiveUp = i
		}
	}
	if gaveUp != 1 {
		t.Fatalf("give-up lines = %d, want 1", gaveUp)
	}
	if lastBackOff > lastGiveUp {
		t.Fatal(`"backing off" was logged after the decision to give up`)
	}
	if backingOff >= maxFailures {
		t.Fatalf("backing off logged %d times over %d drops: the terminal drop logged it too",
			backingOff, maxFailures)
	}
}

func TestCapacityAndStartAttachesOnlyOKChatAccounts(t *testing.T) {
	h := newHarness(t)
	a := h.link(t, "1")
	b := h.link(t, "2")
	c := h.link(t, "3")
	_ = h.db.SetAccountStatus(c, model.AccountCredentials)
	_ = h.db.UpsertAccount(model.Account{ID: "acc_mail", DeveloperID: "dev_1", Provider: "OUTLOOK", Email: "m@x.com", Status: model.AccountOK})
	h.rt.Start(context.Background()) // idempotent re-scan
	waitFor(t, func() bool { return h.fake.Sink(a) != nil && h.fake.Sink(b) != nil })
	if h.fake.Sink(c) != nil || h.rt.Count() != 2 {
		t.Fatalf("attached wrong set: count=%d", h.rt.Count())
	}
	d := h.link(t, "4")
	if err := h.rt.Attach(d); !errors.Is(err, ErrCapacity) {
		t.Fatalf("over capacity err = %v", err)
	}
	h.rt.Detach(a)
	waitFor(t, func() bool { return h.fake.Sink(a) == nil })
	if err := h.rt.Attach(d); err != nil {
		t.Fatal(err)
	}
}

// Spec §3, /docs §7.6, the README and llms.txt all promise a 5-minute
// ceiling. Jittering after the cap made the real ceiling 6 minutes.
func TestBackoffNeverExceedsTheDocumentedCap(t *testing.T) {
	for attempt := 0; attempt < 40; attempt++ {
		for i := 0; i < 200; i++ {
			d := next(attempt)
			if d > maxBackoff {
				t.Fatalf("next(%d) = %v, above the documented %v cap", attempt, d, maxBackoff)
			}
			if d <= 0 {
				t.Fatalf("next(%d) = %v, must be positive", attempt, d)
			}
		}
	}
	// Jitter still spreads a fleet out at the cap rather than collapsing to it.
	spread := map[time.Duration]bool{}
	for i := 0; i < 200; i++ {
		spread[next(20)] = true
	}
	if len(spread) < 10 {
		t.Fatalf("capping removed the jitter: %d distinct waits at the ceiling", len(spread))
	}
}

// A finishLink that lands after the runtime has stopped must not start a
// doomed connection — and must not wg.Add(1) after Wait() returned, which is
// a sync.WaitGroup reuse hazard.
func TestAttachAfterShutdownIsRefused(t *testing.T) {
	h := newHarness(t)
	acc := h.link(t, "1")
	ctx, cancel := context.WithCancel(context.Background())
	h.rt.ctx = ctx // the harness started the runtime on its own context
	cancel()
	h.rt.Wait()

	err := h.rt.Attach(acc)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Attach after shutdown = %v, want context.Canceled", err)
	}
	if h.fake.Sink(acc) != nil {
		t.Fatal("Attach after shutdown opened a connection anyway")
	}
}
