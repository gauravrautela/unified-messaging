package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gauravrautela/unified-messaging/internal/accounts"
	"github.com/gauravrautela/unified-messaging/internal/logx"
	"github.com/gauravrautela/unified-messaging/internal/model"
	"github.com/gauravrautela/unified-messaging/internal/store"
)

// ---- accounts ----

func (s *Server) handleListAccounts(w http.ResponseWriter, r *http.Request) {
	dev, _ := developerFrom(r.Context())
	accts, err := s.store.ListAccounts(dev.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, listResponse[model.Account]{Items: accts})
}

func (s *Server) handleGetAccount(w http.ResponseWriter, r *http.Request) {
	dev, _ := developerFrom(r.Context())
	acct, err := s.store.GetAccount(dev.ID, r.PathValue("id"))
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
	dev, _ := developerFrom(r.Context())
	id := r.PathValue("id")
	if _, err := s.store.GetAccount(dev.ID, id); errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "account_not_found", "no such account")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	s.syncer.RemoveSubscriptions(ctx, id)

	logx.From(r.Context()).Info("deleting account", "account_id", id)
	if err := s.store.DeleteAccount(id); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleResync(w http.ResponseWriter, r *http.Request) {
	dev, _ := developerFrom(r.Context())
	id := r.PathValue("id")
	acct, err := s.store.GetAccount(dev.ID, id)
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
	logx.From(r.Context()).Info("resync requested", "account_id", id)
	s.syncer.Wake(id)
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "queued"})
}

// ---- webhooks ----

// webhookRequest is the body for registering a hook, global or per-account.
type webhookRequest struct {
	Name   string   `json:"name,omitempty"`
	URL    string   `json:"url"`
	Secret string   `json:"secret,omitempty"`
	Events []string `json:"events,omitempty"`
}

func (r webhookRequest) validate() error {
	if r.URL == "" {
		return errors.New("url is required")
	}
	u, err := url.Parse(r.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("url %q must be an absolute http(s) URL", r.URL)
	}
	for _, e := range r.Events {
		if !model.KnownEvent(e) {
			return fmt.Errorf("unknown event %q", e)
		}
	}
	return nil
}

// eventsOrDefault narrows an unspecified filter to new mail. Account-level
// hooks are configured by callers who want "tell me when this user gets
// mail"; the global endpoint keeps its historical "empty means everything".
func (r webhookRequest) eventsOrDefault() []string {
	if len(r.Events) == 0 {
		return []string{model.EventMailReceived}
	}
	return r.Events
}

func newWebhook(developerID, accountID string, req webhookRequest) (model.Webhook, error) {
	id, err := accounts.NewID("wh")
	if err != nil {
		return model.Webhook{}, err
	}
	return model.Webhook{
		ID: id, DeveloperID: developerID, AccountID: accountID, Name: req.Name, URL: req.URL, Secret: req.Secret,
		Events: req.Events, CreatedAt: time.Now().UTC(),
	}, nil
}

// createAccountWebhook is shared by the REST handler and the OAuth callback.
func (s *Server) createAccountWebhook(developerID, accountID string, req webhookRequest) (model.Webhook, error) {
	req.Events = req.eventsOrDefault()
	hook, err := newWebhook(developerID, accountID, req)
	if err != nil {
		return hook, err
	}
	return hook, s.store.SaveWebhook(hook)
}

func (s *Server) handleCreateWebhook(w http.ResponseWriter, r *http.Request) {
	dev, _ := developerFrom(r.Context())
	var req webhookRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	if err := req.validate(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_webhook", err.Error())
		return
	}
	hook, err := newWebhook(dev.ID, "", req)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if err := s.store.SaveWebhook(hook); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	// Echo the secret back once so the caller can configure verification, then
	// never again.
	writeJSON(w, http.StatusCreated, hook)
}

// handleListWebhookDeliveries shows what is still waiting for a retry and what
// was abandoned, so a caller can tell an outage's cost.
func (s *Server) handleListWebhookDeliveries(w http.ResponseWriter, r *http.Request) {
	dev, _ := developerFrom(r.Context())
	id := r.PathValue("id")
	if _, err := s.store.GetWebhook(dev.ID, id); err != nil {
		writeError(w, http.StatusNotFound, "not_found", "webhook not found")
		return
	}
	items, err := s.store.ListDeliveries(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, listResponse[store.Delivery]{Items: items})
}

// ---- per-account webhooks ----

func (s *Server) handleCreateAccountWebhook(w http.ResponseWriter, r *http.Request) {
	dev, _ := developerFrom(r.Context())
	accountID := r.PathValue("id")
	if _, err := s.store.GetAccount(dev.ID, accountID); err != nil {
		writeError(w, http.StatusNotFound, "not_found", "account not found")
		return
	}
	var req webhookRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	if err := req.validate(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_webhook", err.Error())
		return
	}
	hook, err := s.createAccountWebhook(dev.ID, accountID, req)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, hook)
}

func (s *Server) handleListAccountWebhooks(w http.ResponseWriter, r *http.Request) {
	dev, _ := developerFrom(r.Context())
	accountID := r.PathValue("id")
	if _, err := s.store.GetAccount(dev.ID, accountID); err != nil {
		writeError(w, http.StatusNotFound, "not_found", "account not found")
		return
	}
	hooks, err := s.store.ListAccountWebhooks(dev.ID, accountID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	for i := range hooks {
		hooks[i].Secret = ""
	}
	writeJSON(w, http.StatusOK, listResponse[model.Webhook]{Items: hooks})
}

func (s *Server) handleDeleteAccountWebhook(w http.ResponseWriter, r *http.Request) {
	dev, _ := developerFrom(r.Context())
	hook, err := s.store.GetWebhook(dev.ID, r.PathValue("wid"))
	if err != nil || hook.AccountID != r.PathValue("id") {
		writeError(w, http.StatusNotFound, "not_found", "webhook not found")
		return
	}
	if err := s.store.DeleteWebhook(dev.ID, hook.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListWebhooks(w http.ResponseWriter, r *http.Request) {
	dev, _ := developerFrom(r.Context())
	hooks, err := s.store.ListWebhooks(dev.ID)
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
	dev, _ := developerFrom(r.Context())
	err := s.store.DeleteWebhook(dev.ID, r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "webhook not found")
		return
	}
	if err != nil {
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
