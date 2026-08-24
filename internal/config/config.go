// Package config loads POC settings from the environment.
package config

import (
	"fmt"
	"os"
	"strings"
)

type Config struct {
	ListenAddr string
	DBPath     string

	// PublicBaseURL is the externally reachable origin providers POST change
	// notifications to. During local development this is an ngrok or cloudflared
	// tunnel. Providers generally refuse to create a subscription unless they can
	// complete a validation handshake against this host, so localhost is unusable.
	PublicBaseURL string

	ClientID     string
	ClientSecret string
	// Authority segment: "consumers" (personal Microsoft accounts only),
	// "common" (personal + work/school), "organizations", or a tenant GUID.
	Tenant      string
	RedirectURI string
	Scopes      []string

	// APIKey guards our own REST surface. The Graph notification endpoint is
	// exempt because Graph cannot send custom headers; it is authenticated by
	// the per-subscription clientState secret instead.
	APIKey string

	// TokenKey is the 32-byte AES key protecting refresh tokens at rest.
	TokenKey []byte
}

func Load() (*Config, error) {
	c := &Config{
		ListenAddr:    env("LISTEN_ADDR", ":8080"),
		DBPath:        env("DB_PATH", "./unified-messaging.db"),
		PublicBaseURL: strings.TrimRight(os.Getenv("PUBLIC_BASE_URL"), "/"),
		ClientID:      os.Getenv("MS_CLIENT_ID"),
		ClientSecret:  os.Getenv("MS_CLIENT_SECRET"),
		Tenant:        env("MS_TENANT", "consumers"),
		RedirectURI:   env("MS_REDIRECT_URI", "http://localhost:8080/oauth/callback"),
		APIKey:        os.Getenv("API_KEY"),
	}

	// offline_access is what earns us a refresh token; without it the
	// integration dies the moment the first access token expires.
	c.Scopes = strings.Fields(env("MS_SCOPES",
		"offline_access openid profile User.Read Mail.Read Mail.ReadWrite Mail.Send"))

	if c.ClientID == "" {
		return nil, fmt.Errorf("MS_CLIENT_ID is required (see README for app registration steps)")
	}
	if c.APIKey == "" {
		return nil, fmt.Errorf("API_KEY is required")
	}

	key, err := decodeKey(os.Getenv("TOKEN_ENCRYPTION_KEY"))
	if err != nil {
		return nil, err
	}
	c.TokenKey = key
	return c, nil
}

// PushEnabled reports whether providers can reach us for push notifications.
// Without a public HTTPS origin the service still works, on polling alone.
func (c *Config) PushEnabled() bool {
	return strings.HasPrefix(c.PublicBaseURL, "https://")
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
