package api

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/gauravrautela/unified-messaging/internal/web/docs"
)

// The reference page is generated from internal/web/docs, but the server's
// route table is the only thing that decides what actually exists. These two
// tests are the bridge: one proves the data covers every registered route,
// the other proves the page renders a linkable block for each.

func TestDocsDataCoversApiRoutes(t *testing.T) {
	have := map[string]bool{}
	for _, e := range docs.Endpoints {
		have[e.Method+" "+e.Path] = true
	}
	for _, p := range apiRoutes {
		if !have[p] {
			t.Errorf("no docs.Endpoint for %s", p)
		}
	}
	// And nothing documented that the server does not serve.
	registered := map[string]bool{}
	for _, p := range apiRoutes {
		registered[p] = true
	}
	for _, e := range docs.Endpoints {
		if !registered[e.Method+" "+e.Path] {
			t.Errorf("docs documents %s %s, which the server does not register", e.Method, e.Path)
		}
	}
}

func TestDocsListsEveryRouteWithAnchors(t *testing.T) {
	s, _ := newTestServer(t)
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/docs", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 without a session", rec.Code)
	}
	body := rec.Body.String()
	for _, p := range apiRoutes {
		method, path, _ := strings.Cut(p, " ")
		if !strings.Contains(body, `id="`+docs.Anchor(method, path)+`"`) {
			t.Errorf("docs missing endpoint block for %s", p)
		}
	}
	for _, want := range []string{"Quickstart", `id="events"`, `id="errors"`, "data-copy", `aria-label="Contents"`, "Entropix"} {
		if !strings.Contains(body, want) {
			t.Errorf("docs missing %q", want)
		}
	}
	// Task 6's marketing page deep-links to a single event, so every event
	// needs its own id.
	for _, e := range docs.Events {
		if !strings.Contains(body, `id="event-`+e.Type+`"`) {
			t.Errorf("docs missing id=%q for event %s", "event-"+e.Type, e.Type)
		}
	}
	// The docs page is on the shared shell, not its own hand-rolled <style>.
	if !strings.Contains(body, `/static/app.css?v=`) || !strings.Contains(body, `/static/docs.css?v=`) {
		t.Error("docs page does not load the shared stylesheet plus docs.css")
	}
	// Snippets are shown three ways, with the tabs toggling panes.
	for _, want := range []string{`data-lang="curl"`, `data-lang="node"`, `data-lang="go"`} {
		if !strings.Contains(body, want) {
			t.Errorf("docs missing snippet pane %q", want)
		}
	}
	if !strings.Contains(body, `id="toc-filter"`) {
		t.Error("docs missing the table-of-contents filter")
	}
}

// getDocs renders GET /docs, optionally with a session, and returns the
// recorder so a caller can look at headers as well as the body.
func getDocs(t *testing.T, signedIn bool) *httptest.ResponseRecorder {
	t.Helper()
	s, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/docs", nil)
	if signedIn {
		dev, _ := seedDev(t, s, "a@x.com")
		req = withSession(t, s, req, dev.ID)
	}
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /docs (signedIn=%v) = %d", signedIn, rec.Code)
	}
	return rec
}

// /docs is public and cacheable — that is the point of leaving it out of
// noStorePrefixes — but only while it is the same document for everyone.
// Once a session resolves it carries an email and a live CSRF token, so that
// response must not be parked in any shared cache, and it varies by cookie.
func TestDocsIsCacheableAnonymouslyAndPrivateWhenSignedIn(t *testing.T) {
	rec := getDocs(t, true)
	if cc := rec.Header().Get("Cache-Control"); cc != "private, no-store" {
		t.Errorf("signed-in cache-control = %q, want %q", cc, "private, no-store")
	}
	if v := rec.Header().Values("Vary"); !slices.Contains(v, "Cookie") {
		t.Errorf("signed-in Vary = %v, want it to include Cookie", v)
	}

	rec = getDocs(t, false)
	if cc := rec.Header().Get("Cache-Control"); strings.Contains(cc, "no-store") {
		t.Errorf("anonymous cache-control = %q, want it cacheable", cc)
	}
}

// The reference renders on both shells (spec §5): an anonymous reader has no
// console to navigate, so they get the site chrome — wordmark to the
// homepage, Sign in — rather than a Mail/Chat nav that would bounce them
// straight to a login form.
func TestDocsUsesTheSiteShellWhenAnonymous(t *testing.T) {
	body := getDocs(t, false).Body.String()
	if !strings.Contains(body, `href="/"`) {
		t.Error("anonymous docs has no wordmark link to the homepage")
	}
	if strings.Contains(body, `href="/mail"`) {
		t.Error("anonymous docs renders the console nav, which needs a session")
	}
	if !strings.Contains(body, `href="/login"`) {
		t.Error("anonymous docs offers no way to sign in")
	}

	body = getDocs(t, true).Body.String()
	for _, want := range []string{`href="/dashboard"`, `href="/mail"`, `href="/chat"`, "Sign out"} {
		if !strings.Contains(body, want) {
			t.Errorf("signed-in docs missing console nav %q", want)
		}
	}
}
