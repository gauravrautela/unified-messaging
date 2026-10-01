package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gauravrautela/unified-messaging/internal/events"
	"github.com/gauravrautela/unified-messaging/internal/model"
	"github.com/gauravrautela/unified-messaging/internal/store"
)

func post(t *testing.T, s *Server, key, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, withKey(httptest.NewRequest(http.MethodPost, path, nil), key))
	return rec
}

func TestWebhookTestReportsAcceptance(t *testing.T) {
	s, db := newTestServer(t)
	dev, key := seedDev(t, s, "a@x.com")
	var code atomic.Int32
	code.Store(http.StatusOK)
	rcv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(int(code.Load()))
	}))
	t.Cleanup(rcv.Close)
	if err := db.SaveWebhook(model.Webhook{ID: "wh_1", DeveloperID: dev.ID, URL: rcv.URL, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}

	rec := post(t, s, key, "/api/v1/webhooks/wh_1/test")
	var got events.Attempt
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &got) != nil || !got.Accepted {
		t.Fatalf("accepted test: %d %s", rec.Code, rec.Body.String())
	}

	code.Store(http.StatusInternalServerError)
	rec = post(t, s, key, "/api/v1/webhooks/wh_1/test")
	got = events.Attempt{}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &got) != nil || got.Accepted || got.Error == "" {
		t.Fatalf("refused test: %d %s", rec.Code, rec.Body.String())
	}
	if q, _ := db.ListDeliveries("wh_1", 10, 0); len(q) != 1 || q[0].ID != got.DeliveryID {
		t.Fatalf("refused test not in the delivery log: %+v", q)
	}

	if rec := post(t, s, key, "/api/v1/webhooks/wh_nope/test"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown webhook: %d", rec.Code)
	}
}

func TestRedeliverStatuses(t *testing.T) {
	s, db := newTestServer(t)
	dev, key := seedDev(t, s, "a@x.com")
	rcv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	t.Cleanup(rcv.Close)
	if err := db.SaveWebhook(model.Webhook{ID: "wh_1", DeveloperID: dev.ID, URL: rcv.URL, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := db.SetRetentionMaxAge(dev.ID, 3600); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for _, dl := range []store.Delivery{
		{ID: "dl_dead", Dead: true, CreatedAt: now.Add(-time.Minute)},
		{ID: "dl_live", Dead: false, CreatedAt: now},
		{ID: "dl_old", Dead: true, CreatedAt: now.Add(-2 * time.Hour)},
	} {
		dl.WebhookID, dl.EventType, dl.Payload, dl.Attempts, dl.NextAttemptAt = "wh_1", "mail_received", []byte(`{"type":"mail_received"}`), 8, now.Add(time.Hour)
		if err := db.SaveDelivery(dl); err != nil {
			t.Fatal(err)
		}
	}

	for _, tc := range []struct {
		id   string
		want int
	}{
		{"dl_dead", http.StatusOK},
		{"dl_dead", http.StatusNotFound}, // accepted, so gone: never replayed
		{"dl_live", http.StatusConflict},
		{"dl_old", http.StatusGone},
		{"dl_nope", http.StatusNotFound},
	} {
		rec := post(t, s, key, "/api/v1/webhooks/wh_1/deliveries/"+tc.id+"/redeliver")
		if rec.Code != tc.want {
			t.Errorf("%s: status = %d, want %d (%s)", tc.id, rec.Code, tc.want, rec.Body.String())
		}
	}
}
