package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gauravrautela/unified-messaging/internal/accounts"
	"github.com/gauravrautela/unified-messaging/internal/logx"
	"github.com/gauravrautela/unified-messaging/internal/model"
	"github.com/gauravrautela/unified-messaging/internal/store"
)

// ---- accounts ----

// decorate adds runtime state to a chat account before it is serialised. Mail
// accounts, and chat accounts when the runtime is not wired (some tests build
// a Server without one), pass through unchanged.
func (s *Server) decorate(a model.Account) model.Account {
	if a.Kind == model.AccountKindChat && s.chat != nil {
		if c, ok := s.chat.HealthFor(a.ID); ok {
			c.LastError = scrubErr(c.LastError)
			a.Connection = &c
		} else {
			a.Connection = &model.Connection{State: "stopped"}
		}
	}
	return a
}

// scrubErr keeps a chat connection's last error from ever putting a JID —
// which embeds a phone number — in front of an API caller or a log line. A
// message with no "@" carries no JID and passes through as-is.
func scrubErr(msg string) string {
	if strings.Contains(msg, "@") {
		return logx.Digest(msg)
	}
	return msg
}

func (s *Server) handleListAccounts(w http.ResponseWriter, r *http.Request) {
	dev, _ := developerFrom(r.Context())
	accts, err := s.store.ListAccounts(dev.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	for i := range accts {
		accts[i] = s.decorate(accts[i])
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
	writeJSON(w, http.StatusOK, s.decorate(acct))
}

// handleDeleteAccount tears an account down. A mail account loses its upstream
// Graph subscription too — leaving that behind would have Microsoft pushing
// notifications at us for an account we can no longer authenticate. A chat
// account is detached from the runtime, logged out best-effort, and unlinked.
func (s *Server) handleDeleteAccount(w http.ResponseWriter, r *http.Request) {
	dev, _ := developerFrom(r.Context())
	id := r.PathValue("id")
	acct, err := s.store.GetAccount(dev.ID, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "account_not_found", "no such account")
			return
		}
		logx.From(r.Context()).Error("checking account ownership before delete", "account_id", id, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	if acct.Kind == model.AccountKindChat {
		logx.From(r.Context()).Info("deleting chat account", "account_id", id)
		s.chat.Detach(id)
		if p, err := s.registry.Get(acct.Provider); err == nil && p.Chat() != nil {
			if err := p.Chat().Logout(ctx, id); err != nil {
				logx.From(r.Context()).Warn("logout on delete", "account_id", id, "err", err)
			}
		}
		if err := s.accts.DeleteLinked(ctx, id); err != nil {
			writeError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}

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
	if acct.Kind != model.AccountKindMail {
		writeError(w, http.StatusBadRequest, "unsupported_for_kind", "resync applies to mail accounts; use reconnect for chat")
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

// handleReconnect is resync's chat counterpart: rather than pull a delta, it
// tears the live socket down and brings it back up, the same recovery a
// process restart would give the account for free.
func (s *Server) handleReconnect(w http.ResponseWriter, r *http.Request) {
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
	if acct.Kind != model.AccountKindChat {
		writeError(w, http.StatusBadRequest, "unsupported_for_kind", "reconnect applies to chat accounts; use resync for mail")
		return
	}
	if acct.Status != model.AccountOK {
		writeError(w, http.StatusConflict, "account_not_ok",
			"account status is "+acct.Status+"; it must be relinked first")
		return
	}
	logx.From(r.Context()).Info("reconnect requested", "account_id", acct.ID)
	s.chat.Detach(acct.ID)
	if err := s.chat.Attach(acct.ID); err != nil {
		writeError(w, http.StatusServiceUnavailable, "capacity", err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "reconnecting"})
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
	if err := publicHTTPURL(r.URL); err != nil {
		return err
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
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "webhook not found")
			return
		}
		logx.From(r.Context()).Error("loading webhook", "webhook_id", id, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
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
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "account not found")
			return
		}
		logx.From(r.Context()).Error("loading account", "account_id", accountID, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
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
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "account not found")
			return
		}
		logx.From(r.Context()).Error("loading account", "account_id", accountID, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
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
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		logx.From(r.Context()).Error("loading webhook", "webhook_id", r.PathValue("wid"), "err", err)
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	// A hook that does not exist and one bound to a different account are the
	// same 404: ids must not be probeable across accounts.
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
	Kind string `json:"kind"`
	Push bool   `json:"push_notifications"`
	Auth string `json:"auth"` // "link" for a QR-linked chat provider, "oauth" otherwise
}

// handleListProviders reports which backends this deployment can connect,
// what kind of account each produces, how a caller authenticates it, and
// whether it can deliver push notifications or must be polled.
func (s *Server) handleListProviders(w http.ResponseWriter, r *http.Request) {
	names := s.registry.Names()
	out := make([]providerInfo, 0, len(names))
	for _, n := range names {
		p, err := s.registry.Get(n)
		if err != nil {
			continue
		}
		auth := "oauth"
		if p.Linker() != nil {
			auth = "link"
		}
		out = append(out, providerInfo{Name: n, Kind: p.Kind(), Push: p.Push() != nil, Auth: auth})
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
