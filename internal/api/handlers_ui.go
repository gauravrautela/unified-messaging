package api

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/gauravrautela/unified-messaging/internal/model"
	"github.com/gauravrautela/unified-messaging/internal/web"
)

// This file holds the two screens a human being actually looks at.
//
// The landing page is end-user facing: it appears once, mid-OAuth, and has no
// login of its own — its only authority is the single-use state token already
// embedded in its URL. It renders through the public layout (no nav, no
// session), so an end user of somebody else's app sees a page that belongs to
// the same product as the rest of it. The dashboard is operator/developer
// facing: it requires a signed-in browser session (the um_session cookie), its
// markup lives with the rest of the console in internal/web/templates, and its
// own fetches ride that same cookie, so account data stays gated by the API
// middleware.
//
// Both are plain html/template + vanilla JS. No build step, no framework, no
// external assets — this ships inside the Go binary.

// ---------- hosted-auth landing page ----------

// scopeText turns a provider's scope vocabulary into a sentence the person
// clicking Continue can actually act on. A scope string is an identifier the
// provider chose for its own API surface, not copy: "Mail.ReadWrite" tells an
// end user nothing about what an app is asking to do to their mailbox.
//
// It is keyed on the bare scope name, so a fully-qualified scope
// ("https://graph.microsoft.com/Mail.Read") resolves through the same entry
// as its short form — see scopeSentences.
var scopeText = map[string]string{
	"Mail.Read":      "Read your mail",
	"Mail.ReadWrite": "Read, move and mark your mail",
	"Mail.Send":      "Send mail as you",
	"User.Read":      "See your name and email address",
	"openid":         "See your name and email address",
	"profile":        "See your name and email address",
	"email":          "See your name and email address",
	"offline_access": "Stay connected without asking again",
}

// scopeSentences renders scopes for the landing page: known scopes as
// sentences, unknown ones verbatim rather than silently dropped — an end user
// consenting to something we have no copy for should still see that it is
// being asked for. Duplicates collapse, because several scopes routinely map
// to the same sentence (openid/profile/User.Read all mean "who you are").
func scopeSentences(scopes []string) []string {
	out := make([]string, 0, len(scopes))
	seen := make(map[string]bool, len(scopes))
	for _, raw := range scopes {
		name := raw
		if i := strings.LastIndex(name, "/"); i >= 0 {
			name = name[i+1:]
		}
		text, ok := scopeText[name]
		if !ok {
			text = raw
		}
		if seen[text] {
			continue
		}
		seen[text] = true
		out = append(out, text)
	}
	return out
}

// renderConnectOAuth is the branded confirmation screen an end user sees
// before being handed to the provider's own consent page. cancel is the
// developer's failure redirect when they supplied one; with no such URL there
// is nowhere honest to send someone who changed their mind, so the page says
// so instead of offering a button that goes nowhere.
func (s *Server) renderConnectOAuth(w http.ResponseWriter, display, authorizeURL, cancel string) {
	s.renderPage(w, "connect_oauth", map[string]any{
		"Shell":        web.Shell{Title: "Connect " + display, Version: web.Version},
		"Provider":     display,
		"AuthorizeURL": authorizeURL,
		"Scopes":       scopeSentences(s.cfg.Scopes),
		"CancelURL":    cancel,
	})
}

// ---------- dashboard ----------

// webhookEvents is every event a hook can subscribe to, in the order the
// dashboard's picker lists them: mail first, then chat, then the lifecycle
// event. It is rendered into the page from these constants rather than
// hard-coded in JavaScript, so adding an event name in internal/model is all
// it takes for the picker to offer it.
var webhookEvents = []string{
	model.EventMailReceived, model.EventMailSent, model.EventMailUpdated, model.EventMailDeleted,
	model.EventChatReceived, model.EventChatSent, model.EventChatUpdated, model.EventChatReaction, model.EventChatDeleted,
	model.EventAccountError,
}

// handleDashboard requires a browser session. The page's own fetches then
// ride the same cookie, so account data stays gated by the API middleware.
func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	dev, ok := s.sessionDeveloper(w, r)
	if !ok {
		http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusFound)
		return
	}
	// The token has to be minted before anything is written, and the layout's
	// logout form carries it as a hidden field.
	csrf := s.csrfToken(w, r)
	s.renderPage(w, "dashboard", map[string]any{
		"Shell":  web.Shell{Title: "Dashboard", Version: web.Version, Email: dev.Email, CSRF: csrf, Nav: "dashboard"},
		"Events": webhookEvents,
	})
}
