package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gauravrautela/unified-messaging/internal/model"
	"github.com/gauravrautela/unified-messaging/internal/store"
)

// seedFilterDeliveries stores six deliveries on wh_1, one second apart: even
// indexes are dead, odd ones pending.
func seedFilterDeliveries(t *testing.T, db *store.Store, devID string) (base time.Time) {
	t.Helper()
	base = time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	if err := db.SaveWebhook(model.Webhook{ID: "wh_1", DeveloperID: devID, URL: "https://x.example.com", CreatedAt: base}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		c := base.Add(time.Duration(i) * time.Second)
		if err := db.SaveDelivery(store.Delivery{ID: "dl_" + strconv.Itoa(i), WebhookID: "wh_1", EventType: "mail_received",
			Payload: []byte(`{}`), Attempts: 1, Dead: i%2 == 0, NextAttemptAt: c, CreatedAt: c}); err != nil {
			t.Fatal(err)
		}
	}
	return base
}

func listDeliveries(t *testing.T, s *Server, key, query string, want int) []store.Delivery {
	t.Helper()
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, withKey(httptest.NewRequest(http.MethodGet, "/api/v1/webhooks/wh_1/deliveries"+query, nil), key))
	if rec.Code != want {
		t.Fatalf("%s: status = %d, want %d (body %s)", query, rec.Code, want, rec.Body.String())
	}
	if want != http.StatusOK {
		return nil
	}
	var list listResponse[store.Delivery]
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	return list.Items
}

func ids(ds []store.Delivery) string {
	var out []string
	for _, d := range ds {
		out = append(out, d.ID)
	}
	return strings.Join(out, ",")
}

func TestListWebhookDeliveriesFilters(t *testing.T) {
	s, db := newTestServer(t)
	dev, key := seedDev(t, s, "a@x.com")
	base := seedFilterDeliveries(t, db, dev.ID)
	since := base.Add(2 * time.Second).Format(time.RFC3339)

	for _, tc := range []struct{ query, want string }{
		{"", "dl_0,dl_1,dl_2,dl_3,dl_4,dl_5"},
		{"?status=dead", "dl_0,dl_2,dl_4"},
		{"?status=pending", "dl_1,dl_3,dl_5"},
		{"?since=" + since, "dl_2,dl_3,dl_4,dl_5"},
		{"?status=dead&since=" + since, "dl_2,dl_4"},
		// paging counts only matching rows, across two pages
		{"?status=pending&limit=2", "dl_1,dl_3"},
		{"?status=pending&limit=2&offset=2", "dl_5"},
		{"?status=dead&since=" + since + "&limit=1&offset=1", "dl_4"},
	} {
		if got := ids(listDeliveries(t, s, key, tc.query, http.StatusOK)); got != tc.want {
			t.Errorf("%q: got %s, want %s", tc.query, got, tc.want)
		}
	}
}

func TestListWebhookDeliveriesFilterErrors(t *testing.T) {
	s, db := newTestServer(t)
	dev, key := seedDev(t, s, "a@x.com")
	seedFilterDeliveries(t, db, dev.ID)
	for _, q := range []string{"?status=delivered", "?status=DEAD", "?since=yesterday", "?since=1700000000", "?status=dead&since=nope"} {
		listDeliveries(t, s, key, q, http.StatusBadRequest)
	}
}

func TestListWebhookDeliveriesEmptyIsArrayAndOtherTenant404(t *testing.T) {
	s, db := newTestServer(t)
	dev, key := seedDev(t, s, "a@x.com")
	seedFilterDeliveries(t, db, dev.ID)

	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, withKey(httptest.NewRequest(http.MethodGet,
		"/api/v1/webhooks/wh_1/deliveries?since="+time.Now().Add(time.Hour).UTC().Format(time.RFC3339), nil), key))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"items":[]`) {
		t.Fatalf("empty result: status %d body %s, want items []", rec.Code, rec.Body.String())
	}

	_, otherKey := seedDev(t, s, "b@x.com")
	listDeliveries(t, s, otherKey, "?status=dead", http.StatusNotFound)
}
