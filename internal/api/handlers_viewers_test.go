package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The mail and chat viewers are the same shell as the dashboard: the shared
// layout, the shared stylesheet, and the shared browser helpers. This test is
// the guard rail for that — a viewer that grew its own <style> block, its own
// fetch wrapper, or a blocking alert() would drift back into the two
// hand-rolled pages this overhaul replaced.
func TestMailAndChatPagesUseSharedShell(t *testing.T) {
	s, _ := newTestServer(t)
	dev, _ := seedDev(t, s, "a@x.com")
	for _, path := range []string{"/mail", "/chat"} {
		rec := httptest.NewRecorder()
		s.Routes().ServeHTTP(rec, withSession(t, s, httptest.NewRequest(http.MethodGet, path, nil), dev.ID))
		body := rec.Body.String()
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d", path, rec.Code)
		}
		for _, want := range []string{`class="split"`, `role="listbox"`, `class="menu-btn`, `class="back-btn`, "um.listNav", "/static/app.js", `aria-current="page"`} {
			if !strings.Contains(body, want) {
				t.Fatalf("%s missing %q", path, want)
			}
		}
		if strings.Contains(body, "100vh") || strings.Contains(body, "alert(") {
			t.Fatalf("%s uses 100vh or alert()", path)
		}
	}
}

// The viewers drive the REST API the docs describe; nothing about them is
// server-rendered beyond the shell, so the endpoints they call are the one
// thing worth asserting from Go. The chat page must also read its connection
// badge through um.accountState rather than branching on the raw socket state
// — the same rule the dashboard is held to.
func TestViewersCallTheDocumentedEndpoints(t *testing.T) {
	s, _ := newTestServer(t)
	dev, _ := seedDev(t, s, "a@x.com")

	cases := []struct {
		path  string
		want  []string
		never []string
	}{
		{"/mail",
			[]string{"/api/v1/accounts", "/api/v1/folders", "/api/v1/emails", "/attachments", `"PATCH"`, `sandbox=""`},
			[]string{"c.state", "connection.state"}},
		{"/chat",
			[]string{"/api/v1/accounts", "/api/v1/chats", "/messages", "Idempotency-Key", "um.accountState(", "um.poll("},
			[]string{"c.state", "connection.state"}},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		s.Routes().ServeHTTP(rec, withSession(t, s, httptest.NewRequest(http.MethodGet, tc.path, nil), dev.ID))
		body := rec.Body.String()
		for _, want := range tc.want {
			if !strings.Contains(body, want) {
				t.Errorf("%s missing %q", tc.path, want)
			}
		}
		for _, never := range tc.never {
			if strings.Contains(body, never) {
				t.Errorf("%s interpolates %q instead of going through um.accountState", tc.path, never)
			}
		}
	}
}
