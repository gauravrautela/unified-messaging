package logx

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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

// A digest is stable, short, and does not leak the value it stands for.
func TestDigest(t *testing.T) {
	const phone = "919888000000@s.whatsapp.net"
	d := Digest(phone)
	if d != Digest(phone) {
		t.Fatal("digest must be stable for the same input")
	}
	if d == Digest("919888000001@s.whatsapp.net") {
		t.Fatal("different inputs must digest differently")
	}
	if !strings.HasPrefix(d, "h_") || len(d) != len("h_")+12 {
		t.Fatalf("digest = %q, want h_ + 12 hex chars", d)
	}
	if strings.Contains(d, "919888000000") {
		t.Fatalf("digest leaks its input: %q", d)
	}
	if Digest("") == "" {
		t.Fatal("digest of an empty string must still be a handle")
	}
}

// A digest is the mechanism behind the spec's "never logged: phone numbers of
// attendees". An unkeyed truncated SHA-256 does not provide that: an E.164 JID
// is drawn from a space small enough that a rainbow table over a country's
// numbering plan inverts every handle. Keying it is what makes the doc
// comment's claim true.
func TestDigestIsKeyed(t *testing.T) {
	const phone = "919888000000@s.whatsapp.net"

	SetDigestKey([]byte("00000000000000000000000000000001"))
	first := Digest(phone)
	if first != Digest(phone) {
		t.Fatal("digest must stay stable within a process for a fixed key")
	}

	SetDigestKey([]byte("00000000000000000000000000000002"))
	second := Digest(phone)
	if first == second {
		t.Fatal("digest is not keyed: two different keys produced the same handle")
	}
	if !strings.HasPrefix(second, "h_") || len(second) != len("h_")+12 {
		t.Fatalf("keying changed the handle shape: %q", second)
	}

	// The plain SHA-256 an attacker would precompute must not match.
	sum := sha256.Sum256([]byte(phone))
	if unkeyed := "h_" + hex.EncodeToString(sum[:])[:12]; second == unkeyed || first == unkeyed {
		t.Fatal("digest is still a bare truncated SHA-256")
	}

	// An empty key is ignored rather than silently weakening the digest.
	SetDigestKey(nil)
	if Digest(phone) != second {
		t.Fatal("SetDigestKey(nil) must not replace the key in use")
	}
}
