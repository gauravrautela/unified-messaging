package api

import (
	"html/template"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gauravrautela/unified-messaging/internal/web/docs"
)

// getSite renders GET / and returns the body, failing the test if the page
// did not render.
func getSite(t *testing.T) string {
	t.Helper()
	s, _ := newTestServer(t)
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / = %d", rec.Code)
	}
	return rec.Body.String()
}

// TestSiteHasEverySection pins the marketing page's shape: the anchors the
// site nav links to must exist, the CTAs must point somewhere real, and the
// developer-facing proof points from the spec must actually be on the page.
func TestSiteHasEverySection(t *testing.T) {
	body := getSite(t)
	lower := strings.ToLower(body)
	for _, want := range []string{
		`id="hero"`, `id="how"`, `id="providers"`, `id="features"`, `id="events"`,
		`href="/signup"`, `href="/docs"`, `href="/llms.txt"`, `href="/login"`,
		"One API for mail and chat.",
		"curl", "Idempotency-Key", "llms.txt", "self-host",
		"Hosted auth", "Rotation", "Discord", "Telegram",
		"More providers coming",
		"Free while in beta",
	} {
		if !strings.Contains(lower, strings.ToLower(want)) {
			t.Fatalf("site missing %q", want)
		}
	}
	// The homepage title is deliberately bare: layout_head only appends
	// "· Entropix" when Shell.Title is set.
	if !strings.Contains(body, "<title>Entropix</title>") {
		t.Fatalf("site title is not a bare Entropix")
	}
}

// TestSiteHasNoPricingOrExternalAssets guards the spec's "no pricing, no
// analytics, no cookie banner, no external assets" rule. External assets are
// the load-bearing half: the CSP is default-src 'self', so a CDN link would
// break the page at runtime rather than merely violate the design. Absolute
// URLs inside the shared snippets are fine — they are sample payload text,
// not something the browser fetches — so this looks only at the attributes
// that actually load or link out.
func TestSiteHasNoPricingOrExternalAssets(t *testing.T) {
	body := getSite(t)
	lower := strings.ToLower(body)
	for _, forbidden := range []string{"pricing", "cookie", "analytics"} {
		if strings.Contains(lower, forbidden) {
			t.Fatalf("site must not contain %q", forbidden)
		}
	}
	for _, forbidden := range []string{`src="http`, `href="http`, `src='http`, `href='http`, "@import", "url(http"} {
		if strings.Contains(lower, forbidden) {
			t.Fatalf("site loads an external asset (%q); the CSP would block it", forbidden)
		}
	}
}

// TestSiteListsRegisteredProviders proves the providers section is built
// from the registry rather than hardcoded: newTestServer registers OUTLOOK
// and FAKECHAT, so the page must name both by their display names and carry
// the capabilities for each kind.
func TestSiteListsRegisteredProviders(t *testing.T) {
	body := getSite(t)
	for _, want := range []string{
		"Outlook", "Fakechat", // provider.DisplayName of OUTLOOK / FAKECHAT
		"Folders",   // mail-only capability
		"Reactions", // chat-only capability
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("providers section missing %q", want)
		}
	}
	// WhatsApp is named in the hero copy and in the hosted-auth snippet, so
	// the proof that the cards are registry-driven is the absence of a *card*
	// for a provider this registry never registered.
	if strings.Contains(body, `data-provider="WhatsApp"`) {
		t.Fatalf("providers section has a WhatsApp card, which this registry does not have")
	}
}

// TestSiteLinksEveryEventToItsDocsAnchor keeps the events chips in sync with
// the docs data they came from: a new event type must appear here for free.
func TestSiteLinksEveryEventToItsDocsAnchor(t *testing.T) {
	body := getSite(t)
	for _, ev := range docs.Events {
		if !strings.Contains(body, `href="/docs#event-`+ev.Type+`"`) {
			t.Fatalf("site missing chip linking event %q to its docs anchor", ev.Type)
		}
	}
}

// TestSiteHeroShowsTheSendSnippetInThreeLanguages checks the hero's tabbed
// code block renders the shared docs.SendMessage snippet, so the marketing
// page and the reference cannot drift apart.
func TestSiteHeroShowsTheSendSnippetInThreeLanguages(t *testing.T) {
	body := getSite(t)
	for _, want := range []string{`data-lang="curl"`, `data-lang="node"`, `data-lang="go"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("hero missing code pane %q", want)
		}
	}
	// A distinctive line from each pane of the shared snippet.
	for _, want := range []string{
		"/api/v1/chats/{chat_id}/messages",
		"crypto.randomUUID()",
		"http.NewRequest(http.MethodPost, url, bytes.NewReader(body))",
	} {
		if !strings.Contains(body, template.HTMLEscapeString(want)) &&
			!strings.Contains(body, want) {
			t.Fatalf("hero missing snippet line %q", want)
		}
	}
}
