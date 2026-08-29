package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gauravrautela/unified-messaging/internal/accounts"
	"github.com/gauravrautela/unified-messaging/internal/logx"
	"github.com/gauravrautela/unified-messaging/internal/model"
	"github.com/gauravrautela/unified-messaging/internal/notify"
	"github.com/gauravrautela/unified-messaging/internal/provider"
	"github.com/gauravrautela/unified-messaging/internal/store"
	"github.com/gauravrautela/unified-messaging/internal/web"
	"github.com/gauravrautela/unified-messaging/internal/web/docs"
)

// renderPage executes the named web template into a buffer before writing
// anything to w, so a template error (a typo'd field, a page added to
// Templates() without every layout it needs) becomes a clean 500 instead of
// a 200 whose body silently cuts off mid-render. Every HTML page in the
// server goes through here rather than writing headers and executing a
// template by hand.
//
// status is a parameter because not every page is a 200: an auth form
// re-renders under 400/401/403/500 and a connect result under 404/410, and
// "this link expired" must not answer OK. Anything the handler wants on the
// response (Cache-Control, Vary) must be set before the call — the buffer is
// only written out once the template has succeeded.
func (s *Server) renderPage(w http.ResponseWriter, status int, name string, data any) {
	var buf bytes.Buffer
	if err := web.Templates().ExecuteTemplate(&buf, name, data); err != nil {
		s.log.Error("render page", "page", name, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = buf.WriteTo(w)
}

// siteProvider is one card in the website's providers section. It is built
// from the registry rather than hardcoded, so a deployment that enables a
// different provider set advertises exactly what it can actually connect.
type siteProvider struct {
	Name string   // provider.DisplayName, e.g. "Outlook"
	Kind string   // "mail" or "chat"
	Caps []string // what this kind of account supports
	Note string   // one-sentence caveat, "" for most providers
}

// mailCaps and chatCaps are what an account of each kind can do. They are
// per-kind rather than per-provider because that is the honest granularity
// today: every mail provider goes through the same Mailer/Folderer surface
// and every chat provider through the same Chatter surface.
var (
	mailCaps = []string{"Read", "Send", "Folders", "Webhooks"}
	chatCaps = []string{"Receive", "Send", "Reactions", "Edit", "Delete", "Webhooks"}
)

// providerNotes carries the caveats a developer needs before they build on a
// provider. WhatsApp's is not optional: it is a linked-device integration,
// not a Business API one, and saying so on the marketing page is cheaper for
// everyone than saying it after someone's number is banned.
var providerNotes = map[string]string{
	"WHATSAPP": "Connects as a linked device on a real WhatsApp account — no Business API review, but automated sending carries a genuine risk of Meta banning the number.",
}

// siteProviders maps the registry onto the website's provider cards.
func (s *Server) siteProviders() []siteProvider {
	names := s.registry.Names()
	out := make([]siteProvider, 0, len(names))
	for _, n := range names {
		p, err := s.registry.Get(n)
		if err != nil {
			continue
		}
		caps := mailCaps
		if p.Kind() == model.AccountKindChat {
			caps = chatCaps
		}
		out = append(out, siteProvider{
			Name: provider.DisplayName(n), Kind: p.Kind(), Caps: caps, Note: providerNotes[n],
		})
	}
	return out
}

// handleSite is the public product website. It is the only page that
// renders for both anonymous and signed-in visitors on the same route.
//
// The snippets it shows are the same Go values the reference at /docs
// renders, which is the point of them being shared: a curl line that works
// on the marketing page and a different one in the docs would be worse than
// having no marketing page at all.
func (s *Server) handleSite(w http.ResponseWriter, r *http.Request) {
	email := ""
	if dev, ok := s.sessionDeveloper(w, r); ok {
		email = dev.Email
		// Signed in, the homepage names the developer and swaps the CTAs for
		// a Dashboard link, so it is no longer one document a shared cache
		// may hand to the next visitor.
		markSessionVaried(w)
	}
	// Title is left blank: layout_head renders a bare "Entropix" title when
	// Title is empty, rather than the homepage repeating the name twice.
	s.renderPage(w, http.StatusOK, "site", map[string]any{
		"Shell":     web.Shell{Version: web.Version, Email: email, Styles: []string{"site.css"}},
		"Providers": s.siteProviders(),
		"Events":    docs.Events,
		"Send":      docs.SendMessage,
		"Connect":   docs.HostedAuth,
		"Receive":   docs.WebhookPayload,
	})
}

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
		writeDecodeError(w, err)
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
		writeDecodeError(w, err)
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

	// A burst of inbound notifications must never spawn an unbounded number
	// of background goroutines: dispatchNotification caps how many run in
	// their own long-lived goroutine at once, falling back to handling the
	// rest inline (on a much shorter budget) rather than dropping any of
	// them.
	dispatchNotification(s.log, name, func(ctx context.Context) {
		if err := s.syncer.HandleNotifications(ctx, name, raw); err != nil {
			s.log.Warn("handling push notification", "provider", name, "err", err)
		}
	})
	w.WriteHeader(http.StatusAccepted)
}

// notifySem bounds how many push-notification batches are processed in their
// own dedicated goroutine at once. This is the fix for a route that is,
// necessarily, unauthenticated (see the comment on its route registration):
// nothing stops a burst of inbound requests from spawning one goroutine per
// request, each alive for up to two minutes, without it.
var notifySem = make(chan struct{}, 32)

// dispatchNotification runs fn respecting notifySem. When a slot is free, fn
// runs in its own goroutine on a generous (2 minute) budget and this returns
// immediately, exactly as every request did before this cap existed. When
// every slot is taken, fn instead runs inline, on this goroutine, on a much
// shorter (10 second) budget — slower, but never dropped, and it adds no new
// goroutine to the pile.
//
// The background branch recovers a panic from fn: this route is
// unauthenticated by necessity (providers cannot send our API key), so a
// malformed or malicious payload reaching deep enough to panic must not be
// able to crash the whole process. The inline branch needs no equivalent
// guard — it runs on the request's own goroutine, which net/http already
// recovers.
func dispatchNotification(log *slog.Logger, providerName string, fn func(ctx context.Context)) {
	select {
	case notifySem <- struct{}{}:
		go func() {
			defer func() { <-notifySem }()
			defer func() {
				if r := recover(); r != nil {
					log.Error("push handler panicked", "provider", providerName, "panic", fmt.Sprint(r))
				}
			}()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			fn(ctx)
		}()
	default:
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		fn(ctx)
	}
}
