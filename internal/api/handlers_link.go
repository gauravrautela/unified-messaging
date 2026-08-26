package api

import (
	"context"
	"encoding/base64"
	"errors"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"time"

	qrcode "github.com/skip2/go-qrcode"

	"github.com/gauravrautela/unified-messaging/internal/logx"
	"github.com/gauravrautela/unified-messaging/internal/provider"
	"github.com/gauravrautela/unified-messaging/internal/store"
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

	started time.Time
}

// startError reports whether getOrStart's call to StartLink failed for this
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

// getOrStart returns the existing link for state, or installs a placeholder
// and starts one via start. Only the check-then-insert of the placeholder
// runs under lr.mu; start itself (StartLink, for a real provider a blocking
// network dial) runs after the lock is released, so one state's dial can
// never stall another state's poll, or the sweeper, behind the registry
// lock. created reports whether this call is the one that installed the
// placeholder and ran start — the caller starts pumpLink only then, and only
// once it has confirmed start actually succeeded (l.startErr == nil): a
// concurrent caller that lost the create race gets the same *link back with
// created=false and must wait on l.ready before trusting anything on it.
func (lr *linkRegistry) getOrStart(state string, start func() (provider.LinkSession, error), successURL string) (l *link, created bool) {
	lr.mu.Lock()
	if existing, ok := lr.links[state]; ok {
		lr.mu.Unlock()
		return existing, false
	}
	l = &link{ready: make(chan struct{}), successURL: successURL, started: time.Now()}
	lr.links[state] = l
	lr.mu.Unlock()

	sess, err := start()
	if err != nil {
		// Publish the failure before the placeholder becomes unreachable: a
		// concurrent poll holding this *link must never observe a link that is
		// no longer in the registry and still reports startErr == nil.
		l.mu.Lock()
		l.startErr = err
		l.mu.Unlock()
		close(l.ready)
		lr.mu.Lock()
		if lr.links[state] == l {
			delete(lr.links, state)
		}
		lr.mu.Unlock()
		return l, true
	}

	l.mu.Lock()
	l.session = sess
	l.mu.Unlock()
	close(l.ready)
	return l, true
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

// handleConsent records that the end user accepted the linker disclosure.
// It is the gate /qr checks before it will start spending a real pairing
// session: without it, a bare GET on a guessed-but-valid state could kick off
// a device link the end user never agreed to.
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
	if err := s.store.SetOAuthConsent(state, time.Now().UTC()); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	logx.From(r.Context()).Info("link consent recorded", "state_prefix", statePrefix(state))
	w.WriteHeader(http.StatusNoContent)
}

// handleLinkQR is polled by the connect page every couple of seconds. The
// first call after consent starts the actual pairing session; every call
// after that — including a second poll that arrives while that first call's
// StartLink is still in flight — waits on the same outcome rather than
// reporting "waiting" for a session that may already have failed.
func (s *Server) handleLinkQR(w http.ResponseWriter, r *http.Request) {
	// The pairing state changes underneath a fixed URL every couple of
	// seconds; nothing about this response is ever safe to cache.
	w.Header().Set("Cache-Control", "no-store")
	state := r.PathValue("state")

	l := s.links.get(state)
	created := false
	if l == nil {
		// No link exists yet for this state: this is the validation path
		// that only ever runs for whichever poll actually reaches
		// getOrStart's create branch (or loses that race to a concurrent
		// twin) — the store's copy of this state is gone the moment pairing
		// later succeeds or fails, so this is the last point anything here
		// can check consent or expiry against it.
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

		// The session must outlive this request: the end user has not
		// scanned anything yet, and the request/response round trip is over
		// long before that happens. getOrStart guarantees only one caller
		// ever calls StartLink for this state; everyone else gets the same
		// placeholder back and falls through to the wait below.
		l, created = s.links.getOrStart(state, func() (provider.LinkSession, error) {
			return p.Linker().StartLink(context.Background())
		}, pending.SuccessURL)

		if created && l.startError() == nil {
			go s.pumpLink(state, pending, l)
		}
	}

	if !created {
		// Either this poll found an already-installed link (every poll after
		// the first, the common case), or it just lost the create race for a
		// brand new one. Either way getOrStart's StartLink for it may still
		// be in flight; wait for it to resolve, but only up to a bound — a
		// hung dial must delay this one poll, never every poll for this
		// state forever, and never report "waiting" for a session that has
		// already failed.
		select {
		case <-l.ready:
		case <-time.After(5 * time.Second):
		}
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
			Name: pending.Webhook.Name, URL: pending.Webhook.URL,
			Secret: pending.Webhook.Secret, Events: pending.Webhook.Events,
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

type linkPageData struct {
	Provider string
	State    string
}

// linkTmpl is the QR counterpart of handlers_ui.go's OAuth landing page. It
// never embeds the QR code itself server-side — the browser fetches it from
// /qr only after consent, so a link nobody has accepted yet never even starts
// a pairing session.
var linkTmpl = template.Must(template.New("link").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Connect your {{.Provider}} account</title>
<style>
:root{--bg:#f7f7f8;--card:#fff;--text:#1a1a1a;--muted:#6b6b76;--border:#e6e6ea;--accent:#2563eb;--accent-text:#fff}
@media (prefers-color-scheme:dark){:root{--bg:#0f1115;--card:#171a21;--text:#f0f0f2;--muted:#9a9aa5;--border:#2a2d36;--accent:#3b82f6;--accent-text:#fff}}
*{box-sizing:border-box}
body{margin:0;min-height:100vh;display:flex;align-items:center;justify-content:center;
  background:var(--bg);color:var(--text);font:16px/1.5 -apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif;padding:1.5rem}
.card{background:var(--card);border:1px solid var(--border);border-radius:16px;padding:2.5rem 2rem;
  max-width:26rem;width:100%;text-align:center;box-shadow:0 1px 3px rgba(0,0,0,.06)}
h1{font-size:1.25rem;margin:0 0 .5rem}
p{color:var(--muted);margin:0 0 1rem;font-size:.95rem}
label{display:flex;align-items:flex-start;gap:.6rem;text-align:left;font-size:.85rem;color:var(--muted);margin:1.25rem 0}
.btn{display:inline-block;width:100%;padding:.75rem 1rem;border-radius:10px;background:var(--accent);
  color:var(--accent-text);border:none;text-decoration:none;font-weight:600;font-size:.95rem;cursor:pointer}
.btn:disabled{opacity:.5;cursor:not-allowed}
#qr{display:none;width:220px;height:220px;margin:1.25rem auto 0;border-radius:8px}
#status{margin-top:1rem;font-size:.85rem;color:var(--muted)}
</style></head>
<body>
  <div class="card">
    <h1>Connect your {{.Provider}} account</h1>
    <p>Scanning the code below links your phone number to this app. We can see the
    chats and contacts you give it access to, and we store your messages so the
    app can show them to you. You can disconnect at any time.</p>
    <label><input type="checkbox" name="consent" id="consent"> I understand and agree to share my {{.Provider}} data with this app.</label>
    <button class="btn" id="show-qr" disabled>Show QR code</button>
    <img id="qr" alt="QR code">
    <p id="status"></p>
  </div>
<script>
(function() {
  var state = {{.State}};
  var consent = document.getElementById("consent");
  var showBtn = document.getElementById("show-qr");
  var qr = document.getElementById("qr");
  var status = document.getElementById("status");
  var polling = false;

  consent.addEventListener("change", function() { showBtn.disabled = !consent.checked; });

  showBtn.addEventListener("click", function() {
    showBtn.disabled = true;
    fetch("/connect/" + state + "/consent", { method: "POST" })
      .then(function() { status.textContent = "Waiting for scan…"; poll(); })
      .catch(function() { status.textContent = "Could not start; reload and try again."; });
  });

  function poll() {
    if (polling) return;
    polling = true;
    fetch("/connect/" + state + "/qr").then(function(r) {
      return r.json().then(function(data) { return { ok: r.ok, data: data }; });
    }).then(function(result) {
      polling = false;
      var data = result.data;
      if (!result.ok) {
        // A non-2xx /qr response ({error:{code,message}}) means the pairing
        // attempt itself failed server-side (e.g. the provider dial errored)
        // — terminal, like "failed"/"expired" below, not something another
        // poll will resolve.
        status.textContent = (data.error && data.error.message) || "Could not connect. Reload the page to try again.";
        return;
      }
      if (data.png_base64) {
        qr.src = "data:image/png;base64," + data.png_base64;
        qr.style.display = "block";
      }
      if (data.status === "paired") {
        status.textContent = "Connected.";
        if (data.redirect) { location.href = data.redirect; }
        return;
      }
      if (data.status === "expired") {
        status.textContent = "This link expired. Reload the page to try again.";
        return;
      }
      if (data.status === "failed") {
        status.textContent = "Could not connect. Reload the page to try again.";
        return;
      }
      status.textContent = "Waiting for scan…";
      setTimeout(poll, 2000);
    }).catch(function() {
      polling = false;
      setTimeout(poll, 2000);
    });
  }
})();
</script>
</body></html>`))

func renderLink(w http.ResponseWriter, d linkPageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = linkTmpl.Execute(w, d)
}
