// Package logx carries a per-request logger through context and redacts
// secrets before anything user-supplied is logged.
package logx

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
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

// secretKeys are matched as substrings of lower-cased map keys.
var secretKeys = []string{"password", "secret", "token", "key", "code", "verifier", "cookie", "authorization", "client_state", "clientstate"}

// Redact returns a copy of v with secret-looking map values replaced. It
// walks maps and slices produced by encoding/json; other values pass through.
func Redact(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			if isSecret(k) {
				out[k] = "[redacted]"
			} else {
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
