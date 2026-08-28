package api

import (
	"net/http"
	"net/url"

	"github.com/gauravrautela/unified-messaging/internal/web"
)

// The mail viewer is the third human-facing screen: a read-only, three-pane
// client over the synced mirror. Like the dashboard it requires a browser
// session; the page's own fetches then ride the same cookie, so data stays
// gated exactly where the REST API already gates it. Its markup lives with
// the rest of the console in internal/web/templates, and the only
// server-rendered values are the shell's — the signed-in email and the CSRF
// token the layout's Sign out form has to carry.
func (s *Server) handleMailPage(w http.ResponseWriter, r *http.Request) {
	dev, ok := s.sessionDeveloper(w, r)
	if !ok {
		http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusFound)
		return
	}
	// Minted before anything is written; the layout's Sign out form carries it.
	csrf := s.csrfToken(w, r)
	s.renderPage(w, http.StatusOK, "mail", map[string]any{
		"Shell": web.Shell{Title: "Mail", Version: web.Version, Email: dev.Email, CSRF: csrf, Nav: "mail"},
	})
}
