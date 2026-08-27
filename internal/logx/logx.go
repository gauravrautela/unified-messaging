// Package logx carries a per-request logger through context and redacts
// secrets before anything user-supplied is logged.
package logx

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

type ctxKey struct{}

// With attaches log to ctx. Handlers and stores retrieve it with From.
func With(ctx context.Context, log *slog.Logger) context.Context {
	return context.WithValue(ctx, ctxKey{}, log)
}

// From returns the logger attached to ctx, or the default logger.
func From(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(ctxKey{}).(*slog.Logger); ok && l != nil {
		return l
	}
	return slog.Default()
}

// NewRequestID returns "req_" + 16 hex chars.
func NewRequestID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "req_" + hex.EncodeToString(b)
}

// RandomToken returns nBytes of crypto/rand entropy, base64url-encoded
// without padding — so 32 bytes become 43 URL- and cookie-safe characters.
// It panics if the system has no entropy: every caller is minting a
// credential, and a predictable one is worse than a crash.
func RandomToken(nBytes int) string {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		panic("logx: no entropy for a random token: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// digestKey keys Digest. It defaults to a value generated once per process, so
// a service that never calls SetDigestKey still gets handles nobody can
// precompute — they just stop correlating across restarts. Read on every
// logged chat id, from every actor goroutine, so it is swapped atomically.
var digestKey atomic.Pointer[[]byte]

func init() {
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		panic("logx: no entropy for the digest key: " + err.Error())
	}
	digestKey.Store(&k)
}

// SetDigestKey fixes the key Digest uses, so handles stay comparable across
// restarts and across the instances of a deployment. Call it once, at startup,
// before anything digests: the service passes the same 32-byte key it seals
// tokens with. An empty key is ignored — it would be a silent downgrade to the
// unkeyed hash this exists to avoid.
func SetDigestKey(key []byte) {
	if len(key) == 0 {
		return
	}
	k := append([]byte(nil), key...)
	digestKey.Store(&k)
}

// Digest returns a short, stable, one-way handle for an identifier that is
// itself sensitive — a chat id that happens to be a phone number, say.
//
// It is for correlation, not identification: within a process (and across
// processes sharing a key via SetDigestKey) the same input always yields the
// same handle, so lines about one conversation can be tied together.
//
// It is an HMAC, not a bare hash, precisely because the inputs come from a
// small space: an E.164 number has around 10^12 candidates, so an unkeyed
// truncated SHA-256 is invertible by anyone who can build a table over a
// country's numbering plan. Without the key, a handle cannot be tied back to
// the number that produced it.
func Digest(s string) string {
	mac := hmac.New(sha256.New, *digestKey.Load())
	mac.Write([]byte(s))
	return "h_" + hex.EncodeToString(mac.Sum(nil))[:12]
}

// secretKeys are matched as substrings of lower-cased map keys.
// Substring matching deliberately over-redacts (e.g., keyword, zip_code) because
// under-redaction is the worse failure mode for security.
// "bot_token" is listed explicitly even though the "token" substring already
// covers it, so a Telegram bot token's redaction is not accidental.
var secretKeys = []string{"password", "secret", "token", "bot_token", "key", "code", "verifier", "cookie", "authorization", "client_state", "clientstate", "session"}

// contentKeys hold message content rather than secrets: mail bodies and
// base64 attachment payloads. Per the logging rules they are logged as a size
// only, never as text — but unlike a secret their length is worth keeping,
// because "did the body arrive at all" is the usual debugging question.
// "body" and "content" match as substrings (body_plain, body_html,
// content_bytes); "html" and "text" match exactly, since substring matching
// would swallow innocuous keys like "context" or "text_direction".
var contentKeys = []string{"body", "content"}
var contentKeysExact = []string{"html", "text"}

// Redact returns a copy of v with secret-looking map values replaced,
// message content reduced to a length marker, and email/phone-looking values
// masked. It walks maps and slices produced by encoding/json; other values
// pass through.
//
// The checks run in a fixed order — secret, then content, then email/phone —
// so a key that happens to match more than one (there is none today, but the
// substring matching is deliberately loose) always redacts fully rather than
// getting the weaker masking treatment.
func Redact(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			switch {
			case isSecret(k):
				out[k] = "[redacted]"
			case isContent(k):
				out[k] = contentMarker(val)
			case isEmailKey(k):
				out[k] = maskEmailValue(val)
			case isPhoneKey(k):
				out[k] = maskPhoneValue(val)
			default:
				out[k] = Redact(val)
			}
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = Redact(val)
		}
		return out
	default:
		return v
	}
}

func isSecret(k string) bool {
	lk := strings.ToLower(k)
	for _, s := range secretKeys {
		if strings.Contains(lk, s) {
			return true
		}
	}
	return false
}

func isContent(k string) bool {
	lk := strings.ToLower(k)
	for _, s := range contentKeys {
		if strings.Contains(lk, s) {
			return true
		}
	}
	for _, s := range contentKeysExact {
		if lk == s {
			return true
		}
	}
	return false
}

// contentMarker replaces a content string with its length. Non-strings keep
// walking: a structured body ({"html": ..., "text": ...}) still has its own
// leaf strings marked.
func contentMarker(v any) any {
	if s, ok := v.(string); ok {
		return "[" + strconv.Itoa(len(s)) + " chars]"
	}
	return Redact(v)
}

// isEmailKey matches any key naming an email address: "email", "to_email",
// "notify_email", and so on.
func isEmailKey(k string) bool {
	return strings.Contains(strings.ToLower(k), "email")
}

// isPhoneKey matches keys naming a phone number. "identifier" is included
// exactly (not as a substring, to avoid swallowing unrelated *_identifier
// keys) because that is the field name a chat account's own number rides in.
func isPhoneKey(k string) bool {
	lk := strings.ToLower(k)
	return strings.Contains(lk, "phone") || lk == "identifier"
}

// maskEmailValue masks a string value as an email address; a non-string
// value keeps walking, the same way contentMarker does for content keys.
func maskEmailValue(v any) any {
	if s, ok := v.(string); ok {
		return maskEmail(s)
	}
	return Redact(v)
}

// maskPhoneValue masks a string value as a phone number; a non-string value
// keeps walking, the same way contentMarker does for content keys.
func maskPhoneValue(v any) any {
	if s, ok := v.(string); ok {
		return maskPhone(s)
	}
	return Redact(v)
}

// maskEmail keeps the first rune of the local part and the whole domain:
// "john.doe@example.com" -> "j•••@example.com". A value with no "@" is not
// actually an email address, so it is masked down to "•••" entirely rather
// than partially revealed.
func maskEmail(s string) string {
	local, domain, ok := strings.Cut(s, "@")
	if !ok || local == "" {
		return "•••"
	}
	r := []rune(local)
	return string(r[0]) + "•••@" + domain
}

// maskPhone keeps the country code and first two digits plus the last three:
// +919888000855 -> "+91 98••• •855". Short or odd values keep their first two
// characters only.
//
// This is a byte-for-byte copy of notify.MaskPhone (internal/notify/scrub.go).
// logx cannot import notify to reuse it directly — notify depends on packages
// that already import logx, so doing so would create an import cycle — so the
// algorithm is duplicated here instead. Keep the two in sync by hand.
func maskPhone(p string) string {
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

// Records collects text log lines for assertions in tests.
type Records struct {
	mu    sync.Mutex
	lines []string
}

func (r *Records) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, string(p))
	return len(p), nil
}

func (r *Records) All() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.lines...)
}

func (r *Records) Contains(sub string) bool {
	for _, l := range r.All() {
		if strings.Contains(l, sub) {
			return true
		}
	}
	return false
}

// Capture returns a DEBUG-level logger whose output is kept in memory.
func Capture() (*slog.Logger, *Records) {
	recs := &Records{}
	return slog.New(slog.NewTextHandler(recs, &slog.HandlerOptions{Level: slog.LevelDebug})), recs
}
