package web

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func TestVersionIsStableShortHash(t *testing.T) {
	if !regexp.MustCompile(`^[0-9a-f]{8}$`).MatchString(Version) {
		t.Fatalf("Version = %q, want 8 hex chars", Version)
	}
	if Version != version() {
		t.Fatal("Version changed between calls")
	}
}

func TestStaticServesImmutableCSS(t *testing.T) {
	rec := httptest.NewRecorder()
	Static().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/static/app.css?v="+Version, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/css") {
		t.Fatalf("content-type = %q", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "public, max-age=31536000, immutable" {
		t.Fatalf("cache-control = %q", cc)
	}
	if !strings.Contains(rec.Body.String(), "--accent") {
		t.Fatal("app.css does not define tokens")
	}
}

func TestStaticRejectsTraversalAndMissing(t *testing.T) {
	for _, p := range []string{"/static/../web.go", "/static/nope.css"} {
		rec := httptest.NewRecorder()
		Static().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Code == http.StatusOK {
			t.Fatalf("%s served (%d)", p, rec.Code)
		}
	}
}

// A 404 for a missing or mistyped asset must never carry the immutable
// Cache-Control header: that header is a promise this exact URL will never
// change, and a browser (or shared cache) that believed it for a 404 would
// never re-check even after the file is deployed.
func TestStaticMissingFileIsNotCachedImmutable(t *testing.T) {
	rec := httptest.NewRecorder()
	Static().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/static/nope.css", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "" {
		t.Fatalf("cache-control = %q, want empty on a miss", cc)
	}
}

// accountState is the one place "what state is this account in, in words"
// is decided. The dashboard and the chat page both render from it, so it
// lives in app.js rather than being copied into either template.
func TestAppJSExportsAccountState(t *testing.T) {
	b, err := staticFS.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(b)
	for _, want := range []string{"function accountState(", "accountState,", `"Needs relink"`, `"Needs reconnect"`} {
		if !strings.Contains(js, want) {
			t.Fatalf("app.js missing %q", want)
		}
	}
}

func TestTemplatesParseAndLayoutsRender(t *testing.T) {
	// An empty Shell.Title (the homepage) must render a bare "Entropix"
	// title, not "Entropix · Entropix" — layout_head only prepends "<Title> ·"
	// when Title is set.
	titles := []struct {
		title string
		want  string
	}{
		{title: "T", want: "<title>T · Entropix</title>"},
		{title: "", want: "<title>Entropix</title>"},
	}
	for _, name := range []string{"site"} {
		for _, tc := range titles {
			var sb strings.Builder
			err := Templates().ExecuteTemplate(&sb, name, map[string]any{
				"Shell": Shell{Title: tc.title, Version: Version, Styles: []string{"site.css"}},
			})
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			out := sb.String()
			for _, want := range []string{"<!doctype html>", tc.want, "/static/app.css?v=" + Version, `id="notice"`} {
				if !strings.Contains(out, want) {
					t.Fatalf("%s title=%q: missing %q", name, tc.title, want)
				}
			}
		}
	}
}

// A page's extra stylesheets belong in <head>, after app.css so they can
// override it — not linked from inside <body>, which is invalid HTML and
// costs the browser a re-render when the sheet lands. Shell.Styles is how a
// handler asks for one; an empty Styles must add nothing at all.
func TestLayoutHeadEmitsShellStylesInHead(t *testing.T) {
	render := func(t *testing.T, styles []string) string {
		t.Helper()
		var sb strings.Builder
		if err := Templates().ExecuteTemplate(&sb, "site", map[string]any{
			"Shell": Shell{Version: Version, Styles: styles},
		}); err != nil {
			t.Fatal(err)
		}
		return sb.String()
	}

	out := render(t, []string{"site.css", "docs.css"})
	head, _, ok := strings.Cut(out, "<body>")
	if !ok {
		t.Fatal("rendered page has no <body>")
	}
	for _, want := range []string{
		`<link rel="stylesheet" href="/static/site.css?v=` + Version + `">`,
		`<link rel="stylesheet" href="/static/docs.css?v=` + Version + `">`,
	} {
		if !strings.Contains(head, want) {
			t.Errorf("head missing %q", want)
		}
	}
	// After app.css, so a page sheet wins on equal specificity.
	if strings.Index(head, "app.css") > strings.Index(head, "site.css") {
		t.Error("page stylesheet is linked before app.css")
	}
	// $ inside the range must resolve to the outer Shell; a mis-scoped
	// {{.Shell.Version}} would render an empty ?v=.
	if strings.Contains(out, "?v=\"") || strings.Contains(out, "?v=>") {
		t.Error("stylesheet link lost its cache-busting version")
	}

	if out := render(t, nil); strings.Contains(out, "/static/site.css") {
		t.Error("empty Shell.Styles still emitted a stylesheet link")
	}
}
