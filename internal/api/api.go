// Package api exposes the provider-neutral REST surface plus the two inbound
// endpoints Microsoft Graph itself calls.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gauravrautela/unified-messaging/internal/accounts"
	"github.com/gauravrautela/unified-messaging/internal/auth"
	"github.com/gauravrautela/unified-messaging/internal/chatsync"
	"github.com/gauravrautela/unified-messaging/internal/config"
	"github.com/gauravrautela/unified-messaging/internal/events"
	"github.com/gauravrautela/unified-messaging/internal/logx"
	"github.com/gauravrautela/unified-messaging/internal/model"
	"github.com/gauravrautela/unified-messaging/internal/notify"
	"github.com/gauravrautela/unified-messaging/internal/provider"
	"github.com/gauravrautela/unified-messaging/internal/store"
	"github.com/gauravrautela/unified-messaging/internal/syncer"
)

type Server struct {
	cfg        *config.Config
	store      *store.Store
	registry   *provider.Registry
	accts      *accounts.Manager
	syncer     *syncer.Syncer
	auth       *auth.Service
	chat       *chatsync.Runtime
	dispatcher *events.Dispatcher
	senders    *notify.Registry
	log        *slog.Logger

	// mirrorMiss negatively caches a mailbox miss (provider.ErrNotFound on
	// GetMessage) so a caller hammering a nonexistent or not-yet-synced
	// message id does not turn into one unbounded upstream call per request.
	mirrorMiss missCache

	// links tracks in-flight QR pairing attempts, keyed by connect state.
	links *linkRegistry

	// notifyTransport, when set, replaces the HTTP POST notify() would
	// otherwise make. Tests use it to observe notify_url payloads without a
	// real listener; production leaves it nil.
	notifyTransport func(url string, payload map[string]any)

	// fakeChat is wired only by the test harness, so it can drive a scripted
	// chat provider's link sessions directly. Always nil in production. Typed
	// as any (rather than *providertest.FakeChat) so the test-only
	// providertest package never enters the production import graph; the test
	// harness recovers the concrete type through (*Server).fake() in
	// api_test.go.
	fakeChat any
}

func NewServer(cfg *config.Config, s *store.Store, reg *provider.Registry, a *accounts.Manager,
	sy *syncer.Syncer, au *auth.Service, chat *chatsync.Runtime, dispatcher *events.Dispatcher,
	senders *notify.Registry, log *slog.Logger) *Server {
	if senders == nil {
		senders = notify.NewRegistry(nil)
	}
	srv := &Server{
		cfg: cfg, store: s, registry: reg, accts: a, syncer: sy, auth: au,
		chat: chat, dispatcher: dispatcher, senders: senders, log: log, links: newLinkRegistry(),
	}
	go srv.sweepLinks()
	return srv
}

const sessionCookie = "um_session"

// apiRoutes is every pattern registered under the developer middleware. It
// is a package-level list so the isolation test can prove each one is
// tenant-scoped.
var apiRoutes = []string{
	"POST /api/v1/hosted-auth",
	"GET /api/v1/me",
	"POST /api/v1/me/password",
	"PUT /api/v1/me/redirect-domains",
	"GET /api/v1/api-keys",
	"POST /api/v1/api-keys",
	"DELETE /api/v1/api-keys/{id}",
	"GET /api/v1/providers",
	"GET /api/v1/accounts",
	"GET /api/v1/accounts/{id}",
	"DELETE /api/v1/accounts/{id}",
	"POST /api/v1/accounts/{id}/resync",
	"POST /api/v1/accounts/{id}/reconnect",
	"GET /api/v1/accounts/{id}/webhooks",
	"POST /api/v1/accounts/{id}/webhooks",
	"DELETE /api/v1/accounts/{id}/webhooks/{wid}",
	"GET /api/v1/folders",
	"GET /api/v1/threads",
	"GET /api/v1/emails",
	"POST /api/v1/emails",
	"GET /api/v1/emails/{id}",
	"PATCH /api/v1/emails/{id}",
	"POST /api/v1/emails/{id}/reply",
	"POST /api/v1/emails/{id}/forward",
	"GET /api/v1/emails/{id}/attachments",
	"GET /api/v1/emails/{id}/attachments/{aid}",
	"POST /api/v1/drafts",
	"POST /api/v1/drafts/{id}/send",
	"GET /api/v1/webhooks",
	"POST /api/v1/webhooks",
	"DELETE /api/v1/webhooks/{id}",
	"GET /api/v1/webhooks/{id}/deliveries",

	"GET /api/v1/chats",
	"POST /api/v1/chats",
	"GET /api/v1/chats/{id}",
	"PATCH /api/v1/chats/{id}",
	"GET /api/v1/chats/{id}/messages",
	"POST /api/v1/chats/{id}/messages",
	"GET /api/v1/chats/{id}/messages/{mid}",
	"PATCH /api/v1/chats/{id}/messages/{mid}",
	"DELETE /api/v1/chats/{id}/messages/{mid}",
	"PUT /api/v1/chats/{id}/messages/{mid}/reaction",
	"GET /api/v1/attendees",
	"GET /api/v1/attendees/{id}",
}

// browserRoutes is every pattern Routes registers outside apiRoutes: the
// connect/session/UI surface a browser (not an API key) talks to, plus the
// two provider-facing push endpoints. Like apiRoutes, it exists so the
// isolation test can prove each one is covered — a route added to Routes()
// without being added here fails that test.
var browserRoutes = []string{
	"GET /healthz",

	"GET /connect/{state}",
	"POST /connect/{state}/consent",
	"GET /connect/{state}/qr",
	"GET /oauth/callback",

	"GET /login",
	"POST /login",
	"GET /signup",
	"POST /signup",
	"POST /logout",

	"GET /docs",
	"GET /llms.txt",

	"GET /dashboard",
	"GET /mail",
	"GET /chat",

	"POST /notifications/{provider}",
	"POST /notifications/{provider}/lifecycle",
}

// browserHandlers is the non-API-key counterpart to the apiRoutes/handlers
// map below: every pattern in browserRoutes must have an entry here, and
// vice versa (see TestBrowserHandlersMatchBrowserRoutes), so a route
// registered on mux without a browserRoutes entry — or a browserRoutes entry
// with no handler — fails, instead of silently going unchecked by the
// isolation test.
func (s *Server) browserHandlers() map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		// dropped_events is the one place a lost webhook notification is
		// visible: an event discarded by a saturated dispatcher never reaches
		// webhook_deliveries, so nothing else in the API can report it. db is
		// a live reachability check against the database itself — a process
		// that's up but whose database has fallen over (connection exhaustion,
		// the Postgres instance restarting, ...) is not actually healthy.
		"GET /healthz": func(w http.ResponseWriter, r *http.Request) {
			var dropped int64
			if s.dispatcher != nil {
				dropped = s.dispatcher.Dropped()
			}
			dbStatus := "ok"
			status := http.StatusOK
			if s.store != nil {
				ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
				defer cancel()
				if err := s.store.Ping(ctx); err != nil {
					s.log.Warn("healthz: database ping failed", "error", notify.ScrubErr(err))
					dbStatus = "error"
					status = http.StatusServiceUnavailable
				}
			}
			body := map[string]any{"dropped_events": dropped, "db": dbStatus}
			if status == http.StatusOK {
				body["status"] = "ok"
			} else {
				body["status"] = "error"
			}
			writeJSON(w, status, body)
		},

		// --- connection flow (browser-facing; no API key) ---
		"GET /connect/{state}":          s.handleConnectRedirect,
		"POST /connect/{state}/consent": s.handleConsent,
		"GET /connect/{state}/qr":       s.handleLinkQR,
		"GET /oauth/callback":           s.handleOAuthCallback,

		// --- developer sign-in (form posts, cookie session) ---
		"GET /login":   s.handleLoginPage,
		"POST /login":  s.handleLogin,
		"GET /signup":  s.handleSignupPage,
		"POST /signup": s.handleSignup,
		"POST /logout": s.handleLogout,

		// --- account management + mail viewer UI (server-side session-gated; see handlers_auth.go) ---
		// --- public integration guide ---
		"GET /docs":     s.handleDocs,
		"GET /llms.txt": s.handleLLMsTxt,

		"GET /dashboard": s.handleDashboard,
		"GET /mail":      s.handleMailPage,
		"GET /chat":      s.handleChatPage,

		// --- inbound push from providers ---
		// Deliberately unauthenticated by API key: providers cannot send
		// custom headers. Authenticity comes from the per-subscription
		// clientState secret. Namespaced by provider so each one's
		// validation quirks stay addressable.
		"POST /notifications/{provider}":           s.handleProviderNotification,
		"POST /notifications/{provider}/lifecycle": s.handleProviderNotification,
	}
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	browserHandlers := s.browserHandlers()
	for _, pattern := range browserRoutes {
		hf, ok := browserHandlers[pattern]
		if !ok {
			panic("no handler for " + pattern)
		}
		mux.HandleFunc(pattern, hf)
	}

	// --- the API proper ---
	api := http.NewServeMux()
	handlers := map[string]http.HandlerFunc{
		"POST /api/v1/hosted-auth": s.handleHostedAuth,

		"GET /api/v1/me":                  s.handleMe,
		"POST /api/v1/me/password":        s.handleChangePassword,
		"PUT /api/v1/me/redirect-domains": s.handleSetRedirectDomains,
		"GET /api/v1/api-keys":            s.handleListAPIKeys,
		"POST /api/v1/api-keys":           s.handleCreateAPIKey,
		"DELETE /api/v1/api-keys/{id}":    s.handleRevokeAPIKey,

		"GET /api/v1/providers":                       s.handleListProviders,
		"GET /api/v1/accounts":                        s.handleListAccounts,
		"GET /api/v1/accounts/{id}":                   s.handleGetAccount,
		"DELETE /api/v1/accounts/{id}":                s.handleDeleteAccount,
		"POST /api/v1/accounts/{id}/resync":           s.handleResync,
		"POST /api/v1/accounts/{id}/reconnect":        s.handleReconnect,
		"GET /api/v1/accounts/{id}/webhooks":          s.handleListAccountWebhooks,
		"POST /api/v1/accounts/{id}/webhooks":         s.handleCreateAccountWebhook,
		"DELETE /api/v1/accounts/{id}/webhooks/{wid}": s.handleDeleteAccountWebhook,

		"GET /api/v1/folders": s.handleListFolders,
		"GET /api/v1/threads": s.handleListThreads,

		"GET /api/v1/emails":                        s.handleListEmails,
		"POST /api/v1/emails":                       s.handleSendEmail,
		"GET /api/v1/emails/{id}":                   s.handleGetEmail,
		"PATCH /api/v1/emails/{id}":                 s.handlePatchEmail,
		"POST /api/v1/emails/{id}/reply":            s.handleReply,
		"POST /api/v1/emails/{id}/forward":          s.handleForward,
		"GET /api/v1/emails/{id}/attachments":       s.handleListAttachments,
		"GET /api/v1/emails/{id}/attachments/{aid}": s.handleDownloadAttachment,

		"POST /api/v1/drafts":           s.handleCreateDraft,
		"POST /api/v1/drafts/{id}/send": s.handleSendDraft,

		"GET /api/v1/webhooks":                 s.handleListWebhooks,
		"POST /api/v1/webhooks":                s.handleCreateWebhook,
		"DELETE /api/v1/webhooks/{id}":         s.handleDeleteWebhook,
		"GET /api/v1/webhooks/{id}/deliveries": s.handleListWebhookDeliveries,

		"GET /api/v1/chats":                              s.handleListChats,
		"POST /api/v1/chats":                             s.handleStartChat,
		"GET /api/v1/chats/{id}":                         s.handleGetChat,
		"PATCH /api/v1/chats/{id}":                       s.handlePatchChat,
		"GET /api/v1/chats/{id}/messages":                s.handleListChatMessages,
		"POST /api/v1/chats/{id}/messages":               s.handleSendChatMessage,
		"GET /api/v1/chats/{id}/messages/{mid}":          s.handleGetChatMessage,
		"PATCH /api/v1/chats/{id}/messages/{mid}":        s.handlePatchChatMessage,
		"DELETE /api/v1/chats/{id}/messages/{mid}":       s.handleDeleteChatMessage,
		"PUT /api/v1/chats/{id}/messages/{mid}/reaction": s.handleReactToMessage,
		"GET /api/v1/attendees":                          s.handleListAttendees,
		"GET /api/v1/attendees/{id}":                     s.handleGetAttendee,
	}
	for _, pattern := range apiRoutes {
		hf, ok := handlers[pattern]
		if !ok {
			panic("no handler for " + pattern)
		}
		api.HandleFunc(pattern, hf)
	}

	mux.Handle("/api/v1/", s.withDeveloper(api))

	return s.secureHeaders(s.withRequestID(mux))
}

// withRequestID gives every request an id, a request-scoped logger, and the
// X-Request-Id response header, then logs one summary line at the end.
func (s *Server) withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if !requestIDRe.MatchString(id) {
			id = logx.NewRequestID()
		}
		log := s.log.With("component", "api", "request_id", id)
		w.Header().Set("X-Request-Id", id)
		ctx := logx.With(r.Context(), log)

		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK, ctx: ctx}
		log.Debug("request received",
			"method", r.Method, "path", scrubPath(r.URL.Path), "query", scrubQuery(r.URL.RawQuery),
			"content_type", r.Header.Get("Content-Type"), "content_length", r.ContentLength,
			"has_authorization", r.Header.Get("Authorization") != "" || r.Header.Get("X-API-Key") != "",
			"has_session_cookie", hasCookie(r, sessionCookie),
			"remote", r.RemoteAddr, "user_agent", r.UserAgent())

		next.ServeHTTP(rec, r.WithContext(ctx))

		dev, _ := developerFrom(rec.ctx)
		log.Info("http",
			"method", r.Method, "path", scrubPath(r.URL.Path), "status", rec.status,
			"bytes", rec.bytes, "dur", time.Since(start).Round(time.Millisecond),
			"developer_id", dev.ID, "auth", authKindFrom(rec.ctx))
	})
}

// scrubPath reduces the connect state in a /connect/{state}… path to the same
// short prefix the rest of the codebase logs. The state is a 24-byte
// credential, valid for 30 minutes, that grants the ability to fetch a QR code
// and post consent — i.e. to link a device into someone else's tenant — so it
// must not survive in a log line, at DEBUG or INFO.
func scrubPath(p string) string {
	const prefix = "/connect/"
	if !strings.HasPrefix(p, prefix) {
		return p
	}
	rest := p[len(prefix):]
	tail := ""
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		rest, tail = rest[:i], rest[i:]
	}
	return prefix + statePrefix(rest) + tail
}

// scrubQuery blanks the values of query parameters that are credentials in
// their own right: `code` is an OAuth authorization code (PKCE bounds the
// damage, but it is still a bearer artefact), `state` is the connect token.
// The keys are kept so the shape of the request stays readable.
func scrubQuery(q string) string {
	if q == "" || !strings.Contains(q, "code=") && !strings.Contains(q, "state=") {
		return q
	}
	// Deliberately textual rather than url.ParseQuery: a malformed query still
	// has to be scrubbed, and re-encoding one would change what is logged.
	parts := strings.Split(q, "&")
	for i, kv := range parts {
		k, _, found := strings.Cut(kv, "=")
		if !found {
			continue
		}
		switch k {
		case "code", "state":
			parts[i] = k + "=[redacted]"
		}
	}
	return strings.Join(parts, "&")
}

func hasCookie(r *http.Request, name string) bool {
	_, err := r.Cookie(name)
	return err == nil
}

// isStateChanging reports whether a method can carry a mutating request body.
// DELETE is deliberately excluded: an HTML form cannot issue one, so it needs
// no content-type defence.
func isStateChanging(method string) bool {
	return method == http.MethodPost || method == http.MethodPut || method == http.MethodPatch
}

// withDeveloper resolves the caller from an API key or a session cookie, in
// that order, and rejects the request when neither is valid.
func (s *Server) withDeveloper(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		log := logx.From(ctx)

		bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if bearer == "" {
			bearer = r.Header.Get("X-API-Key")
		}
		if bearer != "" {
			log.Debug("auth: bearer present, resolving api key")
			dev, key, err := s.auth.KeyDeveloper(ctx, bearer)
			if err != nil {
				log.Debug("auth: api key rejected", "err", err)
				writeError(w, http.StatusUnauthorized, "unauthorized", "missing or invalid API key")
				return
			}
			log.Debug("auth: resolved", "developer_id", dev.ID, "key_id", key.ID, "prefix", key.Prefix)
			s.serveAs(w, r, next, dev, authKindAPIKey)
			return
		}

		if c, err := readCookie(r, sessionCookie); err == nil {
			log.Debug("auth: no bearer, session cookie present, resolving")
			dev, exp, err := s.auth.SessionDeveloper(ctx, c.Value)
			if err != nil {
				log.Debug("auth: session rejected", "err", err)
				writeError(w, http.StatusUnauthorized, "unauthorized", "session expired; sign in again")
				return
			}
			// CSRF defence-in-depth: SameSite=Lax keeps the cookie off cross-site
			// requests for anything but top-level navigation (a plain GET), so a
			// state-changing write riding the session cookie can only originate
			// same-site — except a classic HTML form, which SameSite does not
			// stop and which cannot set a JSON content-type. Requiring one here
			// closes that gap without affecting API-key callers.
			if isStateChanging(r.Method) && !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
				log.Debug("auth: session request refused", "reason", "non-json content-type")
				writeError(w, http.StatusUnsupportedMediaType, "json_required",
					"session requests must send Content-Type: application/json")
				return
			}
			// Re-issue the cookie on every successful resolution. Expiry slides
			// in the sessions table, but the cookie's own Expires was fixed at
			// login, so without this the browser drops a session the server
			// still considers live. Unconditional is one header and always
			// correct; comparing against the old cookie's expiry is not even
			// possible here, since the browser does not send it back.
			s.setSessionCookie(w, r, c.Value, exp)
			log.Debug("auth: resolved", "developer_id", dev.ID, "via", "session")
			s.serveAs(w, r, next, dev, authKindSession)
			return
		}

		log.Debug("auth: no credential presented")
		writeError(w, http.StatusUnauthorized, "unauthorized", "missing or invalid API key")
	})
}

// logBody logs a JSON request body at DEBUG with secrets masked, then hands
// the bytes back to the handler. Bodies over 64 KB, or of unknown size (a
// chunked request reports ContentLength == -1), are logged by size only —
// and in either case the body is left completely untouched so the handler
// still gets to read it. The whole thing is a no-op unless DEBUG logging is
// enabled, so it never pays the read cost in production by default.
func logBody(r *http.Request) {
	log := logx.From(r.Context())
	if !log.Enabled(r.Context(), slog.LevelDebug) {
		return
	}
	if r.Body == nil || r.ContentLength == 0 || !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		return
	}
	if r.ContentLength < 0 || r.ContentLength > 64<<10 {
		log.Debug("request body", "bytes", r.ContentLength, "logged", false)
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	r.Body = io.NopCloser(bytes.NewReader(raw))
	if err != nil {
		return
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		log.Debug("request body", "bytes", len(raw), "json", false)
		return
	}
	log.Debug("request body", "bytes", len(raw), "body", scrubValues(logx.Redact(v)))
}

// scrubValues runs every string leaf of a decoded JSON body through
// notify.Scrub. logx.Redact only knows secret-looking *keys*, so a credential
// that arrives inside a URL — a Discord webhook token under "url", a bot token
// inside a Telegram API URL — survives it untouched. This is the second pass
// that catches those; the two together are what makes the DEBUG body line safe
// to keep on in a deployment.
func scrubValues(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = scrubValues(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = scrubValues(val)
		}
		return out
	case string:
		return notify.Scrub(t)
	default:
		return v
	}
}

// serveAs runs next with the developer in context and the logger enriched,
// and records the context on the status recorder so the summary line can
// name the developer.
func (s *Server) serveAs(w http.ResponseWriter, r *http.Request, next http.Handler, dev model.Developer, kind string) {
	ctx := withDeveloperCtx(r.Context(), dev, kind)
	ctx = logx.With(ctx, logx.From(ctx).With("developer_id", dev.ID, "auth", kind))
	if rec, ok := w.(*statusRecorder); ok {
		rec.ctx = ctx
	}
	r = r.WithContext(ctx)
	logBody(r)
	next.ServeHTTP(w, r)
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
	ctx    context.Context
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(p []byte) (int, error) {
	n, err := r.ResponseWriter.Write(p)
	r.bytes += n
	return n, err
}

// ---- helpers ----

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

type apiError struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	var e apiError
	e.Error.Code = code
	e.Error.Message = msg
	writeJSON(w, status, e)
}

// writeProviderError translates backend failures into our own vocabulary, so
// callers never have to special-case any provider's error codes.
func writeProviderError(w http.ResponseWriter, err error) {
	status, body := providerError(err)
	writeJSON(w, status, body)
}

// providerError is the single mapping from a provider sentinel to a status and
// body. It exists as its own function because the send path reports through a
// (status, body) pair rather than writing directly, and the two must not drift:
// before this, an account with no live socket was a 404 on one route and a 502
// on the other.
func providerError(err error) (int, any) {
	switch {
	case errors.Is(err, provider.ErrNotConnected):
		// The account is fine and the caller owns it; only the live session is
		// missing. Same 409 as a dead grant, because the caller's move is the
		// same: wait for the reconnect, or mint a fresh connect link.
		return http.StatusConflict, apiErr("reconnect_required",
			"this account must be reconnected before it can be used")
	case errors.Is(err, provider.ErrNotFound):
		return http.StatusNotFound, apiErr("not_found", err.Error())
	case errors.Is(err, provider.ErrReauthRequired):
		return http.StatusConflict, apiErr("reconnect_required",
			"this account must be reconnected before it can be used")
	default:
		return http.StatusBadGateway, apiErr("provider_error", err.Error())
	}
}

// smallBodyBytes bounds the ordinary JSON routes: patches, flag toggles,
// webhook registrations and the like never legitimately need more than a
// fraction of this.
const smallBodyBytes = 64 << 10

// largeBodyBytes bounds the mail-send family (send/reply/forward/draft),
// whose bodies carry base64 attachment content and so run larger than any
// other route — but still nowhere near the old blanket 32 MB.
const largeBodyBytes = 8 << 20

// errBodyTooLarge is returned by readRawBody when the request body exceeds
// its limit. It plays the same role there that *http.MaxBytesError plays for
// decodeJSON/decodeJSONLarge: writeDecodeError maps either to 413.
var errBodyTooLarge = errors.New("api: request body too large")

func decodeJSON(r *http.Request, v any) error {
	return decodeJSONLimit(r, v, smallBodyBytes)
}

// decodeJSONLarge is decodeJSON with the send-family's larger limit. Use it
// only for handlers whose payload legitimately carries attachment bytes.
func decodeJSONLarge(r *http.Request, v any) error {
	return decodeJSONLimit(r, v, largeBodyBytes)
}

func decodeJSONLimit(r *http.Request, v any, limit int64) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, limit))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// writeDecodeError answers a decodeJSON/decodeJSONLarge/readRawBody failure:
// 413 body_too_large when the body exceeded its limit, 400 invalid_body for
// any other decode failure (malformed JSON, unknown field, wrong type).
// Every call site that used to hand-roll a 400 for these errors goes through
// this instead, so the two cases can never be conflated again.
func writeDecodeError(w http.ResponseWriter, err error) {
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) || errors.Is(err, errBodyTooLarge) {
		writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", "request body exceeds the size limit for this endpoint")
		return
	}
	writeError(w, http.StatusBadRequest, "invalid_body", err.Error())
}

// missCacheTTL is how long a mirror miss (the message was not in our local
// store and the provider itself reported it does not exist) is remembered,
// so a caller retrying against a message id that will never resolve does not
// turn into one Microsoft Graph call per retry.
const missCacheTTL = 60 * time.Second

// missSweepEvery bounds how often remember() walks the whole map pruning
// expired entries, so the cache cannot grow unboundedly from a long-running
// server fielding a steady trickle of distinct misses.
const missSweepEvery = 1000

// missCacheMaxEntries caps how many distinct keys the negative cache may
// hold at once (spec §8), so a caller probing a wide spread of message ids
// that will never resolve cannot grow this map without bound between the
// periodic sweeps above.
const missCacheMaxEntries = 10000

// missCache is a negative cache: it only ever remembers "this key was a
// miss", never a hit, so there is no stale copy of a message to invalidate
// and a genuine miss stops being expensive to repeat. The cost is on the
// other side: a remembered miss suppresses the provider call for up to
// missCacheTTL (60 s), so a message that lands inside that window keeps 404ing
// until the entry expires.
type missCache struct {
	m     sync.Map // key (string) -> expiry (time.Time)
	n     atomic.Int64
	count atomic.Int64 // live entries currently in m
}

// hit reports whether key is a currently-unexpired remembered miss. A stale
// entry is treated as absent and dropped in passing.
func (c *missCache) hit(key string) bool {
	v, ok := c.m.Load(key)
	if !ok {
		return false
	}
	expiry := v.(time.Time)
	if time.Now().After(expiry) {
		c.delete(key)
		return false
	}
	return true
}

// remember records key as a miss for missCacheTTL, sweeping expired entries
// out of the map every missSweepEvery calls, and enforces missCacheMaxEntries
// immediately after any insert that pushes the live count over it.
func (c *missCache) remember(key string) {
	_, hadKey := c.m.Swap(key, time.Now().Add(missCacheTTL))
	if !hadKey {
		c.count.Add(1)
	}
	if c.n.Add(1)%missSweepEvery == 0 {
		c.sweep()
	}
	if c.count.Load() > missCacheMaxEntries {
		// A sweep first, since the excess is very often just entries that
		// have already expired and were merely waiting for the periodic
		// sweep above to catch up to them.
		c.sweep()
		if c.count.Load() > missCacheMaxEntries {
			c.evictExcess()
		}
	}
}

// delete removes key if present, keeping count in sync. Safe to call
// concurrently with itself and with sweep/evictExcess on the same key: only
// whichever call actually removes the entry decrements the count.
func (c *missCache) delete(key any) {
	if _, deleted := c.m.LoadAndDelete(key); deleted {
		c.count.Add(-1)
	}
}

// sweep removes every expired entry. Called periodically from remember
// rather than on a timer, so an idle server does no background work.
func (c *missCache) sweep() {
	now := time.Now()
	c.m.Range(func(k, v any) bool {
		if now.After(v.(time.Time)) {
			c.delete(k)
		}
		return true
	})
}

// evictExcess drops entries, in whatever order sync.Map's Range happens to
// visit them, until the live count is back at or below missCacheMaxEntries.
// Only reached when a fresh sweep of expired entries was not enough — e.g. a
// burst of many distinct misses that are all still within their TTL — so
// which particular unexpired entries get dropped is not load-bearing: they
// are still just a negative cache, and the next lookup for a dropped key
// simply falls through to the provider again.
func (c *missCache) evictExcess() {
	c.m.Range(func(k, v any) bool {
		if c.count.Load() <= missCacheMaxEntries {
			return false
		}
		c.delete(k)
		return true
	})
}

func ctxOf(r *http.Request) context.Context { return r.Context() }
