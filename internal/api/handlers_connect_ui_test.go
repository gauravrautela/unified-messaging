package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gauravrautela/unified-messaging/internal/model"
	"github.com/gauravrautela/unified-messaging/internal/provider"
	"github.com/gauravrautela/unified-messaging/internal/provider/providertest"
	"github.com/gauravrautela/unified-messaging/internal/store"
)

// The three hosted-auth pages are the only screens an end user of a
// developer's app ever sees, and they are rendered for whichever provider the
// connect state names. Nothing on them may be written for one vendor: the
// copy comes from provider.DisplayName, and the QR page has to offer a real
// retry (a countdown and a Try again button) rather than telling a
// non-technical user to reload the browser.

// oauthStub is a mail-kind provider with a working Auth(), so the OAuth
// landing branch of handleConnectRedirect can be rendered without a live
// provider behind it. mailStub (api_test.go) has a nil Auth and would panic
// on the AuthorizeURL call.
type oauthStub struct{ name string }

func (o oauthStub) Name() string                 { return o.name }
func (o oauthStub) Kind() string                 { return model.AccountKindMail }
func (o oauthStub) Auth() provider.Authenticator { return fakeMailAuth{email: "a@x.com"} }
func (o oauthStub) Linker() provider.Linker      { return nil }
func (o oauthStub) Mailbox() provider.Mailbox    { return nil }
func (o oauthStub) Chat() provider.Chatter       { return nil }
func (o oauthStub) Push() provider.Pusher        { return nil }

func TestConnectPagesNameProviderFromRegistry(t *testing.T) {
	s, db := newTestServerWithProviders(t, providertest.NewFakeChat("FAKECHAT"))
	dev, _ := seedDev(t, s, "a@x.com")
	if err := db.SaveOAuthState(store.OAuthState{State: "st_qr", DeveloperID: dev.ID, Provider: "FAKECHAT", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/connect/st_qr", nil))
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, "Fakechat") {
		t.Fatalf("code=%d, body lacks provider display name", rec.Code)
	}
	for _, never := range []string{"Microsoft", "Reload the page"} {
		if strings.Contains(body, never) {
			t.Fatalf("connect page still says %q", never)
		}
	}
	for _, want := range []string{
		`aria-live`, `id="try-again"`, `id="countdown"`, "Entropix",
		// the paired-without-redirect account id lands in this block, which is
		// the same Details/Copy shape the OAuth result page uses
		`id="result-detail"`, `id="account-id"`, `id="copy-id"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("connect page missing %q", want)
		}
	}
}

// The OAuth landing page says what the developer's app is asking for, in
// sentences, and names the provider from the registry rather than assuming
// the mail backend is Microsoft's.
func TestConnectOAuthLandingIsProviderAgnostic(t *testing.T) {
	s, db := newTestServerWithProviders(t, oauthStub{name: "FAKEMAIL"})
	dev, _ := seedDev(t, s, "a@x.com")
	if err := db.SaveOAuthState(store.OAuthState{
		State: "st_oauth", DeveloperID: dev.ID, Provider: "FAKEMAIL",
		FailureURL: "https://app.example.com/cancelled", ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/connect/st_oauth", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"Entropix is connecting your Fakemail account",
		"Continue to Fakemail",
		"Stay connected without asking again", // cfg.Scopes is ["offline_access"]
		"https://app.example.com/cancelled",   // the state's failure redirect, as Cancel
		`class="steps"`,
		"https://example.com/authorize?state=st_oauth",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("oauth landing page missing %q", want)
		}
	}
	for _, never := range []string{"Microsoft", "Continue with"} {
		if strings.Contains(body, never) {
			t.Fatalf("oauth landing page still says %q", never)
		}
	}
}

// Result pages render through the shared public layout — and still answer
// with the status code the failure actually deserves, not a blanket 200.
func TestConnectResultPageKeepsStatusAndUsesLayout(t *testing.T) {
	s, _ := newTestServer(t)
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/connect/nope", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("content-type = %q", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{"Entro", "/static/app.css", "Link not valid"} {
		if !strings.Contains(body, want) {
			t.Fatalf("result page missing %q: %s", want, body)
		}
	}
}

// The account id is an integrator's value, not something the end user who just
// connected their mailbox has to read past: it belongs inside the same single
// "Details" disclosure that carries a provider error, never in the sentence
// above it. And there is no Continue button on any result page — a developer
// who configured success_redirect_url was already 302'd and never sees this.
func TestConnectResultPageKeepsTheAccountIDInsideDetails(t *testing.T) {
	s, _ := newTestServer(t)
	rec := httptest.NewRecorder()
	s.renderResult(rec, http.StatusOK, resultPage{
		Title:   "Account connected",
		Body:    "a@x.com is now connected.",
		Copy:    "acc_123abc",
		Success: true,
	})
	body := rec.Body.String()

	start, end := strings.Index(body, "<details"), strings.Index(body, "</details>")
	if start == -1 || end == -1 {
		t.Fatalf("no <details> on a page carrying an account id: %s", body)
	}
	if n := strings.Count(body, "<details"); n != 1 {
		t.Fatalf("%d <details> elements, want exactly one", n)
	}
	id := strings.Index(body, "acc_123abc")
	if id < start || id > end {
		t.Fatalf("account id is outside <details> (id=%d, details=%d..%d)", id, start, end)
	}
	if !strings.Contains(body[start:end], `data-copy="acc_123abc"`) {
		t.Fatal("no copy button next to the account id")
	}
	if strings.Contains(body, "btn primary") {
		t.Fatal("result page still renders a Continue button")
	}

	// A provider error shares that one block; a page with neither has none.
	rec = httptest.NewRecorder()
	s.renderResult(rec, http.StatusBadRequest, resultPage{
		Title: "Connection cancelled", Body: "The account was not connected, and nothing was shared.",
		Detail: "access_denied: the user declined",
	})
	body = rec.Body.String()
	if !strings.Contains(body, "<details") || !strings.Contains(body, "access_denied: the user declined") {
		t.Fatalf("provider error is not under Details: %s", body)
	}

	rec = httptest.NewRecorder()
	s.renderResult(rec, http.StatusGone, resultPage{Title: "Link expired", Body: "This connection link has expired."})
	if strings.Contains(rec.Body.String(), "<details") {
		t.Fatal("empty Details block rendered with nothing to put in it")
	}
}
