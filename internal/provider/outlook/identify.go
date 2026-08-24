package outlook

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// profile is the subset of Graph's /me we care about.
type profile struct {
	ID                string `json:"id"`
	DisplayName       string `json:"displayName"`
	Mail              string `json:"mail"`
	UserPrincipalName string `json:"userPrincipalName"`
}

// Email is the address treated as the mailbox identity. Personal Microsoft
// accounts frequently return a null `mail`, so userPrincipalName is the fallback.
func (p profile) Email() string {
	if p.Mail != "" {
		return p.Mail
	}
	return p.UserPrincipalName
}

// meWithToken reads the signed-in user's profile using a bare access token.
//
// It exists to break a chicken-and-egg problem: the normal client resolves
// tokens by account ID, but during the OAuth callback we hold a token and no
// account yet. This call is what tells us which mailbox was just connected.
func meWithToken(ctx context.Context, accessToken string) (profile, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		BaseURL+"/me?$select=id,displayName,mail,userPrincipalName", nil)
	if err != nil {
		return profile{}, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")

	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return profile{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return profile{}, classify(resp.StatusCode, body)
	}
	var p profile
	if err := json.Unmarshal(body, &p); err != nil {
		return profile{}, fmt.Errorf("outlook: decoding /me: %w", err)
	}
	return p, nil
}
