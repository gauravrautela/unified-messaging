package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
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
	"github.com/gauravrautela/unified-messaging/internal/safehttp"
	"github.com/gauravrautela/unified-messaging/internal/store"
	"github.com/gauravrautela/unified-messaging/internal/syncer"
)

// newTestServerCore is the one real constructor: every other test-server
// helper is a thin wrapper choosing what to keep. FAKECHAT rides alongside
// Outlook so hosted-auth, the connect page and the QR endpoints can all be
// exercised against a real (if scripted) Linker/Chatter without a network.
func newTestServerCore(t *testing.T, maxChatAccounts int) (*Server, *store.Store, *logx.Records) {
	t.Helper()
	// The default notify registry, the connect-time notify() client, and
	// checkTelegram all deliver through safehttp.Client, which refuses
	// loopback by default; every httptest receiver these tests point at is
	// loopback.
	safehttp.AllowLoopbackForTests(t)
	db := store.OpenForTest(t)
	db.SetSealKey([]byte("0123456789abcdef0123456789abcdef"))
	cfg := &config.Config{
		ClientID: "client-123", Tenant: "consumers",
		RedirectURI: "http://localhost:8080/oauth/callback",
		// httptest.NewRequest's default Host, so this is the origin every
		// test request genuinely arrives on. Required in production (see
		// config.Load), and it is what the redirect allowlist exempts.
		PublicBaseURL: "http://example.com",
		Scopes:        []string{"offline_access", "Mail.Read"},
		SessionTTL:    30 * 24 * time.Hour,
		// The absolute session lifetime the production default uses; without
		// it every session in these tests would be born already too old.
		SessionMaxAge: 90 * 24 * time.Hour,
	}
	log, recs := logx.Capture()
	a := outlook.NewAuth(cfg.ClientID, "", cfg.Tenant, cfg.RedirectURI, cfg.Scopes)
	fakeChat := providertest.NewFakeChat("FAKECHAT")
	registry := provider.NewRegistry(outlook.New(a, stubTokens{}), fakeChat)
	acctMgr := accounts.NewManager(db, make([]byte, 32), log)
	acctMgr.SetRegistry(registry)
	disp := events.NewDispatcher(db, nil, log)
	sync := syncer.New(db, registry, acctMgr, disp, log, syncer.Options{PollInterval: time.Hour})
	authSvc := auth.New(db, log, cfg.SessionTTL, cfg.SessionMaxAge)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	chat := chatsync.New(db, registry, acctMgr, disp, log, chatsync.Options{MaxAccounts: maxChatAccounts})
	chat.Start(ctx)

	srv := NewServer(cfg, db, registry, acctMgr, sync, authSvc, chat, disp, nil, log)
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
	safehttp.AllowLoopbackForTests(t)
	db := store.OpenForTest(t)
	db.SetSealKey([]byte("0123456789abcdef0123456789abcdef"))
	cfg := &config.Config{
		ClientID: "client-123", Tenant: "consumers",
		RedirectURI:   "http://localhost:8080/oauth/callback",
		PublicBaseURL: "http://example.com",
		Scopes:        []string{"offline_access"},
		SessionTTL:    30 * 24 * time.Hour,
		SessionMaxAge: 90 * 24 * time.Hour,
	}
	log, _ := logx.Capture()
	registry := provider.NewRegistry(providers...)
	disp := events.NewDispatcher(db, nil, log)
	sync := syncer.New(db, registry, nil, disp, log, syncer.Options{PollInterval: time.Hour})
	authSvc := auth.New(db, log, cfg.SessionTTL, cfg.SessionMaxAge)
	return NewServer(cfg, db, registry, nil, sync, authSvc, nil, disp, nil, log), db
}

// newTestServerWithProvidersAndLog is newTestServerWithProviders but also
// wires a real accounts.Manager (so a connect flow can run end to end) and
// exposes the captured log records, for tests asserting what that flow does
// and does not log.
func newTestServerWithProvidersAndLog(t *testing.T, providers ...provider.Provider) (*Server, *store.Store, *logx.Records) {
	t.Helper()
	safehttp.AllowLoopbackForTests(t)
	db := store.OpenForTest(t)
	db.SetSealKey([]byte("0123456789abcdef0123456789abcdef"))
	cfg := &config.Config{
		ClientID: "client-123", Tenant: "consumers",
		RedirectURI:   "http://localhost:8080/oauth/callback",
		PublicBaseURL: "http://example.com",
		Scopes:        []string{"offline_access"},
		SessionTTL:    30 * 24 * time.Hour,
		SessionMaxAge: 90 * 24 * time.Hour,
	}
	log, recs := logx.Capture()
	registry := provider.NewRegistry(providers...)
	acctMgr := accounts.NewManager(db, make([]byte, 32), log)
	acctMgr.SetRegistry(registry)
	disp := events.NewDispatcher(db, nil, log)
	sync := syncer.New(db, registry, acctMgr, disp, log, syncer.Options{PollInterval: time.Hour})
	authSvc := auth.New(db, log, cfg.SessionTTL, cfg.SessionMaxAge)
	return NewServer(cfg, db, registry, acctMgr, sync, authSvc, nil, disp, nil, log), db, recs
}

// fakeMailAuth is a minimal provider.Authenticator whose Exchange/Identify
// never touch the network, so a connect flow's success path — code exchange,
// identify, account persistence — can be driven end to end in a test without
// a live provider on the other end.
type fakeMailAuth struct{ email string }

func (f fakeMailAuth) AuthorizeURL(state, challenge string, forceConsent bool) string {
	return "https://example.com/authorize?state=" + state
}

func (f fakeMailAuth) Exchange(ctx context.Context, code, verifier string) (provider.Token, error) {
	return provider.Token{AccessToken: "tok", RefreshToken: "rtok", ExpiresAt: time.Now().Add(time.Hour)}, nil
}

func (f fakeMailAuth) Refresh(ctx context.Context, refreshToken string) (provider.Token, error) {
	return provider.Token{AccessToken: "tok2", RefreshToken: "rtok2", ExpiresAt: time.Now().Add(time.Hour)}, nil
}

func (f fakeMailAuth) Identify(ctx context.Context, accessToken string) (provider.Identity, error) {
	return provider.Identity{Identifier: f.email, Email: f.email, Name: "Victim"}, nil
}

// fakeAuthMail is a FakeMail (a real Mailbox/Pusher with no network
// dependency) fitted with a working Auth(), so the OAuth callback's full
// success path runs against fakeMailAuth instead of a live provider.
type fakeAuthMail struct {
	*providertest.FakeMail
	auth provider.Authenticator
}

func (f fakeAuthMail) Auth() provider.Authenticator { return f.auth }

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
// waitForTimeout is generous enough to cover the async pipeline it polls
// even when every store write in it is a real network round trip (running
// against TEST_DATABASE_URL) rather than SQLite's effectively-free local
// ones. It only bounds how long a test waits before failing — the fast
// (SQLite) path still returns as soon as cond() is true, typically within a
// poll or two.
const waitForTimeout = 20 * time.Second

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(waitForTimeout); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatal("condition not met")
}

// openLink simulates a browser's first visit to a Linker provider's connect
// landing page and returns the um_link cookie handleConnectRedirect mints
// there, so a test can attach it to consent/qr requests the way a real
// browser's cookie jar would.
func openLink(t *testing.T, h http.Handler, state string) *http.Cookie {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/connect/"+state, nil))
	for _, c := range rec.Result().Cookies() {
		if c.Name == cookieLinkName {
			return c
		}
	}
	t.Fatal("connect landing page did not set the um_link cookie")
	return nil
}

// linkReq builds a request for the connect/consent/qr flow carrying cookie,
// the way a browser would attach it automatically.
func linkReq(method, path string, cookie *http.Cookie) *http.Request {
	req := httptest.NewRequest(method, path, nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	return req
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
	if err := db.SetRedirectDomains(dev.ID, []string{"app.example.com"}); err != nil {
		t.Fatal(err)
	}

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

	// The landing page renders through the shared public layout now, so the
	// first href in the document is the stylesheet's. The authorize URL is the
	// one the Continue button carries, and pinning it to that button is also
	// what keeps this test honest: it asserts the link the end user actually
	// clicks, not whichever href happens to come first in the markup.
	const continueHref = `<a class="btn primary" href="`
	start := strings.Index(body, continueHref)
	if start == -1 {
		t.Fatalf("no authorize link found in landing page: %s", body)
	}
	start += len(continueHref)
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

// The dashboard shows which developer is signed in and sections them through
// the shared design system: every panel is addressable by hash, the shared
// stylesheet and helpers are loaded from /static, and nothing on the page
// falls back to a browser dialog or to client-side credential storage.
func TestDashboardShowsDeveloperAndKeysPanel(t *testing.T) {
	s, _ := newTestServer(t)
	dev, _ := seedDev(t, s, "a@x.com")
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, withSession(t, s, httptest.NewRequest(http.MethodGet, "/dashboard", nil), dev.ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"a@x.com", `href="#api-keys"`, `id="api-keys"`, `id="webhooks"`, `id="settings"`,
		"/static/app.js", `aria-current="page"`, "Entropix", "/api/v1/api-keys",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("dashboard missing %q", want)
		}
	}
	// A native browser dialog blocks the page and cannot be styled, and a
	// credential in localStorage outlives the session cookie. The design
	// system has um.notice and um.confirm instead — so the check is for a
	// *bare* alert()/confirm(), not for the um.-qualified helpers a page is
	// meant to call.
	for _, re := range nativeDialogCalls {
		if hit := re.FindString(body); hit != "" {
			t.Fatalf("dashboard calls a native browser dialog: %q", strings.TrimSpace(hit))
		}
	}
	if strings.Contains(body, "localStorage") {
		t.Fatal("dashboard still uses localStorage")
	}
}

// nativeDialogCalls match window.alert(/alert(/confirm( but not um.alert( or
// um.confirm(: the leading class rejects a "." before the name, so only an
// unqualified call (or an explicit window.-qualified one) trips them.
var nativeDialogCalls = []*regexp.Regexp{
	regexp.MustCompile(`(?:^|[^.\w])(?:window\.)?alert\(`),
	regexp.MustCompile(`(?:^|[^.\w])(?:window\.)?confirm\(`),
}

// app.js is loaded with defer, so it has not run while the inline script is
// being evaluated: window.um does not exist yet, and #notice (in layout_end)
// has not been parsed either. Everything the page does therefore has to wait
// for DOMContentLoaded — reading um at the top level would throw a
// ReferenceError and leave the page inert.
//
// Every page with an inline script is checked, not just the dashboard: the
// rule is a property of the shared layout, so a new page inheriting it is
// exactly where the mistake would reappear.
func TestPageScriptsWaitForDeferredHelpers(t *testing.T) {
	s, db := newTestServer(t)
	dev, _ := seedDev(t, s, "a@x.com")
	// The QR connect page needs a live link state to render; nothing else
	// here needs any setup at all.
	if err := db.SaveOAuthState(store.OAuthState{
		State: "st_defer", DeveloperID: dev.ID, Provider: "FAKECHAT",
		ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		path    string
		session bool
	}{
		{"/dashboard", true},
		{"/mail", true},
		{"/chat", true},
		{"/docs", true},
		{"/docs", false},
		{"/login", false},
		{"/", false},
		{"/connect/st_defer", false},
	} {
		name := tc.path
		if tc.session {
			name += " (signed in)"
		}
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			if tc.session {
				req = withSession(t, s, req, dev.ID)
			}
			rec := httptest.NewRecorder()
			s.Routes().ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			body := rec.Body.String()

			if !strings.Contains(body, `src="/static/app.js?v=`) || !strings.Contains(body, "defer") {
				t.Fatal("page does not load app.js with defer; this test's premise no longer holds")
			}
			start := strings.Index(body, "<script>")
			if start < 0 {
				t.Skip("page has no inline script")
			}
			script := body[start:]
			ready := strings.Index(script, "DOMContentLoaded")
			if ready < 0 {
				t.Fatal("page script never waits for DOMContentLoaded, so it runs before the deferred app.js")
			}
			if strings.Contains(script[:ready], "= um") {
				t.Fatal("page script reads um before DOMContentLoaded, when app.js has not run yet")
			}
		})
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
	body := rec.Body.String()
	// Top-level nav reaches mail and chat even with zero accounts.
	for _, want := range []string{`href="/mail"`, `href="/chat"`, `href="/docs"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("dashboard nav missing %q", want)
		}
	}
}

// The dashboard offers a provider picker for the connect flow and renders
// chat accounts with a Reconnect action, a masked-phone helper, and a link
// into the chat viewer. The status badge is never the raw socket state: the
// human mapping is um.accountState in app.js, which the chat page reuses, so
// no "c.state" branch may appear in the page itself.
func TestDashboardShowsProviderPickerAndChatCards(t *testing.T) {
	s, db := newTestServer(t)
	dev, _ := seedDev(t, s, "a@x.com")
	_ = seedChat(t, s, db, dev.ID)
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, withSession(t, s, httptest.NewRequest("GET", "/dashboard", nil), dev.ID))
	body := rec.Body.String()
	for _, want := range []string{`id="connect-dialog"`, `um.accountState(`, `data-action="reconnect"`, `/chat?account_id=`, `um.maskPhone(`} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard missing %q", want)
		}
	}
	for _, never := range []string{"c.state", "connection.state"} {
		if strings.Contains(body, never) {
			t.Errorf("dashboard interpolates %q instead of going through um.accountState", never)
		}
	}
}

// The chat viewer is session-gated exactly like the mail viewer, and renders
// the chat list and message panes plus the chat REST endpoints it drives.
func TestChatViewerIsSessionGated(t *testing.T) {
	s, _ := newTestServer(t)
	dev, _ := seedDev(t, s, "a@x.com")
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, httptest.NewRequest("GET", "/chat?account_id=x", nil))
	if rec.Code != 302 {
		t.Fatalf("no session: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, withSession(t, s, httptest.NewRequest("GET", "/chat?account_id=x", nil), dev.ID))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `id="chats"`) || !strings.Contains(rec.Body.String(), `id="messages"`) || !strings.Contains(rec.Body.String(), `/api/v1/chats`) {
		t.Fatalf("viewer: %d", rec.Code)
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
	// The filter is stored as given; the kind-specific default (new mail for
	// a mailbox, new chat message for a chat account) is applied when the
	// hook is bound to the finished account.
	if len(pending.Webhook.Events) != 0 {
		t.Fatalf("events = %v, want none until bind", pending.Webhook.Events)
	}
}

// The account-connected log lines are the one place an end user's address
// used to reach INFO in clear: both accounts.Connect and handleOAuthCallback
// log "account connected". Both must now carry only a digest, never the
// address itself.
func TestConnectLogsDigestNotEmail(t *testing.T) {
	const victimEmail = "victim@example.com"
	fp := fakeAuthMail{FakeMail: providertest.NewFakeMail("FAKEMAIL"), auth: fakeMailAuth{email: victimEmail}}
	s, _, recs := newTestServerWithProvidersAndLog(t, fp)
	h := s.Routes()
	_, key := seedDev(t, s, "dev@x.com")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, withKey(httptest.NewRequest(http.MethodPost, "/api/v1/hosted-auth", nil), key))
	if rec.Code != http.StatusOK {
		t.Fatalf("hosted-auth: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp hostedAuthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/oauth/callback?code=fakecode&state="+resp.State, nil))
	if rec.Code < 200 || rec.Code >= 400 {
		t.Fatalf("oauth callback: status=%d body=%s", rec.Code, rec.Body.String())
	}

	if !recs.Contains("email_digest=h_") {
		t.Fatalf("account-connected log missing email_digest: %v", recs.All())
	}
	if recs.Contains(victimEmail) {
		t.Fatalf("account-connected log leaked the address: %v", recs.All())
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
	if list.Limit != 50 || list.Offset != 0 {
		t.Fatalf("limit/offset = %d/%d, want defaults 50/0", list.Limit, list.Offset)
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
	for _, want := range []string{`name="kind"`, `value="discord"`, `value="telegram"`, `name="bot_token"`, `name="chat_id"`, `data-kind-fields`} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard missing %q", want)
		}
	}
	// The event checkboxes are rendered from the Go constants, so a new event
	// name cannot go missing from the picker.
	for _, e := range webhookEvents {
		if !model.KnownEvent(e) {
			t.Errorf("webhookEvents lists %q, which the API rejects", e)
		}
		if !strings.Contains(body, `value="`+e+`"`) {
			t.Errorf("dashboard event picker missing %q", e)
		}
	}
	// A Discord webhook URL is a bearer credential, and a dashboard ends up in
	// screenshots and screen shares. The card masks it client-side, mirroring
	// notify.MaskDiscordURL — the hook is still returned in full by the API.
	if !strings.Contains(body, `"$1/•••"`) {
		t.Error("dashboard does not mask the discord token when rendering a hook")
	}
	if !strings.Contains(body, `id="redirect-domains"`) {
		t.Error("dashboard has no redirect-domains settings field")
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

// newCSRF fetches the sign-in page the way a browser would and returns the
// um_csrf cookie it sets, which is half of the double-submit pair every form
// post now has to carry.
func newCSRF(t *testing.T, h http.Handler) *http.Cookie {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/login", nil))
	for _, c := range rec.Result().Cookies() {
		if c.Name == csrfCookie {
			return c
		}
	}
	t.Fatal("no csrf cookie on the sign-in page")
	return nil
}

// postFormCSRF posts a form the way the rendered page does: the um_csrf
// cookie plus the matching hidden field.
func postFormCSRF(t *testing.T, h http.Handler, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	c := newCSRF(t, h)
	form.Set("csrf", c.Value)
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(c)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func loginForm(t *testing.T, h http.Handler, email, password string) *httptest.ResponseRecorder {
	t.Helper()
	return postFormCSRF(t, h, "/login", url.Values{"email": {email}, "password": {password}})
}

func TestSignupSetsSessionCookieAndRedirects(t *testing.T) {
	s, db, recs := newTestServerWithLog(t)
	rec := postFormCSRF(t, s.Routes(), "/signup", url.Values{
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
	if _, _, _, err := db.SessionDeveloper(auth.HashKey(cookie.Value), time.Now()); err != nil {
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

// The login-attempt DEBUG line used to log the submitted address in clear
// (a carry-over from the signup path's own fix); it must now carry only a
// digest, like signup already does.
func TestLoginAttemptLogsDigestNotEmail(t *testing.T) {
	s, _, recs := newTestServerWithLog(t)
	h := s.Routes()
	seedDev(t, s, "a@x.com")

	rec := loginForm(t, h, "a@x.com", "longenoughpassword")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("login: %d", rec.Code)
	}
	var loginLine string
	for _, l := range recs.All() {
		if strings.Contains(l, "login attempt") {
			loginLine = l
		}
	}
	if loginLine == "" || !strings.Contains(loginLine, "email_digest=h_") {
		t.Fatalf("expected a digested login-attempt log, got: %q", loginLine)
	}
	if strings.Contains(loginLine, "email=a@x.com") {
		t.Fatalf("login attempt leaked the address into the log: %q", loginLine)
	}
}

func TestSignupAndLoginRejectBadInputInline(t *testing.T) {
	s, _ := newTestServer(t)
	h := s.Routes()
	rec := postFormCSRF(t, h, "/signup", url.Values{"email": {"bad"}, "password": {"short"}})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), uniformSignupError) {
		t.Fatalf("signup bad input: %d %s", rec.Code, rec.Body.String())
	}
	seedDev(t, s, "a@x.com")
	rec = loginForm(t, h, "a@x.com", "wrongpassword!")
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "invalid email or password") {
		t.Fatalf("login wrong password: %d %s", rec.Code, rec.Body.String())
	}
	rec = loginForm(t, h, "a@x.com", "longenoughpassword")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("login ok: %d", rec.Code)
	}
}

func TestLoginHonoursSameOriginNext(t *testing.T) {
	s, _ := newTestServer(t)
	seedDev(t, s, "a@x.com")
	rec := postFormCSRF(t, s.Routes(), "/login?next=/mail?account_id=x", url.Values{"email": {"a@x.com"}, "password": {"longenoughpassword"}})
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
		rec = postFormCSRF(t, s.Routes(), "/login?next="+url.QueryEscape(next), url.Values{"email": {"a@x.com"}, "password": {"longenoughpassword"}})
		if loc := rec.Header().Get("Location"); loc != "/dashboard" {
			t.Fatalf("open redirect via next=%q: %q", next, loc)
		}
	}
}

func TestLogoutClearsSession(t *testing.T) {
	s, db := newTestServer(t)
	h := s.Routes()
	dev, _ := seedDev(t, s, "a@x.com")

	// An attacker's cross-site POST to /logout has no token, so it cannot
	// plant a logged-out (or, on /login, attacker-owned) session.
	noToken := withSession(t, s, httptest.NewRequest(http.MethodPost, "/logout", nil), dev.ID)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, noToken)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("logout without a csrf token: %d %s", rec.Code, rec.Body.String())
	}
	// Refusing has to mean the session is untouched: a response that 403s but
	// still ships a session-clearing cookie logs the visitor out anyway, which
	// is the exact attack.
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			t.Fatalf("refused logout still touched the session cookie: %+v", c)
		}
	}
	if raw := rec.Header().Get("Set-Cookie"); strings.Contains(raw, sessionCookie+"=;") || strings.Contains(raw, "Max-Age=0") {
		t.Fatalf("refused logout cleared a cookie: %q", raw)
	}
	if _, _, err := s.auth.SessionDeveloper(context.Background(), mustCookie(t, noToken, sessionCookie)); err != nil {
		t.Fatalf("refused logout deleted the session server-side: %v", err)
	}

	c := newCSRF(t, h)
	req := withSession(t, s, httptest.NewRequest(http.MethodPost, "/logout",
		strings.NewReader(url.Values{"csrf": {c.Value}}.Encode())), dev.ID)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(c)
	tok, _ := req.Cookie(sessionCookie)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
		t.Fatalf("status = %d location = %q", rec.Code, rec.Header().Get("Location"))
	}
	if _, _, _, err := db.SessionDeveloper(auth.HashKey(tok.Value), time.Now()); err == nil {
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
	s, db := newTestServer(t)
	dev, key := seedDev(t, s, "a@x.com")
	if err := db.SetRedirectDomains(dev.ID, []string{"localhost"}); err != nil {
		t.Fatal(err)
	}
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
	s, db := newTestServer(t)
	dev, key := seedDev(t, s, "a@x.com")
	if err := db.SetRedirectDomains(dev.ID, []string{"localhost", "127.0.0.1"}); err != nil {
		t.Fatal(err)
	}
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

// The open-redirect fix (I2): a hosted-auth success/failure redirect must
// land on this server's own origin or on the developer's allowlist, a
// wildcard entry covers subdomains but not the apex, an API key cannot touch
// the allowlist, and malformed entries are rejected up front.
func TestHostedAuthRedirectMustBeAllowlisted(t *testing.T) {
	s, _ := newTestServer(t)
	dev, key := seedDev(t, s, "a@x.com")
	h := s.Routes()
	mint := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, withKey(httptest.NewRequest("POST", "/api/v1/hosted-auth", strings.NewReader(body)), key))
		return rec
	}
	if rec := mint(`{"success_redirect_url":"https://app.customer.com/done"}`); rec.Code != 400 || !strings.Contains(rec.Body.String(), "allowlist") {
		t.Fatalf("unlisted host: %d %s", rec.Code, rec.Body.String())
	}
	// own origin is always fine (httptest host is example.com)
	if rec := mint(`{"success_redirect_url":"http://example.com/dashboard"}`); rec.Code != 200 {
		t.Fatalf("own origin: %d %s", rec.Code, rec.Body.String())
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("PUT", "/api/v1/me/redirect-domains", strings.NewReader(`{"domains":["*.customer.com","exact.example.org"]}`))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rec, withSession(t, s, req, dev.ID))
	if rec.Code != 200 {
		t.Fatalf("set: %d %s", rec.Code, rec.Body.String())
	}
	if rec := mint(`{"success_redirect_url":"https://app.customer.com/done","failure_redirect_url":"https://exact.example.org/x"}`); rec.Code != 200 {
		t.Fatalf("listed: %d %s", rec.Code, rec.Body.String())
	}
	if rec := mint(`{"success_redirect_url":"https://customer.com/done"}`); rec.Code != 400 {
		t.Fatal("apex is not covered by *.customer.com")
	}
	// Allowlist bypass shapes: none of these may be accepted, even though
	// *.customer.com is genuinely on the allowlist at this point. url.Parse's
	// Hostname() must resolve to the real authority (never a userinfo
	// prefix or a suffix trick), and a non-http(s) scheme must never reach
	// the host check at all.
	for _, bypass := range []string{
		`https://app.customer.com@evil.com/`, // userinfo: real host is evil.com
		`https://app.customer.com.evil.com/`, // suffix trick: real host ends in evil.com
		`javascript:alert(1)`,                // no host at all; wrong scheme
	} {
		if rec := mint(`{"success_redirect_url":"` + bypass + `"}`); rec.Code != 400 {
			t.Errorf("bypass %q: status = %d, want 400 (body %s)", bypass, rec.Code, rec.Body.String())
		}
	}
	// api key cannot change the list; bad entries rejected
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, withKey(httptest.NewRequest("PUT", "/api/v1/me/redirect-domains", strings.NewReader(`{"domains":[]}`)), key))
	if rec.Code != 403 {
		t.Fatalf("api key: %d", rec.Code)
	}
	for _, bad := range []string{`{"domains":["10.0.0.1"]}`, `{"domains":["not a host"]}`, `{"domains":["http://x.com"]}`} {
		rec = httptest.NewRecorder()
		req = httptest.NewRequest("PUT", "/api/v1/me/redirect-domains", strings.NewReader(bad))
		req.Header.Set("Content-Type", "application/json")
		h.ServeHTTP(rec, withSession(t, s, req, dev.ID))
		if rec.Code != 400 {
			t.Errorf("%s: %d", bad, rec.Code)
		}
	}
	// GET /me shows the list
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, withKey(httptest.NewRequest("GET", "/api/v1/me", nil), key))
	if !strings.Contains(rec.Body.String(), `"redirect_domains":["*.customer.com","exact.example.org"]`) {
		t.Fatalf("me: %s", rec.Body.String())
	}
}

// The own-origin exemption from the redirect allowlist must come from the
// configured PUBLIC_BASE_URL alone. Deriving it from the request's Host
// header let any caller name their own domain as "our own origin" and mint a
// connect link that bounces the end user's browser anywhere they liked —
// audit finding I2, reopened through a header the client fully controls.
func TestHostedAuthRedirectIgnoresSpoofedHostHeader(t *testing.T) {
	s, _ := newTestServer(t)
	_, key := seedDev(t, s, "a@x.com")
	h := s.Routes()

	req := withKey(httptest.NewRequest(http.MethodPost, "/api/v1/hosted-auth",
		strings.NewReader(`{"success_redirect_url":"https://evil.example/landing"}`)), key)
	req.Header.Set("Content-Type", "application/json")
	req.Host = "evil.example"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("spoofed Host: status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
	// The configured origin still is exempt, spoofed Host or not.
	req = withKey(httptest.NewRequest(http.MethodPost, "/api/v1/hosted-auth",
		strings.NewReader(`{"success_redirect_url":"http://example.com/dashboard"}`)), key)
	req.Header.Set("Content-Type", "application/json")
	req.Host = "evil.example"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("configured origin: status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
}

// I-1: um_link's lifetime is the connect state's, not a fixed 180 s.
//
// The cookie used to expire after linkTTL (3 minutes) while the state it
// binds lives 30, and ensureLinkCookie returned early whenever one existed,
// so it never slid either. A user who read the disclosure, consented, and
// then took more than about two minutes to scan lost the cookie mid-flow:
// every /qr poll from then on answered 403 link_browser_mismatch, and
// reloading minted a second value the persisted claim refuses — killing the
// link for its remaining 27 minutes.
func TestLinkCookieLifetimeFollowsTheConnectState(t *testing.T) {
	s, _ := newTestServer(t)
	_, key := seedDev(t, s, "a@x.com")
	h := s.Routes()

	req := withKey(httptest.NewRequest(http.MethodPost, "/api/v1/hosted-auth",
		strings.NewReader(`{"provider":"FAKECHAT","expires_in_minutes":30}`)), key)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("hosted-auth: %d %s", rec.Code, rec.Body.String())
	}
	var minted hostedAuthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &minted); err != nil {
		t.Fatal(err)
	}

	linkCookie := func(rec *httptest.ResponseRecorder) *http.Cookie {
		t.Helper()
		for _, c := range rec.Result().Cookies() {
			if c.Name == cookieLinkName {
				return c
			}
		}
		return nil
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/connect/"+minted.State, nil))
	first := linkCookie(rec)
	if first == nil {
		t.Fatalf("connect landing page did not set %s: %v", cookieLinkName, rec.Header().Values("Set-Cookie"))
	}
	// Whole minutes of slack for the round trip, but nowhere near the old 180.
	if first.MaxAge < 29*60 || first.MaxAge > 30*60 {
		t.Fatalf("%s Max-Age = %d, want ≈ the state's remaining 30 minutes", cookieLinkName, first.MaxAge)
	}

	// A second render of the same page must re-issue the value the browser
	// already has (sliding it), never skip the Set-Cookie and never mint a
	// new value out from under an in-progress pairing attempt.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, linkReq(http.MethodGet, "/connect/"+minted.State, first))
	again := linkCookie(rec)
	if again == nil {
		t.Fatalf("re-render did not re-issue %s: %v", cookieLinkName, rec.Header().Values("Set-Cookie"))
	}
	if again.Value != first.Value {
		t.Fatalf("re-render minted a new %s value; the in-progress claim would be lost", cookieLinkName)
	}
	if again.MaxAge < 29*60 || again.MaxAge > 30*60 {
		t.Fatalf("re-issued %s Max-Age = %d, want ≈ the state's remaining 30 minutes", cookieLinkName, again.MaxAge)
	}

	// And the flow still works with that cookie, which is the point.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, linkReq(http.MethodPost, "/connect/"+minted.State+"/consent", first))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("consent: %d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, linkReq(http.MethodGet, "/connect/"+minted.State+"/qr", first))
	if rec.Code != http.StatusOK {
		t.Fatalf("qr: %d %s", rec.Code, rec.Body.String())
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
	for _, want := range []string{"chat_received", "Linked devices", "Idempotency-Key"} {
		if !strings.Contains(body, want) {
			t.Errorf("docs page missing %q", want)
		}
	}
	for _, want := range []string{"kind", "discord.com/api/webhooks", "bot_token", "@BotFather"} {
		if !strings.Contains(body, want) {
			t.Errorf("docs page missing %q", want)
		}
	}
	for _, want := range []string{"redirect-domains", "body_too_large", "me/password"} {
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
	for _, want := range []string{"chat_received", "Linked devices", "Idempotency-Key"} {
		if !strings.Contains(body, want) {
			t.Errorf("llms.txt missing %q", want)
		}
	}
	for _, want := range []string{"kind", "discord.com/api/webhooks", "bot_token", "@BotFather"} {
		if !strings.Contains(body, want) {
			t.Errorf("llms.txt missing %q", want)
		}
	}
	for _, want := range []string{"redirect-domains", "body_too_large", "me/password"} {
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
	var cookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == cookieLinkName {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("connect landing page did not set the um_link cookie")
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/connect/"+state+"/qr", nil))
	if rec.Code != http.StatusConflict {
		t.Fatalf("qr before consent: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, linkReq(http.MethodPost, "/connect/"+state+"/consent", cookie))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("consent: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, linkReq(http.MethodGet, "/connect/"+state+"/qr", cookie))
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
		h.ServeHTTP(rec, linkReq(http.MethodGet, "/connect/"+state+"/qr", cookie))
		_ = json.Unmarshal(rec.Body.Bytes(), &q)
		return q.PNG != ""
	})
	// A different browser's cookie must never see this state's QR code.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, linkReq(http.MethodGet, "/connect/"+state+"/qr", &http.Cookie{Name: cookieLinkName, Value: "someone-elses-browser"}))
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "link_browser_mismatch") {
		t.Fatalf("qr with the wrong browser cookie: %d %s", rec.Code, rec.Body.String())
	}
	// Pair -> account under the minting developer, notify + webhook bound, state consumed.
	notified := make(chan map[string]any, 1)
	s.notifyTransport = func(url string, payload map[string]any) { notified <- payload }
	s.fake().Pair(provider.Identity{Identifier: "+919888000000", Name: "G"}, "919888000000:5@s.whatsapp.net")
	waitFor(t, func() bool {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, linkReq(http.MethodGet, "/connect/"+state+"/qr", cookie))
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
	cookie := openLink(t, h, r.State)
	notified := make(chan map[string]any, 1)
	s.notifyTransport = func(url string, payload map[string]any) { notified <- payload }
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, linkReq(http.MethodPost, "/connect/"+r.State+"/consent", cookie))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, linkReq(http.MethodGet, "/connect/"+r.State+"/qr", cookie))
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

	cookie := openLink(t, h, r.State)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, linkReq(http.MethodPost, "/connect/"+r.State+"/consent", cookie))
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
			h.ServeHTTP(rec, linkReq(http.MethodGet, "/connect/"+r.State+"/qr", cookie))
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

	cookie := openLink(t, h, r.State)
	notified := make(chan map[string]any, 1)
	s.notifyTransport = func(url string, payload map[string]any) { notified <- payload }

	h.ServeHTTP(httptest.NewRecorder(), linkReq(http.MethodPost, "/connect/"+r.State+"/consent", cookie))
	// Starts the pairing session (and pumpLink) while the state is still valid.
	h.ServeHTTP(httptest.NewRecorder(), linkReq(http.MethodGet, "/connect/"+r.State+"/qr", cookie))

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
		h.ServeHTTP(rec, linkReq(http.MethodGet, "/connect/"+r.State+"/qr", cookie))
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

	cookie := openLink(t, h, r.State)
	notified := make(chan map[string]any, 1)
	s.notifyTransport = func(url string, payload map[string]any) { notified <- payload }

	h.ServeHTTP(httptest.NewRecorder(), linkReq(http.MethodPost, "/connect/"+r.State+"/consent", cookie))
	h.ServeHTTP(httptest.NewRecorder(), linkReq(http.MethodGet, "/connect/"+r.State+"/qr", cookie))

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
		h.ServeHTTP(rec, linkReq(http.MethodGet, "/connect/"+r.State+"/qr", cookie))
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
	s, db := newTestServer(t)
	_, key := seedDev(t, s, "a@x.com")
	h := s.Routes()

	// The margin below only needs to be tight enough to catch the registry
	// holding its lock across the full StartLink call; against a real
	// network database (TEST_DATABASE_URL) every store round trip on the
	// fast path costs real time too, so both the simulated delay and the
	// pass/fail threshold scale up to keep that margin meaningful instead of
	// flaking on round-trip latency that has nothing to do with the lock.
	slowDelay := time.Second
	threshold := 400 * time.Millisecond
	if db.DriverName() != "sqlite" {
		slowDelay = 8 * time.Second
		threshold = 4 * time.Second
	}

	mint := func() (string, *http.Cookie) {
		req := withKey(httptest.NewRequest(http.MethodPost, "/api/v1/hosted-auth", strings.NewReader(`{"provider":"FAKECHAT"}`)), key)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		var r hostedAuthResponse
		_ = json.Unmarshal(rec.Body.Bytes(), &r)
		cookie := openLink(t, h, r.State)
		h.ServeHTTP(httptest.NewRecorder(), linkReq(http.MethodPost, "/connect/"+r.State+"/consent", cookie))
		return r.State, cookie
	}

	slowState, slowCookie := mint()
	fastState, fastCookie := mint()

	s.fake().SetStartLinkDelay(slowDelay)

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ServeHTTP(httptest.NewRecorder(), linkReq(http.MethodGet, "/connect/"+slowState+"/qr", slowCookie))
	}()
	// Give the slow poll time to actually enter StartLink (past the delay
	// read) before the fast one races it.
	time.Sleep(slowDelay / 10)
	s.fake().SetStartLinkDelay(0)

	start := time.Now()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, linkReq(http.MethodGet, "/connect/"+fastState+"/qr", fastCookie))
	elapsed := time.Since(start)
	if rec.Code != http.StatusOK {
		t.Fatalf("fast state poll: %d %s", rec.Code, rec.Body.String())
	}
	if elapsed > threshold {
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
	cookie := openLink(t, h, r.State)
	h.ServeHTTP(httptest.NewRecorder(), linkReq(http.MethodPost, "/connect/"+r.State+"/consent", cookie))

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
			h.ServeHTTP(rec, linkReq(http.MethodGet, "/connect/"+r.State+"/qr", cookie))
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
	h.ServeHTTP(rec, linkReq(http.MethodGet, "/connect/"+r.State+"/qr", cookie))
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

// C1: the public API edit handler must refuse to write over an evicted
// message rather than resurrecting its content — mirroring the guard already
// in place on the WhatsApp inbound edit path (chatsync/sink.go). The refusal
// must happen before the provider is called, not after: a half-performed
// edit (sent to WhatsApp but not recorded) is worse than refusing outright.
func TestPatchChatMessageRefusesEvictedContent(t *testing.T) {
	s, db := newTestServer(t)
	dev, key := seedDev(t, s, "a@x.com")
	acc := seedChat(t, s, db, dev.ID)
	h := s.Routes()
	j := func(method, path, body string) *httptest.ResponseRecorder {
		req := withKey(httptest.NewRequest(method, path, strings.NewReader(body)), key)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	if _, err := db.UpsertChatMessage(model.ChatMessage{
		AccountID: acc, ID: "EVICTED1", ChatID: "c1", IsFromMe: true, Kind: "text",
		Text: "gone", SentAt: time.Now(), Sender: model.Attendee{ID: "self"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.EvictChatMessageContent(acc, "EVICTED1", time.Now()); err != nil {
		t.Fatal(err)
	}
	rec := j("PATCH", "/api/v1/chats/c1/messages/EVICTED1?account_id="+acc, `{"text":"resurrected"}`)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "content_evicted") {
		t.Fatalf("edit evicted message: %d %s", rec.Code, rec.Body.String())
	}
	if got := s.fake().Commands(); len(got) != 0 {
		t.Fatalf("provider was called for a refused edit: %d commands", len(got))
	}
	got, err := db.GetChatMessage(acc, "EVICTED1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Text != "" {
		t.Fatalf("text = %q after a refused edit, want it to stay blank", got.Text)
	}

	// The same request against a normal own-message still succeeds.
	if _, err := db.UpsertChatMessage(model.ChatMessage{
		AccountID: acc, ID: "NORMAL1", ChatID: "c1", IsFromMe: true, Kind: "text",
		Text: "hi", SentAt: time.Now(), Sender: model.Attendee{ID: "self"},
	}); err != nil {
		t.Fatal(err)
	}
	rec = j("PATCH", "/api/v1/chats/c1/messages/NORMAL1?account_id="+acc, `{"text":"hi!"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("edit normal message: %d %s", rec.Code, rec.Body.String())
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

// TestChatAccountLifecycleRoutes covers the chat account surface added in
// task 9: connection state on the account payload, resync's kind rejection,
// reconnect (detach + attach), and delete unlinking the device.
func TestChatAccountLifecycleRoutes(t *testing.T) {
	s, db := newTestServer(t)
	dev, key := seedDev(t, s, "a@x.com")
	acc := seedChat(t, s, db, dev.ID)
	h := s.Routes()
	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, withKey(httptest.NewRequest("GET", path, nil), key))
		return rec
	}
	post := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, withKey(httptest.NewRequest("POST", path, nil), key))
		return rec
	}
	body := get("/api/v1/accounts/" + acc).Body.String()
	if !strings.Contains(body, `"kind":"chat"`) || !strings.Contains(body, `"identifier":"+91`) || !strings.Contains(body, `"connection":{"state":"connected"`) {
		t.Fatalf("account = %s", body)
	}
	if rec := post("/api/v1/accounts/" + acc + "/resync"); rec.Code != 400 || !strings.Contains(rec.Body.String(), "unsupported_for_kind") {
		t.Fatalf("resync chat: %d", rec.Code)
	}
	if rec := post("/api/v1/accounts/" + acc + "/reconnect"); rec.Code != 202 {
		t.Fatalf("reconnect: %d %s", rec.Code, rec.Body.String())
	}
	waitFor(t, func() bool { c, ok := s.chat.HealthFor(acc); return ok && c.State == "connected" })
	prov := get("/api/v1/providers").Body.String()
	if !strings.Contains(prov, `"name":"FAKECHAT"`) || !strings.Contains(prov, `"auth":"link"`) || !strings.Contains(prov, `"kind":"chat"`) {
		t.Fatalf("providers = %s", prov)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, withKey(httptest.NewRequest("DELETE", "/api/v1/accounts/"+acc, nil), key))
	if rec.Code != 204 {
		t.Fatalf("delete: %d", rec.Code)
	}
	cmds := s.fake().Commands()
	if len(cmds) == 0 || !strings.HasPrefix(cmds[len(cmds)-1], "Logout "+acc) {
		t.Fatalf("logout not sent: %v", cmds)
	}
	if len(s.fake().Forgotten()) != 1 {
		t.Fatal("device not forgotten")
	}
	if _, ok := s.chat.HealthFor(acc); ok {
		t.Fatal("runtime still has the account")
	}
}

// TestScrubErrRedactsJIDs proves a connection error carrying a JID (a phone
// number in disguise) never reaches an API caller or a log line verbatim,
// while an ordinary error message — no "@" in it — passes through unchanged.
func TestScrubErrRedactsJIDs(t *testing.T) {
	plain := "context deadline exceeded"
	if got := scrubErr(plain); got != plain {
		t.Fatalf("scrubErr(%q) = %q, want unchanged", plain, got)
	}
	withJID := "stream error for 919900000000@s.whatsapp.net: conflict"
	got := scrubErr(withJID)
	if got == withJID {
		t.Fatalf("scrubErr(%q) left the JID in place", withJID)
	}
	if strings.Contains(got, "919900000000") || strings.Contains(got, "@s.whatsapp.net") {
		t.Fatalf("scrubErr(%q) = %q, still leaks the JID", withJID, got)
	}
	if got != logx.Digest(withJID) {
		t.Fatalf("scrubErr(%q) = %q, want the digest %q", withJID, got, logx.Digest(withJID))
	}
}

// TestDeleteChatAccountScrubsLogoutError proves a whatsmeow-shaped logout
// error carrying a JID (which embeds a phone number) never reaches the log
// verbatim: scrubErr's digest shows up instead of the phone number.
func TestDeleteChatAccountScrubsLogoutError(t *testing.T) {
	s, db, recs := newTestServerWithLog(t)
	dev, key := seedDev(t, s, "a@x.com")
	acc := seedChat(t, s, db, dev.ID)
	s.fake().CommandErr = errors.New("logout failed for 919888000000@s.whatsapp.net")
	defer func() { s.fake().CommandErr = nil }()

	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, withKey(httptest.NewRequest("DELETE", "/api/v1/accounts/"+acc, nil), key))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}
	if recs.Contains("919888000000") {
		t.Fatalf("logout error leaked the phone number into the logs: %v", recs.All())
	}
	if !recs.Contains(logx.Digest("logout failed for 919888000000@s.whatsapp.net")) {
		t.Fatalf("expected the scrubbed digest in the logs: %v", recs.All())
	}
}

// TestDeleteChatAccountToleratesNilChatRuntime proves delete does not panic
// when the chat runtime is not wired (chat disabled deployments): DeleteLinked
// still runs and the account is removed, with no Detach call against a nil
// runtime.
func TestDeleteChatAccountToleratesNilChatRuntime(t *testing.T) {
	s, db := newTestServer(t)
	dev, key := seedDev(t, s, "a@x.com")
	acc := seedChat(t, s, db, dev.ID)
	s.chat = nil

	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, withKey(httptest.NewRequest("DELETE", "/api/v1/accounts/"+acc, nil), key))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete with nil chat runtime: %d %s", rec.Code, rec.Body.String())
	}
}

// TestReconnectWithoutChatRuntimeIs503 proves reconnect fails safely — 503
// capacity, not a nil-pointer panic — when the chat runtime is not wired.
func TestReconnectWithoutChatRuntimeIs503(t *testing.T) {
	s, db := newTestServer(t)
	dev, key := seedDev(t, s, "a@x.com")
	acc := seedChat(t, s, db, dev.ID)
	s.chat = nil

	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, withKey(httptest.NewRequest("POST", "/api/v1/accounts/"+acc+"/reconnect", nil), key))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "chat runtime disabled") {
		t.Fatalf("reconnect without chat runtime: %d %s", rec.Code, rec.Body.String())
	}
}

// A dropped event is a silent, unrecoverable loss of a webhook notification
// (it never reaches webhook_deliveries), so the count has to be visible
// somewhere an operator already looks.
func TestHealthzReportsDroppedEvents(t *testing.T) {
	s, _ := newTestServer(t)
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz = %d", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["status"] != "ok" {
		t.Fatalf("healthz body = %v", body)
	}
	if _, ok := body["dropped_events"]; !ok {
		t.Fatalf("healthz does not report the dropped-event counter: %v", body)
	}
}

// healthz's "db" field is the one place an operator can tell the database
// itself (not just the process) is reachable — separate from dropped_events,
// which only ever reflects the in-process dispatcher.
func TestHealthzReportsDB(t *testing.T) {
	s, _ := newTestServer(t)
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz = %d", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["db"] != "ok" {
		t.Fatalf("healthz body = %v, want db:ok", body)
	}
}

// --- request-log scrubbing (I3) ---

func TestScrubPathReducesConnectState(t *testing.T) {
	const state = "s3cr3tstate-24-bytes-worth"
	for _, tc := range []struct{ in, want string }{
		{"/connect/" + state, "/connect/" + statePrefix(state)},
		{"/connect/" + state + "/qr", "/connect/" + statePrefix(state) + "/qr"},
		{"/connect/" + state + "/consent", "/connect/" + statePrefix(state) + "/consent"},
		{"/connect/", "/connect/"},
		{"/api/v1/accounts/acc_1", "/api/v1/accounts/acc_1"},
		{"/oauth/callback", "/oauth/callback"},
	} {
		if got := scrubPath(tc.in); got != tc.want {
			t.Fatalf("scrubPath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestScrubQueryBlanksCodeAndState(t *testing.T) {
	got := scrubQuery("code=M.C107_BAY.2.U.abc&state=s3cr3tstate&account_id=acc_1")
	for _, leak := range []string{"M.C107_BAY", "s3cr3tstate"} {
		if strings.Contains(got, leak) {
			t.Fatalf("scrubQuery leaked %q: %q", leak, got)
		}
	}
	for _, keep := range []string{"code=", "state="} {
		if !strings.Contains(got, keep) {
			t.Fatalf("scrubQuery dropped the %q key entirely: %q", keep, got)
		}
	}
	if got := scrubQuery("account_id=acc_1&limit=10"); got != "account_id=acc_1&limit=10" {
		t.Fatalf("scrubQuery mangled a harmless query: %q", got)
	}
	if got := scrubQuery(""); got != "" {
		t.Fatalf("scrubQuery(\"\") = %q", got)
	}
}

// The connect state is a 24-byte credential that can link an attacker's own
// number into the developer's tenant, and an OAuth authorization code is no
// better. Neither may reach the request log, at DEBUG or at INFO.
func TestRequestLogScrubsConnectStateAndOAuthCode(t *testing.T) {
	s, _, recs := newTestServerWithLog(t)
	h := s.Routes()
	const state = "aaaaaabbbbbbccccccdddddd"
	const code = "M.C107_BAY.2.U.authcode"

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/connect/"+state, nil))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/connect/"+state+"/qr", nil))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/connect/"+state+"/consent", nil))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet,
		"/oauth/callback?code="+code+"&state="+state, nil))

	if recs.Contains(state) {
		t.Fatalf("connect state leaked into the request log: %v", recs.All())
	}
	if recs.Contains(code) {
		t.Fatalf("oauth authorization code leaked into the request log: %v", recs.All())
	}
	// The lines themselves must still be there, and still say what was called.
	if !recs.Contains("/connect/" + statePrefix(state)) {
		t.Fatalf("scrubbing removed the connect path entirely: %v", recs.All())
	}
}

// --- not-connected is 409, not 404 (I5) ---

// A command issued while the socket is in backoff must not look like a
// cross-tenant 404: the account exists, the message exists, and the caller's
// correct response is to wait or reconnect, not to conclude the message is
// gone.
func TestNotConnectedIsReconnectRequiredNot404(t *testing.T) {
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

	// The account is still OK; only the live socket is gone.
	s.fake().CommandErr = provider.ErrNotConnected

	for _, tc := range []struct{ method, path, body string }{
		{"PUT", "/api/v1/chats/c1/messages/M1/reaction?account_id=" + acc, `{"emoji":"👍"}`},
		{"PATCH", "/api/v1/chats/c1/messages/M1?account_id=" + acc, `{"text":"edited"}`},
		{"DELETE", "/api/v1/chats/c1/messages/M1?account_id=" + acc, ``},
		{"PATCH", "/api/v1/chats/c1?account_id=" + acc, `{"read":true}`},
		{"POST", "/api/v1/chats/c1/messages?account_id=" + acc, `{"text":"again"}`},
	} {
		req := withKey(httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body)), key)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		s.Routes().ServeHTTP(rec, req)
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "reconnect_required") {
			t.Fatalf("%s %s while disconnected: %d %s", tc.method, tc.path, rec.Code, rec.Body.String())
		}
	}
}

// --- sender resolution (I6) ---

// The API's ChatMessage carries sender: Attendee, not a bare id. Until the
// store joined attendees, every listed message — and every chat_* webhook
// payload — went out with an empty name and is_self: false, including on the
// caller's own messages.
func TestListedMessagesCarryAResolvedSender(t *testing.T) {
	s, db := newTestServer(t)
	dev, key := seedDev(t, s, "a@x.com")
	acc := seedChat(t, s, db, dev.ID)
	if err := db.UpsertAttendee(model.Attendee{ID: "919888000001@s.whatsapp.net",
		Phone: "+919888000001", Name: "Ada"}, acc); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertAttendee(model.Attendee{ID: "919900000000@s.whatsapp.net",
		Phone: "+919900000000", Name: "Me", IsSelf: true}, acc); err != nil {
		t.Fatal(err)
	}
	for _, m := range []model.ChatMessage{
		{AccountID: acc, ID: "M1", ChatID: "c1", Sender: model.Attendee{ID: "919888000001@s.whatsapp.net"},
			Kind: "text", Text: "hi", SentAt: time.Now()},
		{AccountID: acc, ID: "M2", ChatID: "c1", Sender: model.Attendee{ID: "919900000000@s.whatsapp.net"},
			IsFromMe: true, Kind: "text", Text: "hello", SentAt: time.Now().Add(time.Minute)},
	} {
		if _, err := db.UpsertChatMessage(m); err != nil {
			t.Fatal(err)
		}
	}

	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, withKey(httptest.NewRequest("GET", "/api/v1/chats/c1/messages?account_id="+acc, nil), key))
	if rec.Code != 200 {
		t.Fatalf("list messages: %d %s", rec.Code, rec.Body.String())
	}
	var page struct {
		Items []model.ChatMessage `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	byID := map[string]model.ChatMessage{}
	for _, m := range page.Items {
		byID[m.ID] = m
	}
	if got := byID["M1"].Sender; got.Name != "Ada" || got.Phone != "+919888000001" || got.IsSelf {
		t.Fatalf("inbound sender = %+v", got)
	}
	if got := byID["M2"].Sender; got.Name != "Me" || !got.IsSelf {
		t.Fatalf("own-message sender = %+v", got)
	}
	// The single-message route reads through the same select.
	rec = httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, withKey(httptest.NewRequest("GET", "/api/v1/chats/c1/messages/M1?account_id="+acc, nil), key))
	var one model.ChatMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &one); err != nil {
		t.Fatal(err)
	}
	if one.Sender.Name != "Ada" {
		t.Fatalf("get message sender = %+v", one.Sender)
	}
}

// Before the roster has produced a self attendee, a send has no stable sender
// id to record. It must leave the id empty rather than mint the account's
// E.164 identifier, which is not the …@s.whatsapp.net JID the roster uses and
// would 404 on GET /api/v1/attendees/{id}.
func TestSendWithoutSelfAttendeeLeavesSenderIDEmpty(t *testing.T) {
	s, db := newTestServer(t)
	dev, key := seedDev(t, s, "a@x.com")
	acc := seedChat(t, s, db, dev.ID) // seeds attendee a1, which is not is_self
	if _, err := db.SelfAttendee(acc); err == nil {
		t.Fatal("test premise: this account must have no self attendee yet")
	}
	s.fake().SendResult = provider.SendResult{MessageID: "M1"}
	req := withKey(httptest.NewRequest("POST", "/api/v1/chats/c1/messages?account_id="+acc, strings.NewReader(`{"text":"hi"}`)), key)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)
	if rec.Code != 201 {
		t.Fatalf("send: %d %s", rec.Code, rec.Body.String())
	}
	var msg model.ChatMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &msg); err != nil {
		t.Fatal(err)
	}
	if msg.Sender.ID != "" {
		t.Fatalf("sender id = %q, want empty (a minted id no /attendees lookup resolves)", msg.Sender.ID)
	}
	// is_from_me is what identifies the message as ours; the sender attendee
	// simply is not resolvable yet, and the next roster pull fills it in.
	if !msg.IsFromMe {
		t.Fatalf("own send must still be is_from_me: %+v", msg)
	}
}

// --- a pair that lands as the window closes (I7) ---

// lateSession delivers its result only once Close has been called, which is
// exactly the shape of the race pumpLink lost: whatsmeow's PairSuccess handler
// resolves the session while the 3-minute timer is firing, so the timeout
// branch runs with a successful result already on (or about to reach) the
// result channel. Discarding it left the device linked and visible in the end
// user's "Linked devices" list with no account behind it.
type lateSession struct {
	codes  chan provider.LinkCode
	result chan provider.LinkResult
	res    provider.LinkResult
	once   sync.Once
}

func (s *lateSession) Codes() <-chan provider.LinkCode    { return s.codes }
func (s *lateSession) Result() <-chan provider.LinkResult { return s.result }
func (s *lateSession) Close() {
	s.once.Do(func() {
		go func() {
			time.Sleep(30 * time.Millisecond)
			s.result <- s.res
		}()
	})
}

func TestPumpLinkHonoursAPairThatLandsAsTheWindowCloses(t *testing.T) {
	s, db := newTestServer(t)
	dev, key := seedDev(t, s, "a@x.com")
	h := s.Routes()
	req := withKey(httptest.NewRequest(http.MethodPost, "/api/v1/hosted-auth",
		strings.NewReader(`{"provider":"FAKECHAT","notify_url":"https://api.example.com/hook"}`)), key)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var r hostedAuthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	pending, err := db.PeekOAuthState(r.State)
	if err != nil {
		t.Fatal(err)
	}

	notified := make(chan map[string]any, 1)
	s.notifyTransport = func(url string, payload map[string]any) { notified <- payload }

	sess := &lateSession{
		codes:  make(chan provider.LinkCode, 1),
		result: make(chan provider.LinkResult, 1),
		res: provider.LinkResult{
			Identity:  provider.Identity{Identifier: "+919888000007", Name: "Ada"},
			DeviceJID: "919888000007:1@s.whatsapp.net",
		},
	}
	l := &link{ready: make(chan struct{}), session: sess, started: time.Now()}
	close(l.ready)
	s.links.ttl = 10 * time.Millisecond

	go s.pumpLink(r.State, pending, l)

	select {
	case p := <-notified:
		if p["status"] != "CREATED" {
			t.Fatalf("a pair that landed as the window closed was reported as %v", p)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("nothing notified")
	}
	accts, _ := db.ListAccounts(dev.ID)
	if len(accts) != 1 {
		t.Fatalf("accounts = %+v, want the paired device to have become one", accts)
	}
	for _, jid := range s.fake().Forgotten() {
		if jid == "919888000007:1@s.whatsapp.net" {
			t.Fatal("a successful pairing must not have its device forgotten")
		}
	}
}

// --- paired status carries the account id (I8) ---

// /docs and llms.txt both document {"status":"paired","account_id":"acc_…"}.
// A connect page built from the docs with no success_redirect_url got "paired"
// and no id at all.
func TestQRPairedStatusCarriesAccountID(t *testing.T) {
	s, db := newTestServer(t)
	dev, key := seedDev(t, s, "a@x.com")
	h := s.Routes()
	req := withKey(httptest.NewRequest(http.MethodPost, "/api/v1/hosted-auth",
		strings.NewReader(`{"provider":"FAKECHAT"}`)), key)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var r hostedAuthResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &r)

	cookie := openLink(t, h, r.State)
	h.ServeHTTP(httptest.NewRecorder(), linkReq(http.MethodPost, "/connect/"+r.State+"/consent", cookie))
	h.ServeHTTP(httptest.NewRecorder(), linkReq(http.MethodGet, "/connect/"+r.State+"/qr", cookie))
	s.fake().Pair(provider.Identity{Identifier: "+919888000008", Name: "Ada"}, "919888000008:1@s.whatsapp.net")

	var q map[string]any
	waitFor(t, func() bool {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, linkReq(http.MethodGet, "/connect/"+r.State+"/qr", cookie))
		q = map[string]any{}
		_ = json.Unmarshal(rec.Body.Bytes(), &q)
		return q["status"] == "paired"
	})
	accts, _ := db.ListAccounts(dev.ID)
	if len(accts) != 1 {
		t.Fatalf("accounts = %+v", accts)
	}
	if q["account_id"] != accts[0].ID {
		t.Fatalf("paired status = %v, want account_id %q", q, accts[0].ID)
	}
}

// --- link minors ---

// A QR code whose expiry has already passed must not report a negative
// countdown to the connect page.
func TestStatusResponseClampsExpiresIn(t *testing.T) {
	l := &link{code: provider.LinkCode{Code: "2@abc", ExpiresAt: time.Now().Add(-time.Minute)}}
	got := l.statusResponse()
	if got["expires_in"] != 0 {
		t.Fatalf("expires_in = %v, want 0", got["expires_in"])
	}
}

// Consent is a linker concept. Recording consented_at on an OUTLOOK state
// stores a fact that means nothing there — /qr already refuses such a state,
// and consent must refuse it the same way.
func TestConsentRejectsANonLinkerState(t *testing.T) {
	s, _ := newTestServer(t)
	_, key := seedDev(t, s, "a@x.com")
	h := s.Routes()
	req := withKey(httptest.NewRequest(http.MethodPost, "/api/v1/hosted-auth",
		strings.NewReader(`{"provider":"OUTLOOK"}`)), key)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var r hostedAuthResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &r)

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/connect/"+r.State+"/consent", nil))
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "not_found") {
		t.Fatalf("consent on a mail state: %d %s", rec.Code, rec.Body.String())
	}
}

// A webhook set on a chat account without an explicit filter must subscribe
// to chat events, not new mail — otherwise a WhatsApp account's dashboard
// "Set webhook" silently never fires.
func TestAccountWebhookDefaultsToChatReceivedForChatAccounts(t *testing.T) {
	s, db := newTestServer(t)
	dev, _ := seedDev(t, s, "a@x.com")
	acc := seedChat(t, s, db, dev.ID)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/accounts/"+acc+"/webhooks",
		strings.NewReader(`{"url":"https://hook.example.com","secret":"s3"}`))
	req.Header.Set("Content-Type", "application/json")
	s.Routes().ServeHTTP(rec, withSession(t, s, req, dev.ID))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: status = %d body = %s", rec.Code, rec.Body.String())
	}
	var created model.Webhook
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if len(created.Events) != 1 || created.Events[0] != model.EventChatReceived {
		t.Fatalf("events = %v, want [chat_received]", created.Events)
	}
}

// telegramStub answers getChat/sendMessage; per-test success or rejection.
func telegramStub(t *testing.T, ok bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ok {
			_, _ = w.Write([]byte(`{"ok":true,"result":{}}`))
			return
		}
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"ok":false,"description":"Bad Request: chat not found"}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestCreateWebhookKinds(t *testing.T) {
	s, _ := newTestServer(t)
	s.senders.SetTelegramBase(telegramStub(t, true).URL)
	h := s.Routes()
	_, key := seedDev(t, s, "a@x.com")
	post := func(body string) (*httptest.ResponseRecorder, map[string]any) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, withKey(httptest.NewRequest(http.MethodPost, "/api/v1/webhooks", strings.NewReader(body)), key))
		var m map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &m)
		return rec, m
	}
	// webhook (default kind) keeps returning the secret once.
	rec, m := post(`{"url":"https://hook.example.com","secret":"s3"}`)
	if rec.Code != 201 || m["kind"] != "webhook" || m["secret"] != "s3" {
		t.Fatalf("webhook: %d %v", rec.Code, m)
	}
	// discord: only its own host, no secret.
	rec, m = post(`{"kind":"discord","url":"https://discord.com/api/webhooks/1/abc"}`)
	if rec.Code != 201 || m["kind"] != "discord" || m["url"] != "https://discord.com/api/webhooks/1/abc" || m["secret"] != nil {
		t.Fatalf("discord: %d %v", rec.Code, m)
	}
	rec, _ = post(`{"kind":"discord","url":"https://evil.example.com/api/webhooks/1/abc"}`)
	if rec.Code != 400 {
		t.Fatalf("discord bad host: %d", rec.Code)
	}
	rec, _ = post(`{"kind":"discord","url":"https://discord.com/api/webhooks/1/abc","secret":"x"}`)
	if rec.Code != 400 {
		t.Fatalf("discord with secret: %d", rec.Code)
	}
	rec, _ = post(`{"kind":"discord","url":"https://user:pass@discord.com/api/webhooks/1/abc"}`)
	if rec.Code != 400 {
		t.Fatalf("discord url with credentials: %d", rec.Code)
	}
	// An explicit port is rejected: it survives Hostname() checks but would
	// slip past the host-anchored scrubbers in logs and last_error.
	rec, _ = post(`{"kind":"discord","url":"https://discord.com:8443/api/webhooks/1/abc"}`)
	if rec.Code != 400 {
		t.Fatalf("discord url with port: %d", rec.Code)
	}
	// telegram: token never comes back, url absent, chat_id present.
	rec, m = post(`{"kind":"telegram","bot_token":"123:ABC","chat_id":"-100"}`)
	if rec.Code != 201 || m["kind"] != "telegram" || m["url"] != nil || m["bot_token"] != nil ||
		m["telegram"].(map[string]any)["chat_id"] != "-100" || strings.Contains(rec.Body.String(), "123:ABC") {
		t.Fatalf("telegram: %d %s", rec.Code, rec.Body.String())
	}
	rec, _ = post(`{"kind":"telegram","chat_id":"-100"}`)
	if rec.Code != 400 {
		t.Fatalf("telegram missing token: %d", rec.Code)
	}
	rec, _ = post(`{"kind":"slack","url":"https://hooks.slack.com/x"}`)
	if rec.Code != 400 {
		t.Fatalf("unknown kind: %d", rec.Code)
	}
	// Listing never leaks the token either.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, withKey(httptest.NewRequest(http.MethodGet, "/api/v1/webhooks", nil), key))
	if strings.Contains(rec.Body.String(), "123:ABC") || !strings.Contains(rec.Body.String(), `"chat_id":"-100"`) {
		t.Fatalf("list: %s", rec.Body.String())
	}
}

// logBody runs on every authenticated request under DEBUG logging. logx.Redact
// only knows secret-looking *keys*, and "url" is not one of them, so the body
// has to go through notify.Scrub as well or a live Discord bearer credential
// lands in the log on the feature's own happy path.
func TestRequestBodyLogScrubsDiscordToken(t *testing.T) {
	s, _, recs := newTestServerWithLog(t)
	_, key := seedDev(t, s, "a@x.com")
	req := withKey(httptest.NewRequest(http.MethodPost, "/api/v1/webhooks",
		strings.NewReader(`{"kind":"discord","url":"https://discord.com/api/webhooks/1/SUPERSECRET"}`)), key)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)
	if rec.Code != 201 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	if !recs.Contains("/api/webhooks/1/•••") {
		t.Fatalf("expected the request-body log to carry a masked url, got: %v", recs.All())
	}
	for _, l := range recs.All() {
		if strings.Contains(l, "SUPERSECRET") {
			t.Fatalf("log leaked the discord token: %q", l)
		}
	}
}

// Tenancy is decided before anything leaves the process: an account id that
// belongs to another developer is a 404, and Telegram is never called with
// the (real) bot token the caller supplied.
func TestAccountWebhookCrossTenantIs404BeforeTelegram(t *testing.T) {
	s, db := newTestServer(t)
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("telegram called for a cross-tenant account: %s", r.URL.Path)
		_, _ = w.Write([]byte(`{"ok":true,"result":{}}`))
	}))
	t.Cleanup(stub.Close)
	s.senders.SetTelegramBase(stub.URL)

	owner, _ := seedDev(t, s, "owner@x.com")
	if err := db.UpsertAccount(model.Account{ID: "acc_owned", DeveloperID: owner.ID,
		Provider: "OUTLOOK", Email: "u@x.com", Status: model.AccountOK}); err != nil {
		t.Fatal(err)
	}
	_, key := seedDev(t, s, "intruder@x.com")

	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, withKey(httptest.NewRequest(http.MethodPost,
		"/api/v1/accounts/acc_owned/webhooks",
		strings.NewReader(`{"kind":"telegram","bot_token":"123:ABC","chat_id":"-100"}`)), key))
	if rec.Code != 404 {
		t.Fatalf("cross-tenant account webhook: status = %d, want 404 (%s)", rec.Code, rec.Body.String())
	}
}

func TestCreateTelegramWebhookRejectedByTelegramIs400(t *testing.T) {
	s, _ := newTestServer(t)
	s.senders.SetTelegramBase(telegramStub(t, false).URL)
	_, key := seedDev(t, s, "a@x.com")
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, withKey(httptest.NewRequest(http.MethodPost, "/api/v1/webhooks",
		strings.NewReader(`{"kind":"telegram","bot_token":"123:ABC","chat_id":"-100"}`)), key))
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "chat not found") || strings.Contains(rec.Body.String(), "123:ABC") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestHostedAuthCarriesDiscordHookToTheAccount(t *testing.T) {
	// Same flow as TestHostedAuthStoresPendingWebhook, with kind=discord: the
	// pending state must keep the kind and the bound hook must be discord.
	s, db := newTestServer(t)
	dev, key := seedDev(t, s, "a@x.com")
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, withKey(httptest.NewRequest(http.MethodPost, "/api/v1/hosted-auth",
		strings.NewReader(`{"webhook":{"kind":"discord","url":"https://discord.com/api/webhooks/1/abc"}}`)), key))
	if rec.Code != 200 {
		t.Fatalf("hosted-auth: %d %s", rec.Code, rec.Body.String())
	}
	var out struct{ State string }
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	pending, err := db.PeekOAuthState(out.State)
	if err != nil {
		t.Fatal(err)
	}
	if pending.Webhook == nil || pending.Webhook.Kind != "discord" {
		t.Fatalf("pending = %+v", pending.Webhook)
	}
	_ = dev
}

// A telegram hook attached to a hosted-auth link is checked against Telegram
// right then, the same as a direct POST /api/v1/webhooks — waiting until the
// link completes would strand the caller with a broken hook and no way to
// have caught it earlier.
func TestHostedAuthRejectsBadTelegramAtMintTime(t *testing.T) {
	s, db := newTestServer(t)
	_, key := seedDev(t, s, "a@x.com")

	s.senders.SetTelegramBase(telegramStub(t, false).URL)
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, withKey(httptest.NewRequest(http.MethodPost, "/api/v1/hosted-auth",
		strings.NewReader(`{"webhook":{"kind":"telegram","bot_token":"123:ABC","chat_id":"-100"}}`)), key))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "chat not found") ||
		strings.Contains(rec.Body.String(), "123:ABC") {
		t.Fatalf("rejected: %d %s", rec.Code, rec.Body.String())
	}

	s.senders.SetTelegramBase(telegramStub(t, true).URL)
	rec = httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, withKey(httptest.NewRequest(http.MethodPost, "/api/v1/hosted-auth",
		strings.NewReader(`{"webhook":{"kind":"telegram","bot_token":"123:ABC","chat_id":"-100"}}`)), key))
	if rec.Code != http.StatusOK {
		t.Fatalf("accepted: %d %s", rec.Code, rec.Body.String())
	}
	var out struct{ State string }
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	pending, err := db.PeekOAuthState(out.State)
	if err != nil {
		t.Fatal(err)
	}
	if pending.Webhook == nil || pending.Webhook.Kind != "telegram" ||
		pending.Webhook.ChatID != "-100" || pending.Webhook.BotToken != "123:ABC" {
		t.Fatalf("pending = %+v", pending.Webhook)
	}
}

// The sessions table must hold hashes only: a leaked database read (a backup,
// a SQL injection, an operator) must not yield a replayable cookie.
func TestSessionTokenIsHashedAtRest(t *testing.T) {
	s, db := newTestServer(t)
	dev, _ := seedDev(t, s, "a@x.com")
	tok, _, err := s.auth.NewSession(context.Background(), dev.ID)
	if err != nil {
		t.Fatal(err)
	}
	exists, err := db.SessionRowExists(tok)
	if err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("raw token stored")
	}
	if exists, err = db.SessionRowExists(auth.HashKey(tok)); err != nil || !exists {
		t.Fatalf("hashed row missing: %v", err)
	}
	if _, _, err := s.auth.SessionDeveloper(context.Background(), tok); err != nil {
		t.Fatal("token must still resolve")
	}
}

func TestSessionAbsoluteLifetime(t *testing.T) {
	s, db := newTestServer(t)
	dev, _ := seedDev(t, s, "a@x.com")
	tok, _, _ := s.auth.NewSession(context.Background(), dev.ID)
	// Age the row past the absolute limit while keeping it unexpired by the
	// sliding rule.
	if err := db.SetSessionCreatedAt(auth.HashKey(tok), time.Now().Add(-91*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.auth.SessionDeveloper(context.Background(), tok); err == nil {
		t.Fatal("session older than max age must be rejected")
	}
}

func TestLoginRequiresCSRFAndSameOrigin(t *testing.T) {
	s, _ := newTestServer(t)
	seedDev(t, s, "a@x.com")
	h := s.Routes()
	// 1. no token
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/login", strings.NewReader("email=a%40x.com&password=longenoughpassword"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	h.ServeHTTP(rec, req)
	// The refusal is the form again, with the reason shown inline — these
	// routes are reached by a person in a browser. Assert on the message, not
	// on the string "csrf", which the re-rendered form's hidden field would
	// satisfy no matter what the handler did.
	if rec.Code != 403 || !strings.Contains(rec.Body.String(), csrfExpiredMessage) {
		t.Fatalf("no token: %d %s", rec.Code, rec.Body.String())
	}
	// 2. fetch the form to get the token cookie, then post with it
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/login", nil))
	var csrf *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == "um_csrf" {
			csrf = c
		}
	}
	if csrf == nil {
		t.Fatal("no csrf cookie on the form page")
	}
	if !csrf.HttpOnly || csrf.SameSite != http.SameSiteStrictMode || csrf.Path != "/" || len(csrf.Value) != 43 {
		t.Fatalf("csrf cookie = %+v", csrf)
	}
	if !strings.Contains(rec.Body.String(), `name="csrf" value="`+csrf.Value+`"`) {
		t.Fatal("form lacks the csrf field")
	}
	post := func(origin string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/login", strings.NewReader("email=a%40x.com&password=longenoughpassword&csrf="+csrf.Value))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(csrf)
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		h.ServeHTTP(rec, req)
		return rec
	}
	if rec := post("https://evil.example"); rec.Code != 403 {
		t.Fatalf("cross-origin: %d", rec.Code)
	}
	if rec := post("http://example.com"); rec.Code != 303 {
		t.Fatalf("same-origin (httptest host is example.com): %d %s", rec.Code, rec.Body.String())
	}
	// This service sends Referrer-Policy: no-referrer, so a real browser posts
	// this form with `Origin: null` and no Referer. That is the *normal* case,
	// not an attack: refusing it would refuse every genuine sign-in.
	if rec := post("null"); rec.Code != 303 {
		t.Fatalf("opaque origin (the browser case under no-referrer): %d %s", rec.Code, rec.Body.String())
	}
	// A stale or foreign token with the right cookie shape is still refused.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/login", strings.NewReader("email=a%40x.com&password=longenoughpassword&csrf="+strings.Repeat("A", 43)))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(csrf)
	h.ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Fatalf("mismatched token: %d", rec.Code)
	}
}

// Signup must not tell a stranger which emails already have an account, so a
// taken address and a malformed one fail identically.
func TestSignupErrorIsUniform(t *testing.T) {
	s, _ := newTestServer(t)
	h := s.Routes()
	seedDev(t, s, "taken@x.com")

	taken := postFormCSRF(t, h, "/signup", url.Values{"email": {"taken@x.com"}, "password": {"longenoughpassword"}})
	bad := postFormCSRF(t, h, "/signup", url.Values{"email": {"nonsense"}, "password": {"short"}})
	if taken.Code != http.StatusBadRequest || bad.Code != http.StatusBadRequest {
		t.Fatalf("statuses differ or are not 400: taken %d, bad %d", taken.Code, bad.Code)
	}
	if !strings.Contains(taken.Body.String(), uniformSignupError) || !strings.Contains(bad.Body.String(), uniformSignupError) {
		t.Fatalf("messages are not the uniform one:\ntaken: %s\nbad: %s", taken.Body.String(), bad.Body.String())
	}
}

func TestChangePasswordInvalidatesOtherSessions(t *testing.T) {
	s, _ := newTestServer(t)
	dev, _ := seedDev(t, s, "a@x.com")
	tokA, _, _ := s.auth.NewSession(context.Background(), dev.ID)
	tokB, _, _ := s.auth.NewSession(context.Background(), dev.ID)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/me/password", strings.NewReader(`{"current_password":"longenoughpassword","new_password":"another strong one"}`))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: "um_session", Value: tokA})
	s.Routes().ServeHTTP(rec, req)
	if rec.Code != 204 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if _, _, err := s.auth.SessionDeveloper(context.Background(), tokB); err == nil {
		t.Fatal("other session must be gone")
	}
	if _, _, err := s.auth.SessionDeveloper(context.Background(), tokA); err != nil {
		t.Fatal("current session must survive")
	}
	if _, err := s.auth.Login(context.Background(), "a@x.com", "another strong one"); err != nil {
		t.Fatal("new password must work")
	}
	// API key must not reach it
	_, key := seedDev(t, s, "b@x.com")
	rec = httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, withKey(httptest.NewRequest("POST", "/api/v1/me/password", strings.NewReader(`{}`)), key))
	if rec.Code != 403 {
		t.Fatalf("api key: %d", rec.Code)
	}
}

func TestChangePasswordRejectsWrongCurrentAndWeakNew(t *testing.T) {
	s, _, recs := newTestServerWithLog(t)
	dev, _ := seedDev(t, s, "a@x.com")
	tok, _, _ := s.auth.NewSession(context.Background(), dev.ID)
	post := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/api/v1/me/password", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: tok})
		s.Routes().ServeHTTP(rec, req)
		return rec
	}
	if rec := post(`{"current_password":"nope nope nope","new_password":"another strong one"}`); rec.Code != 400 ||
		!strings.Contains(rec.Body.String(), "invalid_credentials") {
		t.Fatalf("wrong current: %d %s", rec.Code, rec.Body.String())
	}
	if rec := post(`{"current_password":"longenoughpassword","new_password":"short"}`); rec.Code != 400 ||
		!strings.Contains(rec.Body.String(), "invalid_body") {
		t.Fatalf("weak new: %d %s", rec.Code, rec.Body.String())
	}
	// The old password still works: nothing changed.
	if _, err := s.auth.Login(context.Background(), "a@x.com", "longenoughpassword"); err != nil {
		t.Fatal(err)
	}
	if recs.Contains("another strong one") || recs.Contains("longenoughpassword") {
		t.Fatal("a password reached the logs")
	}
}

// Every server-rendered page that can post to /logout has to carry the token,
// or its own Log out button 403s.
func TestSessionPagesCarryTheCSRFField(t *testing.T) {
	s, _ := newTestServer(t)
	h := s.Routes()
	dev, _ := seedDev(t, s, "a@x.com")
	for _, path := range []string{"/dashboard", "/mail", "/chat"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, withSession(t, s, httptest.NewRequest(http.MethodGet, path, nil), dev.ID))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d", path, rec.Code)
		}
		var csrf *http.Cookie
		for _, c := range rec.Result().Cookies() {
			if c.Name == csrfCookie {
				csrf = c
			}
		}
		if csrf == nil {
			t.Fatalf("%s: no csrf cookie", path)
		}
		if !strings.Contains(rec.Body.String(), `name="csrf" value="`+csrf.Value+`"`) {
			t.Fatalf("%s: logout form lacks the csrf field", path)
		}
	}
}

// mustCookie reads a cookie off a request the test built, so an assertion can
// name the exact token the server was handed.
func mustCookie(t *testing.T, r *http.Request, name string) string {
	t.Helper()
	c, err := r.Cookie(name)
	if err != nil {
		t.Fatalf("request carries no %s cookie: %v", name, err)
	}
	return c.Value
}

// The CSRF cookie's 12-hour window slides: every render of a form re-issues
// it, so a dashboard left open all day does not reach a point where its Log
// out button starts failing.
func TestCSRFCookieIsReissuedOnEveryRender(t *testing.T) {
	s, _ := newTestServer(t)
	h := s.Routes()

	c := newCSRF(t, h)

	// Come back with the cookie already held, as a browser would.
	second := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/login", nil)
	req.AddCookie(c)
	h.ServeHTTP(second, req)

	if raw := second.Header().Get("Set-Cookie"); !strings.Contains(raw, csrfCookie+"=") {
		t.Fatalf("second render did not re-issue the csrf cookie: %q", raw)
	}
	var got *http.Cookie
	for _, sc := range second.Result().Cookies() {
		if sc.Name == csrfCookie {
			got = sc
		}
	}
	if got == nil || got.Value != c.Value {
		t.Fatalf("re-issued cookie = %+v, want the same value %q (a new one would invalidate open tabs)", got, c.Value)
	}
	if got.MaxAge != 12*3600 {
		t.Fatalf("re-issued max-age = %d, want the full window back", got.MaxAge)
	}
}

// GET /webhooks/{id}/deliveries must paginate: an outage can pile up hundreds
// of dead deliveries, and returning them all in one response is unbounded.
func TestListWebhookDeliveriesIsPaginated(t *testing.T) {
	s, db := newTestServer(t)
	dev, key := seedDev(t, s, "a@x.com")
	now := time.Now().UTC()
	if err := db.SaveWebhook(model.Webhook{ID: "wh_1", DeveloperID: dev.ID, URL: "https://x.example.com", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		created := now.Add(time.Duration(i) * time.Second)
		if err := db.SaveDelivery(store.Delivery{
			ID: "dl_" + strconv.Itoa(i), WebhookID: "wh_1", AccountID: "acc_1", EventType: "mail_received",
			Payload: []byte(`{}`), Attempts: 1, NextAttemptAt: created, CreatedAt: created,
		}); err != nil {
			t.Fatal(err)
		}
	}

	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, withKey(httptest.NewRequest(http.MethodGet,
		"/api/v1/webhooks/wh_1/deliveries?limit=2&offset=2", nil), key))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	var list listResponse[store.Delivery]
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 2 || list.Limit != 2 || list.Offset != 2 {
		t.Fatalf("items/limit/offset = %d/%d/%d, want 2/2/2", len(list.Items), list.Limit, list.Offset)
	}

	rec = httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, withKey(httptest.NewRequest(http.MethodGet,
		"/api/v1/webhooks/wh_1/deliveries?limit=500", nil), key))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	list = listResponse[store.Delivery]{}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if list.Limit != 200 {
		t.Fatalf("limit = %d, want clamped to 200", list.Limit)
	}

	// Garbage paging input (negative, non-numeric) must fall back to the
	// defaults, not 400 or panic: deliveriesPaging only accepts a parsed value
	// that also satisfies the sign check.
	for _, query := range []string{"?limit=-5", "?limit=abc", "?offset=-1"} {
		rec = httptest.NewRecorder()
		s.Routes().ServeHTTP(rec, withKey(httptest.NewRequest(http.MethodGet,
			"/api/v1/webhooks/wh_1/deliveries"+query, nil), key))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200 (body %s)", query, rec.Code, rec.Body.String())
		}
		list = listResponse[store.Delivery]{}
		if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
			t.Fatal(err)
		}
		if list.Limit != 50 || list.Offset != 0 {
			t.Fatalf("%s: limit/offset = %d/%d, want defaults 50/0", query, list.Limit, list.Offset)
		}
	}
}

// ---- Task 6: per-route body limits, attachment cap, negative cache, bounded push ----

// seedFakeMailAccount registers a developer and a FAKEMAIL-provider account,
// for tests that need a real Mailbox/Pusher with no network dependency.
func seedFakeMailAccount(t *testing.T, s *Server, db *store.Store) (model.Developer, string, string) {
	t.Helper()
	dev, key := seedDev(t, s, "a@x.com")
	acctID := "acc_1"
	if err := db.UpsertAccount(model.Account{
		ID: acctID, DeveloperID: dev.ID, Provider: "FAKEMAIL", Email: "u@x.com", Status: model.AccountOK,
	}); err != nil {
		t.Fatal(err)
	}
	return dev, key, acctID
}

func TestSmallRoutesRejectLargeBodies(t *testing.T) {
	s, db := newTestServerWithProviders(t, providertest.NewFakeMail("FAKEMAIL"))
	_, key, acctID := seedFakeMailAccount(t, s, db)

	// A small route (decodeJSON, 64 KB) must reject a 100 KB body with 413,
	// not fall through to a 400 for whatever else might be wrong with it.
	pad := strings.Repeat(" ", 100<<10)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/emails/M1?account_id="+acctID,
		strings.NewReader(pad+`{"read":true}`))
	s.Routes().ServeHTTP(rec, withKey(req, key))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("100 KB body on a small route: status = %d, want 413; body=%s", rec.Code, rec.Body.String())
	}
	var e apiError
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatal(err)
	}
	if e.Error.Code != "body_too_large" {
		t.Fatalf("code = %q, want body_too_large", e.Error.Code)
	}

	// A large route (decodeJSONLarge, 8 MB) must accept a 1 MB body.
	payload, err := json.Marshal(sendPayload{
		AccountID: acctID,
		SendRequest: model.SendRequest{
			To:      []model.Recipient{{Email: "x@y.com"}},
			Subject: "hi",
			Body:    "hello",
			Attachments: []model.SendAttachment{{
				Name: "a.txt", MimeType: "text/plain",
				Content: base64.StdEncoding.EncodeToString([]byte("small content")),
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	padded := append([]byte(strings.Repeat(" ", 1<<20)), payload...)
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/v1/emails", bytes.NewReader(padded))
	s.Routes().ServeHTTP(rec, withKey(req, key))
	if rec.Code == http.StatusRequestEntityTooLarge {
		t.Fatalf("1 MB body on a large route was rejected: %d, body=%s", rec.Code, rec.Body.String())
	}
	if rec.Code != http.StatusAccepted {
		t.Fatalf("1 MB body on a large route: status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
}

func TestAttachmentsAreCappedServerSide(t *testing.T) {
	s, db := newTestServerWithProviders(t, providertest.NewFakeMail("FAKEMAIL"))
	_, key, acctID := seedFakeMailAccount(t, s, db)

	send := func(decodedSize int) *httptest.ResponseRecorder {
		content := base64.StdEncoding.EncodeToString(make([]byte, decodedSize))
		body, err := json.Marshal(sendPayload{
			AccountID: acctID,
			SendRequest: model.SendRequest{
				To:      []model.Recipient{{Email: "x@y.com"}},
				Subject: "hi",
				Body:    "hello",
				Attachments: []model.SendAttachment{{
					Name: "a.bin", MimeType: "application/octet-stream", Content: content,
				}},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		rec := httptest.NewRecorder()
		s.Routes().ServeHTTP(rec, withKey(httptest.NewRequest(http.MethodPost, "/api/v1/emails", bytes.NewReader(body)), key))
		return rec
	}

	// 4 MB decoded exceeds the 3 MB cap.
	rec := send(4 << 20)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("4 MB attachment: status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	var e apiError
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatal(err)
	}
	if e.Error.Code != "attachment_too_large" {
		t.Fatalf("code = %q, want attachment_too_large", e.Error.Code)
	}

	// 2 MB decoded is under the cap and reaches the (fake) mailbox.
	rec = send(2 << 20)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("2 MB attachment: status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
}

func TestMirrorMissIsNegativelyCached(t *testing.T) {
	fm := providertest.NewFakeMail("FAKEMAIL")
	s, db := newTestServerWithProviders(t, fm)
	_, key, acctID := seedFakeMailAccount(t, s, db)

	get := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		s.Routes().ServeHTTP(rec, withKey(httptest.NewRequest(http.MethodGet,
			"/api/v1/emails/M1?account_id="+acctID, nil), key))
		return rec
	}

	if rec := get(); rec.Code != http.StatusNotFound {
		t.Fatalf("first get: status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	if rec := get(); rec.Code != http.StatusNotFound {
		t.Fatalf("second get: status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	if n := fm.GetMessageCalls(); n != 1 {
		t.Fatalf("GetMessage calls = %d, want 1 (the second get should have hit the negative cache)", n)
	}
}

// Server.complete lazily fetches attachment metadata from the provider and
// caches it on the row. On an evicted message the store write is already
// refused by UpsertEmail's guard, but the response would still carry the
// filenames — and a filename is as revealing as a subject line.
func TestGetEmailDoesNotFetchAttachmentsForEvictedMessage(t *testing.T) {
	fm := providertest.NewFakeMail("FAKEMAIL")
	fm.Attachments = []model.Attachment{{ID: "a1", Name: "payroll.xlsx"}}
	s, db := newTestServerWithProviders(t, fm)
	_, key, acctID := seedFakeMailAccount(t, s, db)

	if err := db.UpsertEmail(model.Email{
		AccountID: acctID, ID: "M1", Subject: "quarterly numbers",
		From: model.Recipient{Email: "alice@example.com"},
		Body: "<p>secret</p>", BodyType: "html", Date: time.Now().UTC(),
		HasAttachments: true,
	}); err != nil {
		t.Fatal(err)
	}

	// A normal read fetches and caches the metadata: the baseline that proves
	// the assertion below is about eviction and not about a broken fixture.
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, withKey(httptest.NewRequest(http.MethodGet,
		"/api/v1/emails/M1?account_id="+acctID, nil), key))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "payroll.xlsx") {
		t.Fatalf("baseline read did not return attachment metadata: %s", rec.Body.String())
	}

	if err := db.EvictEmailContent(acctID, "M1", time.Now()); err != nil {
		t.Fatal(err)
	}
	before := fm.AttachmentCalls.Load()

	rec = httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, withKey(httptest.NewRequest(http.MethodGet,
		"/api/v1/emails/M1?account_id="+acctID, nil), key))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "payroll.xlsx") {
		t.Errorf("evicted message returned attachment filenames: %s", body)
	}
	if !strings.Contains(body, `"content_evicted":true`) {
		t.Errorf("evicted message not flagged: %s", body)
	}
	if got := fm.AttachmentCalls.Load(); got != before {
		t.Errorf("ListAttachments called %d extra times for an evicted message, want 0", got-before)
	}
}

// TestNotificationHandlingIsBounded fires 100 concurrent notifications whose
// ParseNotifications call blocks, and checks that at most 32 of them are
// ever running inside the dedicated goroutine dispatchNotification spawns
// (notifySem's occupancy) — the rest run inline, on the requesting
// goroutine, and are never dropped, so every one of the 100 still answers
// 202 once released.
func TestNotificationHandlingIsBounded(t *testing.T) {
	fm := providertest.NewFakeMail("FAKEMAIL")
	block := make(chan struct{})
	fm.SetParseBlock(block)
	s, _ := newTestServerWithProviders(t, fm)

	const n = 100
	var wg sync.WaitGroup
	codes := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/notifications/FAKEMAIL", strings.NewReader(`{}`))
			s.Routes().ServeHTTP(rec, req)
			codes[i] = rec.Code
		}(i)
	}

	// Wait for every request to have entered ParseNotifications and started
	// blocking, so the semaphore occupancy below is a stable snapshot rather
	// than a race against requests still arriving.
	waitFor(t, func() bool { return fm.InFlight() == n })

	if occ := len(notifySem); occ != cap(notifySem) {
		t.Fatalf("notifySem occupancy = %d, want %d (32 requests should have won a background slot)", occ, cap(notifySem))
	}

	close(block) // release every blocked ParseNotifications call
	wg.Wait()

	for i, code := range codes {
		if code != http.StatusAccepted {
			t.Fatalf("request %d: status = %d, want 202", i, code)
		}
	}
}

// --- QR link browser binding (I11) ---

// A leaked /connect/{state} URL must not let a second browser hijack a
// pairing attempt someone else already started: consent and every /qr poll
// have to come from the same browser that first opened the connect landing
// page, proven by a cookie no leaked URL alone carries.
func TestQRLinkIsBoundToTheBrowserThatOpenedIt(t *testing.T) {
	s, _ := newTestServer(t)
	_, key := seedDev(t, s, "a@x.com")
	h := s.Routes()

	req := withKey(httptest.NewRequest(http.MethodPost, "/api/v1/hosted-auth", strings.NewReader(`{"provider":"FAKECHAT"}`)), key)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var r hostedAuthResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &r)

	// Opening the connect landing page mints the browser-binding cookie.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/connect/"+r.State, nil))
	var cookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == cookieLinkName {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("connect landing page did not set the um_link cookie")
	}

	// Consent without the cookie at all is refused outright.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/connect/"+r.State+"/consent", nil))
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "link_browser_mismatch") {
		t.Fatalf("consent without cookie: %d %s", rec.Code, rec.Body.String())
	}

	// Consent with the real cookie succeeds and claims the state for it.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, linkReq(http.MethodPost, "/connect/"+r.State+"/consent", cookie))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("consent with cookie: %d %s", rec.Code, rec.Body.String())
	}

	// A different browser's cookie is refused at /qr, even though the state
	// itself is perfectly valid and consented.
	other := &http.Cookie{Name: cookieLinkName, Value: "attacker-browser-value"}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, linkReq(http.MethodGet, "/connect/"+r.State+"/qr", other))
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "link_browser_mismatch") {
		t.Fatalf("qr with a different browser's cookie: %d %s", rec.Code, rec.Body.String())
	}

	// The real browser's own cookie still works.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, linkReq(http.MethodGet, "/connect/"+r.State+"/qr", cookie))
	var q struct {
		Status string `json:"status"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &q)
	if rec.Code != http.StatusOK || q.Status != "waiting" {
		t.Fatalf("qr with the right cookie: %d %+v", rec.Code, q)
	}
}

// ---- Task 6 fix round 1: missCache cap, chat body 413, push-handler recover ----

// TestMissCacheEntryCapEnforced is a direct unit test of missCache, not an
// HTTP-level one: it inserts more distinct keys than missCacheMaxEntries,
// all with the far-future expiry remember() always uses (60s, far beyond
// this test's runtime), so the only thing that can bring the live count
// back down is the entry cap itself — not the TTL-expiry sweep path, which
// TestMirrorMissIsNegativelyCached already covers.
func TestMissCacheEntryCapEnforced(t *testing.T) {
	var mc missCache
	const n = missCacheMaxEntries + 500
	for i := 0; i < n; i++ {
		mc.remember("key-" + strconv.Itoa(i))
	}
	if got := mc.count.Load(); got > missCacheMaxEntries {
		t.Fatalf("count = %d, want <= %d", got, missCacheMaxEntries)
	}
}

func TestChatSendRejectsLargeBody(t *testing.T) {
	s, db := newTestServer(t)
	dev, key := seedDev(t, s, "a@x.com")
	acctID := seedChat(t, s, db, dev.ID)

	big := strings.Repeat("a", 70<<10) // over readRawBody's 64 KB limit
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chats/c1/messages?account_id="+acctID, strings.NewReader(big))
	s.Routes().ServeHTTP(rec, withKey(req, key))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413; body=%s", rec.Code, rec.Body.String())
	}
	var e apiError
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatal(err)
	}
	if e.Error.Code != "body_too_large" {
		t.Fatalf("code = %q, want body_too_large", e.Error.Code)
	}
}

// TestPushHandlerRecoversFromPanic drives dispatchNotification's background
// (goroutine) branch directly with a payload handler that panics, and checks
// the panic is contained: it is logged rather than crashing the test
// process, and the semaphore slot it held is still released afterward so a
// panicking handler cannot leak capacity.
func TestPushHandlerRecoversFromPanic(t *testing.T) {
	log, recs := logx.Capture()

	before := len(notifySem)
	dispatchNotification(log, "TESTPANIC", func(ctx context.Context) {
		panic("boom")
	})
	waitFor(t, func() bool { return recs.Contains("push handler panicked") })
	waitFor(t, func() bool { return len(notifySem) == before })
}

// ---- Task 7 fix round 1: persisted QR link browser claim ----

// After a StartLink attempt fails, dropFailed removes the in-memory link
// entry entirely — but the state's browser claim must still be enforced from
// the persisted oauth_states row, or any browser could slip into the gap and
// retry the pairing attempt the original browser started.
func TestQRRetryAfterFailedStartLinkStillEnforcesTheOriginalBrowserClaim(t *testing.T) {
	s, _ := newTestServer(t)
	_, key := seedDev(t, s, "a@x.com")
	h := s.Routes()

	req := withKey(httptest.NewRequest(http.MethodPost, "/api/v1/hosted-auth", strings.NewReader(`{"provider":"FAKECHAT"}`)), key)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var r hostedAuthResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &r)

	cookieA := openLink(t, h, r.State)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, linkReq(http.MethodPost, "/connect/"+r.State+"/consent", cookieA))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("consent: %d %s", rec.Code, rec.Body.String())
	}

	// The first /qr poll's StartLink attempt fails outright; dropFailed
	// removes the in-memory entry, leaving only the persisted claim behind.
	s.fake().StartLinkErr = errors.New("dial failed")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, linkReq(http.MethodGet, "/connect/"+r.State+"/qr", cookieA))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("first qr (forced failure): %d %s", rec.Code, rec.Body.String())
	}
	s.fake().StartLinkErr = nil

	// A different browser, with no in-memory entry left to check against,
	// must still be refused — by the persisted claim on oauth_states.
	cookieB := &http.Cookie{Name: cookieLinkName, Value: "a-different-browser"}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, linkReq(http.MethodGet, "/connect/"+r.State+"/qr", cookieB))
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "link_browser_mismatch") {
		t.Fatalf("retry from a different browser: %d %s", rec.Code, rec.Body.String())
	}

	// The original browser can still retry and start a fresh attempt.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, linkReq(http.MethodGet, "/connect/"+r.State+"/qr", cookieA))
	var q struct {
		Status string `json:"status"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &q)
	if rec.Code != http.StatusOK || q.Status != "waiting" {
		t.Fatalf("retry from the original browser: %d %+v", rec.Code, q)
	}
}

// M-1: nothing pins um_session / um_csrf / um_link to this host, so a
// foothold on a sibling subdomain can set them for the parent domain — and
// SameSite=Strict does not stop a same-site request. The __Host- prefix is
// what closes that: a browser only accepts such a cookie when it is Secure,
// Path=/ and carries no Domain, none of which a sibling origin can satisfy
// for us. It is only usable over HTTPS, so the bare names stay in place for
// a plain-HTTP local run, and every read tries the prefixed name first.
func TestSecureRequestsUseHostPrefixedCookies(t *testing.T) {
	s, _ := newTestServer(t)
	s.cfg.TrustProxy = true
	dev, _ := seedDev(t, s, "a@x.com")
	h := s.Routes()

	// https, as declared by the proxy the operator has vouched for.
	secure := func(req *http.Request) *httptest.ResponseRecorder {
		req.Header.Set("X-Forwarded-Proto", "https")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	find := func(t *testing.T, rec *httptest.ResponseRecorder, name string) *http.Cookie {
		t.Helper()
		for _, c := range rec.Result().Cookies() {
			if c.Name == name {
				return c
			}
		}
		t.Fatalf("no %s cookie: %v", name, rec.Header().Values("Set-Cookie"))
		return nil
	}
	assertHostPrefixed := func(t *testing.T, c *http.Cookie) {
		t.Helper()
		if !c.Secure {
			t.Errorf("%s: not Secure, so a browser rejects the __Host- prefix", c.Name)
		}
		if c.Path != "/" {
			t.Errorf("%s: Path = %q, want %q", c.Name, c.Path, "/")
		}
		if c.Domain != "" {
			t.Errorf("%s: Domain = %q, want none", c.Name, c.Domain)
		}
	}

	// Session: re-issued on any page that resolves one.
	rec := secure(withSession(t, s, httptest.NewRequest(http.MethodGet, "/dashboard", nil), dev.ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("dashboard: %d", rec.Code)
	}
	assertHostPrefixed(t, find(t, rec, "__Host-"+sessionCookie))

	// CSRF: minted while rendering any page carrying a form.
	rec = secure(httptest.NewRequest(http.MethodGet, "/login", nil))
	csrf := find(t, rec, "__Host-"+csrfCookie)
	assertHostPrefixed(t, csrf)

	// And the prefixed cookie is what the form check then reads back.
	form := httptest.NewRequest(http.MethodPost, "/logout",
		strings.NewReader("csrf="+url.QueryEscape(csrf.Value)))
	form.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	form.AddCookie(&http.Cookie{Name: "__Host-" + csrfCookie, Value: csrf.Value})
	if rec = secure(form); rec.Code != http.StatusSeeOther {
		t.Fatalf("logout with the prefixed csrf cookie: %d %s", rec.Code, rec.Body.String())
	}

	// um_link: minted on a Linker provider's connect page. Path widens to "/"
	// with the prefix, which the prefix itself requires.
	_, key := seedDev(t, s, "b@x.com")
	mint := withKey(httptest.NewRequest(http.MethodPost, "/api/v1/hosted-auth",
		strings.NewReader(`{"provider":"FAKECHAT"}`)), key)
	mint.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, mint)
	if rec.Code != http.StatusOK {
		t.Fatalf("hosted-auth: %d %s", rec.Code, rec.Body.String())
	}
	var minted hostedAuthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &minted); err != nil {
		t.Fatal(err)
	}
	rec = secure(httptest.NewRequest(http.MethodGet, "/connect/"+minted.State, nil))
	assertHostPrefixed(t, find(t, rec, "__Host-"+cookieLinkName))

	// Plain HTTP keeps the bare names: __Host- would simply be dropped.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/login", nil))
	find(t, rec, csrfCookie)
}

// M-4: a two-hop proxy chain appends rather than replaces, so the header
// arrives as "https, http". Comparing it whole made that false and silently
// dropped both HSTS and the Secure cookie flag — session cookies then
// travelling without Secure.
func TestForwardedProtoUsesTheFirstToken(t *testing.T) {
	s, _ := newTestServer(t)
	s.cfg.TrustProxy = true
	for _, hdr := range []string{"https", "https, http", " https ,http", "HTTPS,http"} {
		req := httptest.NewRequest(http.MethodGet, "/login", nil)
		req.Header.Set("X-Forwarded-Proto", hdr)
		if !s.requestIsHTTPS(req) {
			t.Errorf("X-Forwarded-Proto %q: requestIsHTTPS = false, want true", hdr)
		}
	}
	for _, hdr := range []string{"http", "http, https", ""} {
		req := httptest.NewRequest(http.MethodGet, "/login", nil)
		req.Header.Set("X-Forwarded-Proto", hdr)
		if s.requestIsHTTPS(req) {
			t.Errorf("X-Forwarded-Proto %q: requestIsHTTPS = true, want false", hdr)
		}
	}
}

// M-7: notify_url is developer-chosen and may carry a credential in its path
// or query — a Telegram bot token being the obvious shape. The delivery
// failure warn logged it verbatim; it goes through the same notify.Scrub the
// body path already used.
func TestNotifyFailureScrubsTheTargetURL(t *testing.T) {
	s, _, recs := newTestServerWithLog(t)
	// Port 1 on loopback: refused immediately, so this fails without a
	// network round trip and without any listener to receive the secret.
	s.notify("http://127.0.0.1:1/bot123456:AA-super-secret-token/notify",
		map[string]any{"status": "CREATED"})
	if !recs.Contains("notify_url delivery failed") {
		t.Fatalf("no delivery-failure warn was logged: %v", recs.All())
	}
	if recs.Contains("AA-super-secret-token") {
		t.Fatal("the notify_url's bot token reached the log verbatim")
	}
	if !recs.Contains("bot•••") {
		t.Fatalf("the logged notify_url was not scrubbed: %v", recs.All())
	}
}

func TestRootServesWebsiteAndStaticIsCacheable(t *testing.T) {
	s, _ := newTestServer(t)
	h := s.Routes()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Entropix") {
		t.Fatalf("GET / = %d %q", rec.Code, rec.Body.String()[:min(200, rec.Body.Len())])
	}
	if rec.Header().Get("Content-Security-Policy") == "" {
		t.Fatal("website served without CSP")
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/static/app.js", nil))
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" {
		t.Fatalf("static: %d cache=%q", rec.Code, rec.Header().Get("Cache-Control"))
	}
}

func TestSetRetention(t *testing.T) {
	s, db := newTestServer(t)
	h := s.Routes()
	dev, key := seedDev(t, s, "a@x.com")

	put := func(t *testing.T, body string, mod func(*http.Request) *http.Request) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPut, "/api/v1/me/retention", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, mod(req))
		return rec
	}
	session := func(req *http.Request) *http.Request { return withSession(t, s, req, dev.ID) }

	for _, tc := range []struct {
		name string
		body string
		want int
	}{
		{"an hour", `{"retention_max_age_secs":3600}`, http.StatusOK},
		{"zero disables", `{"retention_max_age_secs":0}`, http.StatusOK},
		{"negative rejected", `{"retention_max_age_secs":-1}`, http.StatusBadRequest},
		{"over a year rejected", `{"retention_max_age_secs":31536001}`, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if rec := put(t, tc.body, session); rec.Code != tc.want {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, tc.want, rec.Body.String())
			}
		})
	}

	// The value actually persists, and GET /api/v1/me reports it back.
	if rec := put(t, `{"retention_max_age_secs":3600}`, session); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got, err := db.RetentionMaxAge(dev.ID); err != nil || got != time.Hour {
		t.Fatalf("RetentionMaxAge = %v, %v; want 1h", got, err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, withKey(httptest.NewRequest(http.MethodGet, "/api/v1/me", nil), key))
	if !strings.Contains(rec.Body.String(), `"retention_max_age_secs":3600`) {
		t.Fatalf("GET /api/v1/me does not report the policy: %s", rec.Body.String())
	}
}

// An API key must not be able to change how long its developer's content is
// kept — in either direction. Shortening it destroys content; lengthening it
// defeats the policy. Same rule as the other account settings.
func TestSetRetentionIsSessionOnly(t *testing.T) {
	s, _ := newTestServer(t)
	_, key := seedDev(t, s, "a@x.com")
	req := httptest.NewRequest(http.MethodPut, "/api/v1/me/retention", strings.NewReader(`{"retention_max_age_secs":3600}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, withKey(req, key))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
}
