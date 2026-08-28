package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The sign-in and sign-up pages render on the shared public shell rather
// than a bolted-on <style> block of their own, and they are labelled: a
// placeholder is not a label, and a browser's autofill and a screen reader
// both need the real thing.
func TestAuthPagesRenderOnThePublicShell(t *testing.T) {
	s, _ := newTestServer(t)
	h := s.Routes()

	for _, tc := range []struct {
		path, title string
		fields      []string
	}{
		{"/login", "<title>Sign in · Entropix</title>", []string{`autocomplete="current-password"`}},
		{"/signup", "<title>Create account · Entropix</title>", []string{`autocomplete="new-password"`, `minlength="10"`, `for="name"`}},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d", tc.path, rec.Code)
		}
		body := rec.Body.String()
		want := append([]string{
			tc.title,
			`<label for="email">`,
			`<label for="password">`,
			`id="toggle"`,
			`autocomplete="email"`,
			`name="csrf"`,
			"Entropix",
			`/static/app.css?v=`,
		}, tc.fields...)
		for _, w := range want {
			if !strings.Contains(body, w) {
				t.Errorf("%s missing %q", tc.path, w)
			}
		}
		// The old hand-rolled page is gone: no placeholder-only inputs and no
		// private colour palette duplicated out of app.css.
		for _, gone := range []string{`placeholder="Email"`, `--accent-text:#fff`, `margin:4rem auto`} {
			if strings.Contains(body, gone) {
				t.Errorf("%s still carries the old markup %q", tc.path, gone)
			}
		}
	}
}

// An inline error is announced, not just coloured — and it keeps the status
// code and the wording the rest of the suite depends on.
func TestAuthErrorIsAnnounced(t *testing.T) {
	s, _ := newTestServer(t)
	seedDev(t, s, "a@x.com")
	h := s.Routes()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader("email=a%40x.com&password=wrongpassword"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	h.ServeHTTP(rec, req)
	body := rec.Body.String()
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (no csrf token)", rec.Code)
	}
	if !strings.Contains(body, `role="alert"`) {
		t.Error("the inline error is not announced (no role=\"alert\")")
	}
	if !strings.Contains(body, csrfExpiredMessage) {
		t.Errorf("the error text changed; body = %s", body)
	}
	// The re-rendered form keeps what the person typed.
	if !strings.Contains(body, `value="a@x.com"`) {
		t.Error("the re-rendered form dropped the email")
	}
}
