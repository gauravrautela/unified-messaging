package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gauravrautela/unified-messaging/internal/accounts"
	"github.com/gauravrautela/unified-messaging/internal/logx"
	"github.com/gauravrautela/unified-messaging/internal/model"
	"github.com/gauravrautela/unified-messaging/internal/notify"
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
		// Logout and unlink before detaching: if DeleteLinked fails, the actor
		// is still attached, so the account is left in a state the runtime can
		// still explain rather than a live account with no actor behind it. A
		// provider-side logout the live actor observes flips the account to
		// CREDENTIALS on its own, which is a consistent, relinkable state.
		if p, err := s.registry.Get(acct.Provider); err == nil && p.Chat() != nil {
			if err := p.Chat().Logout(ctx, id); err != nil {
				logx.From(r.Context()).Warn("logout on delete", "account_id", id, "err", scrubErr(err.Error()))
			}
		}
		if err := s.accts.DeleteLinked(ctx, id); err != nil {
			writeError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		if s.chat != nil {
			s.chat.Detach(id)
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
	if s.chat == nil {
		writeError(w, http.StatusServiceUnavailable, "capacity", "chat runtime disabled")
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
	Name     string   `json:"name,omitempty"`
	Kind     string   `json:"kind,omitempty"`
	URL      string   `json:"url,omitempty"`
	Secret   string   `json:"secret,omitempty"`
	BotToken string   `json:"bot_token,omitempty"`
	ChatID   string   `json:"chat_id,omitempty"`
	Events   []string `json:"events,omitempty"`
}

// normalise fills in the default kind so every existing caller that never
// mentions "kind" keeps getting a plain JSON webhook.
func (r *webhookRequest) normalise() {
	if r.Kind == "" {
		r.Kind = model.WebhookKindWebhook
	}
}

func (r webhookRequest) validate() error {
	if !model.KnownWebhookKind(r.Kind) {
		return errors.New("kind must be webhook, discord or telegram")
	}
	switch r.Kind {
	case model.WebhookKindWebhook:
		if r.URL == "" {
			return errors.New("url is required")
		}
		if err := publicHTTPURL(r.URL); err != nil {
			return err
		}
		if r.BotToken != "" || r.ChatID != "" {
			return errors.New("bot_token and chat_id apply to kind=telegram only")
		}
	case model.WebhookKindDiscord:
		if err := discordWebhookURL(r.URL); err != nil {
			return err
		}
		if r.Secret != "" {
			return errors.New("secret applies to kind=webhook only")
		}
		if r.BotToken != "" || r.ChatID != "" {
			return errors.New("bot_token and chat_id apply to kind=telegram only")
		}
	case model.WebhookKindTelegram:
		if r.BotToken == "" || r.ChatID == "" {
			return errors.New("bot_token and chat_id are required for kind=telegram")
		}
		if r.URL != "" || r.Secret != "" {
			return errors.New("url and secret do not apply to kind=telegram")
		}
	}
	for _, e := range r.Events {
		if !model.KnownEvent(e) {
			return fmt.Errorf("unknown event %q", e)
		}
	}
	return nil
}

// discordWebhookURL accepts only Discord's own incoming-webhook endpoint.
func discordWebhookURL(raw string) error {
	if raw == "" {
		return errors.New("url is required")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" {
		return errors.New("url must be an https Discord webhook URL")
	}
	if u.User != nil {
		return errors.New("url must not contain credentials")
	}
	host := strings.ToLower(u.Hostname())
	if host != "discord.com" && host != "discordapp.com" {
		return errors.New("url must be on discord.com or discordapp.com")
	}
	// Hostname() strips a port, so "discord.com:8443" would pass the check
	// above while defeating the host-anchored scrubbers that keep the token
	// out of logs and last_error. Discord never needs one.
	if u.Port() != "" {
		return errors.New("url must not specify a port")
	}
	if !strings.HasPrefix(u.Path, "/api/webhooks/") {
		return errors.New("url must be a Discord incoming-webhook URL (/api/webhooks/…)")
	}
	return nil
}

// checkTelegram verifies a telegram target once at creation. It returns the
// HTTP status and error code to answer with, or 0 when the target is fine.
func (s *Server) checkTelegram(ctx context.Context, req webhookRequest) (int, string, string) {
	if req.Kind != model.WebhookKindTelegram {
		return 0, "", ""
	}
	err := s.senders.ValidateTelegram(ctx, req.BotToken, req.ChatID)
	switch {
	case err == nil:
		return 0, "", ""
	case errors.Is(err, notify.ErrTelegramRejected):
		return http.StatusBadRequest, "invalid_webhook", err.Error()
	default:
		return http.StatusBadGateway, "provider_error", "telegram unreachable: " + err.Error()
	}
}

// eventsOrDefault narrows an unspecified filter to "a new message arrived"
// for the account's kind: new mail for a mailbox, new chat message for a
// chat account. Account-level hooks almost always want exactly that, and a
// mail-only default on a WhatsApp account would silently never fire.
func (r webhookRequest) eventsOrDefault(kind string) []string {
	if len(r.Events) == 0 {
		if kind == model.AccountKindChat {
			return []string{model.EventChatReceived}
		}
		return []string{model.EventMailReceived}
	}
	return r.Events
}

func newWebhook(developerID, accountID string, req webhookRequest) (model.Webhook, error) {
	id, err := accounts.NewID("wh")
	if err != nil {
		return model.Webhook{}, err
	}
	w := model.Webhook{
		ID: id, DeveloperID: developerID, AccountID: accountID, Name: req.Name, Kind: req.Kind,
		Events: req.Events, CreatedAt: time.Now().UTC(),
	}
	switch req.Kind {
	case model.WebhookKindWebhook:
		w.URL, w.Secret = req.URL, req.Secret
	case model.WebhookKindDiscord:
		w.URL = req.URL
	case model.WebhookKindTelegram:
		w.Telegram = &model.TelegramTarget{ChatID: req.ChatID, BotToken: req.BotToken}
	}
	return w, nil
}

// createAccountWebhook is shared by the REST handler and the OAuth callback.
func (s *Server) createAccountWebhook(developerID, accountID string, req webhookRequest) (model.Webhook, error) {
	req.normalise()
	acct, err := s.store.GetAccount(developerID, accountID)
	if err != nil {
		return model.Webhook{}, err
	}
	req.Events = req.eventsOrDefault(acct.Kind)
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
	req.normalise()
	if err := req.validate(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_webhook", err.Error())
		return
	}
	if st, code, msg := s.checkTelegram(r.Context(), req); st != 0 {
		writeError(w, st, code, msg)
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
	if hook.Kind != model.WebhookKindWebhook {
		hook.Secret = ""
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
	limit, offset := deliveriesPaging(r)
	items, err := s.store.ListDeliveries(id, limit, offset)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, listResponse[store.Delivery]{Items: items, Limit: limit, Offset: offset})
}

// deliveriesPaging parses limit/offset for the deliveries listing. Unlike
// paging (used elsewhere), an out-of-range limit clamps to the max rather
// than silently falling back to the default: an operator asking for 500
// still gets the largest page this endpoint will hand back, not a smaller
// one than they asked for.
func deliveriesPaging(r *http.Request) (limit, offset int) {
	limit, offset = 50, 0
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
			if limit > 200 {
				limit = 200
			}
		}
	}
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			offset = n
		}
	}
	return limit, offset
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
	req.normalise()
	if err := req.validate(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_webhook", err.Error())
		return
	}
	if st, code, msg := s.checkTelegram(r.Context(), req); st != 0 {
		writeError(w, st, code, msg)
		return
	}
	hook, err := s.createAccountWebhook(dev.ID, accountID, req)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if hook.Kind != model.WebhookKindWebhook {
		hook.Secret = ""
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
