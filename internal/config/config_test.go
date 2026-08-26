package config

import "testing"

// validKey is a throwaway base64-encoded 32-byte key, just enough to satisfy
// decodeKey so Load() can get to the fields this test actually cares about.
const validKey = "MDEyMzQ1Njc4OTAxMjM0NTY3ODkwMTIzNDU2Nzg5MDE="

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
