package logx

import (
	"context"
	"strings"
	"testing"
)

func TestRedactMasksSecretFields(t *testing.T) {
	in := map[string]any{
		"email":      "a@b.com",
		"password":   "hunter22",
		"session_id": "sess-abc",
		"nested":     map[string]any{"secret": "s3", "token": "t0k", "key": "k", "code": "c0de", "ok": "fine"},
		"list":       []any{map[string]any{"refresh_token": "rt"}},
	}
	out := Redact(in).(map[string]any)
	if out["email"] != "a@b.com" {
		t.Fatalf("non-secret changed: %v", out["email"])
	}
	if out["password"] != "[redacted]" {
		t.Fatalf("password not redacted: %v", out["password"])
	}
	if out["session_id"] != "[redacted]" {
		t.Fatalf("session_id not redacted: %v", out["session_id"])
	}
	n := out["nested"].(map[string]any)
	for _, k := range []string{"secret", "token", "key", "code"} {
		if n[k] != "[redacted]" {
			t.Fatalf("%s not redacted: %v", k, n[k])
		}
	}
	if n["ok"] != "fine" {
		t.Fatal("non-secret nested value changed")
	}
	if out["list"].([]any)[0].(map[string]any)["refresh_token"] != "[redacted]" {
		t.Fatal("refresh_token in list not redacted")
	}
}

func TestContextLoggerRoundTrip(t *testing.T) {
	log, recs := Capture()
	ctx := With(context.Background(), log.With("request_id", "req_x"))
	From(ctx).Info("hello")
	if !recs.Contains("request_id=req_x") || !recs.Contains("hello") {
		t.Fatalf("record missing fields: %v", recs.All())
	}
	if From(context.Background()) == nil {
		t.Fatal("From must fall back to a default logger")
	}
}

func TestRequestIDShape(t *testing.T) {
	id := NewRequestID()
	if !strings.HasPrefix(id, "req_") || len(id) != 4+16 {
		t.Fatalf("id = %q", id)
	}
}
