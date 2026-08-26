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

func TestScrubDoesNotOverMatchNonTelegramBotPaths(t *testing.T) {
	unchanged := "https://example.com/bottle/of/wine"
	if got := Scrub(unchanged); got != unchanged {
		t.Fatalf("Scrub masked a non-token /bot path: %q", got)
	}
	got := Scrub("https://api.telegram.org/bot123456:ABC-def_GHI/sendMessage")
	if !strings.Contains(got, "/bot•••/sendMessage") {
		t.Fatalf("real telegram token not scrubbed: %q", got)
	}
}

func TestScrubAndMaskDiscordURLRequireDiscordHost(t *testing.T) {
	nonDiscord := "https://myservice.example.com/api/webhooks/42/secret"
	if got := MaskDiscordURL(nonDiscord); got != nonDiscord {
		t.Fatalf("MaskDiscordURL masked a non-discord host: %q", got)
	}
	if got := Scrub(nonDiscord); got != nonDiscord {
		t.Fatalf("Scrub masked a non-discord host: %q", got)
	}
	for _, host := range []string{"discord.com", "discordapp.com"} {
		u := "https://" + host + "/api/webhooks/42/s3cr3t"
		want := "https://" + host + "/api/webhooks/42/•••"
		if got := MaskDiscordURL(u); got != want {
			t.Errorf("MaskDiscordURL(%q) = %q, want %q", u, got, want)
		}
		if got := Scrub(u); got != want {
			t.Errorf("Scrub(%q) = %q, want %q", u, got, want)
		}
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
