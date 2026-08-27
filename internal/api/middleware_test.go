package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSecurityHeadersOnEveryResponse(t *testing.T) {
	s, _ := newTestServer(t)
	dev, key := seedDev(t, s, "a@x.com")
	for _, tc := range []struct {
		path    string
		auth    func(*http.Request) *http.Request
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

// TestLLMsTxtKeepsItsOwnCacheControl proves the handler's own
// Cache-Control (set after secureHeaders has already set one) wins, since
// /llms.txt is not in noStorePrefixes and Header.Set overwrites in place.
func TestLLMsTxtKeepsItsOwnCacheControl(t *testing.T) {
	s, _ := newTestServer(t)
	req := httptest.NewRequest("GET", "/llms.txt", nil)
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)
	if got := rec.Header().Get("Cache-Control"); got != "public, max-age=300" {
		t.Fatalf("/llms.txt Cache-Control = %q, want %q", got, "public, max-age=300")
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
	req.Header.Set("X-Request-Id", "bad id\twithctrl")
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
