package api

import (
	"net/http"

	"github.com/gauravrautela/unified-messaging/internal/web"
	"github.com/gauravrautela/unified-messaging/internal/web/docs"
)

// handleDocs serves the developer reference. It is public on purpose: an
// integrator reads it before they have an account, and nothing on it is
// secret.
//
// Everything on the page is rendered from internal/web/docs — one Go value
// per endpoint, event and error code — rather than written as prose in the
// template. That is what keeps it honest: TestDocsDataCoversApiRoutes fails
// the build if a route is registered without being documented, and
// TestDocsListsEveryRouteWithAnchors fails it if the page stops rendering a
// linkable block for one.
//
// It renders for a signed-in developer and an anonymous reader alike; the
// only difference is whether the shell shows an account and a working sign-out
// form, which is why the session is resolved but never required.
func (s *Server) handleDocs(w http.ResponseWriter, r *http.Request) {
	email, csrf := "", ""
	if dev, ok := s.sessionDeveloper(w, r); ok {
		email, csrf = dev.Email, s.csrfToken(w, r)
		// Signed in, the page is no longer the same document for everyone:
		// it carries this developer's email and a live CSRF token. Neither
		// may be parked in a shared cache and handed to the next reader, and
		// the response varies by the session cookie.
		markSessionVaried(w)
	}
	s.renderPage(w, http.StatusOK, "docs", map[string]any{
		"Shell":  web.Shell{Title: "API reference", Version: web.Version, Email: email, CSRF: csrf, Nav: "docs", Styles: []string{"docs.css"}},
		"Groups": docs.Grouped(),
		"Events": docs.Events,
		"Errors": docs.Errors,
		"Snippets": map[string]docs.Snippet{
			"send": docs.SendMessage, "hosted": docs.HostedAuth,
			"hook": docs.WebhookPayload, "key": docs.CreateKey,
		},
		// The three delivery formats shown side by side under #events: the
		// JSON event a kind="webhook" hook receives, and the rendered
		// notification the other two kinds get instead.
		"Formats": map[string]any{
			"Discord": docs.DiscordSample, "Telegram": docs.TelegramSample, "Notes": docs.KindNotes,
		},
		"Base": s.baseURL(r),
	})
}
