// Package config loads POC settings from the environment.
package config

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	ListenAddr string
	DBPath     string

	// DBDriver is the persistence engine: "sqlite" (the default, a local
	// file) or "postgres" (a managed database such as Supabase).
	DBDriver string
	// DBDSN is what Open connects with: the DB_PATH file for sqlite, the
	// DATABASE_URL connection string for postgres. It is never logged — a
	// postgres URL carries the password.
	DBDSN string
	// DBMaxOpenConns caps the connection pool. Ignored on sqlite, which is
	// deliberately pinned to a single writer; on postgres it must stay under
	// whatever the provider allows (Supabase's pooler included).
	DBMaxOpenConns int

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
	// WhatsAppRosterGroups controls whether connecting an account also scans
	// its joined groups for the roster. whatsmeow persists every member LID
	// mapping that scan returns one statement at a time while holding the
	// lock every inbound decrypt needs, so on a high-latency database the
	// scan blocks live messages for as long as it runs. Off, groups are still
	// mirrored as their messages arrive; only the up-front names and member
	// lists are skipped.
	WhatsAppRosterGroups bool

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
		ListenAddr: listenAddr(),
		DBPath:     env("DB_PATH", "./unified-messaging.db"),
		DBDriver:   env("DB_DRIVER", "sqlite"),

		DBMaxOpenConns: envInt("DB_MAX_OPEN_CONNS", 10),
		PublicBaseURL:  strings.TrimRight(os.Getenv("PUBLIC_BASE_URL"), "/"),
		ClientID:       os.Getenv("MS_CLIENT_ID"),
		ClientSecret:   os.Getenv("MS_CLIENT_SECRET"),
		Tenant:         env("MS_TENANT", "consumers"),
		RedirectURI:    env("MS_REDIRECT_URI", "http://localhost:8080/oauth/callback"),
		SessionTTL:     time.Duration(envInt("SESSION_TTL_DAYS", 30)) * 24 * time.Hour,
		SessionMaxAge:  time.Duration(envInt("SESSION_MAX_AGE_DAYS", 90)) * 24 * time.Hour,

		WhatsAppEnabled:      envBool("WHATSAPP_ENABLED", false),
		WhatsAppMaxAccounts:  envInt("WHATSAPP_MAX_ACCOUNTS", 200),
		WhatsAppDeviceName:   env("WHATSAPP_DEVICE_NAME", "Unified Messaging"),
		WhatsAppRosterGroups: envBool("WHATSAPP_ROSTER_GROUPS", true),

		TrustProxy: envBool("TRUST_PROXY", false),

		DeliveryRetention: time.Duration(envInt("DELIVERY_RETENTION_DAYS", 7)) * 24 * time.Hour,
	}

	// offline_access is what earns us a refresh token; without it the
	// integration dies the moment the first access token expires.
	c.Scopes = strings.Fields(env("MS_SCOPES",
		"offline_access openid profile User.Read Mail.Read Mail.ReadWrite Mail.Send"))

	// The DSN follows from the driver: a file path for sqlite, DATABASE_URL
	// for postgres. An unset DATABASE_URL is refused rather than silently
	// falling back to a local file the operator did not ask for.
	switch c.DBDriver {
	case "sqlite":
		c.DBDSN = c.DBPath
	case "postgres":
		raw := os.Getenv("DATABASE_URL")
		if raw == "" {
			return nil, fmt.Errorf("DATABASE_URL is required when DB_DRIVER=postgres")
		}
		dsn, err := repairPostgresDSN(raw)
		if err != nil {
			// Never include raw here: it carries the password.
			return nil, err
		}
		c.DBDSN = dsn
	default:
		return nil, fmt.Errorf("DB_DRIVER must be \"sqlite\" or \"postgres\", got %q", c.DBDriver)
	}

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

// repairPostgresDSN validates dsn as a postgres connection URL and, when its
// password segment carries an unescaped character that is structural in a
// URL (@, #, ?, /, %, a space, ...), returns a corrected copy with just that
// segment percent-encoded.
//
// This matters because such a password does not reliably fail loudly: a
// space or a bad %-escape makes url.Parse return an error ("invalid
// userinfo"), but a stray '#' or '@' does not — '#' starts a URL fragment
// and the rest of the string (host, port, path, query) silently becomes
// part of it, so the connection string parses "successfully" into the wrong
// host and database with no error at all. A valid postgres DSN never has a
// URL fragment, so requiring Fragment == "" (and a non-empty Host) catches
// that silent case too, not just the ones url.Parse rejects outright.
func repairPostgresDSN(dsn string) (string, error) {
	invalid := fmt.Errorf("DATABASE_URL is not a valid postgres connection string " +
		"(if the password contains @, #, ?, /, %%, or a space, it must be percent-encoded)")

	schemeEnd := strings.Index(dsn, "://")
	if schemeEnd < 0 {
		return "", invalid
	}
	if dsnLooksValid(dsn) {
		return dsn, nil
	}

	// Locate the password segment by hand rather than trusting url.Parse,
	// which is exactly what just failed (or silently mis-split). The
	// username runs from "://" to the first ':'; the password runs from
	// there to the LAST '@' in the whole string — which is the true
	// userinfo/host separator even when the password itself contains '@',
	// because a legitimate host never contains one.
	rest := dsn[schemeEnd+3:]
	colon := strings.IndexByte(rest, ':')
	if colon < 0 {
		return "", invalid
	}
	passStart := schemeEnd + 3 + colon + 1
	at := strings.LastIndexByte(dsn, '@')
	if at < passStart {
		return "", invalid
	}

	repaired := dsn[:passStart] + percentEncodeUserinfo(dsn[passStart:at]) + dsn[at:]
	if !dsnLooksValid(repaired) {
		return "", invalid
	}
	return repaired, nil
}

// dsnLooksValid reports whether dsn parses as a URL with a real host and no
// fragment — see repairPostgresDSN for why an unwanted fragment, not just a
// parse error, signals a mis-split DSN.
func dsnLooksValid(dsn string) bool {
	u, err := url.Parse(dsn)
	return err == nil && u.Host != "" && u.Fragment == ""
}

// percentEncodeUserinfo percent-encodes every byte that is not safe to place
// literally in a URL's userinfo segment, leaving unreserved characters
// (letters, digits, - . _ ~) untouched.
func percentEncodeUserinfo(s string) string {
	const unreserved = "-._~"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
			b.WriteByte(c)
		case strings.IndexByte(unreserved, c) >= 0:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// PushEnabled reports whether providers can reach us for push notifications.
// Without a public HTTPS origin the service still works, on polling alone.
func (c *Config) PushEnabled() bool {
	return strings.HasPrefix(c.PublicBaseURL, "https://")
}

// listenAddr is LISTEN_ADDR (default :8080), unless the host has assigned a
// port through PORT — the convention on Vercel, Railway, Render, Heroku and
// Cloud Run, all of which health-check exactly that port and kill a server
// that comes up anywhere else. A platform-assigned PORT therefore wins.
func listenAddr() string {
	if p := os.Getenv("PORT"); p != "" {
		return ":" + p
	}
	return env("LISTEN_ADDR", ":8080")
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
