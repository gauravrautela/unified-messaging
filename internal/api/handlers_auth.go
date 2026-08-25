package api

import (
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gauravrautela/unified-messaging/internal/auth"
	"github.com/gauravrautela/unified-messaging/internal/logx"
	"github.com/gauravrautela/unified-messaging/internal/model"
	"github.com/gauravrautela/unified-messaging/internal/store"
)

// ---- cookies ----

func (s *Server) secureCookies(r *http.Request) bool {
	return r.TLS != nil || strings.HasPrefix(s.cfg.PublicBaseURL, "https://")
}

func (s *Server) setSessionCookie(w http.ResponseWriter, r *http.Request, token string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: token, Path: "/", Expires: expires,
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: s.secureCookies(r),
	})
}

func (s *Server) clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: s.secureCookies(r),
	})
}

// sessionDeveloper resolves the browser session for page handlers, which
// sit outside the /api/v1 middleware.
//
// It takes w because resolving a session may slide its expiry, and the new
// expiry only reaches the browser if the cookie is re-issued. Re-setting it on
// every successful resolution is a single header and always correct, which
// beats comparing against whatever the old cookie happened to carry.
func (s *Server) sessionDeveloper(w http.ResponseWriter, r *http.Request) (model.Developer, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return model.Developer{}, false
	}
	d, exp, err := s.auth.SessionDeveloper(r.Context(), c.Value)
	if err != nil {
		return model.Developer{}, false
	}
	s.setSessionCookie(w, r, c.Value, exp)
	return d, true
}

// safeNext keeps ?next= on this origin: a path starting with a single "/".
//
// Both "//" and "/\" are rejected as second characters. Browsers normalise a
// backslash in the authority position to a forward slash, so "/\evil.com"
// navigates to evil.com exactly as "//evil.com" would — it only looks like a
// relative path.
func safeNext(raw string) string {
	if strings.HasPrefix(raw, "/") && !strings.HasPrefix(raw, "//") && !strings.HasPrefix(raw, `/\`) {
		return raw
	}
	return "/dashboard"
}

// ---- pages ----

var authTmpl = template.Must(template.New("auth").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}}</title>
<style>
:root{--bg:#f7f7f8;--card:#fff;--text:#1a1a1a;--muted:#6b6b76;--border:#e6e6ea;--accent:#2563eb;--accent-text:#fff;--danger:#dc2626;--danger-bg:#fef2f2}
@media (prefers-color-scheme:dark){:root{--bg:#0f1115;--card:#171a21;--text:#f0f0f2;--muted:#9a9aa5;--border:#2a2d36;--danger-bg:#2a1414}}
body{margin:0;font-family:system-ui,sans-serif;background:var(--bg);color:var(--text)}
.card{max-width:24rem;margin:4rem auto;background:var(--card);border:1px solid var(--border);border-radius:16px;padding:2rem}
h1{font-size:1.35rem;margin:0 0 .25rem}
p{color:var(--muted);font-size:.9rem;margin:.25rem 0 1rem}
form{display:flex;flex-direction:column;gap:.75rem}
input{font:inherit;padding:.6rem .75rem;border:1px solid var(--border);border-radius:8px;background:var(--bg);color:var(--text)}
button{font:inherit;cursor:pointer;padding:.6rem .9rem;border-radius:8px;border:1px solid var(--accent);background:var(--accent);color:var(--accent-text)}
.err{color:var(--danger);background:var(--danger-bg);border-radius:8px;padding:.6rem .8rem;font-size:.85rem}
a{color:var(--accent)}
</style></head><body>
<div class="card">
  <h1>{{.Title}}</h1>
  <p>{{.Lead}}</p>
  {{if .Error}}<p class="err">{{.Error}}</p>{{end}}
  <form method="post" action="{{.Action}}">
    <input name="email" type="email" placeholder="Email" value="{{.Email}}" required autofocus>
    <input name="password" type="password" placeholder="Password{{if .Signup}} (10+ characters){{end}}" required minlength="{{if .Signup}}10{{else}}1{{end}}">
    {{if .Signup}}<input name="name" type="text" placeholder="Your name (optional)">{{end}}
    <button type="submit">{{.Button}}</button>
  </form>
  <p>{{.AltLead}} <a href="{{.AltHref}}">{{.AltText}}</a></p>
</div>
</body></html>`))

type authPage struct {
	Title, Lead, Action, Button, AltLead, AltHref, AltText, Email, Error string
	Signup                                                               bool
}

func loginPage(next, email, errMsg string) authPage {
	action := "/login"
	if next != "" {
		action += "?next=" + url.QueryEscape(next)
	}
	return authPage{Title: "Sign in", Lead: "Sign in to manage connected accounts and API keys.",
		Action: action, Button: "Sign in", AltLead: "New here?", AltHref: "/signup", AltText: "Create an account",
		Email: email, Error: errMsg}
}

func signupPage(email, errMsg string) authPage {
	return authPage{Title: "Create your account", Lead: "One account per developer. You will get API keys next.",
		Action: "/signup", Button: "Create account", AltLead: "Already have one?", AltHref: "/login", AltText: "Sign in",
		Email: email, Error: errMsg, Signup: true}
}

func renderAuth(w http.ResponseWriter, status int, p authPage) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_ = authTmpl.Execute(w, p)
}

func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.sessionDeveloper(w, r); ok {
		http.Redirect(w, r, safeNext(r.URL.Query().Get("next")), http.StatusFound)
		return
	}
	renderAuth(w, http.StatusOK, loginPage(r.URL.Query().Get("next"), "", ""))
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	log := logx.From(r.Context())
	if err := r.ParseForm(); err != nil {
		renderAuth(w, http.StatusBadRequest, loginPage("", "", "bad form"))
		return
	}
	email := r.PostForm.Get("email")
	next := r.URL.Query().Get("next")
	log.Debug("login attempt", "email", strings.ToLower(strings.TrimSpace(email)), "next", next)
	dev, err := s.auth.Login(r.Context(), email, r.PostForm.Get("password"))
	if err != nil {
		if errors.Is(err, auth.ErrInvalidCredentials) {
			renderAuth(w, http.StatusUnauthorized, loginPage(next, email, err.Error()))
			return
		}
		log.Error("login", "err", err)
		renderAuth(w, http.StatusInternalServerError, loginPage(next, email, "something went wrong"))
		return
	}
	s.startSession(w, r, dev, safeNext(next))
}

func (s *Server) handleSignupPage(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.sessionDeveloper(w, r); ok {
		http.Redirect(w, r, "/dashboard", http.StatusFound)
		return
	}
	renderAuth(w, http.StatusOK, signupPage("", ""))
}

func (s *Server) handleSignup(w http.ResponseWriter, r *http.Request) {
	log := logx.From(r.Context())
	if err := r.ParseForm(); err != nil {
		renderAuth(w, http.StatusBadRequest, signupPage("", "bad form"))
		return
	}
	email := r.PostForm.Get("email")
	log.Debug("signup attempt", "email", strings.ToLower(strings.TrimSpace(email)))
	dev, err := s.auth.Signup(r.Context(), email, r.PostForm.Get("password"), r.PostForm.Get("name"))
	switch {
	case errors.Is(err, auth.ErrInvalidInput):
		renderAuth(w, http.StatusBadRequest, signupPage(email, "invalid email or password (10+ characters)"))
		return
	case errors.Is(err, auth.ErrEmailTaken):
		renderAuth(w, http.StatusConflict, signupPage(email, "could not create account — try signing in"))
		return
	case err != nil:
		log.Error("signup", "err", err)
		renderAuth(w, http.StatusInternalServerError, signupPage(email, "something went wrong"))
		return
	}
	s.startSession(w, r, dev, "/dashboard")
}

func (s *Server) startSession(w http.ResponseWriter, r *http.Request, dev model.Developer, next string) {
	tok, exp, err := s.auth.NewSession(r.Context(), dev.ID)
	if err != nil {
		logx.From(r.Context()).Error("creating session", "developer_id", dev.ID, "err", err)
		renderAuth(w, http.StatusInternalServerError, loginPage("", dev.Email, "something went wrong"))
		return
	}
	s.setSessionCookie(w, r, tok, exp)
	logx.From(r.Context()).Info("session started", "developer_id", dev.ID, "redirect", next)
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		_ = s.auth.DeleteSession(r.Context(), c.Value)
	}
	s.clearSessionCookie(w, r)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// ---- /api/v1/me and API keys ----

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	dev, _ := developerFrom(r.Context())
	writeJSON(w, http.StatusOK, struct {
		model.Developer
		Auth string `json:"auth"`
	}{dev, authKindFrom(r.Context())})
}

// requireSession is for actions that must not be reachable with an API key
// alone, so a leaked key cannot mint or revoke keys.
func (s *Server) requireSession(w http.ResponseWriter, r *http.Request) bool {
	if authKindFrom(r.Context()) != authKindSession {
		logx.From(r.Context()).Debug("session-only endpoint refused api key")
		writeError(w, http.StatusForbidden, "session_required", "sign in to the dashboard to manage API keys")
		return false
	}
	return true
}

func (s *Server) handleListAPIKeys(w http.ResponseWriter, r *http.Request) {
	dev, _ := developerFrom(r.Context())
	keys, err := s.store.ListAPIKeys(dev.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, listResponse[model.APIKey]{Items: keys})
}

type createKeyRequest struct {
	Name string `json:"name"`
}

type createKeyResponse struct {
	model.APIKey
	Key string `json:"key"`
}

func (s *Server) handleCreateAPIKey(w http.ResponseWriter, r *http.Request) {
	if !s.requireSession(w, r) {
		return
	}
	dev, _ := developerFrom(r.Context())
	var req createKeyRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	full, k, err := s.auth.NewAPIKey(r.Context(), dev.ID, req.Name)
	if errors.Is(err, auth.ErrInvalidInput) {
		writeError(w, http.StatusBadRequest, "missing_name", "name is required")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	// The only time the full key is ever returned.
	writeJSON(w, http.StatusCreated, createKeyResponse{APIKey: k, Key: full})
}

func (s *Server) handleRevokeAPIKey(w http.ResponseWriter, r *http.Request) {
	if !s.requireSession(w, r) {
		return
	}
	dev, _ := developerFrom(r.Context())
	err := s.auth.RevokeKey(r.Context(), dev.ID, r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "no such key")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
