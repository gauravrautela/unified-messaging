package api

import (
	"crypto/subtle"
	"net/http"
	"net/url"
	"strings"

	"github.com/gauravrautela/unified-messaging/internal/logx"
)

// The HTML forms in this service — sign in, sign up, log out — are the one
// surface SameSite=Lax does not protect: a browser attaches the session
// cookie to a cross-site form POST it navigates to, and a plain form cannot
// be made to send a JSON content type the way the /api/v1 middleware
// demands. Left open, an attacker's page can log a visitor out, or (worse)
// log them *in* to the attacker's own account so their subsequent work lands
// in a mailbox the attacker controls.
//
// The defence is a double-submit token: a random value in a cookie the
// attacker's origin cannot read, echoed in a hidden field their form
// therefore cannot fill in.

const csrfCookie = "um_csrf"

// csrfTokenLen is the length of a 32-byte value in unpadded base64url. The
// cookie is only trusted at this exact length, so a truncated or planted
// value is re-minted rather than used.
const csrfTokenLen = 43

// csrfToken returns the caller's existing token, or mints one and sets the
// cookie. Call it while rendering any page that carries a form, before
// anything is written to w.
func (s *Server) csrfToken(w http.ResponseWriter, r *http.Request) string {
	if c, err := r.Cookie(csrfCookie); err == nil && len(c.Value) == csrfTokenLen {
		return c.Value
	}
	tok := logx.RandomToken(32)
	http.SetCookie(w, &http.Cookie{Name: csrfCookie, Value: tok, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteStrictMode, Secure: s.requestIsHTTPS(r), MaxAge: 12 * 3600})
	return tok
}

// checkCSRF guards the form posts. Double-submit token plus an Origin check:
// the token defeats cross-site form posts, the Origin check defeats a token
// obtained through any future subdomain or XSS foothold.
//
// A request carrying neither Origin nor Referer passes on the token alone —
// that is the non-browser case (curl, a test), which was never subject to
// the ambient-cookie problem this exists to solve.
func (s *Server) checkCSRF(w http.ResponseWriter, r *http.Request) bool {
	log := logx.From(r.Context())
	c, err := r.Cookie(csrfCookie)
	field := r.PostFormValue("csrf")
	if err != nil || field == "" || subtle.ConstantTimeCompare([]byte(c.Value), []byte(field)) != 1 {
		log.Debug("csrf rejected", "reason", "missing or mismatched token",
			"has_cookie", err == nil, "has_field", field != "")
		writeError(w, http.StatusForbidden, "csrf", "the form has expired — reload the page and try again")
		return false
	}
	if o := r.Header.Get("Origin"); o != "" {
		if u, err := url.Parse(o); err != nil || !strings.EqualFold(u.Host, r.Host) {
			log.Debug("csrf rejected", "reason", "cross-origin", "origin", o)
			writeError(w, http.StatusForbidden, "csrf", "cross-site form submission rejected")
			return false
		}
	} else if ref := r.Header.Get("Referer"); ref != "" {
		if u, err := url.Parse(ref); err != nil || !strings.EqualFold(u.Host, r.Host) {
			log.Debug("csrf rejected", "reason", "cross-origin referer")
			writeError(w, http.StatusForbidden, "csrf", "cross-site form submission rejected")
			return false
		}
	}
	return true
}
