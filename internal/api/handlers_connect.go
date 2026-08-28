package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gauravrautela/unified-messaging/internal/logx"
	"github.com/gauravrautela/unified-messaging/internal/model"
	"github.com/gauravrautela/unified-messaging/internal/notify"
	"github.com/gauravrautela/unified-messaging/internal/provider"
	"github.com/gauravrautela/unified-messaging/internal/safehttp"
	"github.com/gauravrautela/unified-messaging/internal/store"
	"github.com/gauravrautela/unified-messaging/internal/web"
)

// notifyClient delivers notify_url callbacks. notify_url is attacker-chosen
// (any developer can point it anywhere), so it goes through the same
// no-redirect, public-only dial guard as webhook and chat deliveries.
var notifyClient = safehttp.Client(15 * time.Second)

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
			writeDecodeError(w, err)
			return
		}
	}
	if req.ExpiresIn <= 0 {
		req.ExpiresIn = 30
	}
	// notify_url is fetched server-to-server, so it must not name an internal
	// target. The redirect URLs are only ever followed by the end user's own
	// browser (and the dashboard legitimately points them at this origin), so
	// they need to be http(s) but may be local.
	if req.NotifyURL != "" {
		if err := publicHTTPURL(req.NotifyURL); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_url", "notify_url: "+err.Error())
			return
		}
	}
	ownHost := s.ownRedirectHost()
	for _, u := range []string{req.SuccessRedirectURL, req.FailureRedirectURL} {
		if u == "" {
			continue
		}
		parsed, err := url.Parse(u)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			writeError(w, http.StatusBadRequest, "invalid_url", "redirect urls must be absolute http(s) URLs")
			return
		}
		// A genuine Microsoft/WhatsApp consent must not be able to bounce the
		// end user's browser to a domain this developer does not control: the
		// redirect host has to be this server's own origin (the dashboard's
		// own Connect button) or on the developer's allowlist.
		// An authority with no host at all ("http://:8080/x") must not match an
		// empty ownHost and slip through as "our own origin".
		host := strings.ToLower(parsed.Hostname())
		if host == "" {
			writeError(w, http.StatusBadRequest, "invalid_url", "redirect urls must be absolute http(s) URLs")
			return
		}
		if !strings.EqualFold(host, ownHost) && !hostAllowed(host, dev.RedirectDomains) {
			writeError(w, http.StatusBadRequest, "invalid_url",
				"redirect host is not on your allowlist — add it under Settings → Redirect domains")
			return
		}
	}
	var pendingHook *store.PendingWebhook
	if req.Webhook != nil {
		req.Webhook.normalise()
		if err := req.Webhook.validate(); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_webhook", err.Error())
			return
		}
		// A bad Telegram token/chat pair should fail at link-mint time, not
		// silently at bind time once the account already exists.
		if st, code, msg := s.checkTelegram(r.Context(), *req.Webhook); st != 0 {
			writeError(w, st, code, msg)
			return
		}
		pendingHook = &store.PendingWebhook{
			Name: req.Webhook.Name, Kind: req.Webhook.Kind, URL: req.Webhook.URL, Secret: req.Webhook.Secret,
			BotToken: req.Webhook.BotToken, ChatID: req.Webhook.ChatID,
			// Left as given: the default depends on the account's kind, which is
			// only known once the link completes and the hook is bound.
			Events: req.Webhook.Events,
		}
	}

	p, err := s.resolveProvider(req.Provider)
	if err != nil {
		writeError(w, http.StatusBadRequest, "unknown_provider", err.Error())
		return
	}

	// Chat providers hold one live socket per account for as long as it stays
	// connected, unlike a mail grant that costs nothing between syncs. Once
	// this process is already serving as many as it is configured for, a new
	// pairing attempt would either be refused at Attach time after the user
	// has already scanned a code, or would silently evict someone else's
	// connection — so it is refused here, before any QR is ever shown.
	if p.Linker() != nil && s.chat != nil && s.chat.Count() >= s.chat.Max() {
		writeError(w, http.StatusServiceUnavailable, "capacity", "connection capacity reached; try again later")
		return
	}

	// PKCE only matters for the OAuth authorize/exchange dance; a Linker
	// provider has no authorize URL for a challenge to protect.
	var verifier string
	if p.Linker() == nil {
		pkce, err := provider.NewPKCE()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		verifier = pkce.Verifier
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
		Verifier:    verifier,
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

// resolveProvider accepts an explicit name, or falls back to a default when
// the caller did not choose.
//
// The fallback only ever picks a mail provider, and only when exactly one is
// registered: historically, before a chat provider existed at all, an
// unnamed hosted-auth call meant "the one mail backend", and every
// integrator's existing code depends on that. It never falls further to
// registry.Default() — that would let an unnamed call resolve to a Linker
// once the sole-mail-provider case doesn't hold (no mail provider, several of
// them, or only chat providers registered), and pairing a phone number is
// always something a caller must ask for by name. There is no sense in which
// a bare hosted-auth call could mean "whichever chat provider happens to be
// registered" the way it can for mail.
func (s *Server) resolveProvider(name string) (provider.Provider, error) {
	if name != "" {
		return s.registry.Get(strings.ToUpper(name))
	}
	var mail provider.Provider
	nMail := 0
	for _, n := range s.registry.Names() {
		p, err := s.registry.Get(n)
		if err != nil {
			continue
		}
		if p.Kind() == model.AccountKindMail {
			mail = p
			nMail++
		}
	}
	if nMail != 1 {
		return nil, errors.New("provider is required")
	}
	return mail, nil
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
		renderResult(w, http.StatusNotFound, resultPage{
			Title: "Link not valid",
			Body:  "This connection link is unknown or has already been used. Ask the app you came from for a new one.",
		})
		return
	}
	if time.Now().After(pending.ExpiresAt) {
		renderResult(w, http.StatusGone, resultPage{
			Title: "Link expired",
			Body:  "This connection link has expired. Ask the app you came from for a new one.",
		})
		return
	}

	p, err := s.resolveProvider(pending.Provider)
	if err != nil {
		renderResult(w, http.StatusInternalServerError, resultPage{
			Title:  "We can't connect that account right now",
			Body:   "This connection link refers to a backend that is no longer configured. Nothing was shared.",
			Detail: err.Error(),
		})
		return
	}

	// A chat provider has no authorize URL at all — Auth() is nil for it — so
	// this must branch before ever calling it.
	if p.Linker() != nil {
		s.ensureLinkCookie(w, r, pending.ExpiresAt)
		s.renderConnectQR(w, p.Name(), state)
		return
	}

	challenge := provider.ChallengeFor(pending.Verifier)
	force := r.URL.Query().Get("force_consent") == "1"
	authorizeURL := p.Auth().AuthorizeURL(state, challenge, force)

	// The failure redirect doubles as the Cancel destination: it is where this
	// developer already asked us to send a flow that did not complete, and it
	// was checked against their allowlist when the link was minted. With none
	// configured the page offers no Cancel button at all rather than a dead end.
	s.renderConnectOAuth(w, provider.DisplayName(p.Name()), authorizeURL, pending.FailureURL)
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
		renderResult(w, http.StatusBadRequest, resultPage{
			Title:  "Connection cancelled",
			Body:   "The account was not connected, and nothing was shared.",
			Detail: errorDetail(errCode, desc),
		})
		return
	}

	pending, err := s.store.TakeOAuthState(state)
	if err != nil {
		renderResult(w, http.StatusBadRequest, resultPage{
			Title: "Link not valid",
			Body:  "This connection link is unknown, expired, or has already been used. Ask the app you came from for a new one.",
		})
		return
	}

	if code == "" {
		s.failConnect(w, r, pending, "missing_code",
			provider.DisplayName(pending.Provider)+" did not return an authorization code.")
		return
	}

	acct, err := s.accts.Connect(r.Context(), pending.DeveloperID, pending.Provider, code, pending.Verifier)
	if err != nil {
		s.log.Error("connecting account", "err", err)
		s.failConnect(w, r, pending, "connect_failed", err.Error())
		return
	}
	s.log.Info("account connected",
		"account_id", acct.ID, "provider", acct.Provider, "email_digest", logx.Digest(acct.Email))

	// Bind the connect-time webhook before the first sync runs, so nothing
	// that backfill emits is missed.
	if pending.Webhook != nil {
		if _, err := s.createAccountWebhook(pending.DeveloperID, acct.ID, webhookRequest{
			Name: pending.Webhook.Name, Kind: pending.Webhook.Kind, URL: pending.Webhook.URL,
			Secret: pending.Webhook.Secret, BotToken: pending.Webhook.BotToken, ChatID: pending.Webhook.ChatID,
			Events: pending.Webhook.Events,
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
	// No success_redirect_url was configured — a caller that supplied one was
	// already redirected above — so this page is where the flow ends. The
	// account id still rides along, behind Details with a copy button, because
	// an integrator without notify_url has no other way to learn it.
	renderResult(w, http.StatusOK, resultPage{
		Title: "Account connected",
		Body:  acct.Email + " is now connected.",
		Copy:  acct.ID,
	})
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
	renderResult(w, http.StatusBadRequest, resultPage{
		Title:  "We couldn't finish connecting",
		Body:   "The account was not connected, and nothing was shared. You can try again from the app you came from.",
		Detail: errorDetail(code, msg),
	})
}

// errorDetail joins a provider's error code and description into the one
// line that goes under <details>. Either half may be empty — a consent screen
// that reports only "access_denied" is normal — so this never produces a
// dangling separator.
func errorDetail(code, desc string) string {
	switch {
	case code != "" && desc != "":
		return code + ": " + desc
	case code != "":
		return code
	default:
		return desc
	}
}

func (s *Server) notify(target string, payload map[string]any) {
	if s.notifyTransport != nil {
		s.notifyTransport(target, payload)
		return
	}
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
	resp, err := notifyClient.Do(req)
	if err != nil {
		// The target is developer-chosen and may carry a credential in its
		// path or query — the same reason the notify body goes through Scrub.
		// The error gets the same treatment: net/http builds a *url.Error
		// that quotes the whole URL back, so scrubbing only the url attr
		// would leave the credential in the line anyway.
		s.log.Warn("notify_url delivery failed", "url", notify.Scrub(target), "err", notify.ScrubErr(err))
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

// ownRedirectHost is the hostname (no port) of this server's own configured
// origin — always allowed as a hosted-auth redirect target regardless of a
// developer's allowlist, since the dashboard's own Connect button redirects
// here.
//
// It reads PUBLIC_BASE_URL and nothing else. baseURL's Host-header fallback is
// fine for building a link back to ourselves (a caller who lies about Host
// only misleads themselves), but it must never decide who is exempt from the
// allowlist: r.Host is set by the caller, so a fallback here would let any
// developer declare their own domain to be our origin and mint a connect link
// that bounces the end user's browser to it. config.Load requires
// PUBLIC_BASE_URL precisely so this can be unconditional; an empty value here
// exempts nobody rather than exempting everybody.
func (s *Server) ownRedirectHost() string {
	u, err := url.Parse(s.cfg.PublicBaseURL)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// hostAllowed reports whether host is covered by domains: either an exact,
// case-insensitive match, or a "*.example.com" entry, which covers any
// subdomain (x.example.com, a.b.example.com) but not the apex itself.
func hostAllowed(host string, domains []string) bool {
	host = strings.ToLower(host)
	for _, d := range domains {
		d = strings.ToLower(d)
		if rest, ok := strings.CutPrefix(d, "*."); ok {
			suffix := "." + rest
			if strings.HasSuffix(host, suffix) && len(host) > len(suffix) {
				return true
			}
			continue
		}
		if host == d {
			return true
		}
	}
	return false
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

// resultPage is a terminal outcome of a connect flow, in the shape the page
// renders it: one sentence a non-technical person can act on, plus whatever an
// integrator needs, which the page keeps under a "Details" disclosure rather
// than in that sentence. Copy is a value worth carrying away (an account id);
// Detail is the provider's own error text.
//
// There is no "continue here next" field: when the developer configured a
// success_redirect_url the callback 302s to it and this page never renders,
// which is the documented contract and matches what the QR page does.
type resultPage struct {
	Title, Body, Detail string
	Copy                string
}

// renderResult writes one outcome through the same public layout the rest of
// the connect flow uses, at the status code the outcome actually deserves.
//
// It buffers before writing, so a template error becomes a clean 500 rather
// than a half-rendered page under a 200 — the same contract as
// Server.renderPage, which cannot be used here because it always writes 200
// and "this link expired" must not answer OK. Worth folding a status
// parameter into renderPage and deleting this once handlers_misc.go is free
// to change.
func renderResult(w http.ResponseWriter, status int, p resultPage) {
	var buf bytes.Buffer
	if err := web.Templates().ExecuteTemplate(&buf, "connect_result", map[string]any{
		"Shell":  web.Shell{Title: p.Title, Version: web.Version},
		"Title":  p.Title,
		"Body":   p.Body,
		"Detail": p.Detail,
		"Copy":   p.Copy,
	}); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = buf.WriteTo(w)
}
