package events

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gauravrautela/unified-messaging/internal/model"
	"github.com/gauravrautela/unified-messaging/internal/notify"
	"github.com/gauravrautela/unified-messaging/internal/store"
)

// signedReceiver records the body and signature header of each request.
type signedReceiver struct {
	*httptest.Server
	mu   sync.Mutex
	code int
	sigs []string
	body [][]byte
}

func newSignedReceiver(t *testing.T, code int) *signedReceiver {
	t.Helper()
	r := &signedReceiver{code: code}
	r.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		r.sigs = append(r.sigs, req.Header.Get("X-Outlook-Signature"))
		r.body = append(r.body, b)
		code := r.code
		r.mu.Unlock()
		w.WriteHeader(code)
	}))
	t.Cleanup(r.Close)
	return r
}

func (r *signedReceiver) hits() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.body)
}

func sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestSendTestIsSignedAndReportsAccepted(t *testing.T) {
	db := newTestStore(t)
	seedTenant(t, db)
	rcv := newSignedReceiver(t, http.StatusOK)
	h := model.Webhook{ID: "wh_1", DeveloperID: "dev_1", URL: rcv.URL, Secret: "s3cret",
		Events: []string{model.EventMailSent}, CreatedAt: time.Now()}
	if err := db.SaveWebhook(h); err != nil {
		t.Fatal(err)
	}
	d := newFastDispatcher(t, db, time.Hour)

	got, err := d.SendTest(context.Background(), h)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Accepted || got.DeliveryID == "" || got.Error != "" {
		t.Fatalf("attempt = %+v, want accepted", got)
	}
	if rcv.hits() != 1 {
		t.Fatalf("hits = %d, want 1 (the event filter does not apply to a test)", rcv.hits())
	}
	if rcv.sigs[0] != sign("s3cret", rcv.body[0]) {
		t.Fatalf("signature %q does not match the body", rcv.sigs[0])
	}
	var ev model.Event
	if err := json.Unmarshal(rcv.body[0], &ev); err != nil {
		t.Fatal(err)
	}
	if ev.Type != model.EventWebhookTest || ev.Webhook == nil || ev.Webhook.ID != "wh_1" {
		t.Fatalf("event = %+v", ev)
	}
	if q, _ := db.ListDeliveries("wh_1", 10, 0); len(q) != 0 {
		t.Fatalf("an accepted test left %d delivery rows", len(q))
	}
}

func TestSendTestFailureIsQueuedForRetry(t *testing.T) {
	db := newTestStore(t)
	seedTenant(t, db)
	rcv := newSignedReceiver(t, http.StatusInternalServerError)
	h := model.Webhook{ID: "wh_1", DeveloperID: "dev_1", URL: rcv.URL, CreatedAt: time.Now()}
	if err := db.SaveWebhook(h); err != nil {
		t.Fatal(err)
	}
	d := newFastDispatcher(t, db, time.Hour)

	got, err := d.SendTest(context.Background(), h)
	if err != nil {
		t.Fatal(err)
	}
	if got.Accepted || got.Error == "" {
		t.Fatalf("attempt = %+v, want refused with an error", got)
	}
	q, _ := db.ListDeliveries("wh_1", 10, 0)
	if len(q) != 1 || q[0].ID != got.DeliveryID || q[0].Attempts != 1 || q[0].Dead {
		t.Fatalf("delivery log = %+v, want one live retry", q)
	}
}

func seedDead(t *testing.T, db *store.Store, id string, created time.Time) []byte {
	t.Helper()
	payload := []byte(`{"type":"mail_received","account_id":"acc_1","timestamp":"2026-09-01T00:00:00Z","webhook":{"id":"wh_1"},"email":{"id":"M1"}}`)
	if err := db.SaveDelivery(store.Delivery{ID: id, WebhookID: "wh_1", AccountID: "acc_1",
		EventType: model.EventMailReceived, Payload: payload, Attempts: 8, Dead: true,
		LastError: "status 500", NextAttemptAt: created, CreatedAt: created}); err != nil {
		t.Fatal(err)
	}
	return payload
}

func TestRedeliverSendsStoredPayloadAndClearsRow(t *testing.T) {
	db := newTestStore(t)
	seedTenant(t, db)
	rcv := newSignedReceiver(t, http.StatusOK)
	h := model.Webhook{ID: "wh_1", DeveloperID: "dev_1", URL: rcv.URL, Secret: "s3cret", CreatedAt: time.Now()}
	if err := db.SaveWebhook(h); err != nil {
		t.Fatal(err)
	}
	payload := seedDead(t, db, "dl_1", time.Now().Add(-time.Hour))
	d := newFastDispatcher(t, db, time.Hour)

	got, err := d.Redeliver(context.Background(), h, "dl_1")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Accepted || got.DeliveryID != "dl_1" {
		t.Fatalf("attempt = %+v", got)
	}
	if string(rcv.body[0]) != string(payload) || rcv.sigs[0] != sign("s3cret", payload) {
		t.Fatalf("redelivered %s / %s, want the stored payload, signed", rcv.body[0], rcv.sigs[0])
	}
	if _, err := db.GetDelivery("wh_1", "dl_1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("accepted redelivery left its row: %v", err)
	}
	// Accepted deliveries are not kept, so they cannot be replayed.
	if _, err := d.Redeliver(context.Background(), h, "dl_1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("replaying an accepted delivery: err = %v, want not found", err)
	}
}

func TestRedeliverRefusedGoesBackOnSchedule(t *testing.T) {
	db := newTestStore(t)
	seedTenant(t, db)
	rcv := newSignedReceiver(t, http.StatusBadGateway)
	h := model.Webhook{ID: "wh_1", DeveloperID: "dev_1", URL: rcv.URL, CreatedAt: time.Now()}
	if err := db.SaveWebhook(h); err != nil {
		t.Fatal(err)
	}
	seedDead(t, db, "dl_1", time.Now().Add(-time.Hour))
	d := newFastDispatcher(t, db, time.Hour)

	got, err := d.Redeliver(context.Background(), h, "dl_1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Accepted || got.Error == "" {
		t.Fatalf("attempt = %+v, want refused", got)
	}
	dl, err := db.GetDelivery("wh_1", "dl_1")
	if err != nil {
		t.Fatal(err)
	}
	if dl.Dead || dl.Attempts != 1 || time.Until(dl.NextAttemptAt) < 30*time.Minute {
		t.Fatalf("row = %+v, want live with attempt 1 and the schedule's next retry", dl)
	}
}

func TestRedeliverRefusesLiveExpiredAndConcurrent(t *testing.T) {
	db := newTestStore(t)
	seedTenant(t, db)
	rcv := newSignedReceiver(t, http.StatusOK)
	h := model.Webhook{ID: "wh_1", DeveloperID: "dev_1", URL: rcv.URL, CreatedAt: time.Now()}
	if err := db.SaveWebhook(h); err != nil {
		t.Fatal(err)
	}
	d := newFastDispatcher(t, db, time.Hour)
	ctx := context.Background()

	// Still retrying.
	if err := db.SaveDelivery(store.Delivery{ID: "dl_live", WebhookID: "wh_1", EventType: "mail_received",
		Payload: []byte(`{}`), Attempts: 2, NextAttemptAt: time.Now().Add(time.Hour), CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Redeliver(ctx, h, "dl_live"); !errors.Is(err, ErrNotDead) {
		t.Fatalf("live delivery: err = %v, want ErrNotDead", err)
	}

	// Older than the developer's retention age.
	if err := db.SetRetentionMaxAge("dev_1", 3600); err != nil {
		t.Fatal(err)
	}
	seedDead(t, db, "dl_old", time.Now().Add(-2*time.Hour))
	if _, err := d.Redeliver(ctx, h, "dl_old"); !errors.Is(err, ErrExpired) {
		t.Fatalf("expired delivery: err = %v, want ErrExpired", err)
	}
	seedDead(t, db, "dl_young", time.Now().Add(-30*time.Minute))
	if got, err := d.Redeliver(ctx, h, "dl_young"); err != nil || !got.Accepted {
		t.Fatalf("delivery inside retention: %+v, %v", got, err)
	}

	// Another hook's delivery is not this hook's.
	if _, err := d.Redeliver(ctx, model.Webhook{ID: "wh_other", DeveloperID: "dev_1"}, "dl_old"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("foreign hook: err = %v, want not found", err)
	}

	// Two claims on one dead row: only one wins.
	seedDead(t, db, "dl_race", time.Now())
	first, err := db.ClaimDeadDelivery("dl_race", time.Now().Add(time.Minute))
	if err != nil || !first {
		t.Fatalf("first claim = %v, %v", first, err)
	}
	if second, err := db.ClaimDeadDelivery("dl_race", time.Now().Add(time.Minute)); err != nil || second {
		t.Fatalf("second claim = %v, %v, want false", second, err)
	}
}

// A test event reaches Discord and Telegram through their own senders.
func TestSendTestReachesDiscordAndTelegram(t *testing.T) {
	db := newTestStore(t)
	seedTenant(t, db)
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, r.URL.Path+" "+string(b))
		mu.Unlock()
		if strings.HasPrefix(r.URL.Path, "/bot") {
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	reg := notify.NewRegistry(nil)
	reg.SetTelegramBase(srv.URL)
	d := NewDispatcher(db, reg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	now := time.Now().UTC()
	for _, h := range []model.Webhook{
		{ID: "wh_d", DeveloperID: "dev_1", Kind: "discord", URL: srv.URL + "/api/webhooks/1/t", CreatedAt: now},
		{ID: "wh_t", DeveloperID: "dev_1", Kind: "telegram", Telegram: &model.TelegramTarget{BotToken: "1:A", ChatID: "-5"}, CreatedAt: now},
	} {
		got, err := d.SendTest(context.Background(), h)
		if err != nil || !got.Accepted {
			t.Fatalf("%s: %+v, %v", h.Kind, got, err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 || !strings.Contains(bodies[0], "Test event") || !strings.HasPrefix(bodies[1], "/bot1:A/sendMessage") {
		t.Fatalf("bodies = %q", bodies)
	}
}
