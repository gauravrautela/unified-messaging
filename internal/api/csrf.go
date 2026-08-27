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

// csrfTokenLen is the length of a 32-byte value in unpadded base64url. A
// cookie of any other length is treated as absent and re-minted, so a
// truncated or malformed value cannot be submitted back as a "matching" pair.
//
// The length gate is hygiene, not a defence: an attacker who can *write* this
// cookie can write one of the right length and echo it in their own form
// (cookie tossing). What defeats that is SameSite=Strict — a cookie a
// same-site-only attacker cannot set for this host — together with the Origin
// check in checkFormCSRF.
const csrfTokenLen = 43

// The two ways a form post is refused. They are constants because the tests
// assert on the rendered page, and a wording change should not silently turn
// those assertions into tautologies.
const (
	csrfExpiredMessage   = "the form has expired — reload the page and try again"
	csrfCrossSiteMessage = "cross-site form submission rejected"
)

// csrfToken returns the caller's token, minting one if there is none usable,
// and re-issues the cookie either way. Call it while rendering any page that
// carries a form, before anything is written to w.
//
// The re-issue is what makes the 12-hour window slide: a dashboard left open
// all day keeps a fresh cookie as long as the tab is reloaded, instead of the
// Log out button going dead 12 hours after the token was first minted.
func (s *Server) csrfToken(w http.ResponseWriter, r *http.Request) string {
	tok := ""
	if c, err := r.Cookie(csrfCookie); err == nil && len(c.Value) == csrfTokenLen {
		tok = c.Value
	} else {
		tok = logx.RandomToken(32)
	}
	http.SetCookie(w, &http.Cookie{Name: csrfCookie, Value: tok, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteStrictMode, Secure: s.requestIsHTTPS(r), MaxAge: 12 * 3600})
	return tok
}

// csrfFailure reports why a form post must be refused, or "" when it passes.
// Double-submit token plus an Origin check: the token defeats cross-site form
// posts, the Origin check defeats a token obtained through any future
// subdomain or XSS foothold.
func (s *Server) csrfFailure(r *http.Request) string {
	log := logx.From(r.Context())
	c, err := r.Cookie(csrfCookie)
	field := r.PostFormValue("csrf")
	if err != nil || field == "" || subtle.ConstantTimeCompare([]byte(c.Value), []byte(field)) != 1 {
		log.Debug("csrf rejected", "reason", "missing or mismatched token",
			"has_cookie", err == nil, "has_field", field != "")
		return csrfExpiredMessage
	}
	// An opaque origin is treated as absent, not as a mismatch. This service
	// sends Referrer-Policy: no-referrer, and per the Fetch spec a non-GET
	// navigation under that policy carries `Origin: null` and no Referer — so
	// *every* genuine login, signup and logout from a browser arrives this
	// way. Refusing "null" would refuse all of them. In that case the
	// double-submit token, in a SameSite=Strict cookie an off-site page can
	// neither read nor set, is the control.
	if o := r.Header.Get("Origin"); o != "" && o != "null" {
		if u, err := url.Parse(o); err != nil || !strings.EqualFold(u.Host, r.Host) {
			log.Debug("csrf rejected", "reason", "cross-origin", "origin", o)
			return csrfCrossSiteMessage
		}
	} else if ref := r.Header.Get("Referer"); ref != "" {
		if u, err := url.Parse(ref); err != nil || !strings.EqualFold(u.Host, r.Host) {
			log.Debug("csrf rejected", "reason", "cross-origin referer")
			return csrfCrossSiteMessage
		}
	}
	return ""
}

// checkFormCSRF guards the three HTML form posts. On failure it renders the
// form again at 403 with the reason shown inline — these routes are reached
// by a person in a browser, so a JSON error body would be a dead end rather
// than something they can act on. page builds that form around the message.
func (s *Server) checkFormCSRF(w http.ResponseWriter, r *http.Request, page func(errMsg string) authPage) bool {
	msg := s.csrfFailure(r)
	if msg == "" {
		return true
	}
	s.renderAuth(w, r, http.StatusForbidden, page(msg))
	return false
}
