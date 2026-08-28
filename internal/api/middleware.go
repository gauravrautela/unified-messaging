package api

import (
	"net/http"
	"regexp"
	"strings"
)

var requestIDRe = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,64}$`)

const csp = "default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data:; connect-src 'self'; frame-src 'self'; frame-ancestors 'none'; form-action 'self'; base-uri 'self'"

// requestIsHTTPS is the single source of truth for "is this connection
// secure": direct TLS, a declared https public origin, or — only when the
// operator has said the app sits behind a proxy — X-Forwarded-Proto.
func (s *Server) requestIsHTTPS(r *http.Request) bool {
	if r.TLS != nil || strings.HasPrefix(s.cfg.PublicBaseURL, "https://") {
		return true
	}
	// Only the first token counts: a chain of proxies appends rather than
	// replaces, so two hops produce "https, http" and the client-facing hop —
	// the one that decides whether the *browser's* connection was secure — is
	// the first entry. Comparing the header whole made that read as plain HTTP
	// and quietly dropped both HSTS and the Secure cookie flag.
	proto, _, _ := strings.Cut(r.Header.Get("X-Forwarded-Proto"), ",")
	return s.cfg.TrustProxy && strings.EqualFold(strings.TrimSpace(proto), "https")
}

// noStorePrefixes are tenant- or credential-bearing surfaces a shared cache
// must never keep. /llms.txt and /healthz are deliberately absent: they are
// the same bytes for everyone.
//
// /docs and / are absent too, but for a narrower reason — they are cacheable
// only while nobody is signed in. Both render a different document once a
// session resolves (an email, a sign-out CSRF token, different CTAs), so
// those handlers call markSessionVaried themselves rather than being pinned
// no-store here for the anonymous readers who are most of their traffic.
var noStorePrefixes = []string{"/api/", "/dashboard", "/mail", "/chat", "/connect/", "/oauth/", "/login", "/signup", "/logout"}

// markSessionVaried marks a response that is public by default but became
// session-specific on this request. It must be called before any body is
// written — renderPage buffers, so calling it just before the render is fine.
//
// private (not just no-store) keeps a CDN or corporate proxy from holding one
// developer's page at all, and Vary: Cookie keeps the browser's own cache
// from replaying it after sign-out.
func markSessionVaried(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Add("Vary", "Cookie")
}

func (s *Server) secureHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", csp)
		if s.requestIsHTTPS(r) {
			h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		for _, p := range noStorePrefixes {
			if strings.HasPrefix(r.URL.Path, p) {
				h.Set("Cache-Control", "no-store")
				h.Add("Vary", "Cookie")
				h.Add("Vary", "Authorization")
				break
			}
		}
		next.ServeHTTP(w, r)
	})
}
