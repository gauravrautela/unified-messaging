package api

import (
	"html/template"
	"net/http"
	"net/url"

	"github.com/gauravrautela/unified-messaging/internal/model"
	"github.com/gauravrautela/unified-messaging/internal/web"
)

// This file holds the two screens a human being actually looks at.
//
// The landing page is end-user facing: it appears once, mid-OAuth, and has no
// login of its own — its only authority is the single-use state token already
// embedded in its URL. It is still self-contained here because it renders on
// its own, outside the signed-in shell. The dashboard is operator/developer
// facing: it requires a signed-in browser session (the um_session cookie), its
// markup lives with the rest of the console in internal/web/templates, and its
// own fetches ride that same cookie, so account data stays gated by the API
// middleware.
//
// Both are plain html/template + vanilla JS. No build step, no framework, no
// external assets — this ships inside the Go binary.

// ---------- landing page ----------

type landingData struct {
	Provider     string
	AuthorizeURL string
}

var landingTmpl = template.Must(template.New("landing").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Connect your {{.Provider}} account</title>
<style>
:root{--bg:#f7f7f8;--card:#fff;--text:#1a1a1a;--muted:#6b6b76;--border:#e6e6ea;--accent:#2563eb;--accent-text:#fff}
@media (prefers-color-scheme:dark){:root{--bg:#0f1115;--card:#171a21;--text:#f0f0f2;--muted:#9a9aa5;--border:#2a2d36;--accent:#3b82f6;--accent-text:#fff}}
*{box-sizing:border-box}
body{margin:0;min-height:100vh;display:flex;align-items:center;justify-content:center;
  background:var(--bg);color:var(--text);font:16px/1.5 -apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif;padding:1.5rem}
.card{background:var(--card);border:1px solid var(--border);border-radius:16px;padding:2.5rem 2rem;
  max-width:26rem;width:100%;text-align:center;box-shadow:0 1px 3px rgba(0,0,0,.06)}
.badge{width:56px;height:56px;border-radius:14px;background:var(--accent);color:var(--accent-text);
  display:flex;align-items:center;justify-content:center;margin:0 auto 1.25rem;font-size:1.5rem;font-weight:600}
h1{font-size:1.25rem;margin:0 0 .5rem}
p{color:var(--muted);margin:0 0 1.75rem;font-size:.95rem}
.btn{display:inline-block;width:100%;padding:.75rem 1rem;border-radius:10px;background:var(--accent);
  color:var(--accent-text);text-decoration:none;font-weight:600;font-size:.95rem}
.btn:hover{opacity:.92}
.fine{margin-top:1.25rem;font-size:.8rem;color:var(--muted)}
</style></head>
<body>
  <div class="card">
    <div class="badge">{{slice .Provider 0 1}}</div>
    <h1>Connect your {{.Provider}} account</h1>
    <p>You'll sign in on Microsoft's own page and choose what to share. We never see your password.</p>
    <a class="btn" href="{{.AuthorizeURL}}">Continue with {{.Provider}}</a>
    <p class="fine">You can disconnect this at any time.</p>
  </div>
</body></html>`))

func renderLanding(w http.ResponseWriter, d landingData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = landingTmpl.Execute(w, d)
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
