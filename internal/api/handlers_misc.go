package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gauravrautela/unified-messaging/internal/accounts"
	"github.com/gauravrautela/unified-messaging/internal/model"
	"github.com/gauravrautela/unified-messaging/internal/store"
)

// ---- accounts ----

func (s *Server) handleListAccounts(w http.ResponseWriter, r *http.Request) {
	accts, err := s.store.ListAccounts()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, listResponse[model.Account]{Items: accts})
}

func (s *Server) handleGetAccount(w http.ResponseWriter, r *http.Request) {
	acct, err := s.store.GetAccount(r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "account_not_found", "no such account")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, acct)
}

// handleDeleteAccount tears the mailbox out, including the upstream Graph
// subscription. Leaving that behind would have Microsoft pushing notifications
// at us for an account we can no longer authenticate.
func (s *Server) handleDeleteAccount(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.store.GetAccount(id); errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "account_not_found", "no such account")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	s.syncer.RemoveSubscriptions(ctx, id)

	if err := s.store.DeleteAccount(id); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleResync(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	acct, err := s.store.GetAccount(id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "account_not_found", "no such account")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if acct.Status != model.AccountOK {
		writeError(w, http.StatusConflict, "account_not_ok",
			"account status is "+acct.Status+"; it must be reconnected first")
		return
	}
	s.syncer.Wake(id)
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "queued"})
}

// ---- webhooks ----

type createWebhookRequest struct {
	URL    string   `json:"url"`
	Secret string   `json:"secret,omitempty"`
	Events []string `json:"events,omitempty"`
}

func (s *Server) handleCreateWebhook(w http.ResponseWriter, r *http.Request) {
	var req createWebhookRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	if req.URL == "" {
		writeError(w, http.StatusBadRequest, "missing_url", "url is required")
		return
	}
	id, err := accounts.NewID("wh")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	hook := model.Webhook{
		ID: id, URL: req.URL, Secret: req.Secret,
		Events: req.Events, CreatedAt: time.Now().UTC(),
	}
	if err := s.store.SaveWebhook(hook); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	// Echo the secret back once so the caller can configure verification, then
	// never again.
	writeJSON(w, http.StatusCreated, hook)
}

func (s *Server) handleListWebhooks(w http.ResponseWriter, r *http.Request) {
	hooks, err := s.store.ListWebhooks()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	for i := range hooks {
		hooks[i].Secret = ""
	}
	writeJSON(w, http.StatusOK, listResponse[model.Webhook]{Items: hooks})
}

func (s *Server) handleDeleteWebhook(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeleteWebhook(r.PathValue("id")); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- providers ----

type providerInfo struct {
	Name string `json:"name"`
	Push bool   `json:"push_notifications"`
}

// handleListProviders reports which backends this deployment can connect, and
// whether each can deliver push notifications or must be polled.
func (s *Server) handleListProviders(w http.ResponseWriter, r *http.Request) {
	names := s.registry.Names()
	out := make([]providerInfo, 0, len(names))
	for _, n := range names {
		p, err := s.registry.Get(n)
		if err != nil {
			continue
		}
		out = append(out, providerInfo{Name: n, Push: p.Push() != nil})
	}
	writeJSON(w, http.StatusOK, listResponse[providerInfo]{Items: out})
}

// ---- inbound push from providers ----

// handleProviderNotification serves both the change-notification and lifecycle
// endpoints for one provider.
//
// Two rules govern this handler. First, providers generally validate the URL
// before they will register a subscription — Microsoft POSTs a ?validationToken
// and demands a 200 echoing it as text/plain — so validation is checked before
// anything else. Second, real notifications must be acknowledged fast, because
// providers retry anything slow, so we answer 202 and work afterwards.
func (s *Server) handleProviderNotification(w http.ResponseWriter, r *http.Request) {
	name := strings.ToUpper(r.PathValue("provider"))

	pusher, err := s.syncer.PusherByName(name)
	if err != nil {
		writeError(w, http.StatusNotFound, "unknown_provider", err.Error())
		return
	}

	if body, ok := pusher.ValidationResponse(r.URL.Query()); ok {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, body)
		return
	}

	raw, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	w.WriteHeader(http.StatusAccepted)

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := s.syncer.HandleNotifications(ctx, name, raw); err != nil {
			s.log.Warn("handling push notification", "provider", name, "err", err)
		}
	}()
}
