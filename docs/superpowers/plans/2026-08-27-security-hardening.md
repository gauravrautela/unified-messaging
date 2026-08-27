# Security Hardening Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close the code-level findings of the launch security audit (C1, C3–C6, I1–I12, minors) so the service can sit behind a gateway on the public internet.

**Architecture:** A `safehttp` client (no redirects, public-only dial guard) for every outbound call; one header/cache middleware on the router; hashed, absolute-lifetime sessions with CSRF-protected auth forms and proxy-aware cookies; a per-developer redirect allowlist; a bounded delivery worker pool with purges and pagination; tight body limits; provider-push and QR-link tightening; PII out of logs. Everything stays stdlib + existing deps.

**Tech Stack:** Go 1.26 stdlib (`net`, `net/http`, `crypto/*`, `syscall`), `modernc.org/sqlite`, `golang.org/x/crypto/bcrypt`.

**Spec:** `docs/superpowers/specs/2026-08-27-security-hardening-design.md`

## Global Constraints

- No new dependencies. `gofmt -l internal cmd` empty; `go vet ./...` clean; `go test ./...` green after every task; `go test -race ./internal/events/ ./internal/api/` green at the end.
- TDD: failing test first (show RED), then GREEN. Never weaken an existing assertion to pass.
- Rate limiting, per-developer caps, send limits, signup verification are **out of scope** (gateway).
- Never log or return a password, session token, API key, OAuth token, bot token, CSRF token, or link cookie; emails/phones in logs are digested/masked.
- Error codes are new only where the spec names them: `csrf` (403), `body_too_large` (413), `attachment_too_large` (400), `invalid_signup` (400), `invalid_credentials` (400), `link_browser_mismatch` (403), `invalid_url` (400, reused).
- Cross-tenant behaviour unchanged: 404 (403 only `not_own_message`); `isolation_test.go`'s completeness gate must stay green.
- Commit trailers on every commit:
  `Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>` and
  `Claude-Session: https://claude.ai/code/session_01RwMaDW9KNcu6BjtbMkU8mo`

---

## File structure

| File | Responsibility |
|---|---|
| `internal/safehttp/safehttp.go` (+test) | hardened `*http.Client`, dial guard, test override |
| `cmd/server/main.go` | server timeouts, purge ticker, config plumbing |
| `internal/store/store.go`, `schema.go`, `developers.go`, `aux.go`, `chat.go` | DB mode, session hash, `redirect_domains_json`, deliveries pagination/purge, cascade, LIKE escaping |
| `internal/api/middleware.go` (new) | `secureHeaders`, `requestIsHTTPS`, request-id validation |
| `internal/api/handlers_auth.go` (+`csrf.go` new) | hashed sessions, CSRF, uniform signup, password change, redirect-domains endpoint |
| `internal/api/handlers_connect.go`, `handlers_link.go` | allowlist check, `safehttp` for notify, link cookie binding |
| `internal/api/handlers_mail.go`, `api.go`, `handlers_chat.go`, `handlers_misc.go` | body limits, attachment cap, negative cache, notification semaphore |
| `internal/events/events.go` | worker pool |
| `internal/syncer/subscriptions.go`, `internal/chatsync/sink.go` | adoption, constant-time, sink assertion |
| `internal/accounts/accounts.go`, `internal/logx/logx.go` | PII in logs |
| `internal/api/isolation_test.go`, docs, README, `.dockerignore`, `.gitignore` | tests and docs |

---

### Task 1: `safehttp` client, server timeouts, file hygiene (C1, C4, C6)

**Files:**
- Create: `internal/safehttp/safehttp.go`, `internal/safehttp/safehttp_test.go`
- Modify: `internal/notify/sender.go:65-70` (`NewRegistry` default client), `internal/events/events.go:56-58`, `internal/api/handlers_connect.go:379-402` (`notify()`), `internal/api/urlcheck.go` (comment), `cmd/server/main.go:128-132`, `internal/store/store.go:47-75` (`Open`), `.gitignore`
- Create: `.dockerignore`

**Interfaces:**
- Produces: `safehttp.Client(timeout time.Duration) *http.Client`; `safehttp.ErrPrivateAddress`; `safehttp.AllowLoopbackForTests(t testing.TB)` (sets a package var for the test's lifetime; `t.Cleanup` restores it).
- Test helpers in `internal/events`, `internal/notify`, `internal/api` that use `httptest` servers must call `safehttp.AllowLoopbackForTests(t)` (or construct their own permissive `http.Client` as they do today — the registry accepts any client, so existing tests that pass `srv.Client()` keep working).

- [ ] **Step 1: Failing tests** — `internal/safehttp/safehttp_test.go`:

```go
package safehttp

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestClientRefusesLoopbackUnlessAllowed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	defer srv.Close()
	c := Client(2 * time.Second)
	if _, err := c.Get(srv.URL); err == nil {
		t.Fatal("loopback dial must be refused by default")
	}
	AllowLoopbackForTests(t)
	resp, err := Client(2 * time.Second).Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

func TestClientDoesNotFollowRedirects(t *testing.T) {
	AllowLoopbackForTests(t)
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.URL.Path == "/" {
			http.Redirect(w, r, "/target", http.StatusTemporaryRedirect)
			return
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()
	resp, err := Client(2 * time.Second).Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 307 || hits != 1 {
		t.Fatalf("status %d hits %d: redirect was followed", resp.StatusCode, hits)
	}
}

func TestPublicOnlyControlRejectsPrivateRanges(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:80", "10.1.2.3:443", "172.16.0.1:80", "192.168.1.1:80",
		"169.254.169.254:80", "100.64.0.1:80", "[::1]:80", "[fe80::1]:80", "[::ffff:10.0.0.1]:80", "0.0.0.0:80"} {
		if err := PublicOnlyControl("tcp", addr, nil); err == nil {
			t.Errorf("%s: expected refusal", addr)
		}
	}
	for _, addr := range []string{"93.184.216.34:443", "[2606:2800:220:1:248:1893:25c8:1946]:443"} {
		if err := PublicOnlyControl("tcp", addr, nil); err != nil {
			t.Errorf("%s: unexpected refusal: %v", addr, err)
		}
	}
}
```

- [ ] **Step 2: Run** `go test ./internal/safehttp/` → compile failure.

- [ ] **Step 3: Implement** `internal/safehttp/safehttp.go`:

```go
// Package safehttp builds HTTP clients for attacker-influenced destinations:
// webhook targets, notify URLs, chat-platform APIs. They never follow
// redirects and refuse to connect to non-public addresses, checked on the
// resolved IP at dial time so a hostname or a DNS rebind cannot smuggle a
// request into the private network.
package safehttp

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

var ErrPrivateAddress = errors.New("safehttp: destination is not a public address")

var allowLoopback atomic.Bool

// AllowLoopbackForTests lets httptest servers (always loopback) be dialled
// for the lifetime of t. Never called from production code.
func AllowLoopbackForTests(t testing.TB) {
	t.Helper()
	allowLoopback.Store(true)
	t.Cleanup(func() { allowLoopback.Store(false) })
}

var (
	cgnat    = netip.MustParsePrefix("100.64.0.0/10")
	metadata = netip.MustParsePrefix("169.254.0.0/16")
)

// PublicOnlyControl is the net.Dialer.Control hook. address is host:port
// with the host already resolved to an IP literal.
func PublicOnlyControl(network, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return ErrPrivateAddress
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return ErrPrivateAddress
	}
	if ip.Is4In6() {
		ip = ip.Unmap()
	}
	if ip.IsLoopback() && allowLoopback.Load() {
		return nil
	}
	switch {
	case ip.IsLoopback(), ip.IsPrivate(), ip.IsLinkLocalUnicast(), ip.IsLinkLocalMulticast(),
		ip.IsMulticast(), ip.IsUnspecified(), ip.IsInterfaceLocalMulticast(),
		cgnat.Contains(ip), metadata.Contains(ip):
		return ErrPrivateAddress
	}
	return nil
}

// Client returns a client with the dial guard and no redirect following.
// A 3xx answer is returned to the caller as-is, so a webhook that redirects
// simply fails its delivery with that status.
func Client(timeout time.Duration) *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second, Control: PublicOnlyControl}
	transport := &http.Transport{
		Proxy:                 nil, // never honour HTTP_PROXY for attacker-chosen URLs
		DialContext:           func(ctx context.Context, network, addr string) (net.Conn, error) { return dialer.DialContext(ctx, network, addr) },
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: timeout,
		MaxIdleConns:          32,
		IdleConnTimeout:       60 * time.Second,
		ForceAttemptHTTP2:     true,
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}
```

Wire it: `notify.NewRegistry(nil)` → `safehttp.Client(15 * time.Second)`; `events.NewDispatcher` nil-registry default → `notify.NewRegistry(nil)`; `handlers_connect.go` `notify()` → a package-level `var notifyClient = safehttp.Client(15 * time.Second)` (tests that hit a local notify receiver call `safehttp.AllowLoopbackForTests(t)` in `newTestServer`). Trim the "known gap" paragraph in `urlcheck.go`'s comment to say the dial guard closes it.

`cmd/server/main.go`:

```go
	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           api.NewServer(...).Routes(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}
```

`store.Open`: before `sql.Open`, `f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600); f.Close()`; then `if fi, err := os.Stat(path); err == nil && fi.Mode().Perm()&0o077 != 0 { _ = os.Chmod(path, 0o600) }`. Test: open a store in a temp dir, `Stat` → `0600`.

`.gitignore` += `unified-messaging.db.pre-tenancy*`. `.dockerignore` = `.git`, `.superpowers`, `docs/superpowers`, `*.db`, `*.db-wal`, `*.db-shm`, `unified-messaging.db.pre-tenancy*`, `.env`, `.idea`.

- [ ] **Step 4: Run** `go test ./internal/safehttp/ ./internal/store/ ./internal/notify/ ./internal/events/ ./internal/api/` → PASS (add `AllowLoopbackForTests` to the test helpers that need it; `TestFailedDiscordDeliveryMasksTokenInLastError` uses a custom RoundTripper — unaffected).

- [ ] **Step 5: Commit** `feat(safehttp): no-redirect, public-only outbound client; server timeouts; db file 0600`

---

### Task 2: Security headers and cache control (C5, I11-headers)

**Files:**
- Create: `internal/api/middleware.go`, `internal/api/middleware_test.go`
- Modify: `internal/api/api.go` (`Routes()` wraps `mux`; `withRequestID` uses `validRequestID`), `internal/config/config.go` (`TrustProxy bool` from `TRUST_PROXY`), `internal/api/handlers_auth.go:19` (`secureCookies` uses `requestIsHTTPS`)

**Interfaces:**
- Produces: `func (s *Server) secureHeaders(next http.Handler) http.Handler`; `func (s *Server) requestIsHTTPS(r *http.Request) bool`; `var requestIDRe = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,64}$`)`; `cfg.TrustProxy`.

- [ ] **Step 1: Failing tests**

```go
func TestSecurityHeadersOnEveryResponse(t *testing.T) {
	s, _ := newTestServer(t)
	dev, key := seedDev(t, s, "a@x.com")
	for _, tc := range []struct {
		path   string
		auth   func(*http.Request) *http.Request
		nostore bool
	}{
		{"/healthz", nil, false},
		{"/docs", nil, false},
		{"/login", nil, true},
		{"/api/v1/me", func(r *http.Request) *http.Request { return withKey(r, key) }, true},
		{"/dashboard", func(r *http.Request) *http.Request { return withSession(t, s, r, dev.ID) }, true},
		{"/connect/nope", nil, true},
	} {
		req := httptest.NewRequest("GET", tc.path, nil)
		if tc.auth != nil {
			req = tc.auth(req)
		}
		rec := httptest.NewRecorder()
		s.Routes().ServeHTTP(rec, req)
		h := rec.Header()
		if h.Get("X-Frame-Options") != "DENY" || h.Get("X-Content-Type-Options") != "nosniff" ||
			h.Get("Referrer-Policy") != "no-referrer" || !strings.Contains(h.Get("Content-Security-Policy"), "frame-ancestors 'none'") {
			t.Errorf("%s: headers = %v", tc.path, h)
		}
		if tc.nostore && (h.Get("Cache-Control") != "no-store" || !strings.Contains(h.Get("Vary"), "Cookie")) {
			t.Errorf("%s: Cache-Control=%q Vary=%q", tc.path, h.Get("Cache-Control"), h.Get("Vary"))
		}
		if !tc.nostore && h.Get("Cache-Control") == "no-store" && tc.path != "/login" {
			t.Errorf("%s: public route must not be no-store", tc.path)
		}
	}
}

func TestHSTSOnlyWhenHTTPS(t *testing.T) {
	s, _ := newTestServer(t)
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Header().Get("Strict-Transport-Security") != "" {
		t.Fatal("HSTS on plain http")
	}
	s.cfg.TrustProxy = true
	req := httptest.NewRequest("GET", "/healthz", nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	rec = httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)
	if !strings.HasPrefix(rec.Header().Get("Strict-Transport-Security"), "max-age=31536000") {
		t.Fatal("HSTS missing behind trusted proxy")
	}
}

func TestRequestIDCharsetIsEnforced(t *testing.T) {
	s, _ := newTestServer(t)
	req := httptest.NewRequest("GET", "/healthz", nil)
	req.Header.Set("X-Request-Id", "bad id\twithctrl")
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)
	if got := rec.Header().Get("X-Request-Id"); strings.Contains(got, " ") || got == "" {
		t.Fatalf("echoed unsafe id %q", got)
	}
	req.Header.Set("X-Request-Id", "req_abc.DEF:123-x")
	rec = httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)
	if rec.Header().Get("X-Request-Id") != "req_abc.DEF:123-x" {
		t.Fatal("valid id must be kept")
	}
}
```

- [ ] **Step 2: Run** → FAIL (headers absent).

- [ ] **Step 3: Implement** `internal/api/middleware.go`:

```go
package api

import (
	"net/http"
	"regexp"
	"strings"
)

var requestIDRe = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,64}$`)

const csp = "default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data:; connect-src 'self'; frame-src 'self'; frame-ancestors 'none'; form-action 'self'; base-uri 'self'"

// requestIsHTTPS is the single source of truth for "is this connection
// secure": direct TLS, a declared https public origin, or — only when the
// operator has said the app sits behind a proxy — X-Forwarded-Proto.
func (s *Server) requestIsHTTPS(r *http.Request) bool {
	if r.TLS != nil || strings.HasPrefix(s.cfg.PublicBaseURL, "https://") {
		return true
	}
	return s.cfg.TrustProxy && strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// noStorePrefixes are tenant- or credential-bearing surfaces a shared cache
// must never keep. /docs, /llms.txt and /healthz are deliberately absent.
var noStorePrefixes = []string{"/api/", "/dashboard", "/mail", "/chat", "/connect/", "/oauth/", "/login", "/signup", "/logout"}

func (s *Server) secureHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", csp)
		if s.requestIsHTTPS(r) {
			h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		for _, p := range noStorePrefixes {
			if strings.HasPrefix(r.URL.Path, p) {
				h.Set("Cache-Control", "no-store")
				h.Add("Vary", "Cookie")
				h.Add("Vary", "Authorization")
				break
			}
		}
		next.ServeHTTP(w, r)
	})
}
```

In `Routes()`, return `s.secureHeaders(s.withRequestID(mux))` (keep the existing wrapping order otherwise). In `withRequestID`: `if !requestIDRe.MatchString(id) { id = logx.NewRequestID() }`. `secureCookies(r)` → `return s.requestIsHTTPS(r)`. Config: `TrustProxy: envBool("TRUST_PROXY", false)`; document in `.env.example`. `/llms.txt`'s own `Cache-Control: public, max-age=300` is set by its handler after the middleware and therefore wins — verify with a test line.

- [ ] **Step 4: Run** `go test ./internal/api/` → PASS.
- [ ] **Step 5: Commit** `feat(api): security headers, no-store on tenant surfaces, proxy-aware https, request-id charset`

---

### Task 3: Sessions and auth forms (C3, I1, I3, I10)

**Files:**
- Modify: `internal/auth/auth.go` (`NewSession`, `SessionDeveloper`, `DeleteSession`, `ChangePassword`), `internal/store/developers.go` (session hash, `DeleteSessionsExcept`, `UpdatePassword`), `internal/store/schema.go` (no change to sessions; `password_hash` exists), `internal/config/config.go` (`SessionMaxAge` from `SESSION_MAX_AGE_DAYS`, default 90), `internal/api/handlers_auth.go` (CSRF, uniform signup, password endpoint), `internal/api/api.go` (route `POST /api/v1/me/password` + `apiRoutes`), `internal/api/handlers_ui.go` (logout form hidden field; change-password form)
- Create: `internal/api/csrf.go`, tests in `internal/api/api_test.go`, `internal/auth/auth_test.go`, `internal/store/store_test.go`

**Interfaces:**
- `auth.HashKey(token)` reused for sessions; `store.CreateSession(hash, developerID, expiresAt)`, `SessionDeveloper(hash, now) (dev, expiresAt, createdAt, err)`, `ExtendSession(hash, …)`, `DeleteSession(hash)`, `DeleteSessionsExcept(developerID, keepHash string) error`, `UpdatePassword(developerID, hash string) error`.
- `(*Service).ChangePassword(ctx, developerID, current, next string) error` (returns `ErrInvalidCredentials` on wrong current, `ErrWeakPassword` on `len(next) < 10`).
- CSRF: cookie `um_csrf`; `func (s *Server) csrfToken(w, r) string` (returns existing or mints and sets); `func (s *Server) checkCSRF(w, r) bool` (form field `csrf` == cookie via `subtle.ConstantTimeCompare`, and if `Origin`/`Referer` present its host must equal `r.Host`; on failure writes `403 csrf` and returns false).
- Auth templates render `<input type="hidden" name="csrf" value="{{.CSRF}}">`; the dashboard's logout `<form>` gets the same hidden field (server-rendered via the existing template data).

- [ ] **Step 1: Failing tests**

```go
func TestSessionTokenIsHashedAtRest(t *testing.T) {
	s, db := newTestServer(t)
	dev, _ := seedDev(t, s, "a@x.com")
	tok, _, err := s.auth.NewSession(context.Background(), dev.ID)
	if err != nil { t.Fatal(err) }
	var n int
	if err := db.DB().QueryRow(`SELECT COUNT(*) FROM sessions WHERE id = ?`, tok).Scan(&n); err != nil { t.Fatal(err) }
	if n != 0 { t.Fatal("raw token stored") }
	if err := db.DB().QueryRow(`SELECT COUNT(*) FROM sessions WHERE id = ?`, auth.HashKey(tok)).Scan(&n); err != nil || n != 1 { t.Fatalf("hashed row missing: %d %v", n, err) }
	if _, _, err := s.auth.SessionDeveloper(context.Background(), tok); err != nil { t.Fatal("token must still resolve") }
}

func TestSessionAbsoluteLifetime(t *testing.T) {
	s, db := newTestServer(t)
	dev, _ := seedDev(t, s, "a@x.com")
	tok, _, _ := s.auth.NewSession(context.Background(), dev.ID)
	// Age the row past the absolute limit while keeping it unexpired by the sliding rule.
	if _, err := db.DB().Exec(`UPDATE sessions SET created_at = ? WHERE id = ?`, time.Now().Add(-91*24*time.Hour).Unix(), auth.HashKey(tok)); err != nil { t.Fatal(err) }
	if _, _, err := s.auth.SessionDeveloper(context.Background(), tok); err == nil { t.Fatal("session older than max age must be rejected") }
}

func TestLoginRequiresCSRFAndSameOrigin(t *testing.T) {
	s, _ := newTestServer(t)
	seedDev(t, s, "a@x.com") // seedDev uses password "correct horse battery" — check the helper and use its constant
	h := s.Routes()
	// 1. no token
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/login", strings.NewReader("email=a%40x.com&password=correct+horse+battery"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	h.ServeHTTP(rec, req)
	if rec.Code != 403 || !strings.Contains(rec.Body.String(), "csrf") { t.Fatalf("no token: %d %s", rec.Code, rec.Body.String()) }
	// 2. fetch the form to get the token cookie, then post with it
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/login", nil))
	var csrf *http.Cookie
	for _, c := range rec.Result().Cookies() { if c.Name == "um_csrf" { csrf = c } }
	if csrf == nil { t.Fatal("no csrf cookie on the form page") }
	if !strings.Contains(rec.Body.String(), `name="csrf" value="`+csrf.Value+`"`) { t.Fatal("form lacks the csrf field") }
	post := func(origin string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/login", strings.NewReader("email=a%40x.com&password=correct+horse+battery&csrf="+csrf.Value))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(csrf)
		if origin != "" { req.Header.Set("Origin", origin) }
		h.ServeHTTP(rec, req)
		return rec
	}
	if rec := post("https://evil.example"); rec.Code != 403 { t.Fatalf("cross-origin: %d", rec.Code) }
	if rec := post("http://example.com"); rec.Code != 303 { t.Fatalf("same-origin (httptest host is example.com): %d %s", rec.Code, rec.Body.String()) }
}

func TestSignupErrorIsUniform(t *testing.T) { /* existing email and bad input both → 400 with the same message; use the csrf flow above */ }

func TestChangePasswordInvalidatesOtherSessions(t *testing.T) {
	s, _ := newTestServer(t)
	dev, _ := seedDev(t, s, "a@x.com")
	tokA, _, _ := s.auth.NewSession(context.Background(), dev.ID)
	tokB, _, _ := s.auth.NewSession(context.Background(), dev.ID)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/me/password", strings.NewReader(`{"current_password":"correct horse battery","new_password":"another strong one"}`))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: "um_session", Value: tokA})
	s.Routes().ServeHTTP(rec, req)
	if rec.Code != 204 { t.Fatalf("%d %s", rec.Code, rec.Body.String()) }
	if _, _, err := s.auth.SessionDeveloper(context.Background(), tokB); err == nil { t.Fatal("other session must be gone") }
	if _, _, err := s.auth.SessionDeveloper(context.Background(), tokA); err != nil { t.Fatal("current session must survive") }
	if _, err := s.auth.Login(context.Background(), "a@x.com", "another strong one"); err != nil { t.Fatal("new password must work") }
	// API key must not reach it
	_, key := seedDev(t, s, "b@x.com")
	rec = httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, withKey(httptest.NewRequest("POST", "/api/v1/me/password", strings.NewReader(`{}`)), key))
	if rec.Code != 403 { t.Fatalf("api key: %d", rec.Code) }
}
```

Check `seedDev`'s password constant in `api_test.go` and use it. Existing tests that log in via form POST (grep `"/login"` in `api_test.go`) must be updated to fetch the CSRF cookie first — add a helper `loginForm(t, h, email, password) *httptest.ResponseRecorder` and use it everywhere.

- [ ] **Step 2: Run** → FAIL.

- [ ] **Step 3: Implement**

`store/developers.go`: session functions take the hash (rename param `id` → `hash`, no SQL change); add

```go
func (s *Store) DeleteSessionsExcept(developerID, keepHash string) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE developer_id = ? AND id <> ?`, developerID, keepHash)
	return err
}
func (s *Store) UpdatePassword(developerID, hash string) error {
	_, err := s.db.Exec(`UPDATE developers SET password_hash = ? WHERE id = ?`, hash, developerID)
	return err
}
func (s *Store) DeveloperPasswordHash(developerID string) (string, error)  // SELECT password_hash
```

`SessionDeveloper` also returns `created_at` (add to the SELECT and the signature; update the one caller).

`auth.go`: `NewSession` stores `HashKey(tok)`; `SessionDeveloper(ctx, token)` hashes first, then `if now.Sub(created) > a.maxAge { _ = a.store.DeleteSession(hash); return ..., ErrNoSession }`; `New(s, log, sessionTTL, maxAge)`; `DeleteSession` hashes; add

```go
func (a *Service) ChangePassword(ctx context.Context, developerID, current, next string) error {
	if len(next) < 10 { return ErrWeakPassword }
	hash, err := a.store.DeveloperPasswordHash(developerID)
	if err != nil { return err }
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(current)) != nil { return ErrInvalidCredentials }
	newHash, err := bcrypt.GenerateFromPassword([]byte(next), bcryptCost)
	if err != nil { return err }
	return a.store.UpdatePassword(developerID, string(newHash))
}
```

`csrf.go`:

```go
const csrfCookie = "um_csrf"

func (s *Server) csrfToken(w http.ResponseWriter, r *http.Request) string {
	if c, err := r.Cookie(csrfCookie); err == nil && len(c.Value) == 43 {
		return c.Value
	}
	tok := logx.RandomToken(32) // base64url, no padding; add this helper to logx if absent (crypto/rand)
	http.SetCookie(w, &http.Cookie{Name: csrfCookie, Value: tok, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteStrictMode, Secure: s.requestIsHTTPS(r), MaxAge: 12 * 3600})
	return tok
}

// checkCSRF guards the form posts. Double-submit token plus an Origin check:
// the token defeats cross-site form posts, the Origin check defeats a token
// obtained through any future subdomain or XSS foothold.
func (s *Server) checkCSRF(w http.ResponseWriter, r *http.Request) bool {
	c, err := r.Cookie(csrfCookie)
	field := r.PostFormValue("csrf")
	if err != nil || field == "" || subtle.ConstantTimeCompare([]byte(c.Value), []byte(field)) != 1 {
		writeError(w, http.StatusForbidden, "csrf", "the form has expired — reload the page and try again")
		return false
	}
	if o := r.Header.Get("Origin"); o != "" {
		if u, err := url.Parse(o); err != nil || !strings.EqualFold(u.Host, r.Host) {
			writeError(w, http.StatusForbidden, "csrf", "cross-site form submission rejected")
			return false
		}
	} else if ref := r.Header.Get("Referer"); ref != "" {
		if u, err := url.Parse(ref); err != nil || !strings.EqualFold(u.Host, r.Host) {
			writeError(w, http.StatusForbidden, "csrf", "cross-site form submission rejected")
			return false
		}
	}
	return true
}
```

Handlers: `handleLoginPage`/`handleSignupPage` pass `CSRF: s.csrfToken(w, r)` to the template; `handleLogin`/`handleSignup`/`handleLogout` call `if !s.checkCSRF(w, r) { return }` right after `ParseForm`. The dashboard's logout form: `handleDashboard` passes `CSRF` into its template and the form gets the hidden field. Signup: on `auth.ErrEmailTaken` **and** on validation errors, `writeError(w, 400, "invalid_signup", "could not create the account — check the details or sign in")`; log the real reason at DEBUG with `"email_digest", logx.Digest(email)`. Add `POST /api/v1/me/password` (`requireSession`) → `ChangePassword` → `DeleteSessionsExcept(dev.ID, HashKey(currentToken))` (the current token is on the request cookie) → 204; map `ErrInvalidCredentials` → 400 `invalid_credentials`, `ErrWeakPassword` → 400 `invalid_body`. Dashboard: a "Change password" form under API keys posting JSON to the new route (reuse the `api()` helper; show result inline). `main.go`: `auth.New(db, log, cfg.SessionTTL, cfg.SessionMaxAge)`.

- [ ] **Step 4: Run** `go test ./internal/auth/ ./internal/store/ ./internal/api/` → PASS (update the isolation test's expected route list: `POST /api/v1/me/password` answers 403 for an API key like the key routes).
- [ ] **Step 5: Commit** `feat(auth): hashed sessions with absolute lifetime, csrf on auth forms, uniform signup error, password change`

---

### Task 4: Redirect-domain allowlist (I2)

**Files:**
- Modify: `internal/store/schema.go` (migration `ALTER TABLE developers ADD COLUMN redirect_domains_json TEXT NOT NULL DEFAULT '[]'` + CREATE TABLE column), `internal/store/developers.go` (`GetRedirectDomains`, `SetRedirectDomains`), `internal/model/model.go` (`Developer.RedirectDomains []string json:"redirect_domains"`), `internal/api/handlers_auth.go` (`handleMe` includes them; `PUT /api/v1/me/redirect-domains`), `internal/api/handlers_connect.go:76-84` (allowlist check), `internal/api/api.go` (route + `apiRoutes`), `internal/api/handlers_ui.go` (Settings textarea)
- Test: `internal/api/api_test.go`

**Interfaces:** `func hostAllowed(host string, domains []string) bool` — exact match or `*.example.com` allowing any subdomain (not the apex); the server's own host (`PUBLIC_BASE_URL` host, else `r.Host`) is always allowed.

- [ ] **Step 1: Failing tests**

```go
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
	if rec.Code != 200 { t.Fatalf("set: %d %s", rec.Code, rec.Body.String()) }
	if rec := mint(`{"success_redirect_url":"https://app.customer.com/done","failure_redirect_url":"https://exact.example.org/x"}`); rec.Code != 200 {
		t.Fatalf("listed: %d %s", rec.Code, rec.Body.String())
	}
	if rec := mint(`{"success_redirect_url":"https://customer.com/done"}`); rec.Code != 400 { t.Fatal("apex is not covered by *.customer.com") }
	// api key cannot change the list; bad entries rejected
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, withKey(httptest.NewRequest("PUT", "/api/v1/me/redirect-domains", strings.NewReader(`{"domains":[]}`)), key))
	if rec.Code != 403 { t.Fatalf("api key: %d", rec.Code) }
	for _, bad := range []string{`{"domains":["10.0.0.1"]}`, `{"domains":["not a host"]}`, `{"domains":["http://x.com"]}`} {
		rec = httptest.NewRecorder()
		req = httptest.NewRequest("PUT", "/api/v1/me/redirect-domains", strings.NewReader(bad))
		req.Header.Set("Content-Type", "application/json")
		h.ServeHTTP(rec, withSession(t, s, req, dev.ID))
		if rec.Code != 400 { t.Errorf("%s: %d", bad, rec.Code) }
	}
	// GET /me shows the list
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, withKey(httptest.NewRequest("GET", "/api/v1/me", nil), key))
	if !strings.Contains(rec.Body.String(), `"redirect_domains":["*.customer.com","exact.example.org"]`) { t.Fatalf("me: %s", rec.Body.String()) }
}
```

- [ ] **Step 2: Run** → FAIL.
- [ ] **Step 3: Implement** per the spec §6: store column + JSON round-trip on `DeveloperByEmail`/`SessionDeveloper`/`DeveloperByKeyHash` reads (all go through one `scanDeveloper`; add the column to each SELECT); `hostAllowed`; validation `^[a-z0-9-]+(\.[a-z0-9-]+)+$` per label after stripping an optional `*.` prefix, no IPs, ≤ 20 entries, lower-cased; the hosted-auth loop checks `hostAllowed(strings.ToLower(parsed.Hostname()), dev.RedirectDomains)` or own host; the dashboard Settings block with a textarea (one domain per line) and a Save button that PUTs JSON. Existing tests that mint hosted-auth links with redirect URLs on other hosts: update them to use the server's own origin or to set the allowlist first (grep `success_redirect_url` in `api_test.go`).
- [ ] **Step 4: Run** `go test ./internal/api/ ./internal/store/` → PASS (update the isolation test route table for `PUT /api/v1/me/redirect-domains` → 403 with an API key).
- [ ] **Step 5: Commit** `feat(api): per-developer redirect-domain allowlist for hosted-auth`

---

### Task 5: Delivery worker pool, pagination, purges, cascade, LIKE escaping (I4, I8)

**Files:**
- Modify: `internal/events/events.go` (pool), `internal/store/aux.go` (`ListDeliveries(webhookID, limit, offset)`, `PurgeDeadDeliveries(before time.Time) (int64, error)`, `DeleteDeliveriesForAccount` inside `DeleteAccount`), `internal/store/store.go:215` (`DeleteAccount` runs in a tx and deletes deliveries by `account_id`), `internal/store/store.go`/`chat.go` (`escapeLike` + `ESCAPE '\'`), `internal/api/handlers_misc.go:380-398` (pagination), `cmd/server/main.go` (hourly ticker), `internal/config/config.go` (`DeliveryRetention` from `DELIVERY_RETENTION_DAYS`, default 7)
- Tests: `internal/events/events_test.go`, `internal/store/store_test.go`, `internal/api/api_test.go`

- [ ] **Step 1: Failing tests**

```go
func TestSlowHookDoesNotBlockOtherHooks(t *testing.T) {
	db := newTestStore(t); db.SetSealKey(testKey); seedTenant(t, db)
	safehttp.AllowLoopbackForTests(t)
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { time.Sleep(3 * time.Second) }))
	t.Cleanup(slow.Close)
	fast := newReceiver(t, 200)
	for _, h := range []model.Webhook{
		{ID: "wh_slow", DeveloperID: "dev_1", URL: slow.URL, CreatedAt: time.Now()},
		{ID: "wh_fast", DeveloperID: "dev_1", URL: fast.URL, CreatedAt: time.Now()},
	} { if err := db.SaveWebhook(h); err != nil { t.Fatal(err) } }
	d := NewDispatcher(db, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	d.DeliveryWorkers = 4
	ctx, cancel := context.WithCancel(context.Background()); defer cancel()
	d.Start(ctx)
	start := time.Now()
	d.Emit(model.Event{Type: model.EventMailReceived, AccountID: "acc_1", Email: &model.Email{Subject: "x"}})
	waitFor(t, func() bool { return fast.count() == 1 })
	if time.Since(start) > 2*time.Second { t.Fatal("fast hook waited behind the slow one") }
}

func TestDeliveriesArePaginatedAndDeadRowsPurged(t *testing.T) {
	// store: save 5 dead deliveries with created_at spanning 10 days → PurgeDeadDeliveries(now-7d) removes the old ones;
	// api: GET /api/v1/webhooks/{id}/deliveries?limit=2&offset=2 → items len 2, limit 2, offset 2; limit=500 → clamped to 200.
}

func TestDeleteAccountRemovesItsDeliveries(t *testing.T) {
	// developer-wide hook, a delivery row with account_id=acc_1, DeleteAccount("acc_1") → ListDeliveries returns none for acc_1.
}

func TestSearchEscapesLikeWildcards(t *testing.T) {
	// two emails "50% off" and "500 off"; ListEmails q="50%" returns only the first.
}
```

- [ ] **Step 2: Run** → FAIL.
- [ ] **Step 3: Implement**

`events.go`: add `DeliveryWorkers int` (default 8) and a `sem chan struct{}` created in `Start`. In `deliver`, for each subscribed hook `wg.Add(1); sem <- struct{}{}; go func(h, dl) { defer wg.Done(); defer func(){ <-sem }(); if err := d.send(ctx, h, dl, 1); err != nil { d.enqueue(dl, err); return }; log delivered }()`; `wg.Wait()` before returning so events stay ordered per producer but hooks run concurrently. `retryDue` uses the same pool per delivery. `drain` keeps its bound. Keep the `dropped` counter untouched.

`store`: `ListDeliveries(webhookID string, limit, offset int)` with `LIMIT ? OFFSET ?`; `PurgeDeadDeliveries(before time.Time) (int64, error)` → `DELETE FROM webhook_deliveries WHERE dead = 1 AND created_at < ?`; `DeleteAccount` in `inTx`: delete deliveries `WHERE account_id = ?`, webhooks, then the account. `escapeLike(s)` replaces `\`→`\\`, `%`→`\%`, `_`→`\_`; every `LIKE ?` gains ` ESCAPE '\'`.

`handlers_misc.go`: parse `limit` (default 50, max 200) and `offset` (≥ 0) → `{items, limit, offset}`. `main.go`: `go func(){ t := time.NewTicker(time.Hour); for { select { case <-ctx.Done(): return; case <-t.C: db.PurgeExpiredOAuthStates(); n, _ := db.PurgeDeadDeliveries(time.Now().Add(-cfg.DeliveryRetention)); log.Info("purge", "dead_deliveries", n) } } }()` (plus the boot call).

- [ ] **Step 4: Run** `go test -race ./internal/events/ && go test ./internal/store/ ./internal/api/` → PASS.
- [ ] **Step 5: Commit** `feat(events,store): concurrent hook delivery, deliveries pagination, dead-row purge, account cascade, LIKE escaping`

---

### Task 6: Body limits, attachment cap, negative cache, notification semaphore (I5, I7, I6-part)

**Files:**
- Modify: `internal/api/api.go:520` (`decodeJSON` limits), `internal/api/handlers_mail.go` (large decoder on send/reply/forward/draft; attachment cap; negative cache in `handleGetEmail`), `internal/api/handlers_chat.go:74` (`readRawBody` 64 KB), `internal/api/handlers_misc.go:549` (semaphore), `internal/api/api_test.go` (Graph stub for the mirror-miss path: `newTestServerWithProviders` with a fake `Mailbox` whose `GetMessage` returns `provider.ErrNotFound`)

**Interfaces:** `decodeJSON(r, v)` (64 KB) and `decodeJSONLarge(r, v)` (8 MB); `errBodyTooLarge` detection via `*http.MaxBytesError` → `413 body_too_large`; `const maxAttachmentBytes = 3 << 20`; `type missCache struct{ m sync.Map }` with `hit(key) bool`, `remember(key)`, `sweep()` every 1000 inserts; `notifySem = make(chan struct{}, 32)`.

- [ ] **Step 1: Failing tests**

```go
func TestSmallRoutesRejectLargeBodies(t *testing.T) {
	// PATCH /api/v1/emails/M1?account_id=acc_1 with a 100 KB body → 413 body_too_large;
	// POST /api/v1/emails with a 1 MB body (valid JSON, small attachment) → not 413.
}
func TestAttachmentsAreCappedServerSide(t *testing.T) {
	// POST /api/v1/emails with one attachment whose decoded size is 4 MB → 400 attachment_too_large; 2 MB → accepted (202 via the fake mailbox).
}
func TestMirrorMissIsNegativelyCached(t *testing.T) {
	// fake mailbox counts GetMessage calls and returns ErrNotFound; two GET /emails/{id} within 60 s → provider called once; both 404.
}
func TestNotificationHandlingIsBounded(t *testing.T) {
	// fire 100 POST /notifications/OUTLOOK concurrently with a payload whose ParseNotifications blocks on a channel;
	// assert at most 32 are inside HandleNotifications at once (instrument via the fake pusher) and all 100 answered 202.
}
```

Check how `newTestServerWithProviders` accepts a fake mail provider (it exists from the WhatsApp work) and what the Outlook fake in `internal/provider/providertest` offers; add a `providertest.FakeMail` if there is none (small: `Mailbox` with `GetMessage` and `Send` recording calls).

- [ ] **Step 2: Run** → FAIL.
- [ ] **Step 3: Implement** as the spec §8 says. `decodeJSON` wraps `http.MaxBytesReader(w?, …)` — the current helper passes `nil` for the writer; keep that but detect `*http.MaxBytesError` with `errors.As` in every handler that calls `decodeJSON` via a shared `writeDecodeError(w, err)` (413 for too-large, 400 `invalid_body` otherwise) — replace the existing `writeError(w, 400, "invalid_body", err.Error())` calls after `decodeJSON` with it (grep). Attachment cap: in the send/reply/forward/draft handlers, sum `base64.StdEncoding.DecodedLen(len(a.Content))`; over cap → `400 attachment_too_large`. Negative cache keyed `acct.ID + "\x00" + id`, TTL 60 s, only on `provider.ErrNotFound` (check how `writeProviderError` maps provider 404 — reuse that sentinel). Semaphore: `select { case notifySem <- struct{}{}: go func(){ defer func(){ <-notifySem }(); handle(2*time.Minute) }(); default: handle(10*time.Second) /* inline */ }`.
- [ ] **Step 4: Run** `go test ./internal/api/` → PASS; confirm no test reaches the network: `go test ./internal/api/ -run . -v 2>&1 | grep -c graph.microsoft.com` → 0.
- [ ] **Step 5: Commit** `fix(api): per-route body limits, attachment cap, negative cache for mirror misses, bounded push handling`

---

### Task 7: Provider push adoption, chat sink invariant, QR link browser binding (I6, I11, minor)

**Files:**
- Modify: `internal/syncer/subscriptions.go:161-226`, `internal/chatsync/sink.go`, `internal/api/handlers_connect.go:216` (`handleConnectRedirect` sets the link cookie for linker providers), `internal/api/handlers_link.go` (`link.browserHash`; consent/qr check), `internal/api/handlers_auth.go` (cookie helper reuse)
- Tests: `internal/syncer/syncer_test.go`, `internal/chatsync/runtime_test.go`, `internal/api/api_test.go`

**Interfaces:** cookie `um_link` (32 random bytes base64url, `HttpOnly`, `SameSite=Strict`, `Secure` per `requestIsHTTPS`, `MaxAge` = `linkTTL` seconds, `Path: /connect/`); `link.browserHash string` set from `sha256(cookie)` when the link entry is created by the first `/consent` or `/qr`; mismatch → `403 link_browser_mismatch`.

- [ ] **Step 1: Failing tests**

```go
func TestAdoptedSubscriptionIsReplacedNotTrusted(t *testing.T) {
	// fake pusher: Create returns ErrDuplicate once, List returns one remote sub; expect Delete(remote.ID) then Create → stored sub has a non-empty ClientState.
}
func TestNotificationWithoutMatchingClientStateIsRejected(t *testing.T) {
	// stored sub with ClientState "s"; notification with "" and with "S" → no Wake; with "s" → Wake.
}
func TestSinkRejectsForeignAccountID(t *testing.T) {
	// FakeChat emits a message with accountID "acc_other" on acc_1's sink → no row for either account; an ERROR log line with digests.
}
func TestQRLinkIsBoundToTheBrowserThatOpenedIt(t *testing.T) {
	// mint a FAKECHAT hosted-auth link; GET /connect/{state} → Set-Cookie um_link; POST consent without the cookie → 403 link_browser_mismatch;
	// with the cookie → 204; GET /qr with a different um_link value → 403; with the right one → 200/waiting.
}
```

- [ ] **Step 2: Run** → FAIL.
- [ ] **Step 3: Implement** per spec §9–§10. `adoptExisting`: for each remote, `pusher.Delete(ctx, accountID, r.ID)` then return `s.create(ctx, accountID, pusher)` (the existing create path); log `replaced pre-existing subscription`. `HandleNotifications`: `if sub.ClientState == "" || subtle.ConstantTimeCompare([]byte(sub.ClientState), []byte(n.ClientState)) != 1 { warn; continue }`. Sink: at the top of each callback `if accountID != s.a.acct.ID { s.a.log.Error("chat sink: foreign account id", "got", logx.Digest(accountID), "want", logx.Digest(s.a.acct.ID)); return }`. Link cookie: `handleConnectRedirect` (linker branch) sets it when absent; `handleConsent` and `handleLinkQR` read it, hash it, and on the registry entry's first creation store the hash, thereafter compare (constant-time).
- [ ] **Step 4: Run** `go test ./internal/syncer/ ./internal/chatsync/ ./internal/api/` → PASS.
- [ ] **Step 5: Commit** `fix: replace adopted subscriptions, constant-time clientState, sink account invariant, browser-bound QR links`

---

### Task 8: Logging PII and isolation-test extensions (I9, minors)

**Files:**
- Modify: `internal/accounts/accounts.go:122` and `internal/api/handlers_connect.go` "account connected"/"account linked" lines (`email_digest`), `internal/logx/logx.go` (`Redact` masks `email`/`phone`/`identifier` values), `internal/api/isolation_test.go` (session pass + browser-route table)
- Tests: `internal/logx/logx_test.go`, `internal/api/api_test.go` (log capture for connect), `isolation_test.go`

- [ ] **Step 1: Failing tests**

```go
func TestRedactMasksEmailAndPhoneValues(t *testing.T) {
	in := map[string]any{"to": []any{map[string]any{"email": "john.doe@example.com"}}, "phone": "+919888000855", "identifier": "+15551234567", "subject": "keep"}
	out := Redact(in).(map[string]any)
	// expect "j•••@example.com", "+91 98••• •855", "+1 55••• •567", subject unchanged
}
func TestConnectLogsDigestNotEmail(t *testing.T) { /* capture logs around a fake OAuth callback; assert "email_digest=h_" present and the address absent */ }
```

Isolation: (a) run the existing `cases` table a second time with `withSession(t, s, req, devB.ID)` instead of `withKey`, same expectations (session-only routes now answer 404/403 as appropriate); (b) a browser-route table:

| Route | Expectation |
|---|---|
| `GET /dashboard`, `/mail`, `/chat` without session | 302 to `/login` |
| `GET /connect/{stateOfA}` | 200 — but the page never contains A's email or account ids (assert body lacks `a@outlook.com`) |
| `POST /connect/{stateOfA}/consent` with no link cookie | 403 |
| `GET /connect/unknown` | 404 |
| `GET /oauth/callback?state=unknown` | 400/404, never 500 |
| `POST /login` without csrf | 403 |
| `GET /healthz`, `/docs`, `/llms.txt` | 200, no `Set-Cookie` |

The completeness gate: add a second list `browserRoutes` in `api.go` (the patterns registered outside `apiRoutes`) and assert every entry has a row.

- [ ] **Step 2: Run** → FAIL.
- [ ] **Step 3: Implement.** `logx.Redact`: keys containing `email` → `maskEmail` (first char + `•••@` + domain); keys containing `phone` or equal `identifier` → local `maskPhone` (same algorithm as `notify.MaskPhone`, copied to avoid the import cycle, with a comment naming the twin). Replace the two INFO `"email", …` attributes with `"email_digest", logx.Digest(email)`.
- [ ] **Step 4: Run** `go test ./internal/logx/ ./internal/accounts/ ./internal/api/` → PASS.
- [ ] **Step 5: Commit** `fix(logging): digest account emails, mask email/phone in debug bodies; isolation tests for sessions and browser routes`

---

### Task 9: Docs, README, end-to-end verification

**Files:**
- Modify: `internal/api/handlers_docs.go` (§2 auth: password change; §3 connect: redirect allowlist + `PUT /api/v1/me/redirect-domains`; §6 webhooks: deliveries pagination, 3xx is a failure, no redirects; §9 errors: new codes; §12 limits: body limits, retention), `internal/api/handlers_llms.go` (same facts + rules bullets), `README.md` (new "## Security" section: what the code enforces vs what the gateway must do — rate limits, caps, TLS termination with `X-Forwarded-Proto` + `TRUST_PROXY=1`; env vars `TRUST_PROXY`, `SESSION_MAX_AGE_DAYS`, `DELIVERY_RETENTION_DAYS`; the Postgres move), `.env.example`
- Tests: docs tests extended with `redirect-domains`, `body_too_large`, `TRUST_PROXY`

- [ ] **Step 1: Failing tests** — extend `TestDocsPageIsPublicAndCoversEveryRoute` / `TestLLMsTxt…` with `"redirect-domains"`, `"body_too_large"`, `"me/password"`.
- [ ] **Step 2: Run** → FAIL.
- [ ] **Step 3: Write the docs** (facts from the code, not from memory: re-read the handlers for exact codes and limits).
- [ ] **Step 4: End-to-end (verifying-services-end-to-end):** build; run on a **copy** of the DB in the scratch dir with `.env` + `TRUST_PROXY=1 DEBUG=1 LISTEN_ADDR=:8099`; poll `/healthz`; then with curl: headers present on `/login` and `/api/v1/me` (`X-Frame-Options`, CSP, `Cache-Control: no-store`), HSTS only with `X-Forwarded-Proto: https`; `POST /login` without csrf → 403, with the fetched cookie+field → 303 and `um_session` cookie `Secure` when `X-Forwarded-Proto: https`; hosted-auth with an unlisted redirect → 400, after `PUT /api/v1/me/redirect-domains` → 200; a webhook to a public host that redirects (use `https://httpbin.org/redirect-to?url=http://127.0.0.1:1/x` if reachable; otherwise note) → delivery `last_error: status 302`; a webhook to `http://169.254.169.254/` → 400 at registration; a hostname resolving to loopback (`http://localtest.me/`) → registration passes, delivery fails with `not a public address`; `GET /api/v1/webhooks/{id}/deliveries?limit=1` → `{items, limit:1, offset:0}`; a 100 KB PATCH → 413. Leak greps: session tokens, csrf values, emails (`grep -c '@' server.log` should only match digests), bot tokens → 0. Stop the server; DB copy only.
- [ ] **Step 5: Run** `gofmt -l internal cmd; go vet ./...; go test ./...; go test -race ./internal/events/ ./internal/api/`; commit `docs: security hardening — headers, sessions, allowlist, limits, gateway responsibilities`

---

## Self-review

- **Spec coverage:** §1 → T1; §2 → T1; §3 → T2; §4 → T1; §5 → T2 (proxy) + T3; §6 → T4; §7 → T5; §8 → T6 (+ T2 request-id); §9 → T7; §10 → T7; §11 → T8; §12 → T8 + T9.
- **Placeholders:** the T5/T6/T7/T8 test bodies given as comments describe exact inputs and expected outputs; implementers write them out. All code steps carry code or exact edits.
- **Type consistency:** `safehttp.Client`/`AllowLoopbackForTests` used identically in T1, T5; `requestIsHTTPS` defined in T2 and used in T3/T7; `auth.HashKey` reused for sessions (T3); `ListDeliveries(webhookID, limit, offset)` in T5 and its handler; `decodeJSON`/`decodeJSONLarge` in T6; `apiRoutes` gains `POST /api/v1/me/password` (T3) and `PUT /api/v1/me/redirect-domains` (T4) and the isolation table rows for both.
