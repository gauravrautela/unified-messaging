package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gauravrautela/unified-messaging/internal/logx"
	"github.com/gauravrautela/unified-messaging/internal/model"
	"github.com/gauravrautela/unified-messaging/internal/provider"
	"github.com/gauravrautela/unified-messaging/internal/store"
)

type hostedAuthRequest struct {
	// Provider names the backend to connect. Optional while exactly one is
	// registered, required once there are several.
	Provider           string `json:"provider,omitempty"`
	SuccessRedirectURL string `json:"success_redirect_url,omitempty"`
	FailureRedirectURL string `json:"failure_redirect_url,omitempty"`
	// NotifyURL receives a server-to-server POST the moment a mailbox connects,
	// so the caller learns the account_id without depending on the browser
	// completing its redirect.
	NotifyURL string `json:"notify_url,omitempty"`
	// Webhook, when set, is registered against the account the moment it
	// connects, so the caller starts receiving that user's mail with no second
	// API call. Events default to mail_received.
	Webhook   *webhookRequest `json:"webhook,omitempty"`
	ExpiresIn int             `json:"expires_in_minutes,omitempty"`
	// ForceConsent re-prompts even if Microsoft would otherwise sign the user in
	// silently. Useful when scopes changed.
	ForceConsent bool `json:"force_consent,omitempty"`
}

type hostedAuthResponse struct {
	URL       string    `json:"url"`
	State     string    `json:"state"`
	Provider  string    `json:"provider"`
	ExpiresAt time.Time `json:"expires_at"`
}

// handleHostedAuth mints a single-use connect link.
//
// This is the shape Unipile's hosted auth wizard has, minus the hosted UI: the
// caller's backend asks for a link, hands it to their end user, and learns the
// outcome on notify_url. The API key never leaves the caller's server and the
// end user never sees our client credentials.
func (s *Server) handleHostedAuth(w http.ResponseWriter, r *http.Request) {
	dev, _ := developerFrom(r.Context())
	var req hostedAuthRequest
	if r.ContentLength > 0 {
		if err := decodeJSON(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_body", err.Error())
			return
		}
	}
	if req.ExpiresIn <= 0 {
		req.ExpiresIn = 30
	}
	// notify_url is fetched server-to-server, and the redirect URLs are handed
	// to a browser we sent there; none of them may name an internal target.
	for _, u := range []string{req.NotifyURL, req.SuccessRedirectURL, req.FailureRedirectURL} {
		if u == "" {
			continue
		}
		if err := publicHTTPURL(u); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_url", err.Error())
			return
		}
	}
	var pendingHook *store.PendingWebhook
	if req.Webhook != nil {
		if err := req.Webhook.validate(); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_webhook", err.Error())
			return
		}
		pendingHook = &store.PendingWebhook{
			Name: req.Webhook.Name, URL: req.Webhook.URL, Secret: req.Webhook.Secret,
			Events: req.Webhook.eventsOrDefault(),
		}
	}

	p, err := s.resolveProvider(req.Provider)
	if err != nil {
		writeError(w, http.StatusBadRequest, "unknown_provider", err.Error())
		return
	}

	pkce, err := provider.NewPKCE()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	state, err := provider.RandomString(24)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}

	expiresAt := time.Now().Add(time.Duration(req.ExpiresIn) * time.Minute)
	if err := s.store.SaveOAuthState(store.OAuthState{
		State:       state,
		DeveloperID: dev.ID,
		Provider:    p.Name(),
		Verifier:    pkce.Verifier,
		SuccessURL:  req.SuccessRedirectURL,
		FailureURL:  req.FailureRedirectURL,
		NotifyURL:   req.NotifyURL,
		Webhook:     pendingHook,
		ExpiresAt:   expiresAt,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}

	q := ""
	if req.ForceConsent {
		q = "?force_consent=1"
	}
	logx.From(r.Context()).Info("connect link minted",
		"state_prefix", statePrefix(state), "provider", p.Name(), "expires_at", expiresAt, "has_webhook", pendingHook != nil)
	writeJSON(w, http.StatusOK, hostedAuthResponse{
		URL:       s.baseURL(r) + "/connect/" + state + q,
		State:     state,
		Provider:  p.Name(),
		ExpiresAt: expiresAt.UTC(),
	})
}

// resolveProvider accepts an explicit name, or falls back to the only
// registered provider when the caller did not choose.
func (s *Server) resolveProvider(name string) (provider.Provider, error) {
	if name == "" {
		return s.registry.Default()
	}
	return s.registry.Get(strings.ToUpper(name))
}

// handleConnectRedirect shows the end user a branded landing page before
// sending them to the provider's real consent screen.
//
// This is the Unipile "hosted auth wizard" moment: with several providers
// registered it is where a picker would go, one button per provider. With one
// provider it is a single confirmation screen, but it still matters — it is
// the only thing standing between "click a link from Acme" and "type your
// Microsoft password", and a bare redirect makes that jump feel like a
// phishing link. The button's href is the real authorize URL, computed once
// up front, so clicking it costs no extra round trip through us.
func (s *Server) handleConnectRedirect(w http.ResponseWriter, r *http.Request) {
	state := r.PathValue("state")
	pending, err := s.store.PeekOAuthState(state)
	if err != nil {
		renderMessage(w, http.StatusNotFound, "Link not valid",
			"This connection link is unknown or has already been used.")
		return
	}
	if time.Now().After(pending.ExpiresAt) {
		renderMessage(w, http.StatusGone, "Link expired",
			"This connection link has expired. Please request a new one.")
		return
	}

	p, err := s.resolveProvider(pending.Provider)
	if err != nil {
		renderMessage(w, http.StatusInternalServerError, "Provider unavailable",
			"This connection link refers to a backend that is no longer configured.")
		return
	}

	challenge := provider.ChallengeFor(pending.Verifier)
	force := r.URL.Query().Get("force_consent") == "1"
	authorizeURL := p.Auth().AuthorizeURL(state, challenge, force)

	renderLanding(w, landingData{
		Provider:     displayName(p.Name()),
		AuthorizeURL: authorizeURL,
	})
}

// statePrefix logs a short, non-sensitive fragment of an OAuth state, without
// panicking on the attacker-controlled (and possibly short or empty) value
// Microsoft's redirect carries.
func statePrefix(state string) string {
	if len(state) > 6 {
		return state[:6]
	}
	return state
}

func displayName(providerName string) string {
	switch providerName {
	case "OUTLOOK":
		return "Outlook"
	default:
		return providerName
	}
}

// handleOAuthCallback is Microsoft's redirect target.
func (s *Server) handleOAuthCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	state := q.Get("state")
	code := q.Get("code")
	errCode := q.Get("error")
	logx.From(r.Context()).Info("oauth callback",
		"state_prefix", statePrefix(state), "has_code", code != "", "error", errCode)

	if errCode != "" {
		desc := q.Get("error_description")
		s.log.Warn("oauth callback returned an error", "error", errCode, "description", desc)
		// Consume the state so a denied attempt cannot be replayed.
		if pending, err := s.store.TakeOAuthState(state); err == nil && pending.FailureURL != "" {
			http.Redirect(w, r, appendQuery(pending.FailureURL, url.Values{
				"error": {errCode}, "error_description": {desc},
			}), http.StatusFound)
			return
		}
		renderMessage(w, http.StatusBadRequest, "Connection cancelled",
			fmt.Sprintf("%s: %s", errCode, desc))
		return
	}

	pending, err := s.store.TakeOAuthState(state)
	if err != nil {
		renderMessage(w, http.StatusBadRequest, "Invalid state",
			"This connection link is unknown, expired, or already used.")
		return
	}

	if code == "" {
		s.failConnect(w, r, pending, "missing_code", "Microsoft did not return an authorization code.")
		return
	}

	acct, err := s.accts.Connect(r.Context(), pending.DeveloperID, pending.Provider, code, pending.Verifier)
	if err != nil {
		s.log.Error("connecting account", "err", err)
		s.failConnect(w, r, pending, "connect_failed", err.Error())
		return
	}
	s.log.Info("account connected",
		"account_id", acct.ID, "provider", acct.Provider, "email", acct.Email)

	// Bind the connect-time webhook before the first sync runs, so nothing
	// that backfill emits is missed.
	if pending.Webhook != nil {
		if _, err := s.createAccountWebhook(pending.DeveloperID, acct.ID, webhookRequest{
			Name: pending.Webhook.Name, URL: pending.Webhook.URL,
			Secret: pending.Webhook.Secret, Events: pending.Webhook.Events,
		}); err != nil {
			s.log.Error("registering connect-time webhook", "account_id", acct.ID, "err", err)
		}
	}

	// Kick off the first sync and register for push. Detached from the request
	// so the user's browser is not held open through a full backfill.
	go s.afterConnect(acct)

	if pending.NotifyURL != "" {
		go s.notify(pending.NotifyURL, map[string]any{
			"status": "CREATED", "account_id": acct.ID,
			"email": acct.Email, "provider": acct.Provider,
		})
	}

	if pending.SuccessURL != "" {
		http.Redirect(w, r, appendQuery(pending.SuccessURL, url.Values{
			"account_id": {acct.ID},
		}), http.StatusFound)
		return
	}
	renderMessage(w, http.StatusOK, "Account connected",
		fmt.Sprintf("%s is now connected. Account ID: %s", acct.Email, acct.ID))
}

func (s *Server) afterConnect(acct model.Account) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	if err := s.syncer.EnsureSubscription(ctx, acct.ID); err != nil {
		if !errors.Is(err, provider.ErrReauthRequired) {
			s.log.Error("subscribing after connect", "account_id", acct.ID, "err", err)
		}
	}
	s.syncer.Wake(acct.ID)
}

func (s *Server) failConnect(w http.ResponseWriter, r *http.Request, pending store.OAuthState, code, msg string) {
	if pending.NotifyURL != "" {
		go s.notify(pending.NotifyURL, map[string]any{"status": "FAILED", "error": code, "message": msg})
	}
	if pending.FailureURL != "" {
		http.Redirect(w, r, appendQuery(pending.FailureURL, url.Values{
			"error": {code}, "error_description": {msg},
		}), http.StatusFound)
		return
	}
	renderMessage(w, http.StatusBadRequest, "Connection failed", msg)
}

func (s *Server) notify(target string, payload map[string]any) {
	body, err := json.Marshal(payload)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		s.log.Warn("notify_url delivery failed", "url", target, "err", err)
		return
	}
	resp.Body.Close()
}

// baseURL prefers the configured public origin, since that is what the end
// user's browser and Microsoft both have to be able to reach.
func (s *Server) baseURL(r *http.Request) string {
	if s.cfg.PublicBaseURL != "" {
		return s.cfg.PublicBaseURL
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func appendQuery(raw string, extra url.Values) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	q := u.Query()
	for k, vs := range extra {
		for _, v := range vs {
			q.Set(k, v)
		}
	}
	u.RawQuery = q.Encode()
	return u.String()
}

var messageTmpl = template.Must(template.New("msg").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><title>{{.Title}}</title>
<style>body{font:16px/1.5 system-ui,sans-serif;max-width:34rem;margin:15vh auto;padding:0 1rem;color:#1a1a1a}
h1{font-size:1.25rem;margin:0 0 .5rem}p{color:#555;word-break:break-all}</style></head>
<body><h1>{{.Title}}</h1><p>{{.Body}}</p></body></html>`))

func renderMessage(w http.ResponseWriter, status int, title, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_ = messageTmpl.Execute(w, struct{ Title, Body string }{title, body})
}
