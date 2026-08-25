package api

import (
	"context"
	"encoding/json"
	"errors"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gauravrautela/unified-messaging/internal/accounts"
	"github.com/gauravrautela/unified-messaging/internal/auth"
	"github.com/gauravrautela/unified-messaging/internal/chatsync"
	"github.com/gauravrautela/unified-messaging/internal/config"
	"github.com/gauravrautela/unified-messaging/internal/events"
	"github.com/gauravrautela/unified-messaging/internal/logx"
	"github.com/gauravrautela/unified-messaging/internal/model"
	"github.com/gauravrautela/unified-messaging/internal/provider"
	"github.com/gauravrautela/unified-messaging/internal/provider/outlook"
	"github.com/gauravrautela/unified-messaging/internal/provider/providertest"
	"github.com/gauravrautela/unified-messaging/internal/store"
	"github.com/gauravrautela/unified-messaging/internal/syncer"
)

// newTestServerCore is the one real constructor: every other test-server
// helper is a thin wrapper choosing what to keep. FAKECHAT rides alongside
// Outlook so hosted-auth, the connect page and the QR endpoints can all be
// exercised against a real (if scripted) Linker/Chatter without a network.
func newTestServerCore(t *testing.T, maxChatAccounts int) (*Server, *store.Store, *logx.Records) {
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
	fakeChat := providertest.NewFakeChat("FAKECHAT")
	registry := provider.NewRegistry(outlook.New(a, stubTokens{}), fakeChat)
	acctMgr := accounts.NewManager(db, make([]byte, 32), log)
	acctMgr.SetRegistry(registry)
	disp := events.NewDispatcher(db, log)
	sync := syncer.New(db, registry, acctMgr, disp, log, syncer.Options{PollInterval: time.Hour})
	authSvc := auth.New(db, log, cfg.SessionTTL)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	chat := chatsync.New(db, registry, acctMgr, disp, log, chatsync.Options{MaxAccounts: maxChatAccounts})
	chat.Start(ctx)

	srv := NewServer(cfg, db, registry, acctMgr, sync, authSvc, chat, disp, log)
	srv.fakeChat = fakeChat
	return srv, db, recs
}

// fake recovers the concrete FakeChat the test harness wired onto Server.
// Server itself only knows fakeChat as `any` (so the test-only providertest
// package never enters the production import graph); every test goes through
// this instead of asserting the type at each call site.
func (s *Server) fake() *providertest.FakeChat {
	return s.fakeChat.(*providertest.FakeChat)
}

func newTestServerWithLog(t *testing.T) (*Server, *store.Store, *logx.Records) {
	return newTestServerCore(t, 10)
}

func newTestServer(t *testing.T) (*Server, *store.Store) {
	s, db, _ := newTestServerWithLog(t)
	return s, db
}

// newTestServerWithChatCapacity is for tests that need to exhaust the chat
// runtime's connection ceiling on purpose.
func newTestServerWithChatCapacity(t *testing.T, maxChatAccounts int) (*Server, *store.Store) {
	s, db, _ := newTestServerCore(t, maxChatAccounts)
	return s, db
}

// newTestServerWithProviders builds a server around a caller-chosen provider
// set, for tests that need to control provider registration itself (an empty
// mail slate, or more than one mail provider) rather than the standard
// Outlook+FAKECHAT registry every other test uses. Nothing in the resolveProvider
// path this exercises touches chat, sync or accounts, so those are left nil/zero.
func newTestServerWithProviders(t *testing.T, providers ...provider.Provider) (*Server, *store.Store) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "api.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	cfg := &config.Config{
		ClientID: "client-123", Tenant: "consumers",
		RedirectURI: "http://localhost:8080/oauth/callback",
		Scopes:      []string{"offline_access"},
		SessionTTL:  30 * 24 * time.Hour,
	}
	log, _ := logx.Capture()
	registry := provider.NewRegistry(providers...)
	disp := events.NewDispatcher(db, log)
	sync := syncer.New(db, registry, nil, disp, log, syncer.Options{PollInterval: time.Hour})
	authSvc := auth.New(db, log, cfg.SessionTTL)
	return NewServer(cfg, db, registry, nil, sync, authSvc, nil, disp, log), db
}

// mailStub is a bare-bones mail-kind provider.Provider for exercising
// resolveProvider's default-selection logic. It is never actually connected
// to, so every capability but Name/Kind is nil.
type mailStub struct{ name string }

func (m mailStub) Name() string                 { return m.name }
func (m mailStub) Kind() string                 { return model.AccountKindMail }
func (m mailStub) Auth() provider.Authenticator { return nil }
func (m mailStub) Linker() provider.Linker      { return nil }
func (m mailStub) Mailbox() provider.Mailbox    { return nil }
func (m mailStub) Chat() provider.Chatter       { return nil }
func (m mailStub) Push() provider.Pusher        { return nil }

// waitFor polls cond until it is true or the deadline passes, for assertions
// against state a background goroutine (the link pump, the chat actor)
// updates asynchronously.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatal("condition not met")
}

// seedChat links a fake chat account under devID, attaches it to the runtime,
// and gives it one chat with one attendee — the minimum a chat-facing test
// needs to have something to read. It also fills one slot of chat capacity,
// which is exactly what the over-capacity test wants.
func seedChat(t *testing.T, s *Server, db *store.Store, devID string) string {
	t.Helper()
	acct, err := s.accts.ConnectLinked(context.Background(), devID, "FAKECHAT",
		provider.Identity{Identifier: "+919900000000", Name: "Seed"}, "j-seed")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.chat.Attach(acct.ID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return s.fake().Sink(acct.ID) != nil })
	if err := db.UpsertChat(model.Chat{ID: "c1", AccountID: acct.ID, Kind: "direct"}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertAttendee(model.Attendee{ID: "a1", Phone: "+919900000000", Name: "Seed"}, acct.ID); err != nil {
		t.Fatal(err)
	}
	return acct.ID
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

// A delete for an account owned by a different developer must fail closed:
// 404, and the account (and its ownership check) must never be reached with
// an unscoped ID, even on the non-ErrNotFound path.
func TestDeleteAccountFailsClosedAcrossTenants(t *testing.T) {
	s, db := newTestServer(t)
	devA, _ := seedDev(t, s, "a@x.com")
	_, keyB := seedDev(t, s, "b@x.com")
	if err := db.UpsertAccount(model.Account{
		ID: "acc_A", DeveloperID: devA.ID, Provider: "OUTLOOK", Email: "a@x.com", Status: model.AccountOK,
	}); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, withKey(httptest.NewRequest(http.MethodDelete, "/api/v1/accounts/acc_A", nil), keyB))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant delete: status = %d, want 404", rec.Code)
	}
	if _, err := db.GetAccount(devA.ID, "acc_A"); err != nil {
		t.Fatalf("account should have survived the cross-tenant delete attempt: %v", err)
	}
}

// A request whose Content-Length is unknown (a chunked upload, reported as
// -1) must still reach the handler with an intact body: logBody must not
// consume it while deciding whether to log it.
func TestUnsizedBodyIsStillDecoded(t *testing.T) {
	s, _ := newTestServer(t)
	_, key := seedDev(t, s, "a@x.com")

	req := withKey(httptest.NewRequest(http.MethodPost, "/api/v1/webhooks",
		strings.NewReader(`{"url":"https://x","secret":"hush"}`)), key)
	req.Header.Set("Content-Type", "application/json")
	req.ContentLength = -1

	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (an unsized body must still be decoded): %s", rec.Code, rec.Body.String())
	}
}

// CSRF defence-in-depth: a session cookie may only authenticate a
// state-changing write when the request declares Content-Type: application/json,
// since an HTML form cannot set that header. API keys are not subject to
// this check — a form can never carry one.
func TestSessionWritesRequireJSONContentType(t *testing.T) {
	s, _ := newTestServer(t)
	dev, key := seedDev(t, s, "a@x.com")

	form := withSession(t, s, httptest.NewRequest(http.MethodPost, "/api/v1/webhooks",
		strings.NewReader(`url=https://x&secret=hush`)), dev.ID)
	form.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, form)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("session + form content-type: status = %d, want 415", rec.Code)
	}

	jsonReq := withSession(t, s, httptest.NewRequest(http.MethodPost, "/api/v1/webhooks",
		strings.NewReader(`{"url":"https://x","secret":"hush"}`)), dev.ID)
	jsonReq.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, jsonReq)
	if rec.Code != http.StatusCreated {
		t.Fatalf("session + json content-type: status = %d, want 201: %s", rec.Code, rec.Body.String())
	}

	keyForm := withKey(httptest.NewRequest(http.MethodPost, "/api/v1/webhooks",
		strings.NewReader(`url=https://x&secret=hush`)), key)
	keyForm.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, keyForm)
	if rec.Code == http.StatusUnsupportedMediaType {
		t.Fatalf("api key requests must not be subject to the session content-type gate")
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
}

// Without a session, both the dashboard and the mail viewer redirect to
// /login with a next= back to the page the visitor asked for.
func TestPagesRedirectToLoginWithoutSession(t *testing.T) {
	s, _ := newTestServer(t)
	for _, path := range []string{"/dashboard", "/mail?account_id=acc_1"} {
		rec := httptest.NewRecorder()
		s.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusFound {
			t.Fatalf("%s: status = %d, want 302", path, rec.Code)
		}
		loc, _ := url.Parse(rec.Header().Get("Location"))
		if loc.Path != "/login" || loc.Query().Get("next") != path {
			t.Fatalf("%s: location = %q", path, rec.Header().Get("Location"))
		}
	}
}

// The dashboard shows which developer is signed in and lets them manage their
// own API keys; it no longer carries the client-side localStorage key gate.
func TestDashboardShowsDeveloperAndKeysPanel(t *testing.T) {
	s, _ := newTestServer(t)
	dev, _ := seedDev(t, s, "dev@x.com")
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, withSession(t, s, httptest.NewRequest(http.MethodGet, "/dashboard", nil), dev.ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"dev@x.com", `id="keys"`, `data-action="create-key"`, `id="logout-form"`, "/api/v1/api-keys"} {
		if !strings.Contains(body, want) {
			t.Fatalf("dashboard missing %q", want)
		}
	}
	if strings.Contains(body, "um_api_key") || strings.Contains(body, `id="gate-form"`) {
		t.Fatal("dashboard still has the localStorage API-key gate")
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

func postForm(h http.Handler, path string, form url.Values) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestSignupSetsSessionCookieAndRedirects(t *testing.T) {
	s, db, recs := newTestServerWithLog(t)
	rec := postForm(s.Routes(), "/signup", url.Values{
		"email": {"new@x.com"}, "password": {"longenoughpassword"}, "name": {"New"},
	})
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/dashboard" {
		t.Fatalf("status = %d location = %q", rec.Code, rec.Header().Get("Location"))
	}
	var cookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			cookie = c
		}
	}
	if cookie == nil || cookie.Value == "" || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode || cookie.Path != "/" {
		t.Fatalf("session cookie = %+v", cookie)
	}
	if _, _, err := db.SessionDeveloper(cookie.Value, time.Now()); err != nil {
		t.Fatalf("cookie does not resolve to a session: %v", err)
	}
	if _, _, err := db.DeveloperByEmail("new@x.com"); err != nil {
		t.Fatalf("developer not created: %v", err)
	}
	if recs.Contains("longenoughpassword") || recs.Contains(cookie.Value) {
		t.Fatal("password or session id leaked into logs")
	}
	if !recs.Contains("request_id=req_") || !recs.Contains("developer signed up") {
		t.Fatalf("expected request-scoped signup logs: %v", recs.All())
	}
}

func TestSignupAndLoginRejectBadInputInline(t *testing.T) {
	s, _ := newTestServer(t)
	h := s.Routes()
	rec := postForm(h, "/signup", url.Values{"email": {"bad"}, "password": {"short"}})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid email or password") {
		t.Fatalf("signup bad input: %d %s", rec.Code, rec.Body.String())
	}
	seedDev(t, s, "a@x.com")
	rec = postForm(h, "/login", url.Values{"email": {"a@x.com"}, "password": {"wrongpassword!"}})
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "invalid email or password") {
		t.Fatalf("login wrong password: %d %s", rec.Code, rec.Body.String())
	}
	rec = postForm(h, "/login", url.Values{"email": {"a@x.com"}, "password": {"longenoughpassword"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("login ok: %d", rec.Code)
	}
}

func TestLoginHonoursSameOriginNext(t *testing.T) {
	s, _ := newTestServer(t)
	seedDev(t, s, "a@x.com")
	rec := postForm(s.Routes(), "/login?next=/mail?account_id=x", url.Values{"email": {"a@x.com"}, "password": {"longenoughpassword"}})
	if loc := rec.Header().Get("Location"); loc != "/mail?account_id=x" {
		t.Fatalf("location = %q", loc)
	}
	// Browsers normalise a backslash to a forward slash, so "/\evil.com" and
	// "/\/evil.com" are off-origin redirects wearing a same-origin shape.
	for _, next := range []string{
		"https://evil.example.com/",
		`/\evil.com`,
		`/\/evil.com`,
		"//evil.example.com/",
	} {
		rec = postForm(s.Routes(), "/login?next="+url.QueryEscape(next), url.Values{"email": {"a@x.com"}, "password": {"longenoughpassword"}})
		if loc := rec.Header().Get("Location"); loc != "/dashboard" {
			t.Fatalf("open redirect via next=%q: %q", next, loc)
		}
	}
}

func TestLogoutClearsSession(t *testing.T) {
	s, db := newTestServer(t)
	dev, _ := seedDev(t, s, "a@x.com")
	req := withSession(t, s, httptest.NewRequest(http.MethodPost, "/logout", nil), dev.ID)
	tok, _ := req.Cookie(sessionCookie)
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
		t.Fatalf("status = %d location = %q", rec.Code, rec.Header().Get("Location"))
	}
	if _, _, err := db.SessionDeveloper(tok.Value, time.Now()); err == nil {
		t.Fatal("session survived logout")
	}
}

func TestMeReportsAuthKind(t *testing.T) {
	s, _ := newTestServer(t)
	dev, key := seedDev(t, s, "a@x.com")
	for _, tc := range []struct {
		req  *http.Request
		kind string
	}{
		{withKey(httptest.NewRequest(http.MethodGet, "/api/v1/me", nil), key), "api_key"},
		{withSession(t, s, httptest.NewRequest(http.MethodGet, "/api/v1/me", nil), dev.ID), "session"},
	} {
		rec := httptest.NewRecorder()
		s.Routes().ServeHTTP(rec, tc.req)
		var body struct {
			ID    string `json:"id"`
			Email string `json:"email"`
			Auth  string `json:"auth"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body.ID != dev.ID || body.Email != "a@x.com" || body.Auth != tc.kind {
			t.Fatalf("me = %+v, want auth %q", body, tc.kind)
		}
	}
}

func TestAPIKeyEndpoints(t *testing.T) {
	s, _ := newTestServer(t)
	h := s.Routes()
	dev, key := seedDev(t, s, "a@x.com")

	// Minting requires a session.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, withKey(httptest.NewRequest(http.MethodPost, "/api/v1/api-keys", strings.NewReader(`{"name":"x"}`)), key))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("mint with api key: %d, want 403", rec.Code)
	}

	req := withSession(t, s, httptest.NewRequest(http.MethodPost, "/api/v1/api-keys", strings.NewReader(`{"name":"prod"}`)), dev.ID)
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("mint: %d %s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Prefix string `json:"prefix"`
		Key    string `json:"key"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(created.Key, "um_") || created.Prefix != created.Key[:12] || created.Name != "prod" {
		t.Fatalf("created = %+v", created)
	}

	// The new key works, and the list never shows it in full.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, withKey(httptest.NewRequest(http.MethodGet, "/api/v1/api-keys", nil), created.Key))
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), created.Key) || !strings.Contains(rec.Body.String(), created.Prefix) {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}

	// Revoke: session only, and the key dies.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, withKey(httptest.NewRequest(http.MethodDelete, "/api/v1/api-keys/"+created.ID, nil), key))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("revoke with api key: %d, want 403", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, withSession(t, s, httptest.NewRequest(http.MethodDelete, "/api/v1/api-keys/"+created.ID, nil), dev.ID))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("revoke: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, withKey(httptest.NewRequest(http.MethodGet, "/api/v1/me", nil), created.Key))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("revoked key still works: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, withSession(t, s, httptest.NewRequest(http.MethodDelete, "/api/v1/api-keys/key_nope", nil), dev.ID))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("revoke unknown: %d", rec.Code)
	}
}

// The debug body log must never carry message content: a send is logged by
// size, not text. The handler's own outcome is irrelevant here — the body is
// logged by the middleware before the handler ever runs.
func TestRequestBodyLogNeverCarriesMailContent(t *testing.T) {
	s, db, recs := newTestServerWithLog(t)
	dev, key := seedDev(t, s, "a@x.com")
	if err := db.UpsertAccount(model.Account{ID: "acc_1", DeveloperID: dev.ID, Provider: "OUTLOOK", Email: "u@x.com", Status: model.AccountOK}); err != nil {
		t.Fatal(err)
	}
	body := `{"account_id":"acc_1","to":[{"email":"x@y.com"}],"subject":"s","body":"SECRETBODY",` +
		`"attachments":[{"name":"a.txt","content":"QUFBQQ=="}]}`
	req := withKey(httptest.NewRequest(http.MethodPost, "/api/v1/emails", strings.NewReader(body)), key)
	req.Header.Set("Content-Type", "application/json")
	s.Routes().ServeHTTP(httptest.NewRecorder(), req)

	if !recs.Contains("request body") {
		t.Fatalf("the body was never logged, so this test proves nothing: %v", recs.All())
	}
	if recs.Contains("SECRETBODY") {
		t.Fatalf("mail body leaked into the log: %v", recs.All())
	}
	if recs.Contains("QUFBQQ==") {
		t.Fatalf("attachment content leaked into the log: %v", recs.All())
	}
	if !recs.Contains("10 chars") {
		t.Fatalf("body size not recorded: %v", recs.All())
	}
}

// A session that resolves must have its cookie re-issued, otherwise the
// sliding expiry in the database never reaches the browser and the developer
// is logged out mid-work.
func TestSessionRequestRefreshesCookieExpiry(t *testing.T) {
	s, _ := newTestServer(t)
	dev, _ := seedDev(t, s, "a@x.com")
	req := withSession(t, s, httptest.NewRequest(http.MethodGet, "/api/v1/me", nil), dev.ID)
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var got *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			got = c
		}
	}
	if got == nil {
		t.Fatalf("no %s cookie re-issued: %v", sessionCookie, rec.Header().Values("Set-Cookie"))
	}
	if got.Expires.IsZero() || !got.Expires.After(time.Now()) {
		t.Fatalf("cookie expiry = %v, want a future instant", got.Expires)
	}
}

// A page handler must refresh the cookie too, since the dashboard is where a
// long-lived session actually spends its time.
func TestPageSessionRefreshesCookieExpiry(t *testing.T) {
	s, _ := newTestServer(t)
	dev, _ := seedDev(t, s, "a@x.com")
	req := withSession(t, s, httptest.NewRequest(http.MethodGet, "/dashboard", nil), dev.ID)
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie && !c.Expires.IsZero() {
			return
		}
	}
	t.Fatalf("dashboard did not re-issue the session cookie: %v", rec.Header().Values("Set-Cookie"))
}

// Webhook targets are attacker-chosen URLs the server will fetch, and the
// delivery status code comes back through the API — so a loopback or
// link-local target is an SSRF oracle and must be refused up front.
func TestWebhookURLMustBePublic(t *testing.T) {
	s, _ := newTestServer(t)
	_, key := seedDev(t, s, "a@x.com")
	h := s.Routes()

	post := func(t *testing.T, path, body string) int {
		t.Helper()
		req := withKey(httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)), key)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	if code := post(t, "/api/v1/webhooks", `{"url":"http://127.0.0.1:9/x"}`); code != http.StatusBadRequest {
		t.Fatalf("loopback webhook: status = %d, want 400", code)
	}
	if code := post(t, "/api/v1/webhooks", `{"url":"http://169.254.169.254/latest/meta-data/"}`); code != http.StatusBadRequest {
		t.Fatalf("link-local webhook: status = %d, want 400", code)
	}
	if code := post(t, "/api/v1/webhooks", `{"url":"https://hooks.example.com/x"}`); code != http.StatusCreated {
		t.Fatalf("public webhook: status = %d, want 201", code)
	}
	if code := post(t, "/api/v1/hosted-auth", `{"notify_url":"http://10.0.0.5/notify"}`); code != http.StatusBadRequest {
		t.Fatalf("private notify_url: status = %d, want 400", code)
	}
	// Redirect URLs are followed by the browser, not fetched by us: local is fine.
	if code := post(t, "/api/v1/hosted-auth", `{"success_redirect_url":"http://localhost:8080/done"}`); code != http.StatusOK {
		t.Fatalf("localhost success_redirect_url: status = %d, want 200", code)
	}
}

// The dashboard's own Connect button sends success_redirect_url pointing at
// this server's origin (localhost in dev). Redirect URLs are followed by the
// end user's browser, never fetched by us, so the SSRF check must not apply
// to them — only notify_url is a server-to-server target.
func TestHostedAuthAllowsLocalRedirectURLsButNotLocalNotifyURL(t *testing.T) {
	s, _ := newTestServer(t)
	_, key := seedDev(t, s, "a@x.com")
	post := func(body string) int {
		req := withKey(httptest.NewRequest(http.MethodPost, "/api/v1/hosted-auth", strings.NewReader(body)), key)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		s.Routes().ServeHTTP(rec, req)
		return rec.Code
	}
	if c := post(`{"success_redirect_url":"http://localhost:8080/dashboard?connected=1","failure_redirect_url":"http://127.0.0.1:8080/dashboard"}`); c != http.StatusOK {
		t.Fatalf("local redirect urls: status = %d, want 200", c)
	}
	if c := post(`{"notify_url":"http://127.0.0.1:9/hook"}`); c != http.StatusBadRequest {
		t.Fatalf("local notify_url: status = %d, want 400", c)
	}
}

// The integration guide is public: integrators read it before they have an
// account, and it carries nothing secret. It must name every registered API
// route so it cannot drift from what the server actually serves.
func TestDocsPageIsPublicAndCoversEveryRoute(t *testing.T) {
	s, _ := newTestServer(t)
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/docs", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 without a session", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("content-type = %q", ct)
	}
	body := rec.Body.String()
	for _, route := range apiRoutes {
		if !strings.Contains(body, html.EscapeString(route)) {
			t.Errorf("docs page does not mention route %q", route)
		}
	}
	for _, want := range []string{"X-Outlook-Signature", "mail_received", "hosted-auth", "session_required", "30s"} {
		if !strings.Contains(body, want) {
			t.Errorf("docs page missing %q", want)
		}
	}
}

func TestDashboardAndMailLinkToDocs(t *testing.T) {
	s, _ := newTestServer(t)
	dev, _ := seedDev(t, s, "a@x.com")
	for _, path := range []string{"/dashboard", "/mail"} {
		rec := httptest.NewRecorder()
		s.Routes().ServeHTTP(rec, withSession(t, s, httptest.NewRequest(http.MethodGet, path, nil), dev.ID))
		if !strings.Contains(rec.Body.String(), `href="/docs"`) {
			t.Errorf("%s has no link to /docs", path)
		}
	}
}

// llms.txt is the machine-readable twin of /docs: plain Markdown, public,
// exact shapes, every route named.
func TestLLMsTxtIsPublicMarkdownCoveringEveryRoute(t *testing.T) {
	s, _ := newTestServer(t)
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/llms.txt", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/markdown") {
		t.Fatalf("content-type = %q, want text/markdown", ct)
	}
	body := rec.Body.String()
	if strings.Contains(body, "<html") || strings.Contains(body, "<div") {
		t.Fatal("llms.txt must not contain HTML")
	}
	for _, route := range apiRoutes {
		if !strings.Contains(body, route) {
			t.Errorf("llms.txt does not mention %q", route)
		}
	}
	for _, want := range []string{"# ", "Authorization: Bearer", "X-Outlook-Signature", "mail_received", "session_required", "account_id"} {
		if !strings.Contains(body, want) {
			t.Errorf("llms.txt missing %q", want)
		}
	}
	docs := httptest.NewRecorder()
	s.Routes().ServeHTTP(docs, httptest.NewRequest(http.MethodGet, "/docs", nil))
	if !strings.Contains(docs.Body.String(), `href="/llms.txt"`) {
		t.Error("/docs does not link to /llms.txt")
	}
}

func TestLinkerConnectPageRequiresConsentThenServesQR(t *testing.T) {
	s, db := newTestServer(t)
	_, key := seedDev(t, s, "a@x.com")
	h := s.Routes()
	mint := func(body string) string {
		req := withKey(httptest.NewRequest(http.MethodPost, "/api/v1/hosted-auth", strings.NewReader(body)), key)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("hosted-auth: %d %s", rec.Code, rec.Body.String())
		}
		var r hostedAuthResponse
		_ = json.Unmarshal(rec.Body.Bytes(), &r)
		return r.State
	}
	state := mint(`{"provider":"FAKECHAT","notify_url":"https://api.example.com/hook","webhook":{"url":"https://api.example.com/wa"}}`)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/connect/"+state, nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `name="consent"`) || strings.Contains(rec.Body.String(), "login.microsoftonline.com") {
		t.Fatalf("linker page: %d %s", rec.Code, rec.Body.String()[:200])
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/connect/"+state+"/qr", nil))
	if rec.Code != http.StatusConflict {
		t.Fatalf("qr before consent: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/connect/"+state+"/consent", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("consent: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/connect/"+state+"/qr", nil))
	var q struct {
		Status string `json:"status"`
		PNG    string `json:"png_base64"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &q)
	if rec.Code != 200 || q.Status != "waiting" {
		t.Fatalf("first qr poll: %d %+v", rec.Code, q)
	}
	s.fake().EmitCode("qr-abc")
	waitFor(t, func() bool {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/connect/"+state+"/qr", nil))
		_ = json.Unmarshal(rec.Body.Bytes(), &q)
		return q.PNG != ""
	})
	// Pair -> account under the minting developer, notify + webhook bound, state consumed.
	notified := make(chan map[string]any, 1)
	s.notifyTransport = func(url string, payload map[string]any) { notified <- payload }
	s.fake().Pair(provider.Identity{Identifier: "+919888000000", Name: "G"}, "919888000000:5@s.whatsapp.net")
	waitFor(t, func() bool {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/connect/"+state+"/qr", nil))
		_ = json.Unmarshal(rec.Body.Bytes(), &q)
		return q.Status == "paired"
	})
	dev, _, _ := db.DeveloperByEmail("a@x.com")
	accts, _ := db.ListAccounts(dev.ID)
	if len(accts) != 1 || accts[0].Kind != model.AccountKindChat || accts[0].Identifier != "+919888000000" {
		t.Fatalf("accounts = %+v", accts)
	}
	select {
	case p := <-notified:
		if p["status"] != "CREATED" || p["account_id"] != accts[0].ID {
			t.Fatalf("notify = %v", p)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("notify_url not called")
	}
	hooks, _ := db.ListAccountWebhooks(dev.ID, accts[0].ID)
	if len(hooks) != 1 {
		t.Fatalf("connect-time webhook not bound: %+v", hooks)
	}
	if _, err := db.PeekOAuthState(state); err == nil {
		t.Fatal("state not consumed")
	}
	if _, ok := s.chat.HealthFor(accts[0].ID); !ok {
		t.Fatal("runtime did not attach the new account")
	}
}

func TestLinkTimeoutNotifiesFailed(t *testing.T) {
	s, _ := newTestServer(t)
	_, key := seedDev(t, s, "a@x.com")
	h := s.Routes()
	req := withKey(httptest.NewRequest(http.MethodPost, "/api/v1/hosted-auth", strings.NewReader(`{"provider":"FAKECHAT","notify_url":"https://api.example.com/hook"}`)), key)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var r hostedAuthResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &r)
	notified := make(chan map[string]any, 1)
	s.notifyTransport = func(url string, payload map[string]any) { notified <- payload }
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/connect/"+r.State+"/consent", nil))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/connect/"+r.State+"/qr", nil))
	s.fake().FailLink(provider.ErrLinkTimeout)
	select {
	case p := <-notified:
		if p["status"] != "FAILED" || p["error"] != "link_timeout" {
			t.Fatalf("notify = %v", p)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("FAILED not notified")
	}
}

func TestHostedAuthReturnsCapacityWhenRuntimeFull(t *testing.T) {
	s, db := newTestServerWithChatCapacity(t, 1) // same as newTestServer but chatsync.Options{MaxAccounts: 1}
	dev, key := seedDev(t, s, "a@x.com")
	_ = seedChat(t, s, db, dev.ID) // links + attaches one account -> runtime full
	req := withKey(httptest.NewRequest(http.MethodPost, "/api/v1/hosted-auth", strings.NewReader(`{"provider":"FAKECHAT"}`)), key)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "capacity") {
		t.Fatalf("over capacity: %d %s", rec.Code, rec.Body.String())
	}
	// Mail providers are unaffected by chat capacity.
	req = withKey(httptest.NewRequest(http.MethodPost, "/api/v1/hosted-auth", strings.NewReader(`{"provider":"OUTLOOK"}`)), key)
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("mail hosted-auth under chat capacity: %d", rec.Code)
	}
}

// A burst of concurrent first polls for the same state must start exactly
// one pairing session. Before the fix, linkRegistry.put let every racing poll
// call StartLink and then overwrite the registry entry, orphaning every
// loser's session (unreachable by the sweeper) and eventually double-firing
// notify_url / TakeOAuthState from more than one pumpLink goroutine.
func TestLinkQRConcurrentPollsStartExactlyOneSession(t *testing.T) {
	s, _ := newTestServer(t)
	_, key := seedDev(t, s, "a@x.com")
	h := s.Routes()

	req := withKey(httptest.NewRequest(http.MethodPost, "/api/v1/hosted-auth", strings.NewReader(`{"provider":"FAKECHAT"}`)), key)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var r hostedAuthResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &r)

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/connect/"+r.State+"/consent", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("consent: %d", rec.Code)
	}

	const n = 20
	var wg sync.WaitGroup
	codes := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/connect/"+r.State+"/qr", nil))
			codes[i] = rec.Code
		}(i)
	}
	wg.Wait()

	for _, c := range codes {
		if c != http.StatusOK {
			t.Fatalf("a concurrent /qr poll failed: %d", c)
		}
	}
	if n := s.fake().SessionCount(); n != 1 {
		t.Fatalf("StartLink called %d times across concurrent pollers, want 1", n)
	}
}

// /consent and /qr must both refuse a connect link whose own expires_at has
// passed, the same way handleConnectRedirect already does — even though the
// three-minute pairing window (linkTTL) has nothing to do with it.
func TestConsentAndQRRejectExpiredLink(t *testing.T) {
	s, db := newTestServer(t)
	_, key := seedDev(t, s, "a@x.com")
	h := s.Routes()

	req := withKey(httptest.NewRequest(http.MethodPost, "/api/v1/hosted-auth",
		strings.NewReader(`{"provider":"FAKECHAT","expires_in_minutes":1}`)), key)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var r hostedAuthResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &r)

	if err := db.SetOAuthStateExpiry(r.State, time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/connect/"+r.State+"/consent", nil))
	if rec.Code != http.StatusGone {
		t.Fatalf("consent on expired link: %d %s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/connect/"+r.State+"/qr", nil))
	if rec.Code != http.StatusGone {
		t.Fatalf("qr on expired link: %d %s", rec.Code, rec.Body.String())
	}
}

// If the connect link's own expiry elapses while a pairing session is still
// open (a race distinct from the one above: consent and the first /qr poll
// both happened before expiry, but the user took long enough scanning that
// TakeOAuthState fails once they finally do), finishLink must notify FAILED
// link_expired and must not create an account.
func TestFinishLinkNotifiesExpiredWhenLinkExpiresDuringPairing(t *testing.T) {
	s, db := newTestServer(t)
	dev, key := seedDev(t, s, "a@x.com")
	h := s.Routes()

	req := withKey(httptest.NewRequest(http.MethodPost, "/api/v1/hosted-auth",
		strings.NewReader(`{"provider":"FAKECHAT","notify_url":"https://api.example.com/hook"}`)), key)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var r hostedAuthResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &r)

	notified := make(chan map[string]any, 1)
	s.notifyTransport = func(url string, payload map[string]any) { notified <- payload }

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/connect/"+r.State+"/consent", nil))
	// Starts the pairing session (and pumpLink) while the state is still valid.
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/connect/"+r.State+"/qr", nil))

	// Simulate the user taking so long to scan that the connect link's own
	// expiry passes before pairing resolves.
	if err := db.SetOAuthStateExpiry(r.State, time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}

	s.fake().Pair(provider.Identity{Identifier: "+919888000001", Name: "G"}, "919888000001:1@s.whatsapp.net")

	select {
	case p := <-notified:
		if p["status"] != "FAILED" || p["error"] != "link_expired" {
			t.Fatalf("notify = %v", p)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("FAILED link_expired not notified")
	}

	if accts, _ := db.ListAccounts(dev.ID); len(accts) != 0 {
		t.Fatalf("account should not have been created: %+v", accts)
	}
	waitFor(t, func() bool {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/connect/"+r.State+"/qr", nil))
		var q struct {
			Status string `json:"status"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &q)
		return q.Status == "expired"
	})
	// The provider had already paired the device before the link expired;
	// leaving it registered with no account behind it would be a lingering
	// credential, so finishLink must forget it.
	waitFor(t, func() bool {
		for _, jid := range s.fake().Forgotten() {
			if jid == "919888000001:1@s.whatsapp.net" {
				return true
			}
		}
		return false
	})
}

// A pairing that succeeds at the provider but fails to become an account
// (the custodian rejects an empty identifier) must still notify the caller —
// silently dropping it would leave notify_url's contract with "exactly one
// terminal notification per link" broken.
func TestFinishLinkNotifiesFailedWhenAccountCreationFails(t *testing.T) {
	s, db := newTestServer(t)
	dev, key := seedDev(t, s, "a@x.com")
	h := s.Routes()

	req := withKey(httptest.NewRequest(http.MethodPost, "/api/v1/hosted-auth",
		strings.NewReader(`{"provider":"FAKECHAT","notify_url":"https://api.example.com/hook"}`)), key)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var r hostedAuthResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &r)

	notified := make(chan map[string]any, 1)
	s.notifyTransport = func(url string, payload map[string]any) { notified <- payload }

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/connect/"+r.State+"/consent", nil))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/connect/"+r.State+"/qr", nil))

	// An empty Identifier is what accounts.ConnectLinked rejects.
	s.fake().Pair(provider.Identity{Identifier: "", Name: "G"}, "somejid")

	select {
	case p := <-notified:
		if p["status"] != "FAILED" || p["error"] != "link_failed" {
			t.Fatalf("notify = %v", p)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("FAILED link_failed not notified")
	}

	if accts, _ := db.ListAccounts(dev.ID); len(accts) != 0 {
		t.Fatalf("account should not have been created: %+v", accts)
	}
	waitFor(t, func() bool {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/connect/"+r.State+"/qr", nil))
		var q struct {
			Status string `json:"status"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &q)
		return q.Status == "failed"
	})
	// The provider paired "somejid"; our own bookkeeping rejected it, so that
	// device must not be left registered with no account behind it.
	waitFor(t, func() bool {
		for _, jid := range s.fake().Forgotten() {
			if jid == "somejid" {
				return true
			}
		}
		return false
	})
}

// resolveProvider's mail-only fallback must never fall through to
// registry.Default(): an unnamed hosted-auth call must be refused, never
// silently resolved to a Linker, whenever there is not exactly one mail
// provider registered.
func TestResolveProviderRequiresNameWithoutExactlyOneMailProvider(t *testing.T) {
	// Only a chat provider registered: zero mail providers.
	sChatOnly, _ := newTestServerWithProviders(t, providertest.NewFakeChat("FAKECHAT"))
	_, keyChatOnly := seedDev(t, sChatOnly, "a@x.com")
	req := withKey(httptest.NewRequest(http.MethodPost, "/api/v1/hosted-auth", strings.NewReader(`{}`)), keyChatOnly)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	sChatOnly.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("no mail provider registered: status = %d, want 400: %s", rec.Code, rec.Body.String())
	}

	// Two mail providers registered: ambiguous, not "pick one".
	sTwoMail, _ := newTestServerWithProviders(t, mailStub{name: "MAILA"}, mailStub{name: "MAILB"})
	_, keyTwoMail := seedDev(t, sTwoMail, "b@x.com")
	req = withKey(httptest.NewRequest(http.MethodPost, "/api/v1/hosted-auth", strings.NewReader(`{}`)), keyTwoMail)
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	sTwoMail.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("two mail providers registered: status = %d, want 400: %s", rec.Code, rec.Body.String())
	}

	// A named provider still resolves regardless of how many are registered.
	req = withKey(httptest.NewRequest(http.MethodPost, "/api/v1/hosted-auth", strings.NewReader(`{"provider":"MAILA"}`)), keyTwoMail)
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	sTwoMail.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("named provider among several: status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
}

// A slow (or hung) StartLink for one state must never stall a /qr poll for a
// different state. Before the fix, getOrStart held the registry lock across
// the entire StartLink call — for the real whatsmeow adapter, a blocking
// network dial — so one slow dial serialized every other state's poll (and
// the sweeper) behind it.
func TestLinkQRSlowStartLinkDoesNotBlockOtherStates(t *testing.T) {
	s, _ := newTestServer(t)
	_, key := seedDev(t, s, "a@x.com")
	h := s.Routes()

	mint := func() string {
		req := withKey(httptest.NewRequest(http.MethodPost, "/api/v1/hosted-auth", strings.NewReader(`{"provider":"FAKECHAT"}`)), key)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		var r hostedAuthResponse
		_ = json.Unmarshal(rec.Body.Bytes(), &r)
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/connect/"+r.State+"/consent", nil))
		return r.State
	}

	slowState := mint()
	fastState := mint()

	s.fake().SetStartLinkDelay(time.Second)

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/connect/"+slowState+"/qr", nil))
	}()
	// Give the slow poll time to actually enter StartLink (past the delay
	// read) before the fast one races it.
	time.Sleep(100 * time.Millisecond)
	s.fake().SetStartLinkDelay(0)

	start := time.Now()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/connect/"+fastState+"/qr", nil))
	elapsed := time.Since(start)
	if rec.Code != http.StatusOK {
		t.Fatalf("fast state poll: %d %s", rec.Code, rec.Body.String())
	}
	if elapsed > 400*time.Millisecond {
		t.Fatalf("fast state's /qr took %v while a different state's StartLink was in flight; the registry lock is still held across StartLink", elapsed)
	}

	<-done // avoid leaking the slow poll's goroutine past the test
}

// Every /qr poll for a state must see the outcome of that state's StartLink
// call, not just the poll that happened to trigger it. Before the fix, any
// poll that arrived after the placeholder existed but before StartLink
// resolved took a fast path straight to statusResponse() and reported
// "waiting" even once StartLink had already failed.
func TestLinkQRSecondPollerSeesStartLinkFailure(t *testing.T) {
	s, _ := newTestServer(t)
	_, key := seedDev(t, s, "a@x.com")
	h := s.Routes()

	req := withKey(httptest.NewRequest(http.MethodPost, "/api/v1/hosted-auth", strings.NewReader(`{"provider":"FAKECHAT"}`)), key)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var r hostedAuthResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &r)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/connect/"+r.State+"/consent", nil))

	// Set both knobs up front, before any concurrent poll begins, so the two
	// pollers below only ever read them (no concurrent mutation to race on).
	s.fake().SetStartLinkDelay(200 * time.Millisecond)
	s.fake().StartLinkErr = errors.New("dial failed")

	var wg sync.WaitGroup
	codes := make([]int, 2)
	bodies := make([]string, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/connect/"+r.State+"/qr", nil))
			codes[i] = rec.Code
			bodies[i] = rec.Body.String()
		}(i)
	}
	wg.Wait()

	for i, c := range codes {
		if c != http.StatusBadGateway {
			t.Fatalf("poller %d: status = %d, want 502 (body %s)", i, c, bodies[i])
		}
	}
	// StartLink only ever ran once (the loser waited on the winner's outcome
	// rather than dialing itself), and it never produced a session.
	if n := s.fake().SessionCount(); n != 0 {
		t.Fatalf("SessionCount = %d, want 0 (StartLink only ever failed)", n)
	}

	// The failed attempt must not leave the registry stuck: a fresh poll
	// (knobs cleared) starts a brand-new session rather than replaying the
	// old failure.
	s.fake().SetStartLinkDelay(0)
	s.fake().StartLinkErr = nil
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/connect/"+r.State+"/qr", nil))
	var q struct {
		Status string `json:"status"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &q)
	if rec.Code != http.StatusOK || q.Status != "waiting" {
		t.Fatalf("retry after failed start: %d %+v", rec.Code, q)
	}
	if n := s.fake().SessionCount(); n != 1 {
		t.Fatalf("SessionCount = %d, want 1 after the retry succeeds", n)
	}
}

func TestChatRoutesHappyPath(t *testing.T) {
	s, db := newTestServer(t)
	dev, key := seedDev(t, s, "a@x.com")
	acc := seedChat(t, s, db, dev.ID)
	h := s.Routes()
	j := func(method, path, body string, hdr ...string) *httptest.ResponseRecorder {
		req := withKey(httptest.NewRequest(method, path, strings.NewReader(body)), key)
		req.Header.Set("Content-Type", "application/json")
		for i := 0; i+1 < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	if rec := j("GET", "/api/v1/chats?account_id="+acc, ""); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"id":"c1"`) {
		t.Fatalf("list chats: %d %s", rec.Code, rec.Body.String())
	}
	s.fake().SendResult = provider.SendResult{MessageID: "REAL1"}
	rec := j("POST", "/api/v1/chats/c1/messages?account_id="+acc, `{"text":"hello"}`, "Idempotency-Key", "k1")
	if rec.Code != 201 || !strings.Contains(rec.Body.String(), `"id":"REAL1"`) || !strings.Contains(rec.Body.String(), `"status":"sent"`) {
		t.Fatalf("send: %d %s", rec.Code, rec.Body.String())
	}
	replay := j("POST", "/api/v1/chats/c1/messages?account_id="+acc, `{"text":"hello"}`, "Idempotency-Key", "k1")
	if replay.Code != 201 || replay.Body.String() != rec.Body.String() {
		t.Fatalf("idempotent replay differs: %d %s", replay.Code, replay.Body.String())
	}
	if got := s.fake().Commands(); len(got) != 1 {
		t.Fatalf("send called %d times", len(got))
	}
	if c := j("POST", "/api/v1/chats/c1/messages?account_id="+acc, `{"text":"different"}`, "Idempotency-Key", "k1"); c.Code != 409 {
		t.Fatalf("conflict: %d", c.Code)
	}
	if rec := j("GET", "/api/v1/chats/c1/messages?account_id="+acc+"&limit=10", ""); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"id":"REAL1"`) {
		t.Fatalf("list messages: %d %s", rec.Code, rec.Body.String())
	}
	if rec := j("PUT", "/api/v1/chats/c1/messages/REAL1/reaction?account_id="+acc, `{"emoji":"👍"}`); rec.Code != 204 {
		t.Fatalf("react: %d", rec.Code)
	}
	if rec := j("PATCH", "/api/v1/chats/c1/messages/REAL1?account_id="+acc, `{"text":"hello!"}`); rec.Code != 200 {
		t.Fatalf("edit: %d %s", rec.Code, rec.Body.String())
	}
	if rec := j("DELETE", "/api/v1/chats/c1/messages/REAL1?account_id="+acc, ""); rec.Code != 204 {
		t.Fatalf("delete: %d", rec.Code)
	}
	if rec := j("PATCH", "/api/v1/chats/c1?account_id="+acc, `{"read":true}`); rec.Code != 200 {
		t.Fatalf("mark read: %d", rec.Code)
	}
	if rec := j("POST", "/api/v1/chats", `{"account_id":"`+acc+`","phone":"+919888000001","text":"hey"}`); rec.Code != 201 || !strings.Contains(rec.Body.String(), `"chat"`) {
		t.Fatalf("start direct: %d %s", rec.Code, rec.Body.String())
	}
	if rec := j("GET", "/api/v1/attendees?account_id="+acc, ""); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"id":"a1"`) {
		t.Fatalf("attendees: %d", rec.Code)
	}
	// Editing someone else's message is the one legitimate 403.
	_, _ = db.UpsertChatMessage(model.ChatMessage{AccountID: acc, ID: "THEIRS", ChatID: "c1", Sender: model.Attendee{ID: "a1"}, Kind: "text", Text: "x", SentAt: time.Now()})
	if rec := j("PATCH", "/api/v1/chats/c1/messages/THEIRS?account_id="+acc, `{"text":"nope"}`); rec.Code != 403 || !strings.Contains(rec.Body.String(), "not_own_message") {
		t.Fatalf("edit theirs: %d", rec.Code)
	}
	// A mail account cannot use chat routes.
	_ = db.UpsertAccount(model.Account{ID: "acc_mail", DeveloperID: dev.ID, Provider: "OUTLOOK", Email: "m@x.com", Status: model.AccountOK})
	if rec := j("GET", "/api/v1/chats?account_id=acc_mail", ""); rec.Code != 400 || !strings.Contains(rec.Body.String(), "unsupported_for_kind") {
		t.Fatalf("mail on chat route: %d", rec.Code)
	}
}

func TestSendFailureLeavesNoRow(t *testing.T) {
	s, db := newTestServer(t)
	dev, key := seedDev(t, s, "a@x.com")
	acc := seedChat(t, s, db, dev.ID)
	s.fake().CommandErr = errors.New("socket closed")
	req := withKey(httptest.NewRequest("POST", "/api/v1/chats/c1/messages?account_id="+acc, strings.NewReader(`{"text":"x"}`)), key)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)
	if rec.Code != 502 {
		t.Fatalf("send failure: %d", rec.Code)
	}
	msgs, _, _ := db.ListChatMessages(acc, "c1", "", 10)
	if len(msgs) != 0 {
		t.Fatalf("row left behind: %+v", msgs)
	}
}

// TestSendRenameCollisionUsesEchoRow covers fix-round finding 1: the chat
// runtime's own socket can deliver a send's "echo" and insert it under the
// provider's real id before the HTTP handler gets to rename its tmp row, so
// the rename's UPDATE collides on the (account_id, id) primary key. The
// handler must discard its now-stale tmp row and report the pre-existing
// echo row as the outcome, not orphan the tmp row or 500.
func TestSendRenameCollisionUsesEchoRow(t *testing.T) {
	s, db := newTestServer(t)
	dev, key := seedDev(t, s, "a@x.com")
	acc := seedChat(t, s, db, dev.ID)
	s.fake().SendResult = provider.SendResult{MessageID: "ECHO1"}
	if _, err := db.UpsertChatMessage(model.ChatMessage{
		AccountID: acc, ID: "ECHO1", ChatID: "c1",
		Sender: model.Attendee{ID: "seed-self", IsSelf: true}, IsFromMe: true,
		Kind: "text", Text: "hello", SentAt: time.Now(), Status: "sending",
	}); err != nil {
		t.Fatal(err)
	}
	req := withKey(httptest.NewRequest("POST", "/api/v1/chats/c1/messages?account_id="+acc, strings.NewReader(`{"text":"hello"}`)), key)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)
	if rec.Code != 201 || !strings.Contains(rec.Body.String(), `"id":"ECHO1"`) || !strings.Contains(rec.Body.String(), `"status":"sent"`) {
		t.Fatalf("send with rename collision: %d %s", rec.Code, rec.Body.String())
	}
	msgs, _, err := db.ListChatMessages(acc, "c1", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range msgs {
		if strings.HasPrefix(m.ID, "tmp_") {
			t.Fatalf("orphaned tmp row left behind: %+v", m)
		}
	}
}

// TestIdempotencyKeyScopedToOperation covers fix-round finding 2 (ruling):
// the same Idempotency-Key reused across two different chats must conflict
// rather than replay one chat's cached response into the other, since a
// message-send route's account/chat identity rides the URL, not the JSON
// body the hash used to cover alone.
func TestIdempotencyKeyScopedToOperation(t *testing.T) {
	s, db := newTestServer(t)
	dev, key := seedDev(t, s, "a@x.com")
	acc := seedChat(t, s, db, dev.ID)
	if err := db.UpsertChat(model.Chat{ID: "c2", AccountID: acc, Kind: "direct"}); err != nil {
		t.Fatal(err)
	}
	s.fake().SendResult = provider.SendResult{MessageID: "R1"}
	send := func(path string) *httptest.ResponseRecorder {
		req := withKey(httptest.NewRequest("POST", path, strings.NewReader(`{"text":"hi"}`)), key)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", "shared-key")
		rec := httptest.NewRecorder()
		s.Routes().ServeHTTP(rec, req)
		return rec
	}
	first := send("/api/v1/chats/c1/messages?account_id=" + acc)
	if first.Code != 201 {
		t.Fatalf("first send: %d %s", first.Code, first.Body.String())
	}
	second := send("/api/v1/chats/c2/messages?account_id=" + acc)
	if second.Code != 409 || !strings.Contains(second.Body.String(), "idempotency_conflict") {
		t.Fatalf("cross-chat same key: %d %s", second.Code, second.Body.String())
	}
	if got := s.fake().Commands(); len(got) != 1 {
		t.Fatalf("provider called %d times, want 1: %v", len(got), got)
	}
}

// TestConcurrentIdempotentSendsCallProviderOnce covers fix-round finding 5
// (ruling): two requests racing on the same Idempotency-Key must not both
// reach the provider. Exactly one wins the reservation and calls SendText;
// the other either replays the winner's response or gets a conflict — never
// a 5xx, and never a second provider call.
func TestConcurrentIdempotentSendsCallProviderOnce(t *testing.T) {
	s, db := newTestServer(t)
	dev, key := seedDev(t, s, "a@x.com")
	acc := seedChat(t, s, db, dev.ID)
	s.fake().SendResult = provider.SendResult{MessageID: "ONE"}
	h := s.Routes()

	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := withKey(httptest.NewRequest("POST", "/api/v1/chats/c1/messages?account_id="+acc, strings.NewReader(`{"text":"race"}`)), key)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Idempotency-Key", "race-key")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			codes[i] = rec.Code
		}(i)
	}
	wg.Wait()

	for _, c := range codes {
		if c >= 500 {
			t.Fatalf("got a 5xx: %v", codes)
		}
		if c != 201 && c != 409 {
			t.Fatalf("unexpected status: %v", codes)
		}
	}
	if got := s.fake().Commands(); len(got) != 1 {
		t.Fatalf("provider called %d times, want 1: %v", len(got), got)
	}
}

// TestStartChatByAttendeeIDPreservesAttendeeProfile covers fix-round finding
// 3: UpsertAttendee is a full profile overwrite, so starting a chat by
// attendee_id must not blank the attendee's existing name/is_self by writing
// back a bare {id, phone} stand-in.
func TestStartChatByAttendeeIDPreservesAttendeeProfile(t *testing.T) {
	s, db := newTestServer(t)
	dev, key := seedDev(t, s, "a@x.com")
	acc := seedChat(t, s, db, dev.ID) // seeds a1: {ID:"a1", Phone:"+919900000000", Name:"Seed"}
	before, err := db.GetAttendee(acc, "a1")
	if err != nil {
		t.Fatal(err)
	}
	req := withKey(httptest.NewRequest("POST", "/api/v1/chats", strings.NewReader(`{"account_id":"`+acc+`","attendee_id":"a1","text":"hey"}`)), key)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)
	if rec.Code != 201 {
		t.Fatalf("start by attendee_id: %d %s", rec.Code, rec.Body.String())
	}
	after, err := db.GetAttendee(acc, "a1")
	if err != nil {
		t.Fatal(err)
	}
	if after.Name != before.Name || after.IsSelf != before.IsSelf || after.Phone != before.Phone {
		t.Fatalf("attendee profile changed: before=%+v after=%+v", before, after)
	}
}

// TestMailRoutesRejectChatAccount covers fix-round finding 4: resolveID must
// reject a chat account with 400 unsupported_for_kind rather than handing a
// mail handler a nil Mailbox to dereference.
func TestMailRoutesRejectChatAccount(t *testing.T) {
	s, db := newTestServer(t)
	dev, key := seedDev(t, s, "a@x.com")
	acc := seedChat(t, s, db, dev.ID)

	do := func(method, path, body string) *httptest.ResponseRecorder {
		var req *http.Request
		if body != "" {
			req = withKey(httptest.NewRequest(method, path, strings.NewReader(body)), key)
			req.Header.Set("Content-Type", "application/json")
		} else {
			req = withKey(httptest.NewRequest(method, path, nil), key)
		}
		rec := httptest.NewRecorder()
		s.Routes().ServeHTTP(rec, req)
		return rec
	}
	if rec := do("GET", "/api/v1/emails?account_id="+acc, ""); rec.Code != 400 || !strings.Contains(rec.Body.String(), "unsupported_for_kind") {
		t.Fatalf("get emails on chat account: %d %s", rec.Code, rec.Body.String())
	}
	if rec := do("POST", "/api/v1/emails", `{"account_id":"`+acc+`","to":[{"email":"x@y.com"}],"subject":"s","body":"b"}`); rec.Code != 400 || !strings.Contains(rec.Body.String(), "unsupported_for_kind") {
		t.Fatalf("post emails on chat account: %d %s", rec.Code, rec.Body.String())
	}
}

// TestReactionRequiresEmojiField covers the "quick minor": a body that omits
// emoji entirely (or sends invalid JSON for it) is almost certainly a client
// bug and must 400, while an explicit "" is the spec's documented way to
// remove a reaction and must keep working.
func TestReactionRequiresEmojiField(t *testing.T) {
	s, db := newTestServer(t)
	dev, key := seedDev(t, s, "a@x.com")
	acc := seedChat(t, s, db, dev.ID)
	s.fake().SendResult = provider.SendResult{MessageID: "M1"}
	send := withKey(httptest.NewRequest("POST", "/api/v1/chats/c1/messages?account_id="+acc, strings.NewReader(`{"text":"hi"}`)), key)
	send.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, send)
	if rec.Code != 201 {
		t.Fatalf("seed message: %d %s", rec.Code, rec.Body.String())
	}

	missing := withKey(httptest.NewRequest("PUT", "/api/v1/chats/c1/messages/M1/reaction?account_id="+acc, strings.NewReader(`{}`)), key)
	missing.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, missing)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "missing_emoji") {
		t.Fatalf("missing emoji: %d %s", rec.Code, rec.Body.String())
	}

	remove := withKey(httptest.NewRequest("PUT", "/api/v1/chats/c1/messages/M1/reaction?account_id="+acc, strings.NewReader(`{"emoji":""}`)), key)
	remove.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, remove)
	if rec.Code != 204 {
		t.Fatalf("explicit empty emoji (remove) should still work: %d %s", rec.Code, rec.Body.String())
	}
}
