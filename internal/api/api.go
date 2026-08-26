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
	"time"

	"github.com/gauravrautela/unified-messaging/internal/accounts"
	"github.com/gauravrautela/unified-messaging/internal/auth"
	"github.com/gauravrautela/unified-messaging/internal/chatsync"
	"github.com/gauravrautela/unified-messaging/internal/config"
	"github.com/gauravrautela/unified-messaging/internal/events"
	"github.com/gauravrautela/unified-messaging/internal/logx"
	"github.com/gauravrautela/unified-messaging/internal/model"
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
	log        *slog.Logger

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
	sy *syncer.Syncer, au *auth.Service, chat *chatsync.Runtime, dispatcher *events.Dispatcher, log *slog.Logger) *Server {
	srv := &Server{
		cfg: cfg, store: s, registry: reg, accts: a, syncer: sy, auth: au,
		chat: chat, dispatcher: dispatcher, log: log, links: newLinkRegistry(),
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

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	// dropped_events is the one place a lost webhook notification is visible:
	// an event discarded by a saturated dispatcher never reaches
	// webhook_deliveries, so nothing else in the API can report it.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		var dropped int64
		if s.dispatcher != nil {
			dropped = s.dispatcher.Dropped()
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "dropped_events": dropped})
	})

	// --- connection flow (browser-facing; no API key) ---
	mux.HandleFunc("GET /connect/{state}", s.handleConnectRedirect)
	mux.HandleFunc("POST /connect/{state}/consent", s.handleConsent)
	mux.HandleFunc("GET /connect/{state}/qr", s.handleLinkQR)
	mux.HandleFunc("GET /oauth/callback", s.handleOAuthCallback)

	// --- developer sign-in (form posts, cookie session) ---
	mux.HandleFunc("GET /login", s.handleLoginPage)
	mux.HandleFunc("POST /login", s.handleLogin)
	mux.HandleFunc("GET /signup", s.handleSignupPage)
	mux.HandleFunc("POST /signup", s.handleSignup)
	mux.HandleFunc("POST /logout", s.handleLogout)

	// --- account management + mail viewer UI (server-side session-gated; see handlers_auth.go) ---
	// --- public integration guide ---
	mux.HandleFunc("GET /docs", s.handleDocs)
	mux.HandleFunc("GET /llms.txt", s.handleLLMsTxt)

	mux.HandleFunc("GET /dashboard", s.handleDashboard)
	mux.HandleFunc("GET /mail", s.handleMailPage)
	mux.HandleFunc("GET /chat", s.handleChatPage)

	// --- inbound push from providers ---
	// Deliberately unauthenticated by API key: providers cannot send custom
	// headers. Authenticity comes from the per-subscription clientState secret.
	// Namespaced by provider so each one's validation quirks stay addressable.
	mux.HandleFunc("POST /notifications/{provider}", s.handleProviderNotification)
	mux.HandleFunc("POST /notifications/{provider}/lifecycle", s.handleProviderNotification)

	// --- the API proper ---
	api := http.NewServeMux()
	handlers := map[string]http.HandlerFunc{
		"POST /api/v1/hosted-auth": s.handleHostedAuth,

		"GET /api/v1/me":               s.handleMe,
		"GET /api/v1/api-keys":         s.handleListAPIKeys,
		"POST /api/v1/api-keys":        s.handleCreateAPIKey,
		"DELETE /api/v1/api-keys/{id}": s.handleRevokeAPIKey,

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

	return s.withRequestID(mux)
}

// withRequestID gives every request an id, a request-scoped logger, and the
// X-Request-Id response header, then logs one summary line at the end.
func (s *Server) withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if id == "" || len(id) > 64 {
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

		if c, err := r.Cookie(sessionCookie); err == nil {
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
	log.Debug("request body", "bytes", len(raw), "body", logx.Redact(v))
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
	switch {
	case errors.Is(err, provider.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, provider.ErrReauthRequired):
		writeError(w, http.StatusConflict, "reconnect_required",
			"this account must be reconnected before it can be used")
	default:
		writeError(w, http.StatusBadGateway, "provider_error", err.Error())
	}
}

func decodeJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 32<<20))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

func ctxOf(r *http.Request) context.Context { return r.Context() }
