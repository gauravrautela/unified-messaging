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
	"github.com/gauravrautela/unified-messaging/internal/config"
	"github.com/gauravrautela/unified-messaging/internal/logx"
	"github.com/gauravrautela/unified-messaging/internal/model"
	"github.com/gauravrautela/unified-messaging/internal/provider"
	"github.com/gauravrautela/unified-messaging/internal/store"
	"github.com/gauravrautela/unified-messaging/internal/syncer"
)

type Server struct {
	cfg      *config.Config
	store    *store.Store
	registry *provider.Registry
	accts    *accounts.Manager
	syncer   *syncer.Syncer
	auth     *auth.Service
	log      *slog.Logger
}

func NewServer(cfg *config.Config, s *store.Store, reg *provider.Registry,
	a *accounts.Manager, sy *syncer.Syncer, au *auth.Service, log *slog.Logger) *Server {
	return &Server{cfg: cfg, store: s, registry: reg, accts: a, syncer: sy, auth: au, log: log}
}

const sessionCookie = "um_session"

// mailboxFor resolves the provider that owns an account. Every mail handler
// goes through here rather than holding a concrete client, which is what keeps
// the HTTP layer free of provider knowledge.
func (s *Server) mailboxFor(acct model.Account) (provider.Mailbox, error) {
	p, err := s.registry.Get(acct.Provider)
	if err != nil {
		return nil, err
	}
	return p.Mailbox(), nil
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	// --- connection flow (browser-facing; no API key) ---
	mux.HandleFunc("GET /connect/{state}", s.handleConnectRedirect)
	mux.HandleFunc("GET /oauth/callback", s.handleOAuthCallback)

	// --- account management + mail viewer UI (gated client-side by the API key) ---
	mux.HandleFunc("GET /dashboard", s.handleDashboard)
	mux.HandleFunc("GET /mail", s.handleMailPage)

	// --- inbound push from providers ---
	// Deliberately unauthenticated by API key: providers cannot send custom
	// headers. Authenticity comes from the per-subscription clientState secret.
	// Namespaced by provider so each one's validation quirks stay addressable.
	mux.HandleFunc("POST /notifications/{provider}", s.handleProviderNotification)
	mux.HandleFunc("POST /notifications/{provider}/lifecycle", s.handleProviderNotification)

	// --- the API proper ---
	api := http.NewServeMux()
	api.HandleFunc("POST /api/v1/hosted-auth", s.handleHostedAuth)

	api.HandleFunc("GET /api/v1/providers", s.handleListProviders)
	api.HandleFunc("GET /api/v1/accounts", s.handleListAccounts)
	api.HandleFunc("GET /api/v1/accounts/{id}", s.handleGetAccount)
	api.HandleFunc("DELETE /api/v1/accounts/{id}", s.handleDeleteAccount)
	api.HandleFunc("POST /api/v1/accounts/{id}/resync", s.handleResync)
	api.HandleFunc("GET /api/v1/accounts/{id}/webhooks", s.handleListAccountWebhooks)
	api.HandleFunc("POST /api/v1/accounts/{id}/webhooks", s.handleCreateAccountWebhook)
	api.HandleFunc("DELETE /api/v1/accounts/{id}/webhooks/{wid}", s.handleDeleteAccountWebhook)

	api.HandleFunc("GET /api/v1/folders", s.handleListFolders)
	api.HandleFunc("GET /api/v1/threads", s.handleListThreads)

	api.HandleFunc("GET /api/v1/emails", s.handleListEmails)
	api.HandleFunc("POST /api/v1/emails", s.handleSendEmail)
	api.HandleFunc("GET /api/v1/emails/{id}", s.handleGetEmail)
	api.HandleFunc("PATCH /api/v1/emails/{id}", s.handlePatchEmail)
	api.HandleFunc("POST /api/v1/emails/{id}/reply", s.handleReply)
	api.HandleFunc("POST /api/v1/emails/{id}/forward", s.handleForward)
	api.HandleFunc("GET /api/v1/emails/{id}/attachments", s.handleListAttachments)
	api.HandleFunc("GET /api/v1/emails/{id}/attachments/{aid}", s.handleDownloadAttachment)

	api.HandleFunc("POST /api/v1/drafts", s.handleCreateDraft)
	api.HandleFunc("POST /api/v1/drafts/{id}/send", s.handleSendDraft)

	api.HandleFunc("GET /api/v1/webhooks", s.handleListWebhooks)
	api.HandleFunc("POST /api/v1/webhooks", s.handleCreateWebhook)
	api.HandleFunc("DELETE /api/v1/webhooks/{id}", s.handleDeleteWebhook)
	api.HandleFunc("GET /api/v1/webhooks/{id}/deliveries", s.handleListWebhookDeliveries)

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
			"method", r.Method, "path", r.URL.Path, "query", r.URL.RawQuery,
			"content_type", r.Header.Get("Content-Type"), "content_length", r.ContentLength,
			"has_authorization", r.Header.Get("Authorization") != "" || r.Header.Get("X-API-Key") != "",
			"has_session_cookie", hasCookie(r, sessionCookie),
			"remote", r.RemoteAddr, "user_agent", r.UserAgent())

		next.ServeHTTP(rec, r.WithContext(ctx))

		dev, _ := developerFrom(rec.ctx)
		log.Info("http",
			"method", r.Method, "path", r.URL.Path, "status", rec.status,
			"bytes", rec.bytes, "dur", time.Since(start).Round(time.Millisecond),
			"developer_id", dev.ID, "auth", authKindFrom(rec.ctx))
	})
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
			dev, err := s.auth.SessionDeveloper(ctx, c.Value)
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
