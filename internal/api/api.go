// Package api exposes the provider-neutral REST surface plus the two inbound
// endpoints Microsoft Graph itself calls.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gauravrautela/unified-messaging/internal/accounts"
	"github.com/gauravrautela/unified-messaging/internal/config"
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
	log      *slog.Logger
}

func NewServer(cfg *config.Config, s *store.Store, reg *provider.Registry,
	a *accounts.Manager, sy *syncer.Syncer, log *slog.Logger) *Server {
	return &Server{cfg: cfg, store: s, registry: reg, accts: a, syncer: sy, log: log}
}

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

	// --- account management UI (gated client-side by the API key) ---
	mux.HandleFunc("GET /dashboard", s.handleDashboard)

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

	mux.Handle("/api/v1/", s.requireAPIKey(api))

	return s.withLogging(mux)
}

func (s *Server) requireAPIKey(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if got == "" {
			got = r.Header.Get("X-API-Key")
		}
		if got != s.cfg.APIKey {
			writeError(w, http.StatusUnauthorized, "unauthorized", "missing or invalid API key")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		s.log.Info("http",
			"method", r.Method, "path", r.URL.Path,
			"status", rec.status, "dur", time.Since(start).Round(time.Millisecond))
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
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
