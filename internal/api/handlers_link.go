package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	qrcode "github.com/skip2/go-qrcode"

	"github.com/gauravrautela/unified-messaging/internal/logx"
	"github.com/gauravrautela/unified-messaging/internal/provider"
	"github.com/gauravrautela/unified-messaging/internal/store"
	"github.com/gauravrautela/unified-messaging/internal/web"
)

// This file is the chat-provider counterpart of handlers_ui.go's OAuth
// landing page: a Linker provider has no consent screen of its own, so we
// show a disclosure, gate the pairing session on an explicit accept, and
// poll a QR code into existence over an in-memory session rather than a
// browser redirect.

// linkTTL bounds how long a single pairing attempt stays open. It is not
// configurable: a QR code the provider itself already expired long before
// this would be a bug on our side, and any developer-facing tuning belongs on
// notify_url / retry, not the pairing window.
const linkTTL = 3 * time.Minute

// cookieLinkName names the cookie that binds one browser to one connect
// state. handleConnectRedirect mints it — or re-issues the one the browser
// already has — the moment a Linker provider's landing page is shown;
// handleConsent and handleLinkQR both require it and bind a state's pairing
// attempt to whichever value they see first. A /connect/{state} URL that
// leaks after the flow has already started — forwarded, logged, pasted
// somewhere it shouldn't be — cannot then be used from a second browser to
// hijack that attempt and pair an unrelated phone into the tenant.
const cookieLinkName = "um_link"

// ensureLinkCookie mints cookieLinkName the first time a browser reaches a
// Linker provider's landing page for any state, and re-issues whatever value
// the browser already has on every later render. A browser that already has
// one keeps that value, so opening a second connect link in the same browser
// still binds consistently, and reloading the same link never mints a new one
// out from under an in-progress pairing attempt.
//
// expiresAt is the connect state's own expiry, and the cookie's lifetime
// follows it rather than linkTTL. Those are different clocks: linkTTL bounds
// one QR pairing attempt (3 minutes), while the state lives as long as
// hosted-auth asked for (30 minutes by default). A cookie that died with the
// pairing window left the user holding a live link no browser could drive —
// every /qr poll past the third minute answered 403, and reloading minted a
// value the state's persisted claim then refused. Re-issuing on each render
// is what makes that lifetime slide instead of counting down from the first
// page load.
func (s *Server) ensureLinkCookie(w http.ResponseWriter, r *http.Request, expiresAt time.Time) {
	value := logx.RandomToken(32)
	if c, err := readCookie(r, cookieLinkName); err == nil && c.Value != "" {
		value = c.Value
	}
	// At least a second: a state on the edge of expiry still gets a cookie
	// the browser keeps for this request's own round trip, and MaxAge 0 would
	// mean "session cookie" while a negative one would delete it outright.
	maxAge := int(time.Until(expiresAt).Seconds())
	if maxAge < 1 {
		maxAge = 1
	}
	// Scoping the cookie to /connect/ keeps it off every other route, but the
	// __Host- prefix requires Path=/ and pinning the cookie to this exact host
	// is worth more than the path scope: an opaque, HttpOnly, SameSite=Strict
	// value being readable by our own handlers costs nothing, whereas a
	// sibling subdomain being able to set it for the parent domain would let
	// it pre-claim a state. Over plain http there is no prefix and the narrow
	// path stays.
	name := s.cookieName(r, cookieLinkName)
	path := "/connect/"
	if strings.HasPrefix(name, hostCookiePrefix) {
		path = "/"
	}
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     path,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   s.requestIsHTTPS(r),
		MaxAge:   maxAge,
	})
}

// linkBrowserHash reports a one-way handle for the browser's um_link cookie.
// ok is false when no cookie (or an empty one) was sent at all, which is
// refused outright rather than treated as an empty value worth binding — a
// stripped cookie must never pass as "the first caller" for a state nobody
// has claimed yet.
func linkBrowserHash(r *http.Request) (hash string, ok bool) {
	c, err := readCookie(r, cookieLinkName)
	if err != nil || c.Value == "" {
		return "", false
	}
	sum := sha256.Sum256([]byte(c.Value))
	return hex.EncodeToString(sum[:]), true
}

// link is one in-flight (or just-finished) pairing attempt. ready is closed
// exactly once — when session has been set (StartLink succeeded) or startErr
// has been set (it failed) — which is what lets a concurrent poll that lost
// the create race wait for the outcome without ever touching the registry
// lock. Everything after that is read or written through pumpLink's single
// goroutine except for the mutex-guarded fields a concurrent /qr poll also
// touches.
type link struct {
	ready chan struct{}

	mu         sync.Mutex
	session    provider.LinkSession
	startErr   error
	code       provider.LinkCode
	result     *provider.LinkResult
	accountID  string
	successURL string

	// browserHash is the sha256 of the um_link cookie belonging to whichever
	// browser first claimed this state (see linkRegistry.claim). It is set
	// once, at creation, under linkRegistry.mu, and every later read of it
	// happens under that same lock — never mutated afterwards, so it needs
	// no separate guard of its own.
	browserHash string

	// pump ensures startPairing's dial (StartLink) runs exactly once for this
	// entry, however many /qr callers see it not started yet.
	pump sync.Once

	started time.Time
}

// startError reports whether startPairing's call to StartLink failed for this
// link. Safe to call before l.ready closes (it just observes the zero value,
// nil, until the creator sets it).
func (l *link) startError() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.startErr
}

// statusResponse renders the current state for /qr, without ever touching
// the store: by the time a session has resolved, TakeOAuthState has already
// consumed the pending row, so this must be self-contained.
func (l *link) statusResponse() map[string]any {
	l.mu.Lock()
	defer l.mu.Unlock()
	resp := map[string]any{"status": "waiting"}
	switch {
	case l.result != nil && l.result.Err == nil:
		resp["status"] = "paired"
		// The id is what a connect page needs next, and it already leaves
		// through the redirect query string when one was supplied. A page built
		// from /docs with no success_redirect_url had no way to learn it.
		resp["account_id"] = l.accountID
		if l.successURL != "" {
			resp["redirect"] = appendQuery(l.successURL, url.Values{"account_id": {l.accountID}})
		}
	case l.result != nil:
		resp["status"] = "failed"
		if errors.Is(l.result.Err, provider.ErrLinkTimeout) {
			resp["status"] = "expired"
		}
	case l.code.Code != "":
		if png, err := qrcode.Encode(l.code.Code, qrcode.Medium, 512); err == nil {
			resp["png_base64"] = base64.StdEncoding.EncodeToString(png)
			resp["expires_in"] = max(0, int(time.Until(l.code.ExpiresAt).Seconds()))
		}
	}
	return resp
}

// linkRegistry is the in-memory table of in-flight pairing attempts, keyed by
// connect state. It is deliberately not persisted: a process restart mid-QR
// is no worse than the QR simply expiring, and the end user just reloads the
// connect page to mint a fresh session.
type linkRegistry struct {
	mu    sync.Mutex
	links map[string]*link

	// ttl is the pairing window pumpLink enforces. It is linkTTL everywhere
	// except in tests, which shorten it to drive the timeout branch without
	// waiting three minutes. Set once, before any pump goroutine reads it.
	ttl time.Duration
}

func newLinkRegistry() *linkRegistry {
	return &linkRegistry{links: map[string]*link{}, ttl: linkTTL}
}

func (lr *linkRegistry) get(state string) *link {
	lr.mu.Lock()
	defer lr.mu.Unlock()
	return lr.links[state]
}

// claim returns the registry entry for state — creating an inert placeholder
// (no session, no dial attempted) if this is the first request to name state
// at all — and reports whether hash is the browser allowed to keep driving
// it. In the normal flow handleConsent is what creates the entry, since it
// always runs before the first /qr poll; handleLinkQR calls the very same
// method, so a retry after a previous StartLink failure (which drops the
// entry entirely — see dropFailed) is bound exactly the same way, and a /qr
// call that somehow beat consent to naming the state still gets a
// consistent answer rather than a special case. Only the map lookup runs
// under lr.mu; every subsequent poll for the same state is a plain
// constant-time comparison of an already-resident hash, no store or provider
// I/O involved.
func (lr *linkRegistry) claim(state, hash, successURL string) (l *link, ok bool) {
	lr.mu.Lock()
	defer lr.mu.Unlock()
	if existing, found := lr.links[state]; found {
		return existing, subtle.ConstantTimeCompare([]byte(existing.browserHash), []byte(hash)) == 1
	}
	l = &link{ready: make(chan struct{}), browserHash: hash, successURL: successURL, started: time.Now()}
	lr.links[state] = l
	return l, true
}

// startPairing runs start (StartLink) exactly once for l, however many /qr
// polls see it not started yet — whether l was just created by this very
// call or reserved earlier by handleConsent's claim. It reports true only
// for the single caller whose invocation actually ran start; that caller
// owns launching pumpLink on success, or dropping l via dropFailed on
// failure. sync.Once.Do blocks every other caller until start has returned
// and l.ready is closed, so by the time any of them gets control back the
// outcome is already visible on l — no separate wait is needed here.
func (l *link) startPairing(start func() (provider.LinkSession, error)) (ran bool) {
	l.pump.Do(func() {
		ran = true
		sess, err := start()
		l.mu.Lock()
		if err != nil {
			l.startErr = err
		} else {
			l.session = sess
		}
		l.mu.Unlock()
		close(l.ready)
	})
	return ran
}

// dropFailed removes l from the registry after its one StartLink attempt
// failed, so the next /qr poll starts a fresh attempt — and a fresh claim —
// rather than replaying the same failure forever. Guarded against a state
// that has already moved on to a newer entry by the time this runs.
func (lr *linkRegistry) dropFailed(state string, l *link) {
	lr.mu.Lock()
	if lr.links[state] == l {
		delete(lr.links, state)
	}
	lr.mu.Unlock()
}

// sweep closes and drops every link older than maxAge. It is the backstop
// against a browser that never comes back to poll: without it, an abandoned
// session's provider-side link would stay open indefinitely. Closing runs
// after the registry lock is released — a session's Close can be exactly as
// slow as StartLink was, and the lock must never be held across provider I/O.
// A link whose StartLink is still in flight (session not yet set) has
// nothing to close yet; it is still dropped from the registry, and whatever
// StartLink eventually returns for it is simply never used.
func (lr *linkRegistry) sweep(maxAge time.Duration) {
	now := time.Now()
	lr.mu.Lock()
	var stale []*link
	for state, l := range lr.links {
		if now.Sub(l.started) > maxAge {
			stale = append(stale, l)
			delete(lr.links, state)
		}
	}
	lr.mu.Unlock()

	for _, l := range stale {
		l.mu.Lock()
		sess := l.session
		l.mu.Unlock()
		if sess != nil {
			sess.Close()
		}
	}
}

// sweepLinks runs for the lifetime of the process, started once from
// NewServer. Tests never stop it: a leaked one-minute ticker per test server
// is immaterial next to a suite that runs in well under a minute.
func (s *Server) sweepLinks() {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for range t.C {
		s.links.sweep(linkTTL + time.Minute)
	}
}

// linkBrowserMismatch is the 403 both handleConsent and handleLinkQR report
// when a request's um_link cookie is missing or does not match the browser
// that first claimed this connect state.
func linkBrowserMismatch(w http.ResponseWriter) {
	writeError(w, http.StatusForbidden, "link_browser_mismatch",
		"open the link in the browser where you started")
}

// handleConsent records that the end user accepted the linker disclosure.
// It is the gate /qr checks before it will start spending a real pairing
// session: without it, a bare GET on a guessed-but-valid state could kick off
// a device link the end user never agreed to.
//
// It is also, in the normal flow, the first thing to claim this state's
// registry entry for a browser (see linkRegistry.claim): a /connect/{state}
// URL handed to (or intercepted by) a second browser after the first one
// already consented is refused here, before it can ever reach /qr.
func (s *Server) handleConsent(w http.ResponseWriter, r *http.Request) {
	state := r.PathValue("state")
	pending, err := s.store.PeekOAuthState(state)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "unknown link")
		return
	}
	if time.Now().After(pending.ExpiresAt) {
		writeError(w, http.StatusGone, "expired", "this connection link has expired")
		return
	}
	// Consent is a linker concept: an OAuth provider takes consent on its own
	// screen, so consented_at on a mail state records a fact that means
	// nothing. /qr already refuses such a state; refuse it here too, and with
	// the same 404 an unknown state gets — this endpoint is browser-facing and
	// tells an unauthenticated caller nothing about which states exist.
	if p, err := s.registry.Get(pending.Provider); err != nil || p.Linker() == nil {
		writeError(w, http.StatusNotFound, "not_found", "unknown link")
		return
	}
	hash, hasCookie := linkBrowserHash(r)
	if !hasCookie {
		linkBrowserMismatch(w)
		return
	}
	// The persisted claim is checked first: it is what survives an in-memory
	// entry being dropped (a failed StartLink attempt) and a process restart,
	// and it is what a second browser's consent call — arriving after the
	// first browser already claimed this state — must be measured against.
	// The in-memory claim right after it is the fast path for every request
	// from here on.
	if ok, err := s.store.ClaimOAuthStateBrowser(state, hash); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	} else if !ok {
		linkBrowserMismatch(w)
		return
	}
	if _, ok := s.links.claim(state, hash, pending.SuccessURL); !ok {
		linkBrowserMismatch(w)
		return
	}
	if err := s.store.SetOAuthConsent(state, time.Now().UTC()); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	logx.From(r.Context()).Info("link consent recorded", "state_prefix", statePrefix(state))
	w.WriteHeader(http.StatusNoContent)
}

// handleLinkQR is polled by the connect page every couple of seconds. The
// first call whose pairing dial has not been attempted yet — normally the
// very first poll after consent, since consent already claimed the registry
// entry for its browser — starts the actual pairing session; every call
// after that, including one that arrives while that dial is still in
// flight, waits on the same outcome rather than reporting "waiting" for a
// session that may already have failed.
func (s *Server) handleLinkQR(w http.ResponseWriter, r *http.Request) {
	// The pairing state changes underneath a fixed URL every couple of
	// seconds; nothing about this response is ever safe to cache.
	w.Header().Set("Cache-Control", "no-store")
	state := r.PathValue("state")

	l := s.links.get(state)

	// needsStart is true exactly when nothing has attempted this state's
	// StartLink dial yet: either nothing has touched its registry entry at
	// all (l == nil — the usual reason is a retry after a previous attempt's
	// failure dropped the entry via dropFailed), or consent's claim reserved
	// a placeholder for it that no /qr call has started yet. The read of
	// l.ready is non-blocking on purpose: once it is closed, the dial has
	// already been attempted and this poll only needs the outcome.
	needsStart := l == nil
	if !needsStart {
		select {
		case <-l.ready:
		default:
			needsStart = true
		}
	}

	if needsStart {
		// The store's copy of this state is gone the moment pairing later
		// succeeds or fails (finishLink consumes it), so this is the last
		// point anything here can check consent or expiry against it —
		// which is also why this whole branch only ever runs before the
		// dial has been attempted, never on a poll after.
		pending, err := s.store.PeekOAuthState(state)
		if err != nil {
			writeError(w, http.StatusNotFound, "not_found", "unknown link")
			return
		}
		if time.Now().After(pending.ExpiresAt) {
			writeError(w, http.StatusGone, "expired", "this connection link has expired")
			return
		}
		if pending.ConsentedAt == nil {
			writeError(w, http.StatusConflict, "consent_required", "accept the disclosure first")
			return
		}
		p, err := s.registry.Get(pending.Provider)
		if err != nil || p.Linker() == nil {
			writeError(w, http.StatusBadRequest, "unsupported_for_kind", "not a linkable provider")
			return
		}

		// Only past this point is the state itself known-legitimate, which
		// is deliberate: probing /qr before consent must never get to claim
		// (and so bind, or reject) a browser hash — that would let a
		// premature guess lock a legitimate user's own consent call out.
		hash, hasCookie := linkBrowserHash(r)
		if !hasCookie {
			linkBrowserMismatch(w)
			return
		}
		// The persisted claim is what closes the window a dropped in-memory
		// entry would otherwise leave open: if a previous StartLink attempt
		// for this state failed, dropFailed removed the in-memory link
		// entirely, and without this check whichever browser's /qr call
		// happens to land here next would silently re-claim the state. The
		// row-level claim was already set (by consent, in the normal flow)
		// and outlives that in-memory drop, so this is what a second
		// browser's retry attempt is actually measured against.
		if ok, err := s.store.ClaimOAuthStateBrowser(state, hash); err != nil {
			writeError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		} else if !ok {
			linkBrowserMismatch(w)
			return
		}
		var ok bool
		l, ok = s.links.claim(state, hash, pending.SuccessURL)
		if !ok {
			linkBrowserMismatch(w)
			return
		}

		// The session must outlive this request: the end user has not
		// scanned anything yet, and the request/response round trip is over
		// long before that happens. startPairing guarantees only one caller
		// ever calls StartLink for this entry; everyone else blocks inside
		// it until the outcome is already set.
		if l.startPairing(func() (provider.LinkSession, error) {
			return p.Linker().StartLink(context.Background())
		}) {
			if err := l.startError(); err != nil {
				s.links.dropFailed(state, l)
			} else {
				go s.pumpLink(state, pending, l)
			}
		}
	} else {
		// The dial was already attempted by an earlier poll (or by this
		// state's consent-time claim resolving on a previous request); this
		// poll still has to prove it is the same browser before it can see
		// anything — a QR code, a status, anything — about that outcome.
		hash, hasCookie := linkBrowserHash(r)
		if !hasCookie {
			linkBrowserMismatch(w)
			return
		}
		if _, ok := s.links.claim(state, hash, ""); !ok {
			linkBrowserMismatch(w)
			return
		}
	}

	select {
	case <-l.ready:
	case <-time.After(5 * time.Second):
	}

	if err := l.startError(); err != nil {
		writeError(w, http.StatusBadGateway, "provider_error", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, l.statusResponse())
}

// pumpLink forwards QR codes into the link as they arrive and completes it
// the moment the provider reports a result or the pairing window elapses.
// It owns l.session and l.code exclusively; only the mutex-guarded fields are
// shared with a concurrent /qr poll.
func (s *Server) pumpLink(state string, pending store.OAuthState, l *link) {
	timeout := time.NewTimer(s.links.ttl)
	defer timeout.Stop()

	codes := l.session.Codes()
	for {
		select {
		case c, ok := <-codes:
			if !ok {
				// The session closed without ever sending a Result — should
				// not happen, but stop selecting a closed channel rather than
				// spin on it while we wait for Result or the timeout.
				codes = nil
				continue
			}
			l.mu.Lock()
			l.code = c
			l.mu.Unlock()
		case res := <-l.session.Result():
			s.finishLink(state, pending, l, res)
			return
		case <-timeout.C:
			l.session.Close()
			// Close and the phone's scan can be simultaneous: the provider may
			// have resolved this session successfully a moment ago, or be about
			// to. Dropping such a result linked the device for real — it stays
			// in the end user's "Linked devices" list, with its keys on disk —
			// while we reported link_timeout and created no account, leaving
			// them to unlink it by hand. Give a late result a bounded chance to
			// arrive, and honour it only if it actually succeeded (our own
			// Close resolves an unpaired session as ErrLinkCancelled).
			res := provider.LinkResult{Err: provider.ErrLinkTimeout}
			if late, ok := lateResult(l.session, lateResultWait); ok && late.Err == nil {
				res = late
			}
			s.finishLink(state, pending, l, res)
			return
		}
	}
}

// lateResultWait bounds how long the closing pairing window waits for a result
// that may already be on its way. Short: nothing is waiting on this but the
// pump goroutine, and a session that has not resolved by now never will.
const lateResultWait = time.Second

// lateResult reports a result already delivered, or delivered within wait.
func lateResult(sess provider.LinkSession, wait time.Duration) (provider.LinkResult, bool) {
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case res := <-sess.Result():
		return res, true
	case <-t.C:
		return provider.LinkResult{}, false
	}
}

// finishLink turns a resolved pairing attempt into a connected account (or a
// failure notification), and consumes the pending state either way so it
// cannot be replayed. It runs on pumpLink's goroutine, never on a request's,
// so its context is a fresh background one carrying its own logger.
func (s *Server) finishLink(state string, pending store.OAuthState, l *link, res provider.LinkResult) {
	ctx := logx.With(context.Background(), s.log.With(
		"component", "api", "state_prefix", statePrefix(state), "developer_id", pending.DeveloperID))
	log := logx.From(ctx)

	if _, err := s.store.TakeOAuthState(state); err != nil {
		// The connect link itself (independent of the 3-minute pairing
		// window) expired, or something else already consumed it, while this
		// session was in flight. Either way there is no pending state left to
		// create an account against, so this is fatal — not a bookkeeping
		// warning — and must not fall through to ConnectLinked.
		log.Warn("link state expired or already consumed before pairing completed", "err", err)
		if pending.NotifyURL != "" {
			s.notify(pending.NotifyURL, map[string]any{
				"status": "FAILED", "error": "link_expired",
				"message": "connect link expired before pairing completed",
			})
		}
		// The provider may have already paired successfully before the link
		// itself expired; that device must not linger registered with no
		// account behind it.
		s.forgetDeviceOnFailure(ctx, log, pending.Provider, res.DeviceJID)
		expired := provider.LinkResult{Err: provider.ErrLinkTimeout}
		l.mu.Lock()
		l.result = &expired
		l.mu.Unlock()
		return
	}

	if res.Err != nil {
		code := "link_failed"
		switch {
		case errors.Is(res.Err, provider.ErrLinkTimeout):
			code = "link_timeout"
		case errors.Is(res.Err, provider.ErrLinkCancelled):
			code = "link_cancelled"
		}
		log.Info("link failed", "code", code)
		if pending.NotifyURL != "" {
			s.notify(pending.NotifyURL, map[string]any{"status": "FAILED", "error": code, "message": res.Err.Error()})
		}
		l.mu.Lock()
		l.result = &res
		l.mu.Unlock()
		return
	}

	acct, err := s.accts.ConnectLinked(ctx, pending.DeveloperID, pending.Provider, res.Identity, res.DeviceJID)
	if err != nil {
		log.Error("recording linked account", "err", err)
		if pending.NotifyURL != "" {
			s.notify(pending.NotifyURL, map[string]any{"status": "FAILED", "error": "link_failed", "message": err.Error()})
		}
		// The provider paired this device; our own bookkeeping is what
		// rejected it, so the device is real and must not be left registered
		// with no account behind it.
		s.forgetDeviceOnFailure(ctx, log, pending.Provider, res.DeviceJID)
		failed := provider.LinkResult{Err: err}
		l.mu.Lock()
		l.result = &failed
		l.mu.Unlock()
		return
	}

	if pending.Webhook != nil {
		if _, err := s.createAccountWebhook(pending.DeveloperID, acct.ID, webhookRequest{
			Name: pending.Webhook.Name, Kind: pending.Webhook.Kind, URL: pending.Webhook.URL,
			Secret: pending.Webhook.Secret, BotToken: pending.Webhook.BotToken, ChatID: pending.Webhook.ChatID,
			Events: pending.Webhook.Events,
		}); err != nil {
			log.Error("binding connect-time webhook", "account_id", acct.ID, "err", err)
		}
	}

	if s.chat != nil {
		if err := s.chat.Attach(acct.ID); err != nil {
			log.Warn("attaching linked account", "account_id", acct.ID, "err", err)
		}
	}

	if pending.NotifyURL != "" {
		s.notify(pending.NotifyURL, map[string]any{
			"status": "CREATED", "account_id": acct.ID,
			"identifier": acct.Identifier, "provider": acct.Provider,
		})
	}

	log.Info("account linked", "account_id", acct.ID)
	l.mu.Lock()
	l.result = &res
	l.accountID = acct.ID
	l.mu.Unlock()
}

// forgetDeviceOnFailure drops a device's local credentials after a pairing
// attempt is abandoned post-hoc — the connect link expired, or account
// bookkeeping rejected an otherwise-successful pairing — even though the
// provider itself considers the device paired. A no-op when the pairing
// never actually produced a device (res.DeviceJID empty) or the provider is
// no longer resolvable.
func (s *Server) forgetDeviceOnFailure(ctx context.Context, log *slog.Logger, providerName, deviceJID string) {
	if deviceJID == "" {
		return
	}
	p, err := s.registry.Get(providerName)
	if err != nil || p.Chat() == nil {
		return
	}
	if err := p.Chat().Forget(ctx, deviceJID); err != nil {
		log.Warn("forgetting device after failed link", "err", err)
	}
}

// ---------- the linker connect page ----------

// renderConnectQR is the QR counterpart of handlers_ui.go's OAuth landing
// page. It never embeds the QR code itself server-side — the browser fetches
// it from /qr only after consent, so a link nobody has accepted yet never even
// starts a pairing session.
//
// There is no CSRF token on this page: /connect/{state}/consent takes no
// session and is not protected by one. What binds the POST to this browser is
// the SameSite=Strict, HttpOnly um_link cookie ensureLinkCookie has just
// issued, which handleConsent then claims for the state (see
// linkRegistry.claim) — a cross-site POST carries no such cookie at all.
func (s *Server) renderConnectQR(w http.ResponseWriter, providerName, state string) {
	display := provider.DisplayName(providerName)
	s.renderPage(w, "connect_qr", map[string]any{
		"Shell":    web.Shell{Title: "Connect " + display, Version: web.Version},
		"Provider": display,
		"State":    state,
	})
}
