package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gauravrautela/unified-messaging/internal/model"
	"github.com/gauravrautela/unified-messaging/internal/store"
)

// sendJSON runs one request through the full router as the holder of key.
func sendJSON(t *testing.T, s *Server, key, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, withKey(req, key))
	return rec
}

// errCode reads the error.code of an error body.
func errCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var e struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("not an error body: %s", rec.Body.String())
	}
	return e.Error.Code
}

// PATCH changes url and events in place: the id, the signing secret, the
// paused state, the scope and the delivery log all stay, and the secret is
// never echoed.
func TestPatchWebhookKeepsIdentitySecretPausedStateAndDeliveries(t *testing.T) {
	s, db := newTestServer(t)
	dev, key := seedDev(t, s, "a@x.com")
	created := time.Now().UTC().Truncate(time.Second)
	if err := db.SaveWebhook(model.Webhook{ID: "wh_1", DeveloperID: dev.ID, Name: "prod", URL: "https://old.example.com/in",
		Secret: "whsec_1", Events: []string{"mail_received"}, Paused: true, CreatedAt: created}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := db.SaveDelivery(store.Delivery{ID: "dl_1", WebhookID: "wh_1", EventType: "mail_received",
		Payload: []byte(`{}`), Attempts: 8, Dead: true, NextAttemptAt: now, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}

	rec := sendJSON(t, s, key, http.MethodPatch, "/api/v1/webhooks/wh_1",
		`{"url":"https://new.example.com/in","events":["mail_received","chat_received"]}`)
	var got model.Webhook
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &got) != nil {
		t.Fatalf("patch: %d %s", rec.Code, rec.Body.String())
	}
	if got.ID != "wh_1" || got.Name != "prod" || got.URL != "https://new.example.com/in" ||
		!slices.Equal(got.Events, []string{"mail_received", "chat_received"}) ||
		!got.Paused || got.Secret != "" || !got.CreatedAt.Equal(created) {
		t.Fatalf("answered %+v", got)
	}
	if strings.Contains(rec.Body.String(), "whsec_1") {
		t.Fatalf("response leaks the secret: %s", rec.Body.String())
	}

	stored, err := db.GetWebhook(dev.ID, "wh_1")
	if err != nil || stored.URL != "https://new.example.com/in" || stored.Secret != "whsec_1" || !stored.Paused ||
		!slices.Equal(stored.Events, []string{"mail_received", "chat_received"}) {
		t.Fatalf("stored %+v, %v", stored, err)
	}
	if q, _ := db.ListDeliveries("wh_1", 10, 0); len(q) != 1 || q[0].ID != "dl_1" {
		t.Fatalf("delivery log changed: %+v", q)
	}

	// Each field alone, and the name (cleared with ""), leave the others be.
	rec = sendJSON(t, s, key, http.MethodPatch, "/api/v1/webhooks/wh_1", `{"name":"staging"}`)
	got = model.Webhook{}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &got) != nil ||
		got.Name != "staging" || got.URL != "https://new.example.com/in" || len(got.Events) != 2 {
		t.Fatalf("name only: %d %s", rec.Code, rec.Body.String())
	}
	rec = sendJSON(t, s, key, http.MethodPatch, "/api/v1/webhooks/wh_1", `{"name":""}`)
	got = model.Webhook{}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &got) != nil || got.Name != "" {
		t.Fatalf("clear name: %d %s", rec.Code, rec.Body.String())
	}
	rec = sendJSON(t, s, key, http.MethodPatch, "/api/v1/webhooks/wh_1", `{"events":["*"]}`)
	got = model.Webhook{}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &got) != nil ||
		!slices.Equal(got.Events, []string{"*"}) || got.URL != "https://new.example.com/in" {
		t.Fatalf("events only: %d %s", rec.Code, rec.Body.String())
	}
}

// Account-scoped hooks are reachable through /webhooks/{id} (as delete and
// pause already are) and keep their account.
func TestPatchWebhookOnAccountScopedHookKeepsItsAccount(t *testing.T) {
	s, db := newTestServer(t)
	dev, key := seedDev(t, s, "a@x.com")
	if err := db.UpsertAccount(model.Account{ID: "acc_1", DeveloperID: dev.ID, Provider: "OUTLOOK", Email: "u@x.com", Status: model.AccountOK}); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveWebhook(model.Webhook{ID: "wh_acc", DeveloperID: dev.ID, AccountID: "acc_1", URL: "https://old.example.com",
		Secret: "whsec_acc", Events: []string{"mail_received"}, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	rec := sendJSON(t, s, key, http.MethodPatch, "/api/v1/webhooks/wh_acc", `{"events":["mail_sent"]}`)
	var got model.Webhook
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &got) != nil ||
		got.AccountID != "acc_1" || !slices.Equal(got.Events, []string{"mail_sent"}) || got.Secret != "" {
		t.Fatalf("patch: %d %s", rec.Code, rec.Body.String())
	}
	list, err := db.ListAccountWebhooks(dev.ID, "acc_1")
	if err != nil || len(list) != 1 || list[0].ID != "wh_acc" || list[0].Secret != "whsec_acc" ||
		!slices.Equal(list[0].Events, []string{"mail_sent"}) {
		t.Fatalf("account list after patch: %+v, %v", list, err)
	}
}

// Every field is held to the rule create applies to it, and a refused edit
// changes nothing.
func TestPatchWebhookValidation(t *testing.T) {
	s, db := newTestServer(t)
	dev, key := seedDev(t, s, "a@x.com")
	save := func(w model.Webhook) {
		t.Helper()
		w.DeveloperID, w.CreatedAt = dev.ID, time.Now()
		if err := db.SaveWebhook(w); err != nil {
			t.Fatal(err)
		}
	}
	save(model.Webhook{ID: "wh_web", URL: "https://old.example.com", Secret: "whsec_1", Events: []string{"mail_received"}})
	save(model.Webhook{ID: "wh_dc", Kind: model.WebhookKindDiscord, URL: "https://discord.com/api/webhooks/1/abc", Events: []string{"account_status"}})
	save(model.Webhook{ID: "wh_tg", Kind: model.WebhookKindTelegram, Telegram: &model.TelegramTarget{ChatID: "-1", BotToken: "1:A"}, Events: []string{"chat_received"}})

	for _, tc := range []struct {
		label, id, body, code string
		want                  int
	}{
		{"loopback url", "wh_web", `{"url":"http://127.0.0.1/hook"}`, "invalid_webhook", 400},
		{"localhost url", "wh_web", `{"url":"http://localhost/hook"}`, "invalid_webhook", 400},
		{"private ip url", "wh_web", `{"url":"http://10.0.0.5/hook"}`, "invalid_webhook", 400},
		{"metadata ip url", "wh_web", `{"url":"http://169.254.169.254/latest"}`, "invalid_webhook", 400},
		{"non-http scheme", "wh_web", `{"url":"ftp://hook.example.com/x"}`, "invalid_webhook", 400},
		{"not a url", "wh_web", `{"url":"hook.example.com"}`, "invalid_webhook", 400},
		{"empty url", "wh_web", `{"url":""}`, "invalid_webhook", 400},
		{"unknown event", "wh_web", `{"events":["mail_received","no_such_event"]}`, "invalid_webhook", 400},
		{"webhook_test is not subscribable", "wh_web", `{"events":["webhook_test"]}`, "invalid_webhook", 400},
		{"empty events", "wh_web", `{"events":[]}`, "invalid_webhook", 400},
		{"bad url beside good events", "wh_web", `{"url":"http://127.0.0.1","events":["mail_sent"]}`, "invalid_webhook", 400},
		{"empty object", "wh_web", `{}`, "empty_patch", 400},
		{"null fields", "wh_web", `{"url":null,"events":null,"name":null}`, "empty_patch", 400},
		{"no body", "wh_web", ``, "invalid_body", 400},
		{"malformed json", "wh_web", `{"url":`, "invalid_body", 400},
		{"secret cannot be patched", "wh_web", `{"secret":"whsec_new"}`, "invalid_body", 400},
		{"kind cannot be patched", "wh_web", `{"kind":"discord"}`, "invalid_body", 400},
		{"account cannot be patched", "wh_web", `{"account_id":"acc_x"}`, "invalid_body", 400},
		{"telegram target cannot be patched", "wh_tg", `{"chat_id":"-2"}`, "invalid_body", 400},
		{"discord url must stay discord", "wh_dc", `{"url":"https://hook.example.com/x"}`, "invalid_webhook", 400},
		{"discord url with a port", "wh_dc", `{"url":"https://discord.com:8443/api/webhooks/1/abc"}`, "invalid_webhook", 400},
		{"telegram has no url", "wh_tg", `{"url":"https://hook.example.com/x"}`, "invalid_webhook", 400},
		{"unknown hook", "wh_nope", `{"events":["*"]}`, "not_found", 404},
		{"unknown hook, bad body", "wh_nope", `{}`, "not_found", 404},
	} {
		rec := sendJSON(t, s, key, http.MethodPatch, "/api/v1/webhooks/"+tc.id, tc.body)
		if rec.Code != tc.want || errCode(t, rec) != tc.code {
			t.Errorf("%s: %d %s, want %d %s", tc.label, rec.Code, rec.Body.String(), tc.want, tc.code)
		}
	}

	// None of the refusals changed anything.
	web, _ := db.GetWebhook(dev.ID, "wh_web")
	if web.URL != "https://old.example.com" || web.Secret != "whsec_1" || !slices.Equal(web.Events, []string{"mail_received"}) {
		t.Fatalf("a refused patch changed wh_web: %+v", web)
	}
	dc, _ := db.GetWebhook(dev.ID, "wh_dc")
	if dc.URL != "https://discord.com/api/webhooks/1/abc" {
		t.Fatalf("a refused patch changed wh_dc: %+v", dc)
	}
	tg, _ := db.GetWebhook(dev.ID, "wh_tg")
	if tg.Telegram == nil || tg.Telegram.ChatID != "-1" || tg.Telegram.BotToken != "1:A" {
		t.Fatalf("a refused patch changed wh_tg: %+v", tg)
	}

	// What is valid for the kind goes through: a new Discord URL, and a name
	// and filter on a telegram hook without losing its sealed target.
	rec := sendJSON(t, s, key, http.MethodPatch, "/api/v1/webhooks/wh_dc", `{"url":"https://discord.com/api/webhooks/2/def"}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "/api/webhooks/2/def") {
		t.Fatalf("discord patch: %d %s", rec.Code, rec.Body.String())
	}
	rec = sendJSON(t, s, key, http.MethodPatch, "/api/v1/webhooks/wh_tg", `{"name":"ops","events":["chat_received","chat_sent"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("telegram patch: %d %s", rec.Code, rec.Body.String())
	}
	tg, _ = db.GetWebhook(dev.ID, "wh_tg")
	if tg.Name != "ops" || tg.Telegram == nil || tg.Telegram.ChatID != "-1" || tg.Telegram.BotToken != "1:A" {
		t.Fatalf("telegram hook after patch: %+v (%+v)", tg, tg.Telegram)
	}
}

// GET /webhooks/{id} reads one hook: the same shape list gives, no secret,
// 404 for an unknown hook and for another developer's.
func TestGetWebhook(t *testing.T) {
	s, db := newTestServer(t)
	devA, keyA := seedDev(t, s, "a@x.com")
	_, keyB := seedDev(t, s, "b@x.com")
	if err := db.SaveWebhook(model.Webhook{ID: "wh_1", DeveloperID: devA.ID, Name: "prod", URL: "https://h.example.com/in",
		Secret: "whsec_1", Events: []string{"mail_received"}, Paused: true, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}

	rec := sendJSON(t, s, keyA, http.MethodGet, "/api/v1/webhooks/wh_1", "")
	var got model.Webhook
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &got) != nil {
		t.Fatalf("get: %d %s", rec.Code, rec.Body.String())
	}
	if got.ID != "wh_1" || got.Name != "prod" || got.URL != "https://h.example.com/in" || !got.Paused ||
		got.Kind != model.WebhookKindWebhook || got.Secret != "" || strings.Contains(rec.Body.String(), "whsec_1") {
		t.Fatalf("answered %+v", got)
	}

	// Same bytes as the hook's entry in the listing.
	list := sendJSON(t, s, keyA, http.MethodGet, "/api/v1/webhooks", "")
	var l listResponse[json.RawMessage]
	if err := json.Unmarshal(list.Body.Bytes(), &l); err != nil || len(l.Items) != 1 {
		t.Fatalf("list: %s", list.Body.String())
	}
	var a, b map[string]any
	_ = json.Unmarshal(l.Items[0], &a)
	_ = json.Unmarshal(rec.Body.Bytes(), &b)
	if len(a) != len(b) {
		t.Fatalf("get and list disagree on shape: %v vs %v", b, a)
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || !jsonEqual(v, bv) {
			t.Fatalf("get and list disagree on %q: %v vs %v", k, bv, v)
		}
	}

	for _, tc := range []struct{ label, key, id string }{
		{"unknown id", keyA, "wh_nope"},
		{"another developer's hook", keyB, "wh_1"},
	} {
		rec := sendJSON(t, s, tc.key, http.MethodGet, "/api/v1/webhooks/"+tc.id, "")
		if rec.Code != http.StatusNotFound || errCode(t, rec) != "not_found" {
			t.Errorf("%s: %d %s", tc.label, rec.Code, rec.Body.String())
		}
	}
}

func jsonEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

// Another developer's hook cannot be edited, with a valid or an invalid
// body, and is left untouched.
func TestPatchWebhookOfAnotherDeveloperIs404AndChangesNothing(t *testing.T) {
	s, db := newTestServer(t)
	devA, _ := seedDev(t, s, "a@x.com")
	_, keyB := seedDev(t, s, "b@x.com")
	if err := db.SaveWebhook(model.Webhook{ID: "wh_A", DeveloperID: devA.ID, Name: "prod", URL: "https://a.example.com",
		Secret: "whsec_A", Events: []string{"mail_received"}, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{`{"url":"https://evil.example.com","events":["*"]}`, `{}`, `{"url":"http://127.0.0.1"}`, ``} {
		rec := sendJSON(t, s, keyB, http.MethodPatch, "/api/v1/webhooks/wh_A", body)
		if rec.Code != http.StatusNotFound || errCode(t, rec) != "not_found" || strings.Contains(rec.Body.String(), "whsec_A") {
			t.Errorf("body %q as B: %d %s", body, rec.Code, rec.Body.String())
		}
	}
	got, err := db.GetWebhook(devA.ID, "wh_A")
	if err != nil || got.URL != "https://a.example.com" || got.Name != "prod" || got.Secret != "whsec_A" ||
		!slices.Equal(got.Events, []string{"mail_received"}) {
		t.Fatalf("A's hook changed: %+v, %v", got, err)
	}
}
