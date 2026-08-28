package config

import (
	"net/url"
	"strings"
	"testing"
)

// validKey is a throwaway base64-encoded 32-byte key, just enough to satisfy
// decodeKey so Load() can get to the fields this test actually cares about.
const validKey = "MDEyMzQ1Njc4OTAxMjM0NTY3ODkwMTIzNDU2Nzg5MDE="

// PUBLIC_BASE_URL is not a tuning knob: the hosted-auth redirect allowlist
// exempts this server's own origin, and without a configured origin there is
// nothing to compare against but the caller-controlled Host header. So Load
// refuses to start rather than leave that exemption forgeable, and the error
// has to name both the variable and the value a local run wants.
func TestLoadRequiresPublicBaseURL(t *testing.T) {
	t.Setenv("MS_CLIENT_ID", "client-id")
	t.Setenv("TOKEN_ENCRYPTION_KEY", validKey)
	t.Setenv("PUBLIC_BASE_URL", "")

	_, err := Load()
	if err == nil {
		t.Fatal("Load() with no PUBLIC_BASE_URL succeeded, want an error")
	}
	for _, want := range []string{"PUBLIC_BASE_URL", "http://localhost:8080"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Load() error = %q, want it to mention %q", err, want)
		}
	}

	t.Setenv("PUBLIC_BASE_URL", "https://um.example.com/")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() with PUBLIC_BASE_URL set: %v", err)
	}
	if cfg.PublicBaseURL != "https://um.example.com" {
		t.Errorf("PublicBaseURL = %q, want the trailing slash trimmed", cfg.PublicBaseURL)
	}
}

// A password containing both @ and # defeats a naive url.Parse: '#' starts a
// URL fragment and '@' separates userinfo from host, so an unescaped
// password carrying either (here, both at once) either fails to parse or —
// worse — silently mis-splits into the wrong host without any error at all.
// Load must recognize either failure mode and repair the DSN by
// percent-encoding just the password segment, rather than refusing to start
// (or, worse, connecting to the wrong place) whenever a generated password
// happens to contain one of these characters.
func TestLoadRepairsUnescapedPostgresPassword(t *testing.T) {
	t.Setenv("MS_CLIENT_ID", "client-id")
	t.Setenv("TOKEN_ENCRYPTION_KEY", validKey)
	t.Setenv("PUBLIC_BASE_URL", "http://localhost:8080")
	t.Setenv("DB_DRIVER", "postgres")
	t.Setenv("DATABASE_URL", "postgresql://postgres:p@ss#1@db.example.com:5432/postgres?sslmode=require")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	u, err := url.Parse(cfg.DBDSN)
	if err != nil {
		t.Fatalf("repaired DBDSN does not parse: %v", err)
	}
	if u.Host != "db.example.com:5432" {
		t.Errorf("host = %q, want %q", u.Host, "db.example.com:5432")
	}
	if pass, ok := u.User.Password(); !ok || pass != "p@ss#1" {
		t.Errorf("password = %q, ok=%v, want %q", pass, ok, "p@ss#1")
	}
	if u.Path != "/postgres" {
		t.Errorf("path = %q, want /postgres", u.Path)
	}
	if u.RawQuery != "sslmode=require" {
		t.Errorf("query = %q, want sslmode=require", u.RawQuery)
	}
	if u.Fragment != "" {
		t.Errorf("fragment = %q, want empty (the password swallowed the rest of the URL)", u.Fragment)
	}
}

// A DATABASE_URL that isn't a postgres connection string at all (no "://",
// so there is no password segment to repair) must still fail with a plain
// error rather than panicking or silently proceeding with garbage.
func TestLoadRejectsUnrepairablePostgresURL(t *testing.T) {
	t.Setenv("MS_CLIENT_ID", "client-id")
	t.Setenv("TOKEN_ENCRYPTION_KEY", validKey)
	t.Setenv("PUBLIC_BASE_URL", "http://localhost:8080")
	t.Setenv("DB_DRIVER", "postgres")
	t.Setenv("DATABASE_URL", "not-a-url at all")

	if _, err := Load(); err == nil {
		t.Fatal("Load() with a garbage DATABASE_URL succeeded, want an error")
	}
}

func TestLoadWhatsAppDefaultsAndParsing(t *testing.T) {
	cases := []struct {
		name            string
		enabled         string
		maxAccounts     string
		deviceName      string
		wantEnabled     bool
		wantMaxAccounts int
		wantDeviceName  string
	}{
		{
			name:            "defaults when unset",
			wantEnabled:     false,
			wantMaxAccounts: 200,
			wantDeviceName:  "Unified Messaging",
		},
		{
			name:            "enabled via 1",
			enabled:         "1",
			wantEnabled:     true,
			wantMaxAccounts: 200,
			wantDeviceName:  "Unified Messaging",
		},
		{
			name:            "enabled via true, case-insensitive",
			enabled:         "TRUE",
			wantEnabled:     true,
			wantMaxAccounts: 200,
			wantDeviceName:  "Unified Messaging",
		},
		{
			name:            "explicit false stays disabled",
			enabled:         "false",
			wantEnabled:     false,
			wantMaxAccounts: 200,
			wantDeviceName:  "Unified Messaging",
		},
		{
			name:            "max accounts and device name override",
			enabled:         "1",
			maxAccounts:     "50",
			deviceName:      "Acme Bot",
			wantEnabled:     true,
			wantMaxAccounts: 50,
			wantDeviceName:  "Acme Bot",
		},
		{
			name:            "invalid max accounts falls back to default",
			maxAccounts:     "not-a-number",
			wantEnabled:     false,
			wantMaxAccounts: 200,
			wantDeviceName:  "Unified Messaging",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Required by Load() regardless of what this test is checking.
			t.Setenv("MS_CLIENT_ID", "client-id")
			t.Setenv("TOKEN_ENCRYPTION_KEY", validKey)
			t.Setenv("PUBLIC_BASE_URL", "http://localhost:8080")

			t.Setenv("WHATSAPP_ENABLED", tc.enabled)
			t.Setenv("WHATSAPP_MAX_ACCOUNTS", tc.maxAccounts)
			t.Setenv("WHATSAPP_DEVICE_NAME", tc.deviceName)

			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			if cfg.WhatsAppEnabled != tc.wantEnabled {
				t.Errorf("WhatsAppEnabled = %v, want %v", cfg.WhatsAppEnabled, tc.wantEnabled)
			}
			if cfg.WhatsAppMaxAccounts != tc.wantMaxAccounts {
				t.Errorf("WhatsAppMaxAccounts = %d, want %d", cfg.WhatsAppMaxAccounts, tc.wantMaxAccounts)
			}
			if cfg.WhatsAppDeviceName != tc.wantDeviceName {
				t.Errorf("WhatsAppDeviceName = %q, want %q", cfg.WhatsAppDeviceName, tc.wantDeviceName)
			}
		})
	}
}
