package outlook

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gauravrautela/unified-messaging/internal/logx"
	"github.com/gauravrautela/unified-messaging/internal/provider"
)

// Auth implements provider.Authenticator against the Microsoft identity
// platform's /oauth2/v2.0 endpoints.
//
// We speak to them directly rather than pulling in MSAL. The flow is small, and
// an opaque SDK would hide exactly the mechanics this service exists to manage.
type Auth struct {
	clientID     string
	clientSecret string
	// tenant selects the authority: "consumers" for personal Microsoft accounts
	// only, "common" for personal plus work/school, "organizations", or a GUID.
	tenant      string
	redirectURI string
	scopes      []string
	http        *http.Client
}

func NewAuth(clientID, clientSecret, tenant, redirectURI string, scopes []string) *Auth {
	return &Auth{
		clientID:     clientID,
		clientSecret: clientSecret,
		tenant:       tenant,
		redirectURI:  redirectURI,
		scopes:       scopes,
		http:         &http.Client{Timeout: 30 * time.Second},
	}
}

func (a *Auth) authority() string {
	return "https://login.microsoftonline.com/" + a.tenant + "/oauth2/v2.0"
}

func (a *Auth) AuthorizeURL(state, challenge string, forceConsent bool) string {
	q := url.Values{}
	q.Set("client_id", a.clientID)
	q.Set("response_type", "code")
	q.Set("redirect_uri", a.redirectURI)
	q.Set("response_mode", "query")
	q.Set("scope", strings.Join(a.scopes, " "))
	q.Set("state", state)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	// select_account stops a second connect from silently reusing the browser's
	// existing session, which would make "add another mailbox" a no-op.
	if forceConsent {
		q.Set("prompt", "consent")
	} else {
		q.Set("prompt", "select_account")
	}
	return a.authority() + "/authorize?" + q.Encode()
}

func (a *Auth) Exchange(ctx context.Context, code, verifier string) (provider.Token, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", a.redirectURI)
	form.Set("code_verifier", verifier)
	return a.token(ctx, form)
}

func (a *Auth) Refresh(ctx context.Context, refreshToken string) (provider.Token, error) {
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	form.Set("scope", strings.Join(a.scopes, " "))
	return a.token(ctx, form)
}

// Identify reports which mailbox an access token belongs to.
func (a *Auth) Identify(ctx context.Context, accessToken string) (provider.Identity, error) {
	p, err := meWithToken(ctx, accessToken)
	if err != nil {
		return provider.Identity{}, err
	}
	email := p.Email()
	if email == "" {
		return provider.Identity{}, fmt.Errorf("outlook: could not determine mailbox address")
	}
	return provider.Identity{Identifier: email, Email: email, Name: p.DisplayName}, nil
}

type tokenResponse struct {
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	ExpiresIn        int    `json:"expires_in"`
	Scope            string `json:"scope"`
	TokenType        string `json:"token_type"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

func (a *Auth) token(ctx context.Context, form url.Values) (provider.Token, error) {
	form.Set("client_id", a.clientID)
	if a.clientSecret != "" {
		form.Set("client_secret", a.clientSecret)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		a.authority()+"/token", strings.NewReader(form.Encode()))
	if err != nil {
		return provider.Token{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	// Only the grant type is logged. The form carries the code, the verifier,
	// the refresh token and the client secret; none of them is loggable.
	log := logx.From(ctx).With("component", "outlook")
	log.Debug("token endpoint request", "grant", form.Get("grant_type"))

	start := time.Now()
	resp, err := a.http.Do(req)
	if err != nil {
		log.Debug("token endpoint response", "status", 0,
			"dur", time.Since(start).Round(time.Millisecond), "err", err)
		return provider.Token{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	log.Debug("token endpoint response", "status", resp.StatusCode,
		"bytes", len(body), "dur", time.Since(start).Round(time.Millisecond))

	var tr tokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return provider.Token{}, fmt.Errorf("outlook: bad token response (%d): %s", resp.StatusCode, truncate(body))
	}
	if tr.Error != "" {
		// error/error_description are the provider's own diagnostics, not credentials.
		log.Debug("token endpoint error", "grant", form.Get("grant_type"), "error", tr.Error)
		// invalid_grant is terminal: revoked, expired, or invalidated by a
		// password change. Surfacing it as the shared sentinel is what lets the
		// core mark the account and stop retrying.
		if tr.Error == "invalid_grant" {
			return provider.Token{}, fmt.Errorf("%w: %s", provider.ErrReauthRequired, tr.ErrorDescription)
		}
		return provider.Token{}, fmt.Errorf("outlook: %s: %s", tr.Error, tr.ErrorDescription)
	}
	if tr.AccessToken == "" {
		return provider.Token{}, fmt.Errorf("outlook: empty access_token (%d): %s", resp.StatusCode, truncate(body))
	}

	// Expire a minute early so a token never dies mid-request.
	return provider.Token{
		AccessToken:  tr.AccessToken,
		RefreshToken: tr.RefreshToken,
		Scope:        tr.Scope,
		ExpiresAt:    time.Now().Add(time.Duration(tr.ExpiresIn)*time.Second - time.Minute),
	}, nil
}

func truncate(b []byte) string {
	if len(b) > 300 {
		return string(b[:300]) + "..."
	}
	return string(b)
}
