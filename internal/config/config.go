// Package config loads POC settings from the environment.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
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

	// SessionTTL is how long a dashboard login lasts without use.
	SessionTTL time.Duration

	// TokenKey is the 32-byte AES key protecting refresh tokens at rest.
	TokenKey []byte

	// WhatsAppEnabled turns on the WhatsApp adapter (a whatsmeow linked-device
	// client) at startup. Off by default: it opens a socket per linked account
	// and stores device keys unsealed in SQLite, so an operator opts in
	// deliberately.
	WhatsAppEnabled bool
	// WhatsAppMaxAccounts caps how many WhatsApp accounts the chat runtime
	// keeps a live socket open for at once.
	WhatsAppMaxAccounts int
	// WhatsAppDeviceName is what the end user sees in WhatsApp's own "Linked
	// devices" list after pairing.
	WhatsAppDeviceName string

	// TrustProxy tells the server it sits behind a TLS-terminating reverse
	// proxy, so X-Forwarded-Proto may be trusted to decide whether the
	// original request was HTTPS (for HSTS and the Secure cookie flag).
	// Only set this behind a proxy that strips any client-supplied
	// X-Forwarded-Proto before setting its own — otherwise a client can
	// spoof "https" and downgrade those protections.
	TrustProxy bool
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
		SessionTTL:    time.Duration(envInt("SESSION_TTL_DAYS", 30)) * 24 * time.Hour,

		WhatsAppEnabled:     envBool("WHATSAPP_ENABLED", false),
		WhatsAppMaxAccounts: envInt("WHATSAPP_MAX_ACCOUNTS", 200),
		WhatsAppDeviceName:  env("WHATSAPP_DEVICE_NAME", "Unified Messaging"),

		TrustProxy: envBool("TRUST_PROXY", false),
	}

	// offline_access is what earns us a refresh token; without it the
	// integration dies the moment the first access token expires.
	c.Scopes = strings.Fields(env("MS_SCOPES",
		"offline_access openid profile User.Read Mail.Read Mail.ReadWrite Mail.Send"))

	if c.ClientID == "" {
		return nil, fmt.Errorf("MS_CLIENT_ID is required (see README for app registration steps)")
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

func envInt(k string, def int) int {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

// envBool accepts "1" or "true" (case-insensitive) as truthy; anything else,
// including unset, is false.
func envBool(k string, def bool) bool {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	return v == "1" || strings.EqualFold(v, "true")
}
