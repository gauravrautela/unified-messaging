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
		"subject":    "hello there",
		"body":       "<p>hi</p>",
		"attachments": []any{map[string]any{
			"name": "a.txt", "content": "AAAA",
		}},
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
	// Message content is replaced by a length marker: the text never reaches
	// the log, but its size stays visible for debugging.
	if out["body"] != "[9 chars]" {
		t.Fatalf("body not length-marked: %v", out["body"])
	}
	if got := out["attachments"].([]any)[0].(map[string]any)["content"]; got != "[4 chars]" {
		t.Fatalf("attachment content not length-marked: %v", got)
	}
	if out["attachments"].([]any)[0].(map[string]any)["name"] != "a.txt" {
		t.Fatal("attachment name changed")
	}
	// Envelope fields are not bodies and stay readable.
	if out["subject"] != "hello there" {
		t.Fatalf("subject changed: %v", out["subject"])
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
