package api

import (
	"context"
	"encoding/json"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gauravrautela/unified-messaging/internal/auth"
	"github.com/gauravrautela/unified-messaging/internal/config"
	"github.com/gauravrautela/unified-messaging/internal/events"
	"github.com/gauravrautela/unified-messaging/internal/logx"
	"github.com/gauravrautela/unified-messaging/internal/model"
	"github.com/gauravrautela/unified-messaging/internal/provider"
	"github.com/gauravrautela/unified-messaging/internal/provider/outlook"
	"github.com/gauravrautela/unified-messaging/internal/store"
	"github.com/gauravrautela/unified-messaging/internal/syncer"
)

func newTestServerWithLog(t *testing.T) (*Server, *store.Store, *logx.Records) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "api.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	cfg := &config.Config{
		ClientID: "client-123", Tenant: "consumers",
		RedirectURI: "http://localhost:8080/oauth/callback",
		Scopes:      []string{"offline_access", "Mail.Read"},
		SessionTTL:  30 * 24 * time.Hour,
	}
	log, recs := logx.Capture()
	a := outlook.NewAuth(cfg.ClientID, "", cfg.Tenant, cfg.RedirectURI, cfg.Scopes)
	registry := provider.NewRegistry(outlook.New(a, stubTokens{}))
	disp := events.NewDispatcher(db, log)
	sync := syncer.New(db, registry, nil, disp, log, syncer.Options{PollInterval: time.Hour})
	authSvc := auth.New(db, log, cfg.SessionTTL)
	return NewServer(cfg, db, registry, nil, sync, authSvc, log), db, recs
}

func newTestServer(t *testing.T) (*Server, *store.Store) {
	s, db, _ := newTestServerWithLog(t)
	return s, db
}

type stubTokens struct{}

func (stubTokens) AccessToken(context.Context, string, bool) (string, error) {
	return "test-token", nil
}

// seedDev creates a developer and one API key, returning the full key.
func seedDev(t *testing.T, s *Server, email string) (model.Developer, string) {
	t.Helper()
	ctx := context.Background()
	d, err := s.auth.Signup(ctx, email, "longenoughpassword", "Dev")
	if err != nil {
		t.Fatal(err)
	}
	key, _, err := s.auth.NewAPIKey(ctx, d.ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	return d, key
}

func withKey(req *http.Request, key string) *http.Request {
	req.Header.Set("Authorization", "Bearer "+key)
	return req
}

func withSession(t *testing.T, s *Server, req *http.Request, devID string) *http.Request {
	t.Helper()
	tok, _, err := s.auth.NewSession(context.Background(), devID)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: tok})
	return req
}

func TestAPIRequiresDeveloperCredential(t *testing.T) {
	s, _, recs := newTestServerWithLog(t)
	h := s.Routes()
	dev, key := seedDev(t, s, "a@x.com")

	do := func(req *http.Request) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	if rec := do(httptest.NewRequest(http.MethodGet, "/api/v1/accounts", nil)); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no credential: %d, want 401", rec.Code)
	}
	if rec := do(withKey(httptest.NewRequest(http.MethodGet, "/api/v1/accounts", nil), key)); rec.Code != http.StatusOK {
		t.Fatalf("api key: %d, want 200", rec.Code)
	}
	if rec := do(withKey(httptest.NewRequest(http.MethodGet, "/api/v1/accounts", nil), "um_"+strings.Repeat("x", 40))); rec.Code != http.StatusUnauthorized {
		t.Fatalf("bogus key: %d, want 401", rec.Code)
	}
	if rec := do(withSession(t, s, httptest.NewRequest(http.MethodGet, "/api/v1/accounts", nil), dev.ID)); rec.Code != http.StatusOK {
		t.Fatalf("session cookie: %d, want 200", rec.Code)
	}
	if rec := do(httptest.NewRequest(http.MethodGet, "/api/v1/accounts", nil)); rec.Header().Get("X-Request-Id") == "" {
		t.Fatal("responses must carry X-Request-Id")
	}

	// Request bodies are logged at DEBUG with secrets redacted, never in the clear.
	bodyReq := withKey(httptest.NewRequest(http.MethodPost, "/api/v1/webhooks",
		strings.NewReader(`{"url":"https://x","secret":"hush"}`)), key)
	bodyReq.Header.Set("Content-Type", "application/json")
	if rec := do(bodyReq); rec.Code != http.StatusCreated {
		t.Fatalf("create webhook: %d, want 201", rec.Code)
	}
	var bodyLine string
	for _, line := range recs.All() {
		if strings.Contains(line, "request body") {
			bodyLine = line
		}
	}
	if bodyLine == "" {
		t.Fatal("no request body log line found")
	}
	if !strings.Contains(bodyLine, "url:https://x") {
		t.Fatalf("body log missing url: %s", bodyLine)
	}
	if !strings.Contains(bodyLine, "secret:[redacted]") {
		t.Fatalf("body log did not redact secret: %s", bodyLine)
	}
	if strings.Contains(bodyLine, "hush") {
		t.Fatalf("body log leaked the secret value: %s", bodyLine)
	}
}

func TestRevokedKeyIsRejected(t *testing.T) {
	s, _ := newTestServer(t)
	dev, key := seedDev(t, s, "a@x.com")
	keys, _ := s.store.ListAPIKeys(dev.ID)
	if err := s.auth.RevokeKey(context.Background(), dev.ID, keys[0].ID); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, withKey(httptest.NewRequest(http.MethodGet, "/api/v1/accounts", nil), key))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("revoked key: %d, want 401", rec.Code)
	}
}

// Graph refuses to create a subscription unless this exact handshake works:
// a POST carrying ?validationToken must come back 200, text/plain, token echoed.
// The route is namespaced per provider so each one's scheme stays addressable.
func TestGraphNotificationValidationHandshake(t *testing.T) {
	s, _ := newTestServer(t)
	h := s.Routes()

	const token = "Validation: Testing client application reachability for subscription"
	req := httptest.NewRequest(http.MethodPost,
		"/notifications/outlook?validationToken="+url.QueryEscape(token), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("content-type = %q, want text/plain", ct)
	}
	if rec.Body.String() != token {
		t.Fatalf("body = %q, want the token echoed verbatim", rec.Body.String())
	}
}

// The notification endpoint must not require our API key: providers cannot
// send custom headers.
func TestGraphNotificationDoesNotRequireAPIKey(t *testing.T) {
	s, _ := newTestServer(t)
	h := s.Routes()

	body := strings.NewReader(`{"value":[{"subscriptionId":"unknown","clientState":"x","changeType":"created"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/notifications/outlook", body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
}

func TestHostedAuthMintsSingleUseConnectLink(t *testing.T) {
	s, db := newTestServer(t)
	h := s.Routes()
	dev, key := seedDev(t, s, "a@x.com")

	req := withKey(httptest.NewRequest(http.MethodPost, "/api/v1/hosted-auth",
		strings.NewReader(`{"success_redirect_url":"https://app.example.com/done"}`)), key)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	var resp hostedAuthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.State == "" || !strings.Contains(resp.URL, "/connect/"+resp.State) {
		t.Fatalf("unexpected connect url: %+v", resp)
	}
	pending, err := db.PeekOAuthState(resp.State)
	if err != nil {
		t.Fatalf("state was not persisted: %v", err)
	}
	if pending.DeveloperID != dev.ID {
		t.Fatalf("pending state owner = %q", pending.DeveloperID)
	}

	// Following the link should render the landing page, whose button embeds a
	// fully-formed Microsoft authorize URL with PKCE set up. This is a page, not
	// a redirect: a bare 302 straight to a login prompt is what a phishing link
	// looks like, so the end user sees a branded confirmation screen first.
	req = httptest.NewRequest(http.MethodGet, "/connect/"+resp.State, nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("connect status = %d, want 200 (a landing page, not a redirect)", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("content-type = %q, want text/html", ct)
	}
	body := rec.Body.String()

	start := strings.Index(body, `href="`)
	if start == -1 {
		t.Fatalf("no link found in landing page: %s", body)
	}
	start += len(`href="`)
	end := strings.Index(body[start:], `"`)
	if end == -1 {
		t.Fatalf("malformed href in landing page: %s", body)
	}
	authorizeURL := html.UnescapeString(body[start : start+end])

	loc, err := url.Parse(authorizeURL)
	if err != nil {
		t.Fatal(err)
	}
	if loc.Host != "login.microsoftonline.com" {
		t.Fatalf("authorize url host = %q", loc.Host)
	}
	q := loc.Query()
	if q.Get("code_challenge") == "" || q.Get("code_challenge_method") != "S256" {
		t.Fatalf("PKCE missing from authorize url: %v", q)
	}
	if q.Get("client_id") != "client-123" || q.Get("state") != resp.State {
		t.Fatalf("authorize params wrong: %v", q)
	}
	if !strings.Contains(q.Get("scope"), "offline_access") {
		t.Fatalf("offline_access missing; there would be no refresh token: %q", q.Get("scope"))
	}
}

// The dashboard shell requires a signed-in session now that hosted auth is
// tenant-scoped: an anonymous visitor has no developer to show it for.
func TestDashboardServesWithSession(t *testing.T) {
	s, _ := newTestServer(t)
	dev, _ := seedDev(t, s, "a@x.com")
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, withSession(t, s, httptest.NewRequest(http.MethodGet, "/dashboard", nil), dev.ID))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `id="gate-form"`) {
		t.Fatal("dashboard did not render the API key gate")
	}
}

// The mail viewer is the same kind of shell as the dashboard: rendered for a
// signed-in developer, not gated client-side by a pasted API key.
func TestMailPageServesWithSession(t *testing.T) {
	s, _ := newTestServer(t)
	dev, _ := seedDev(t, s, "a@x.com")
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, withSession(t, s, httptest.NewRequest(http.MethodGet, "/mail", nil), dev.ID))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("content-type = %q, want text/html", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `id="folders"`) || !strings.Contains(body, `id="messages"`) {
		t.Fatal("mail page did not render the folder/message panes")
	}
}

func TestDashboardLinksToMailPage(t *testing.T) {
	s, _ := newTestServer(t)
	dev, _ := seedDev(t, s, "a@x.com")
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, withSession(t, s, httptest.NewRequest(http.MethodGet, "/dashboard", nil), dev.ID))

	if !strings.Contains(rec.Body.String(), `"/mail`) {
		t.Fatal("dashboard has no link to the mail viewer")
	}
}

func TestConnectRejectsUnknownState(t *testing.T) {
	s, _ := newTestServer(t)
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/connect/nope", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// The connect-time webhook rides on the pending state so the callback can
// bind it to the account once one exists.
func TestHostedAuthStoresPendingWebhook(t *testing.T) {
	s, db := newTestServer(t)
	dev, key := seedDev(t, s, "a@x.com")
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, withKey(httptest.NewRequest(http.MethodPost, "/api/v1/hosted-auth",
		strings.NewReader(`{"webhook":{"url":"https://hook.example.com/in","secret":"s3"}}`)), key))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	var resp hostedAuthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	pending, err := db.PeekOAuthState(resp.State)
	if err != nil {
		t.Fatal(err)
	}
	if pending.DeveloperID != dev.ID {
		t.Fatalf("pending state owner = %q", pending.DeveloperID)
	}
	if pending.Webhook == nil || pending.Webhook.URL != "https://hook.example.com/in" || pending.Webhook.Secret != "s3" {
		t.Fatalf("pending webhook not stored: %+v", pending.Webhook)
	}
	// Unspecified events default to new mail only — that is what the connect
	// caller almost always wants, and it avoids surprising them with updates.
	if len(pending.Webhook.Events) != 1 || pending.Webhook.Events[0] != "mail_received" {
		t.Fatalf("events = %v, want [mail_received]", pending.Webhook.Events)
	}
}

func TestHostedAuthRejectsBadWebhookURL(t *testing.T) {
	s, _ := newTestServer(t)
	_, key := seedDev(t, s, "a@x.com")
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, withKey(httptest.NewRequest(http.MethodPost, "/api/v1/hosted-auth",
		strings.NewReader(`{"webhook":{"url":"not a url"}}`)), key))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestAccountWebhookCRUD(t *testing.T) {
	s, db := newTestServer(t)
	h := s.Routes()
	dev, key := seedDev(t, s, "a@x.com")
	if err := db.UpsertAccount(model.Account{ID: "acc_1", DeveloperID: dev.ID, Provider: "OUTLOOK", Email: "u@x.com", Status: model.AccountOK}); err != nil {
		t.Fatal(err)
	}

	// Unknown account -> 404.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, withKey(httptest.NewRequest(http.MethodPost, "/api/v1/accounts/acc_nope/webhooks",
		strings.NewReader(`{"url":"https://hook.example.com"}`)), key))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown account: status = %d, want 404", rec.Code)
	}

	// Create.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, withKey(httptest.NewRequest(http.MethodPost, "/api/v1/accounts/acc_1/webhooks",
		strings.NewReader(`{"url":"https://hook.example.com","secret":"s3"}`)), key))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: status = %d body = %s", rec.Code, rec.Body.String())
	}
	var created model.Webhook
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.AccountID != "acc_1" || created.Secret != "s3" {
		t.Fatalf("created = %+v", created)
	}
	if len(created.Events) != 1 || created.Events[0] != "mail_received" {
		t.Fatalf("events = %v, want [mail_received]", created.Events)
	}

	// List: scoped to the account, secret hidden.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, withKey(httptest.NewRequest(http.MethodGet, "/api/v1/accounts/acc_1/webhooks", nil), key))
	if rec.Code != http.StatusOK {
		t.Fatalf("list: status = %d", rec.Code)
	}
	var list listResponse[model.Webhook]
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 || list.Items[0].ID != created.ID || list.Items[0].Secret != "" {
		t.Fatalf("list = %+v", list.Items)
	}

	// Delete through the wrong account must not work.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, withKey(httptest.NewRequest(http.MethodDelete, "/api/v1/accounts/acc_other/webhooks/"+created.ID, nil), key))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-account delete: status = %d, want 404", rec.Code)
	}

	// Delete.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, withKey(httptest.NewRequest(http.MethodDelete, "/api/v1/accounts/acc_1/webhooks/"+created.ID, nil), key))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete: status = %d", rec.Code)
	}
	if got, _ := db.ListAccountWebhooks(dev.ID, "acc_1"); len(got) != 0 {
		t.Fatalf("webhook survived delete: %+v", got)
	}
}

// Dead and pending deliveries are visible per webhook, without their payloads.
func TestListWebhookDeliveries(t *testing.T) {
	s, db := newTestServer(t)
	dev, key := seedDev(t, s, "a@x.com")
	now := time.Now().UTC()
	if err := db.SaveWebhook(model.Webhook{ID: "wh_1", DeveloperID: dev.ID, URL: "https://x.example.com", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveDelivery(store.Delivery{
		ID: "dl_1", WebhookID: "wh_1", AccountID: "acc_1", EventType: "mail_received",
		Payload: []byte(`{"secret":"body"}`), Attempts: 8, Dead: true, LastError: "status 500",
		NextAttemptAt: now, CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, withKey(httptest.NewRequest(http.MethodGet, "/api/v1/webhooks/wh_1/deliveries", nil), key))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	var list listResponse[store.Delivery]
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 || !list.Items[0].Dead || list.Items[0].LastError != "status 500" {
		t.Fatalf("items = %+v", list.Items)
	}
	if strings.Contains(rec.Body.String(), "secret") {
		t.Fatal("payload leaked into delivery listing")
	}

	rec = httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, withKey(httptest.NewRequest(http.MethodGet, "/api/v1/webhooks/wh_nope/deliveries", nil), key))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown webhook: status = %d, want 404", rec.Code)
	}
}

// Each account card carries a small form to set that account's webhook.
func TestDashboardRendersWebhookForm(t *testing.T) {
	s, _ := newTestServer(t)
	dev, _ := seedDev(t, s, "a@x.com")
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, withSession(t, s, httptest.NewRequest(http.MethodGet, "/dashboard", nil), dev.ID))
	body := rec.Body.String()
	if !strings.Contains(body, `data-action="set-webhook"`) || !strings.Contains(body, `/webhooks`) {
		t.Fatal("dashboard has no set-webhook form wired to the account webhooks API")
	}
}

// One GET should give a caller the whole message: plain-text body, the folder
// role, and attachment metadata, without further calls on their side.
func TestGetEmailIsComplete(t *testing.T) {
	graph := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/me/messages/M1/attachments" {
			t.Errorf("unexpected Graph call: %s", r.URL)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"value":[{"id":"A1","name":"q3.pdf","contentType":"application/pdf","size":10,"isInline":false}]}`)
	}))
	defer graph.Close()
	prev := outlook.BaseURL
	outlook.BaseURL = graph.URL
	defer func() { outlook.BaseURL = prev }()

	s, db := newTestServer(t)
	dev, key := seedDev(t, s, "a@x.com")
	if err := db.UpsertAccount(model.Account{ID: "acc_1", DeveloperID: dev.ID, Provider: outlook.Name, Email: "u@x.com", Status: model.AccountOK}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertFolder(model.Folder{AccountID: "acc_1", ID: "F1", Name: "Inbox", Role: "inbox"}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertEmail(model.Email{
		AccountID: "acc_1", ID: "M1", ThreadID: "C1", FolderID: "F1", Subject: "Hi",
		Date: time.Now(), Body: "<p>Hello <b>there</b></p>", BodyType: "html", HasAttachments: true,
	}); err != nil {
		t.Fatal(err)
	}

	get := func() model.Email {
		rec := httptest.NewRecorder()
		s.Routes().ServeHTTP(rec, withKey(httptest.NewRequest(http.MethodGet, "/api/v1/emails/M1?account_id=acc_1", nil), key))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
		}
		var e model.Email
		if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
			t.Fatal(err)
		}
		return e
	}

	e := get()
	if e.BodyPlain != "Hello there" {
		t.Fatalf("body_plain = %q", e.BodyPlain)
	}
	if e.Role != "inbox" {
		t.Fatalf("role = %q, want inbox", e.Role)
	}
	if len(e.Attachments) != 1 || e.Attachments[0].Name != "q3.pdf" {
		t.Fatalf("attachments = %+v", e.Attachments)
	}

	// The attachment list is cached: a second read must not go back to Graph.
	graph.Close()
	if e := get(); len(e.Attachments) != 1 {
		t.Fatalf("attachments not cached after first read: %+v", e.Attachments)
	}
}
