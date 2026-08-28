package api

import (
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/gauravrautela/unified-messaging/internal/auth"
	"github.com/gauravrautela/unified-messaging/internal/logx"
	"github.com/gauravrautela/unified-messaging/internal/model"
	"github.com/gauravrautela/unified-messaging/internal/store"
)

// ---- cookies ----

// hostCookiePrefix pins a cookie to exactly this host. A browser only accepts
// a cookie whose name starts with it when the cookie is Secure, has Path=/
// and carries no Domain attribute — which is precisely what a sibling
// subdomain (foo.example.com, holding a foothold) cannot arrange for
// example.com. Without it, nothing stops such a sibling from writing our
// session, CSRF or link cookie for the parent domain, and SameSite=Strict is
// no help because a request from a sibling *is* same-site.
//
// It only works over HTTPS, by design: over plain http a browser drops the
// cookie entirely rather than downgrading, so cookieName falls back to the
// bare name and a local http://localhost run keeps working.
const hostCookiePrefix = "__Host-"

// cookieName is the name to write for one of this service's browser cookies
// on this request. Always pair it with readCookie, which accepts either form:
// a browser that got a bare cookie over http keeps sending it under that name
// after a deployment moves behind TLS, and a request must not be treated as
// unauthenticated just because the name it carries is the older one.
func (s *Server) cookieName(r *http.Request, base string) string {
	if s.requestIsHTTPS(r) {
		return hostCookiePrefix + base
	}
	return base
}

// readCookie finds one of this service's browser cookies under its pinned
// name first, then its bare name. Prefixed wins: it is the one a sibling
// origin provably could not have written, so where a browser somehow sends
// both, the trustworthy one is the one that counts.
func readCookie(r *http.Request, base string) (*http.Cookie, error) {
	if c, err := r.Cookie(hostCookiePrefix + base); err == nil {
		return c, nil
	}
	return r.Cookie(base)
}

func (s *Server) secureCookies(r *http.Request) bool {
	return s.requestIsHTTPS(r)
}

func (s *Server) setSessionCookie(w http.ResponseWriter, r *http.Request, token string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name: s.cookieName(r, sessionCookie), Value: token, Path: "/", Expires: expires,
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: s.secureCookies(r),
	})
}

// clearSessionCookie expires both spellings. A browser holding the bare name
// from before the deployment moved behind TLS must still be logged out by a
// logout served over TLS, and vice versa.
func (s *Server) clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	names := []string{sessionCookie}
	if s.requestIsHTTPS(r) {
		names = append(names, hostCookiePrefix+sessionCookie)
	}
	for _, name := range names {
		http.SetCookie(w, &http.Cookie{
			Name: name, Value: "", Path: "/", MaxAge: -1,
			HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: s.secureCookies(r),
		})
	}
}

// sessionDeveloper resolves the browser session for page handlers, which
// sit outside the /api/v1 middleware.
//
// It takes w because resolving a session may slide its expiry, and the new
// expiry only reaches the browser if the cookie is re-issued. Re-setting it on
// every successful resolution is a single header and always correct, which
// beats comparing against whatever the old cookie happened to carry.
func (s *Server) sessionDeveloper(w http.ResponseWriter, r *http.Request) (model.Developer, bool) {
	c, err := readCookie(r, sessionCookie)
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
    <input type="hidden" name="csrf" value="{{.CSRF}}">
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
	// CSRF is filled in by renderAuth, never by the page constructors, so no
	// path can render a form without it.
	CSRF   string
	Signup bool
}

// uniformSignupError is the only thing a failed signup ever says, whatever
// the real reason: see handleSignup.
const uniformSignupError = "could not create the account — check the details or sign in"

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

// renderAuth is a method so that every rendering of a form — the happy path
// and all six error paths — mints or refreshes the CSRF cookie and embeds the
// matching field. A page rendered without one would 403 on submit.
func (s *Server) renderAuth(w http.ResponseWriter, r *http.Request, status int, p authPage) {
	p.CSRF = s.csrfToken(w, r)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_ = authTmpl.Execute(w, p)
}

func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.sessionDeveloper(w, r); ok {
		http.Redirect(w, r, safeNext(r.URL.Query().Get("next")), http.StatusFound)
		return
	}
	s.renderAuth(w, r, http.StatusOK, loginPage(r.URL.Query().Get("next"), "", ""))
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	log := logx.From(r.Context())
	if err := r.ParseForm(); err != nil {
		s.renderAuth(w, r, http.StatusBadRequest, loginPage("", "", "bad form"))
		return
	}
	email := r.PostForm.Get("email")
	next := r.URL.Query().Get("next")
	if !s.checkFormCSRF(w, r, func(msg string) authPage { return loginPage(next, email, msg) }) {
		return
	}
	log.Debug("login attempt", "email_digest", logx.Digest(strings.ToLower(strings.TrimSpace(email))), "next", next)
	dev, err := s.auth.Login(r.Context(), email, r.PostForm.Get("password"))
	if err != nil {
		if errors.Is(err, auth.ErrInvalidCredentials) {
			s.renderAuth(w, r, http.StatusUnauthorized, loginPage(next, email, err.Error()))
			return
		}
		log.Error("login", "err", err)
		s.renderAuth(w, r, http.StatusInternalServerError, loginPage(next, email, "something went wrong"))
		return
	}
	s.startSession(w, r, dev, safeNext(next))
}

func (s *Server) handleSignupPage(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.sessionDeveloper(w, r); ok {
		http.Redirect(w, r, "/dashboard", http.StatusFound)
		return
	}
	s.renderAuth(w, r, http.StatusOK, signupPage("", ""))
}

func (s *Server) handleSignup(w http.ResponseWriter, r *http.Request) {
	log := logx.From(r.Context())
	if err := r.ParseForm(); err != nil {
		s.renderAuth(w, r, http.StatusBadRequest, signupPage("", "bad form"))
		return
	}
	email := r.PostForm.Get("email")
	if !s.checkFormCSRF(w, r, func(msg string) authPage { return signupPage(email, msg) }) {
		return
	}
	digest := logx.Digest(strings.ToLower(strings.TrimSpace(email)))
	log.Debug("signup attempt", "email_digest", digest)
	dev, err := s.auth.Signup(r.Context(), email, r.PostForm.Get("password"), r.PostForm.Get("name"))
	switch {
	// A taken email and a malformed one fail identically, in status and in
	// wording: anything else turns the signup form into an oracle for "does
	// this address already have an account here". The real reason is kept for
	// the operator, against a digest of the address rather than the address.
	case errors.Is(err, auth.ErrInvalidInput), errors.Is(err, auth.ErrEmailTaken):
		log.Debug("signup rejected", "reason", err.Error(), "email_digest", digest)
		s.renderAuth(w, r, http.StatusBadRequest, signupPage(email, uniformSignupError))
		return
	case err != nil:
		log.Error("signup", "err", err)
		s.renderAuth(w, r, http.StatusInternalServerError, signupPage(email, "something went wrong"))
		return
	}
	s.startSession(w, r, dev, "/dashboard")
}

func (s *Server) startSession(w http.ResponseWriter, r *http.Request, dev model.Developer, next string) {
	tok, exp, err := s.auth.NewSession(r.Context(), dev.ID)
	if err != nil {
		logx.From(r.Context()).Error("creating session", "developer_id", dev.ID, "err", err)
		s.renderAuth(w, r, http.StatusInternalServerError, loginPage("", dev.Email, "something went wrong"))
		return
	}
	s.setSessionCookie(w, r, tok, exp)
	logx.From(r.Context()).Info("session started", "developer_id", dev.ID, "redirect", next)
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	// Logging someone out is state-changing and forgeable from any page, so
	// it needs the same token as the sign-in forms. A refusal renders the
	// sign-in page: whoever is here was on their way out anyway.
	if !s.checkFormCSRF(w, r, func(msg string) authPage { return loginPage("", "", msg) }) {
		return
	}
	if c, err := readCookie(r, sessionCookie); err == nil {
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
		writeError(w, http.StatusForbidden, "session_required", "sign in to the dashboard to manage your account")
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
		writeDecodeError(w, err)
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

// ---- password ----

type changePasswordRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

// handleChangePassword is session-only: an API key must not be able to take
// over the account that issued it. Changing the password also signs every
// other browser out, so it is a real remedy for a session somebody else is
// holding — which is the whole reason to offer it.
func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	if !s.requireSession(w, r) {
		return
	}
	dev, _ := developerFrom(r.Context())
	var req changePasswordRequest
	if err := decodeJSON(r, &req); err != nil {
		writeDecodeError(w, err)
		return
	}
	err := s.auth.ChangePassword(r.Context(), dev.ID, req.CurrentPassword, req.NewPassword)
	switch {
	case errors.Is(err, auth.ErrWeakPassword):
		writeError(w, http.StatusBadRequest, "invalid_body", "the new password must be at least 10 characters")
		return
	case errors.Is(err, auth.ErrInvalidCredentials):
		writeError(w, http.StatusBadRequest, "invalid_credentials", "current password is incorrect")
		return
	case err != nil:
		logx.From(r.Context()).Error("changing password", "developer_id", dev.ID, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not change the password")
		return
	}
	// The cookie on this request identifies the one browser that stays signed
	// in; every other session dies with the old password.
	keep := ""
	if c, cookieErr := readCookie(r, sessionCookie); cookieErr == nil {
		keep = c.Value
	}
	if err := s.auth.DeleteOtherSessions(r.Context(), dev.ID, keep); err != nil {
		// The password did change, but "signed out everywhere else" is half
		// of what this endpoint promises, so a failure here is not a success.
		logx.From(r.Context()).Error("signing out other sessions", "developer_id", dev.ID, "err", err)
		writeError(w, http.StatusInternalServerError, "internal",
			"the password was changed, but other sessions could not be signed out")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- redirect-domain allowlist ----

type setRedirectDomainsRequest struct {
	Domains []string `json:"domains"`
}

// redirectDomainLabelRe is the shape of one dot-separated label of a redirect
// domain entry, after an optional leading "*." wildcard is stripped.
var redirectDomainLabelRe = regexp.MustCompile(`^[a-z0-9-]+$`)

// maxRedirectDomains bounds how large one developer's allowlist can grow, so
// the hosted-auth check and the dashboard textarea both stay cheap to render.
const maxRedirectDomains = 20

// normaliseRedirectDomains lower-cases, validates the shape of, and dedupes a
// caller-supplied redirect-domain allowlist. Each entry must be a bare host —
// no scheme, no path, no port — optionally prefixed with "*." to cover every
// subdomain; an IP literal is rejected, since a "domain" allowlist entry that
// is actually an address does not mean what a developer likely intends.
func normaliseRedirectDomains(raw []string) ([]string, error) {
	if len(raw) > maxRedirectDomains {
		return nil, fmt.Errorf("at most %d redirect domains are allowed", maxRedirectDomains)
	}
	seen := make(map[string]bool, len(raw))
	out := make([]string, 0, len(raw))
	for _, d := range raw {
		d = strings.ToLower(strings.TrimSpace(d))
		host := strings.TrimPrefix(d, "*.")
		if host == "" {
			return nil, fmt.Errorf("%q is not a valid redirect domain", d)
		}
		if _, err := netip.ParseAddr(host); err == nil {
			return nil, fmt.Errorf("%q must not be an IP address", d)
		}
		labels := strings.Split(host, ".")
		if len(labels) < 2 {
			return nil, fmt.Errorf("%q must have at least two labels", d)
		}
		for _, l := range labels {
			if !redirectDomainLabelRe.MatchString(l) {
				return nil, fmt.Errorf("%q is not a valid redirect domain", d)
			}
		}
		if seen[d] {
			continue
		}
		seen[d] = true
		out = append(out, d)
	}
	return out, nil
}

// handleSetRedirectDomains is session-only, like the other account-settings
// mutations: an API key must not be able to widen its own developer's
// hosted-auth redirect allowlist.
func (s *Server) handleSetRedirectDomains(w http.ResponseWriter, r *http.Request) {
	if !s.requireSession(w, r) {
		return
	}
	dev, _ := developerFrom(r.Context())
	var req setRedirectDomainsRequest
	if err := decodeJSON(r, &req); err != nil {
		writeDecodeError(w, err)
		return
	}
	domains, err := normaliseRedirectDomains(req.Domains)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	if err := s.store.SetRedirectDomains(dev.ID, domains); err != nil {
		logx.From(r.Context()).Error("setting redirect domains", "developer_id", dev.ID, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not save the redirect domains")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"redirect_domains": domains})
}
