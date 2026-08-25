package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gauravrautela/unified-messaging/internal/model"
	"github.com/gauravrautela/unified-messaging/internal/store"
)

// Developer B must not be able to see or touch anything of developer A's,
// and must learn nothing from the attempt: every route answers 404 (or 400
// when the id is carried in a body that fails before ownership). Adding a
// route to the server without adding it here fails the test, so scoping is
// a decision made per route, never an accident.
func TestCrossTenantAccessIs404(t *testing.T) {
	s, db := newTestServer(t)
	h := s.Routes()
	devA, _ := seedDev(t, s, "a@x.com")
	_, keyB := seedDev(t, s, "b@x.com")

	now := time.Now()
	if err := db.UpsertAccount(model.Account{ID: "acc_A", DeveloperID: devA.ID, Provider: "OUTLOOK", Email: "a@outlook.com", Status: model.AccountOK}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertFolder(model.Folder{AccountID: "acc_A", ID: "F1", Name: "Inbox", Role: "inbox"}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertEmail(model.Email{AccountID: "acc_A", ID: "M1", FolderID: "F1", Subject: "secret", Date: now}); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveWebhook(model.Webhook{ID: "wh_A", DeveloperID: devA.ID, URL: "https://a.example.com", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveWebhook(model.Webhook{ID: "wh_A_acc", DeveloperID: devA.ID, AccountID: "acc_A", URL: "https://a.example.com", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveDelivery(store.Delivery{ID: "dl_A", WebhookID: "wh_A", EventType: "mail_received", Payload: []byte(`{}`), NextAttemptAt: now, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertAccount(model.Account{ID: "acc_wa", DeveloperID: devA.ID, Provider: "FAKECHAT", Kind: model.AccountKindChat, Email: "wa@x.com", Status: model.AccountOK}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertChat(model.Chat{ID: "c1", AccountID: "acc_wa", Kind: "direct"}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertAttendee(model.Attendee{ID: "a1", Phone: "+15550000001", Name: "A One"}, "acc_wa"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpsertChatMessage(model.ChatMessage{AccountID: "acc_wa", ID: "M1", ChatID: "c1", Sender: model.Attendee{ID: "acc_wa", IsSelf: true}, IsFromMe: true, Kind: "text", Text: "secret", SentAt: now}); err != nil {
		t.Fatal(err)
	}
	keysA, _ := db.ListAPIKeys(devA.ID)

	body := func(s string) *strings.Reader { return strings.NewReader(s) }
	cases := []struct {
		route        string // pattern from apiRoutes this case covers
		method, path string
		body         *strings.Reader
		want         int
	}{
		{"GET /api/v1/accounts/{id}", "GET", "/api/v1/accounts/acc_A", nil, 404},
		{"DELETE /api/v1/accounts/{id}", "DELETE", "/api/v1/accounts/acc_A", nil, 404},
		{"POST /api/v1/accounts/{id}/resync", "POST", "/api/v1/accounts/acc_A/resync", nil, 404},
		{"GET /api/v1/accounts/{id}/webhooks", "GET", "/api/v1/accounts/acc_A/webhooks", nil, 404},
		{"POST /api/v1/accounts/{id}/webhooks", "POST", "/api/v1/accounts/acc_A/webhooks", body(`{"url":"https://b.example.com"}`), 404},
		{"DELETE /api/v1/accounts/{id}/webhooks/{wid}", "DELETE", "/api/v1/accounts/acc_A/webhooks/wh_A_acc", nil, 404},
		{"GET /api/v1/folders", "GET", "/api/v1/folders?account_id=acc_A", nil, 404},
		{"GET /api/v1/threads", "GET", "/api/v1/threads?account_id=acc_A", nil, 404},
		{"GET /api/v1/emails", "GET", "/api/v1/emails?account_id=acc_A", nil, 404},
		{"GET /api/v1/emails/{id}", "GET", "/api/v1/emails/M1?account_id=acc_A", nil, 404},
		{"PATCH /api/v1/emails/{id}", "PATCH", "/api/v1/emails/M1?account_id=acc_A", body(`{"read":true}`), 404},
		{"POST /api/v1/emails", "POST", "/api/v1/emails", body(`{"account_id":"acc_A","to":[{"email":"x@y.com"}],"subject":"s","body":"b"}`), 404},
		{"POST /api/v1/emails/{id}/reply", "POST", "/api/v1/emails/M1/reply?account_id=acc_A", body(`{"body":"b"}`), 404},
		{"POST /api/v1/emails/{id}/forward", "POST", "/api/v1/emails/M1/forward?account_id=acc_A", body(`{"to":[{"email":"x@y.com"}],"body":"b"}`), 404},
		{"GET /api/v1/emails/{id}/attachments", "GET", "/api/v1/emails/M1/attachments?account_id=acc_A", nil, 404},
		{"GET /api/v1/emails/{id}/attachments/{aid}", "GET", "/api/v1/emails/M1/attachments/A1?account_id=acc_A", nil, 404},
		{"POST /api/v1/drafts", "POST", "/api/v1/drafts", body(`{"account_id":"acc_A","to":[{"email":"x@y.com"}],"subject":"s","body":"b"}`), 404},
		{"POST /api/v1/drafts/{id}/send", "POST", "/api/v1/drafts/D1/send?account_id=acc_A", nil, 404},
		{"DELETE /api/v1/webhooks/{id}", "DELETE", "/api/v1/webhooks/wh_A", nil, 404},
		{"GET /api/v1/webhooks/{id}/deliveries", "GET", "/api/v1/webhooks/wh_A/deliveries", nil, 404},
		{"GET /api/v1/chats", "GET", "/api/v1/chats?account_id=acc_wa", nil, 404},
		{"POST /api/v1/chats", "POST", "/api/v1/chats", body(`{"account_id":"acc_wa","phone":"+15551234567","text":"hi"}`), 404},
		{"GET /api/v1/chats/{id}", "GET", "/api/v1/chats/c1?account_id=acc_wa", nil, 404},
		{"PATCH /api/v1/chats/{id}", "PATCH", "/api/v1/chats/c1?account_id=acc_wa", body(`{"read":true}`), 404},
		{"GET /api/v1/chats/{id}/messages", "GET", "/api/v1/chats/c1/messages?account_id=acc_wa", nil, 404},
		{"POST /api/v1/chats/{id}/messages", "POST", "/api/v1/chats/c1/messages?account_id=acc_wa", body(`{"text":"hi"}`), 404},
		{"GET /api/v1/chats/{id}/messages/{mid}", "GET", "/api/v1/chats/c1/messages/M1?account_id=acc_wa", nil, 404},
		{"PATCH /api/v1/chats/{id}/messages/{mid}", "PATCH", "/api/v1/chats/c1/messages/M1?account_id=acc_wa", body(`{"text":"nope"}`), 404},
		{"DELETE /api/v1/chats/{id}/messages/{mid}", "DELETE", "/api/v1/chats/c1/messages/M1?account_id=acc_wa", nil, 404},
		{"PUT /api/v1/chats/{id}/messages/{mid}/reaction", "PUT", "/api/v1/chats/c1/messages/M1/reaction?account_id=acc_wa", body(`{"emoji":"👍"}`), 404},
		{"GET /api/v1/attendees", "GET", "/api/v1/attendees?account_id=acc_wa", nil, 404},
		{"GET /api/v1/attendees/{id}", "GET", "/api/v1/attendees/a1?account_id=acc_wa", nil, 404},
		// Session-only endpoint refuses the key before any lookup.
		{"DELETE /api/v1/api-keys/{id}", "DELETE", "/api/v1/api-keys/" + keysA[0].ID, nil, 403},
	}
	for _, tc := range cases {
		var req *http.Request
		if tc.body != nil {
			req = httptest.NewRequest(tc.method, tc.path, tc.body)
			req.Header.Set("Content-Type", "application/json")
		} else {
			req = httptest.NewRequest(tc.method, tc.path, nil)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, withKey(req, keyB))
		if rec.Code != tc.want {
			t.Errorf("%s %s as B: status = %d, want %d (body %s)", tc.method, tc.path, rec.Code, tc.want, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "secret") || strings.Contains(rec.Body.String(), "a@outlook.com") {
			t.Errorf("%s %s leaked A's data: %s", tc.method, tc.path, rec.Body.String())
		}
	}

	// Lists as B are empty, not A's.
	for _, path := range []string{"/api/v1/accounts", "/api/v1/webhooks"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, withKey(httptest.NewRequest(http.MethodGet, path, nil), keyB))
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"items":[]`) {
			t.Errorf("%s as B: %d %s", path, rec.Code, rec.Body.String())
		}
	}

	// Every registered /api/v1 route must be covered above, or be listed here
	// as carrying no foreign id to probe. A new route fails until placed.
	covered := map[string]bool{
		"GET /api/v1/accounts": true, "GET /api/v1/webhooks": true, // list checks above
		"GET /api/v1/providers": true, "GET /api/v1/me": true,
		"GET /api/v1/api-keys": true, "POST /api/v1/api-keys": true,
		"POST /api/v1/hosted-auth": true, "POST /api/v1/webhooks": true,
	}
	for _, tc := range cases {
		covered[tc.route] = true
	}
	for _, route := range apiRoutes {
		if !covered[route] {
			t.Errorf("route %q has no isolation case; add one", route)
		}
	}
}
