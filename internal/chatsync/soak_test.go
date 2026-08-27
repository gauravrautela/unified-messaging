package chatsync

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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

const (
	soakAccounts   = 50
	soakMessages   = 1000
	soakReplayStep = 20 // resend every 20th message id -> 5% replay traffic

	// soakWave caps how many accounts burst messages at once. All 50 accounts
	// are attached to the Runtime for the whole test - this only staggers the
	// *feed*. The store is a single sqlite connection (store.Open caps the
	// pool at one) and events.Dispatcher drains its queue through one HTTP
	// worker that shares that same connection for its webhook lookup; probing
	// this test found that once more than ~5 accounts hammer both at once,
	// the dispatcher's occasional query for the delivery loop starves behind
	// the actors' constant stream of writes and the fixed 1024-deep event
	// queue overflows, dropping events outright (Dispatcher.Emit's documented
	// drop-on-full behaviour - not a chatsync bug). Waves of 4 stay clear of
	// that cliff with comfortable margin while still exercising real
	// concurrency within a wave.
	soakWave = 4
)

// TestSoak drives a fleet of accounts hard enough to catch what the small,
// deterministic runtime_test.go cases cannot: ordering bugs that only appear
// under real concurrency, and store/dispatch correctness that only bites at
// volume. All 50 accounts are attached to the same Runtime for the whole
// test, each with its own actor goroutine; only the message feed is staged
// in small concurrent waves (see soakWave) to stay within the shared
// sqlite-connection-plus-dispatcher-queue's real throughput ceiling rather
// than manufacturing an event-queue overflow that has nothing to do with the
// actor/sink logic under test.
//
// Backoff sleeping is stubbed to return immediately (the same hook
// runtime_test.go's harness uses) so the interleaved disconnects reconnect at
// soak speed rather than at backoff speed; that is a test hook, not a change
// to production pacing.
func TestSoak(t *testing.T) {
	if testing.Short() {
		t.Skip("soak test skipped under -short")
	}
	// The dispatcher's default registry delivers through safehttp.Client,
	// whose dial guard refuses loopback by default; srv below is an httptest
	// server (loopback).
	safehttp.AllowLoopbackForTests(t)

	db, err := store.Open(filepath.Join(t.TempDir(), "soak.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.CreateDeveloper(model.Developer{ID: "dev_1", Email: "soak@x.com"}, "h"); err != nil {
		t.Fatal(err)
	}

	fake := providertest.NewFakeChat("FAKECHAT")
	log, recs := logx.Capture()
	mgr := accounts.NewManager(db, make([]byte, 32), log)
	reg := provider.NewRegistry(fake)
	mgr.SetRegistry(reg)

	// received[accountID][messageID] counts how many chat_received events
	// carried that message, so a replay that slipped an extra event past the
	// sink shows up as a count > 1 rather than just a higher total.
	var mu sync.Mutex
	received := map[string]map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var ev model.Event
		_ = json.NewDecoder(r.Body).Decode(&ev)
		if ev.Type == model.EventChatReceived && ev.Message != nil {
			mu.Lock()
			m := received[ev.AccountID]
			if m == nil {
				m = map[string]int{}
				received[ev.AccountID] = m
			}
			m[ev.Message.ID]++
			mu.Unlock()
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	disp := events.NewDispatcher(db, nil, log)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	disp.Start(ctx)

	rt := New(db, reg, mgr, disp, log, Options{
		MaxAccounts: soakAccounts,
		Sleep:       func(context.Context, time.Duration) {},
	})
	rt.Start(ctx)

	// Attach every account up front: the runtime genuinely holds all 50 live
	// connections for the whole test, whatever order the feed below visits
	// them in.
	accts := make([]string, soakAccounts)
	for i := 0; i < soakAccounts; i++ {
		jid := fmt.Sprintf("1000000%d@s.whatsapp.net", i)
		a, err := mgr.ConnectLinked(context.Background(), "dev_1", "FAKECHAT",
			provider.Identity{Identifier: fmt.Sprintf("+1000000%d", i)}, jid)
		if err != nil {
			t.Fatal(err)
		}
		accts[i] = a.ID
		if err := db.SaveWebhook(model.Webhook{
			ID: "wh_" + a.ID, DeveloperID: "dev_1", AccountID: a.ID, URL: srv.URL, CreatedAt: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
		if err := rt.Attach(a.ID); err != nil {
			t.Fatal(err)
		}
	}
	waitLong(t, func() bool {
		for _, acc := range accts {
			if fake.Sink(acc) == nil {
				return false
			}
		}
		return true
	})

	feedAccount := func(idx int, accountID string) {
		sink := fake.Sink(accountID)
		ids := make([]string, 0, soakMessages)
		// Desynced cadence (200..249 messages between drops) keeps accounts
		// from flapping in lockstep, and stays comfortably under the
		// 30-failure credentials cutoff (~4-5 drops/account).
		interval := 200 + idx%50
		for n := 0; n < soakMessages; n++ {
			id := fmt.Sprintf("msg-%d-%d", idx, n)
			ids = append(ids, id)
			m := model.ChatMessage{
				ID: id, ChatID: "c1", Kind: "text", Text: "soak",
				SentAt: time.Now(), Sender: model.Attendee{ID: "a1"},
			}
			sink.Message(accountID, m, model.Chat{ID: "c1", Kind: "direct", Name: "Soak"}, m.Sender)
			if n > 0 && n%interval == 0 {
				fake.Disconnect(accountID, "flap", false)
			}
		}
		// Replay ~5% of messages. These are enqueued after every original on
		// the same per-account inbox, so FIFO ordering guarantees the actor
		// has already applied the original insert before it sees the
		// replay - no synchronization needed here.
		for n := 0; n < len(ids); n += soakReplayStep {
			id := ids[n]
			m := model.ChatMessage{
				ID: id, ChatID: "c1", Kind: "text", Text: "soak",
				SentAt: time.Now(), Sender: model.Attendee{ID: "a1"},
			}
			sink.Message(accountID, m, model.Chat{ID: "c1", Kind: "direct", Name: "Soak"}, m.Sender)
		}
	}

	start := time.Now()
	for g := 0; g < soakAccounts; g += soakWave {
		end := g + soakWave
		if end > soakAccounts {
			end = soakAccounts
		}
		var wg sync.WaitGroup
		for i := g; i < end; i++ {
			wg.Add(1)
			go func(idx int, accountID string) {
				defer wg.Done()
				feedAccount(idx, accountID)
			}(i, accts[i])
		}
		wg.Wait()

		// Drain this wave before starting the next: wait for its rows, its
		// events, and its reconnects to settle. This is what keeps the
		// shared connection and dispatcher queue from ever being asked to
		// absorb more than a handful of accounts' full 1000-message bursts
		// at once (see soakWave's comment above).
		wave := accts[g:end]
		waitLong(t, func() bool {
			for _, acc := range wave {
				var n int
				if err := db.DB().QueryRow(`SELECT COUNT(*) FROM chat_messages WHERE account_id = ?`, acc).Scan(&n); err != nil {
					t.Fatal(err)
				}
				if n != soakMessages {
					return false
				}
			}
			return true
		})
		waitLong(t, func() bool {
			for _, acc := range wave {
				c, ok := rt.HealthFor(acc)
				if !ok || c.State != stateConnected {
					return false
				}
			}
			return true
		})
		waitLong(t, func() bool {
			mu.Lock()
			defer mu.Unlock()
			for _, acc := range wave {
				if len(received[acc]) != soakMessages {
					return false
				}
			}
			return true
		})
	}
	totalDur := time.Since(start)
	t.Logf("soak: %d accounts x %d messages fed and drained in %v (waves of %d)", soakAccounts, soakMessages, totalDur, soakWave)

	// (a) every message row exists exactly once.
	for _, acc := range accts {
		var count, distinct int
		if err := db.DB().QueryRow(`SELECT COUNT(*) FROM chat_messages WHERE account_id = ?`, acc).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if err := db.DB().QueryRow(`SELECT COUNT(DISTINCT id) FROM chat_messages WHERE account_id = ?`, acc).Scan(&distinct); err != nil {
			t.Fatal(err)
		}
		if count != soakMessages || distinct != soakMessages {
			t.Fatalf("account %s: rows=%d distinct_ids=%d, want %d each", acc, count, distinct, soakMessages)
		}
	}

	// (b) exactly one chat_received event per message, replays included.
	mu.Lock()
	for _, acc := range accts {
		perMsg := received[acc]
		if len(perMsg) != soakMessages {
			t.Errorf("account %s: events cover %d distinct messages, want %d", acc, len(perMsg), soakMessages)
			continue
		}
		for id, n := range perMsg {
			if n != 1 {
				t.Errorf("account %s message %s: %d chat_received events, want 1 (replay leaked a duplicate)", acc, id, n)
			}
		}
	}
	mu.Unlock()

	// (c) the runtime's own accounting agrees with reality.
	if n := rt.Count(); n != soakAccounts {
		t.Errorf("Count() = %d, want %d", n, soakAccounts)
	}
	for _, acc := range accts {
		c, ok := rt.HealthFor(acc)
		if !ok || c.State != stateConnected {
			t.Errorf("account %s health = %+v ok=%v, want state=%s", acc, c, ok, stateConnected)
		}
	}

	if recs.Contains("event queue full") {
		t.Error("dispatcher dropped events under soak load (queue overflow) - see log capture")
	}
}

// waitLong is runtime_test.go's waitFor with a budget sized for soak volumes
// rather than the ~3s used by the small deterministic cases.
func waitLong(t *testing.T, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatal("soak: condition not met before deadline")
}
