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

func TestTemplatesParseAndLayoutsRender(t *testing.T) {
	for _, name := range []string{"site"} {
		var sb strings.Builder
		err := Templates().ExecuteTemplate(&sb, name, map[string]any{
			"Shell": Shell{Title: "T", Version: Version},
		})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		out := sb.String()
		for _, want := range []string{"<!doctype html>", "Entropix", "/static/app.css?v=" + Version, `id="notice"`} {
			if !strings.Contains(out, want) {
				t.Fatalf("%s: missing %q", name, want)
			}
		}
	}
}
