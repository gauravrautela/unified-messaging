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

	// PublicBaseURL is this deployment's own origin. It is required.
	//
	// Two things depend on it. Providers POST change notifications to it, and
	// generally refuse to create a subscription unless they can complete a
	// validation handshake against the host — so for push to work at all it has
	// to be an externally reachable https origin (an ngrok or cloudflared tunnel
	// during local development; PushEnabled reports whether it qualifies).
	// Separately, and regardless of push, it is the origin the hosted-auth
	// redirect allowlist exempts: a redirect target on this host is accepted
	// without a developer allowlist entry, because the dashboard's own Connect
	// button points there. That exemption must never be derived from the
	// request's Host header, which any caller can set to anything, so a
	// deployment with no configured origin is refused at startup rather than
	// run with a forgeable allowlist.
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

	// SessionMaxAge is the absolute lifetime of a dashboard session: however
	// active it has been, it dies this long after it was created and the
	// developer signs in again. Sliding expiry alone would let a stolen
	// cookie be renewed forever.
	SessionMaxAge time.Duration

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

	// DeliveryRetention is how long an abandoned (dead) webhook delivery is
	// kept before the hourly purge deletes it. A dead delivery still carries
	// the full message payload it failed to send, so this bounds how long
	// that content sits in the database after the subscriber has given up on
	// it.
	DeliveryRetention time.Duration
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
		SessionMaxAge: time.Duration(envInt("SESSION_MAX_AGE_DAYS", 90)) * 24 * time.Hour,

		WhatsAppEnabled:     envBool("WHATSAPP_ENABLED", false),
		WhatsAppMaxAccounts: envInt("WHATSAPP_MAX_ACCOUNTS", 200),
		WhatsAppDeviceName:  env("WHATSAPP_DEVICE_NAME", "Unified Messaging"),

		TrustProxy: envBool("TRUST_PROXY", false),

		DeliveryRetention: time.Duration(envInt("DELIVERY_RETENTION_DAYS", 7)) * 24 * time.Hour,
	}

	// offline_access is what earns us a refresh token; without it the
	// integration dies the moment the first access token expires.
	c.Scopes = strings.Fields(env("MS_SCOPES",
		"offline_access openid profile User.Read Mail.Read Mail.ReadWrite Mail.Send"))

	if c.ClientID == "" {
		return nil, fmt.Errorf("MS_CLIENT_ID is required (see README for app registration steps)")
	}

	// See the PublicBaseURL field comment: the hosted-auth redirect allowlist
	// exempts this origin, and there is no safe fallback for it.
	if c.PublicBaseURL == "" {
		return nil, fmt.Errorf("PUBLIC_BASE_URL is required: set it to this deployment's own origin " +
			"(http://localhost:8080 for a local run, or the https tunnel origin when you want push notifications)")
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
