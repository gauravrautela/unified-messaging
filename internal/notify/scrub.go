// Package notify turns events into notifications for chat targets (Discord,
// Telegram) and delivers them, next to the raw JSON webhook.
package notify

import (
	"fmt"
	"regexp"
	"strings"
)

var (
	// telegramToken matches only Telegram's actual bot-token shape
	// (<digits>:<[A-Za-z0-9_-]+>), so a customer webhook path that merely
	// starts with "/bot" — e.g. "/bottle/of/wine" or "/botdetection/" — is
	// left untouched.
	telegramToken = regexp.MustCompile(`/bot\d+:[A-Za-z0-9_-]+/`)
	// discordToken requires an actual Discord webhook host (discord.com or
	// discordapp.com, optionally subdomained) so a developer's own webhook
	// URL that merely happens to look like "/api/webhooks/<id>/<token>" is
	// never masked.
	discordToken = regexp.MustCompile(`(?i)(https?://(?:[a-z0-9-]+\.)?discord(?:app)?\.com/api/webhooks/\d+)/[^/\s"?]+`)
)

// Scrub removes credentials that transports embed in URLs: the Telegram bot
// token path segment and the Discord webhook token.
func Scrub(s string) string {
	s = telegramToken.ReplaceAllString(s, "/bot•••/")
	return discordToken.ReplaceAllString(s, "$1/•••")
}

// ScrubErr is Scrub for errors; nil stays nil.
func ScrubErr(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s", Scrub(err.Error()))
}

// MaskDiscordURL hides the token segment of a Discord incoming-webhook URL
// for logs; other URLs pass through unchanged.
func MaskDiscordURL(u string) string { return discordToken.ReplaceAllString(u, "$1/•••") }

// MaskPhone keeps the country code and first two digits plus the last three:
// +919888000855 -> "+91 98••• •855". Short or odd values keep their first
// two characters only.
func MaskPhone(p string) string {
	if p == "" {
		return ""
	}
	digits := strings.TrimPrefix(p, "+")
	if len(digits) < 8 {
		if len(digits) <= 2 {
			return p
		}
		return p[:len(p)-len(digits)+2] + "•••"
	}
	cc := ""
	rest := digits
	// Country codes are 1–3 digits; take 1 for +1, 2 otherwise (good enough
	// for a notification — this is display, not parsing).
	if strings.HasPrefix(p, "+") {
		n := 2
		if strings.HasPrefix(digits, "1") {
			n = 1
		}
		cc, rest = "+"+digits[:n], digits[n:]
	}
	if len(rest) < 5 {
		return cc + " " + rest[:1] + "•••"
	}
	return strings.TrimSpace(cc + " " + rest[:2] + "••• •" + rest[len(rest)-3:])
}
