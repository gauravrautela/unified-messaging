package api

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/gauravrautela/unified-messaging/internal/model"
	"github.com/gauravrautela/unified-messaging/internal/store"
)

// Pause and resume answer with the hook as it now stands, never its secret;
// repeating either is not an error; the hook's setup and delivery log come
// through both unchanged.
func TestPauseAndResumeWebhook(t *testing.T) {
	s, db := newTestServer(t)
	dev, key := seedDev(t, s, "a@x.com")
	in := model.Webhook{ID: "wh_1", DeveloperID: dev.ID, Name: "prod", URL: "https://h.example.com/in",
		Secret: "whsec_1", Events: []string{"mail_received"}, CreatedAt: time.Now()}
	if err := db.SaveWebhook(in); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := db.SaveDelivery(store.Delivery{ID: "dl_1", WebhookID: "wh_1", EventType: "mail_received",
		Payload: []byte(`{}`), Attempts: 8, Dead: true, NextAttemptAt: now, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}

	for _, step := range []struct {
		path   string
		paused bool
	}{
		{"/api/v1/webhooks/wh_1/pause", true},
		{"/api/v1/webhooks/wh_1/pause", true},
		{"/api/v1/webhooks/wh_1/resume", false},
		{"/api/v1/webhooks/wh_1/resume", false},
	} {
		rec := post(t, s, key, step.path)
		var got model.Webhook
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &got) != nil {
			t.Fatalf("%s: %d %s", step.path, rec.Code, rec.Body.String())
		}
		if got.Paused != step.paused || got.Secret != "" || got.ID != "wh_1" || got.Name != "prod" ||
			got.URL != in.URL || len(got.Events) != 1 {
			t.Fatalf("%s: answered %+v", step.path, got)
		}
		stored, err := db.GetWebhook(dev.ID, "wh_1")
		if err != nil || stored.Paused != step.paused || stored.Secret != "whsec_1" {
			t.Fatalf("%s: stored %+v, %v", step.path, stored, err)
		}
		if q, _ := db.ListDeliveries("wh_1", 10, 0); len(q) != 1 || q[0].ID != "dl_1" {
			t.Fatalf("%s: delivery log changed: %+v", step.path, q)
		}
	}

	for _, path := range []string{"/api/v1/webhooks/wh_nope/pause", "/api/v1/webhooks/wh_nope/resume"} {
		if rec := post(t, s, key, path); rec.Code != http.StatusNotFound {
			t.Fatalf("%s: %d", path, rec.Code)
		}
	}
}
