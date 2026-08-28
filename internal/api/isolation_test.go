package api

import (
	"net/http"
	"net/http/httptest"
	"net/url"
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
	devB, keyB := seedDev(t, s, "b@x.com")

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

	// body is the raw request body, "" meaning none. It is kept as a string
	// (not a reader) because this table is driven twice — once per credential
	// kind — and an io.Reader can only be consumed once.
	cases := []struct {
		route        string // pattern from apiRoutes this case covers
		method, path string
		body         string
		want         int
	}{
		{"GET /api/v1/accounts/{id}", "GET", "/api/v1/accounts/acc_A", "", 404},
		{"DELETE /api/v1/accounts/{id}", "DELETE", "/api/v1/accounts/acc_A", "", 404},
		{"POST /api/v1/accounts/{id}/resync", "POST", "/api/v1/accounts/acc_A/resync", "", 404},
		{"POST /api/v1/accounts/{id}/reconnect", "POST", "/api/v1/accounts/acc_A/reconnect", "", 404},
		{"GET /api/v1/accounts/{id}/webhooks", "GET", "/api/v1/accounts/acc_A/webhooks", "", 404},
		{"POST /api/v1/accounts/{id}/webhooks", "POST", "/api/v1/accounts/acc_A/webhooks", `{"url":"https://b.example.com"}`, 404},
		{"DELETE /api/v1/accounts/{id}/webhooks/{wid}", "DELETE", "/api/v1/accounts/acc_A/webhooks/wh_A_acc", "", 404},
		{"GET /api/v1/folders", "GET", "/api/v1/folders?account_id=acc_A", "", 404},
		{"GET /api/v1/threads", "GET", "/api/v1/threads?account_id=acc_A", "", 404},
		{"GET /api/v1/emails", "GET", "/api/v1/emails?account_id=acc_A", "", 404},
		{"GET /api/v1/emails/{id}", "GET", "/api/v1/emails/M1?account_id=acc_A", "", 404},
		{"PATCH /api/v1/emails/{id}", "PATCH", "/api/v1/emails/M1?account_id=acc_A", `{"read":true}`, 404},
		{"POST /api/v1/emails", "POST", "/api/v1/emails", `{"account_id":"acc_A","to":[{"email":"x@y.com"}],"subject":"s","body":"b"}`, 404},
		{"POST /api/v1/emails/{id}/reply", "POST", "/api/v1/emails/M1/reply?account_id=acc_A", `{"body":"b"}`, 404},
		{"POST /api/v1/emails/{id}/forward", "POST", "/api/v1/emails/M1/forward?account_id=acc_A", `{"to":[{"email":"x@y.com"}],"body":"b"}`, 404},
		{"GET /api/v1/emails/{id}/attachments", "GET", "/api/v1/emails/M1/attachments?account_id=acc_A", "", 404},
		{"GET /api/v1/emails/{id}/attachments/{aid}", "GET", "/api/v1/emails/M1/attachments/A1?account_id=acc_A", "", 404},
		{"POST /api/v1/drafts", "POST", "/api/v1/drafts", `{"account_id":"acc_A","to":[{"email":"x@y.com"}],"subject":"s","body":"b"}`, 404},
		{"POST /api/v1/drafts/{id}/send", "POST", "/api/v1/drafts/D1/send?account_id=acc_A", "", 404},
		{"DELETE /api/v1/webhooks/{id}", "DELETE", "/api/v1/webhooks/wh_A", "", 404},
		{"GET /api/v1/webhooks/{id}/deliveries", "GET", "/api/v1/webhooks/wh_A/deliveries", "", 404},
		{"GET /api/v1/chats", "GET", "/api/v1/chats?account_id=acc_wa", "", 404},
		{"POST /api/v1/chats", "POST", "/api/v1/chats", `{"account_id":"acc_wa","phone":"+15551234567","text":"hi"}`, 404},
		{"GET /api/v1/chats/{id}", "GET", "/api/v1/chats/c1?account_id=acc_wa", "", 404},
		{"PATCH /api/v1/chats/{id}", "PATCH", "/api/v1/chats/c1?account_id=acc_wa", `{"read":true}`, 404},
		{"GET /api/v1/chats/{id}/messages", "GET", "/api/v1/chats/c1/messages?account_id=acc_wa", "", 404},
		{"POST /api/v1/chats/{id}/messages", "POST", "/api/v1/chats/c1/messages?account_id=acc_wa", `{"text":"hi"}`, 404},
		{"GET /api/v1/chats/{id}/messages/{mid}", "GET", "/api/v1/chats/c1/messages/M1?account_id=acc_wa", "", 404},
		{"PATCH /api/v1/chats/{id}/messages/{mid}", "PATCH", "/api/v1/chats/c1/messages/M1?account_id=acc_wa", `{"text":"nope"}`, 404},
		{"DELETE /api/v1/chats/{id}/messages/{mid}", "DELETE", "/api/v1/chats/c1/messages/M1?account_id=acc_wa", "", 404},
		{"PUT /api/v1/chats/{id}/messages/{mid}/reaction", "PUT", "/api/v1/chats/c1/messages/M1/reaction?account_id=acc_wa", `{"emoji":"👍"}`, 404},
		{"GET /api/v1/attendees", "GET", "/api/v1/attendees?account_id=acc_wa", "", 404},
		{"GET /api/v1/attendees/{id}", "GET", "/api/v1/attendees/a1?account_id=acc_wa", "", 404},
		// Session-only endpoints refuse the key before any lookup.
		{"DELETE /api/v1/api-keys/{id}", "DELETE", "/api/v1/api-keys/" + keysA[0].ID, "", 403},
		{"POST /api/v1/me/password", "POST", "/api/v1/me/password", `{"current_password":"longenoughpassword","new_password":"another strong one"}`, 403},
		{"PUT /api/v1/me/redirect-domains", "PUT", "/api/v1/me/redirect-domains", `{"domains":[]}`, 403},
	}
	// buildRequest constructs a fresh request for tc each time it is called,
	// so the same table can be driven once per credential kind: an
	// *http.Request's body is consumed by ServeHTTP, so it cannot be reused
	// across the two passes below the way a shared *strings.Reader would be.
	buildRequest := func(tc struct {
		route        string
		method, path string
		body         string
		want         int
	}) *http.Request {
		var req *http.Request
		if tc.body == "" {
			req = httptest.NewRequest(tc.method, tc.path, nil)
		} else {
			req = httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		}
		// Set even on a bodyless POST/PUT/PATCH: a session credential (unlike
		// an API key) requires Content-Type: application/json on every
		// state-changing request, empty body or not (see isStateChanging).
		if tc.method == http.MethodPost || tc.method == http.MethodPut || tc.method == http.MethodPatch {
			req.Header.Set("Content-Type", "application/json")
		}
		return req
	}

	for _, tc := range cases {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, withKey(buildRequest(tc), keyB))
		if rec.Code != tc.want {
			t.Errorf("%s %s as B (key): status = %d, want %d (body %s)", tc.method, tc.path, rec.Code, tc.want, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "secret") || strings.Contains(rec.Body.String(), "a@outlook.com") {
			t.Errorf("%s %s as B (key) leaked A's data: %s", tc.method, tc.path, rec.Body.String())
		}
	}

	// The same probes again, this time authenticated by session instead of
	// API key. Session and key auth share the same tenant-scoping code for
	// every A-scoped id, so the 404s above must hold here too. The three
	// session-only management endpoints are the exception: a session (unlike
	// a key) actually clears requireSession, so each of those now succeeds —
	// but only against developer B's own account, never against A's data.
	sessionWant := map[string]int{
		// keysA[0].ID belongs to A; B's session does not own it, so revoking
		// it by id still 404s exactly like the cross-account delete above.
		"DELETE /api/v1/api-keys/{id}": 404,
		// The body's current_password is every seeded dev's real password
		// (see seedDev), so under B's own session this actually changes B's
		// own password — success, and still nothing of A's.
		"POST /api/v1/me/password": 204,
		// Sets B's own redirect-domain allowlist; 200 with B's own (empty)
		// list back, never A's.
		"PUT /api/v1/me/redirect-domains": 200,
	}
	for _, tc := range cases {
		want := tc.want
		if w, ok := sessionWant[tc.route]; ok {
			want = w
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, withSession(t, s, buildRequest(tc), devB.ID))
		if rec.Code != want {
			t.Errorf("%s %s as B (session): status = %d, want %d (body %s)", tc.method, tc.path, rec.Code, want, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "secret") || strings.Contains(rec.Body.String(), "a@outlook.com") {
			t.Errorf("%s %s as B (session) leaked A's data: %s", tc.method, tc.path, rec.Body.String())
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

// TestBrowserRoutesIsolation is TestCrossTenantAccessIs404's counterpart for
// the browser-facing surface: the routes Routes() registers outside
// apiRoutes, none of which take an API key. Each must either refuse an
// anonymous caller outright, or — for the connect flow, which is reachable
// by design before any credential exists — never let one browser's request
// surface another developer's data. Adding a route to browserRoutes without
// a case here fails the completeness gate below, the same way apiRoutes does.
func TestBrowserRoutesIsolation(t *testing.T) {
	s, db := newTestServer(t)
	h := s.Routes()
	devA, _ := seedDev(t, s, "browserA@x.com")

	now := time.Now()
	// A pending mail (Outlook) connect attempt: reachable pre-consent, so
	// GET /connect/{state} for it must never be the place A's address leaks.
	stateA := "st_browser_mail_A"
	if err := db.SaveOAuthState(store.OAuthState{
		State: stateA, DeveloperID: devA.ID, Provider: "OUTLOOK",
		Verifier: "verifier-A", ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	// A pending chat (FAKECHAT) connect attempt, for the consent endpoint:
	// only a Linker provider's state reaches handleConsent's cookie check
	// rather than 404ing on "not a linker" first.
	stateChatA := "st_browser_chat_A"
	if err := db.SaveOAuthState(store.OAuthState{
		State: stateChatA, DeveloperID: devA.ID, Provider: "FAKECHAT",
		ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name  string
		route string
		req   *http.Request
		check func(t *testing.T, rec *httptest.ResponseRecorder)
	}{
		{
			name: "dashboard without session redirects to login", route: "GET /dashboard",
			req: httptest.NewRequest(http.MethodGet, "/dashboard", nil),
			check: func(t *testing.T, rec *httptest.ResponseRecorder) {
				if rec.Code != http.StatusFound || !strings.HasPrefix(rec.Header().Get("Location"), "/login") {
					t.Errorf("dashboard: status=%d location=%q, want 302 to /login", rec.Code, rec.Header().Get("Location"))
				}
			},
		},
		{
			name: "mail without session redirects to login", route: "GET /mail",
			req: httptest.NewRequest(http.MethodGet, "/mail", nil),
			check: func(t *testing.T, rec *httptest.ResponseRecorder) {
				if rec.Code != http.StatusFound || !strings.HasPrefix(rec.Header().Get("Location"), "/login") {
					t.Errorf("mail: status=%d location=%q, want 302 to /login", rec.Code, rec.Header().Get("Location"))
				}
			},
		},
		{
			name: "chat without session redirects to login", route: "GET /chat",
			req: httptest.NewRequest(http.MethodGet, "/chat", nil),
			check: func(t *testing.T, rec *httptest.ResponseRecorder) {
				if rec.Code != http.StatusFound || !strings.HasPrefix(rec.Header().Get("Location"), "/login") {
					t.Errorf("chat: status=%d location=%q, want 302 to /login", rec.Code, rec.Header().Get("Location"))
				}
			},
		},
		{
			name: "connect landing page for A's state never carries A's address", route: "GET /connect/{state}",
			req: httptest.NewRequest(http.MethodGet, "/connect/"+stateA, nil),
			check: func(t *testing.T, rec *httptest.ResponseRecorder) {
				if rec.Code != http.StatusOK {
					t.Errorf("connect/%s: status=%d, want 200 (body %s)", stateA, rec.Code, rec.Body.String())
				}
				if strings.Contains(rec.Body.String(), devA.Email) || strings.Contains(rec.Body.String(), devA.ID) {
					t.Errorf("connect/%s leaked A's identity: %s", stateA, rec.Body.String())
				}
			},
		},
		{
			name: "connect landing page for an unknown state is 404", route: "GET /connect/{state}",
			req: httptest.NewRequest(http.MethodGet, "/connect/unknown", nil),
			check: func(t *testing.T, rec *httptest.ResponseRecorder) {
				if rec.Code != http.StatusNotFound {
					t.Errorf("connect/unknown: status=%d, want 404", rec.Code)
				}
			},
		},
		{
			name: "consent without the link cookie is refused", route: "POST /connect/{state}/consent",
			req: httptest.NewRequest(http.MethodPost, "/connect/"+stateChatA+"/consent", nil),
			check: func(t *testing.T, rec *httptest.ResponseRecorder) {
				if rec.Code != http.StatusForbidden {
					t.Errorf("consent without cookie: status=%d, want 403 (body %s)", rec.Code, rec.Body.String())
				}
			},
		},
		{
			name: "oauth callback for an unknown state is never a 500", route: "GET /oauth/callback",
			req: httptest.NewRequest(http.MethodGet, "/oauth/callback?state=unknown", nil),
			check: func(t *testing.T, rec *httptest.ResponseRecorder) {
				if rec.Code != http.StatusBadRequest && rec.Code != http.StatusNotFound {
					t.Errorf("oauth/callback?state=unknown: status=%d, want 400 or 404", rec.Code)
				}
			},
		},
		{
			name: "login post without a csrf token is refused", route: "POST /login",
			req: func() *http.Request {
				form := url.Values{"email": {"nobody@x.com"}, "password": {"whatever-password"}}
				req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
				req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				return req
			}(),
			check: func(t *testing.T, rec *httptest.ResponseRecorder) {
				if rec.Code != http.StatusForbidden {
					t.Errorf("login without csrf: status=%d, want 403 (body %s)", rec.Code, rec.Body.String())
				}
			},
		},
		{
			name: "healthz is public and sets no cookie", route: "GET /healthz",
			req: httptest.NewRequest(http.MethodGet, "/healthz", nil),
			check: func(t *testing.T, rec *httptest.ResponseRecorder) {
				if rec.Code != http.StatusOK || rec.Header().Get("Set-Cookie") != "" {
					t.Errorf("healthz: status=%d set-cookie=%q", rec.Code, rec.Header().Get("Set-Cookie"))
				}
			},
		},
		{
			name: "docs is public and sets no cookie", route: "GET /docs",
			req: httptest.NewRequest(http.MethodGet, "/docs", nil),
			check: func(t *testing.T, rec *httptest.ResponseRecorder) {
				if rec.Code != http.StatusOK || rec.Header().Get("Set-Cookie") != "" {
					t.Errorf("docs: status=%d set-cookie=%q", rec.Code, rec.Header().Get("Set-Cookie"))
				}
			},
		},
		{
			name: "llms.txt is public and sets no cookie", route: "GET /llms.txt",
			req: httptest.NewRequest(http.MethodGet, "/llms.txt", nil),
			check: func(t *testing.T, rec *httptest.ResponseRecorder) {
				if rec.Code != http.StatusOK || rec.Header().Get("Set-Cookie") != "" {
					t.Errorf("llms.txt: status=%d set-cookie=%q", rec.Code, rec.Header().Get("Set-Cookie"))
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, tc.req)
			tc.check(t, rec)
		})
	}

	// Every registered browser route must be covered above, or be listed here
	// as exercised elsewhere in this file — a new route fails until placed.
	covered := map[string]bool{
		// The sign-in/out and QR-poll surface get far more thorough coverage
		// elsewhere (TestLoginHonoursSameOriginNext, TestLoginRequiresCSRFAndSameOrigin,
		// TestSignupSetsSessionCookieAndRedirects, TestSignupErrorIsUniform,
		// TestLogoutClearsSession, and the many TestLinkQR* tests) than this
		// isolation-focused table would add.
		"GET /login": true, "GET /signup": true, "POST /signup": true,
		"POST /logout": true, "GET /connect/{state}/qr": true,
		// Provider push endpoints carry no session or API key at all — their
		// own authenticity check (clientState) is exercised in the
		// notification-handling tests (TestNotification*, TestValidation*).
		"POST /notifications/{provider}":           true,
		"POST /notifications/{provider}/lifecycle": true,
	}
	for _, tc := range cases {
		covered[tc.route] = true
	}
	for _, route := range browserRoutes {
		if !covered[route] {
			t.Errorf("route %q has no browser isolation case; add one", route)
		}
	}
}

// browserRoutes only gates the isolation test above through Routes()'s own
// panic when a listed pattern has no handler. That catches a route dropped
// from the map but not the reverse — a handler added to browserHandlers with
// no browserRoutes entry, which Routes() would happily leave unregistered.
// This test catches both directions directly by comparing the two.
func TestBrowserHandlersMatchBrowserRoutes(t *testing.T) {
	s, _ := newTestServer(t)
	handlers := s.browserHandlers()
	if len(handlers) != len(browserRoutes) {
		t.Fatalf("browserHandlers has %d entries, browserRoutes lists %d", len(handlers), len(browserRoutes))
	}
	listed := make(map[string]bool, len(browserRoutes))
	for _, route := range browserRoutes {
		listed[route] = true
	}
	for pattern := range handlers {
		if !listed[pattern] {
			t.Errorf("browserHandlers has %q, which is not in browserRoutes", pattern)
		}
	}
}
