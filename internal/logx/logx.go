// Package logx carries a per-request logger through context and redacts
// secrets before anything user-supplied is logged.
package logx

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"strconv"
	"strings"
	"sync"
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

// Digest returns a short, stable, one-way handle for an identifier that is
// itself sensitive — a chat id that happens to be a phone number, say.
//
// It is for correlation, not identification: the same input always yields the
// same handle, so lines about one conversation can be tied together, but the
// original value cannot be read back out of a log.
func Digest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "h_" + hex.EncodeToString(sum[:])[:12]
}

// secretKeys are matched as substrings of lower-cased map keys.
// Substring matching deliberately over-redacts (e.g., keyword, zip_code) because
// under-redaction is the worse failure mode for security.
var secretKeys = []string{"password", "secret", "token", "key", "code", "verifier", "cookie", "authorization", "client_state", "clientstate", "session"}

// contentKeys hold message content rather than secrets: mail bodies and
// base64 attachment payloads. Per the logging rules they are logged as a size
// only, never as text — but unlike a secret their length is worth keeping,
// because "did the body arrive at all" is the usual debugging question.
// "body" and "content" match as substrings (body_plain, body_html,
// content_bytes); "html" and "text" match exactly, since substring matching
// would swallow innocuous keys like "context" or "text_direction".
var contentKeys = []string{"body", "content"}
var contentKeysExact = []string{"html", "text"}

// Redact returns a copy of v with secret-looking map values replaced and
// message content reduced to a length marker. It walks maps and slices
// produced by encoding/json; other values pass through.
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
