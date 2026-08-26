package notify

import (
	"errors"
	"strings"
	"testing"
)

func TestMaskPhone(t *testing.T) {
	cases := map[string]string{
		"+919888000855": "+91 98••• •855",
		"+15551234567":  "+1 55••• •567",
		"12345":         "12•••",
		"":              "",
	}
	for in, want := range cases {
		if got := MaskPhone(in); got != want {
			t.Errorf("MaskPhone(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMaskDiscordURL(t *testing.T) {
	got := MaskDiscordURL("https://discord.com/api/webhooks/1234567890/AbCdEf-ghIJ_kl")
	if got != "https://discord.com/api/webhooks/1234567890/•••" {
		t.Fatalf("got %q", got)
	}
	if MaskDiscordURL("https://x.example.com/hook") != "https://x.example.com/hook" {
		t.Fatal("non-discord URL must pass through")
	}
}

func TestScrubErrHidesTelegramTokenAndDiscordToken(t *testing.T) {
	err := ScrubErr(errors.New(`Post "https://api.telegram.org/bot123456:ABC-def_GHI/sendMessage": dial tcp: timeout`))
	if strings.Contains(err.Error(), "123456:ABC") || !strings.Contains(err.Error(), "bot•••/sendMessage") {
		t.Fatalf("telegram not scrubbed: %v", err)
	}
	err = ScrubErr(errors.New("status 404 from https://discord.com/api/webhooks/42/s3cr3t-token"))
	if strings.Contains(err.Error(), "s3cr3t") || !strings.Contains(err.Error(), "/api/webhooks/42/•••") {
		t.Fatalf("discord not scrubbed: %v", err)
	}
	if ScrubErr(nil) != nil {
		t.Fatal("nil stays nil")
	}
}
