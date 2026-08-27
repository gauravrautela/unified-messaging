package logx

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
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
	// email is not a secret, but it is PII: masked, not redacted outright and
	// not left in clear either.
	if out["email"] != "a•••@b.com" {
		t.Fatalf("email not masked: %v", out["email"])
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

// Email and phone values are masked wherever they appear, at any depth, and
// regardless of which of the recognised key names carries them.
func TestRedactMasksEmailAndPhoneValues(t *testing.T) {
	in := map[string]any{
		"to":         []any{map[string]any{"email": "john.doe@example.com"}},
		"phone":      "+919888000855",
		"identifier": "+15551234567",
		"subject":    "keep",
	}
	out := Redact(in).(map[string]any)
	toEmail := out["to"].([]any)[0].(map[string]any)["email"]
	if toEmail != "j•••@example.com" {
		t.Fatalf("nested email = %v, want j•••@example.com", toEmail)
	}
	if out["phone"] != "+91 98••• •855" {
		t.Fatalf("phone = %v, want +91 98••• •855", out["phone"])
	}
	if out["identifier"] != "+1 55••• •567" {
		t.Fatalf("identifier = %v, want +1 55••• •567", out["identifier"])
	}
	if out["subject"] != "keep" {
		t.Fatalf("subject changed: %v", out["subject"])
	}
	// No raw address or number may survive into the redacted copy.
	for _, leak := range []string{"john.doe@example.com", "919888000855", "5551234567"} {
		if strings.Contains(toString(out), leak) {
			t.Fatalf("redacted output leaked %q: %v", leak, out)
		}
	}
}

// M-2: an array *under* one of these keys is masked element by element.
// The *Value helpers fell through to Redact for anything that was not a
// string, and Redact's []any branch recurses with Redact — whose default
// case returns a bare string untouched. So {"emails":["victim@x.com"]} was
// logged verbatim at DEBUG. Nothing the API itself decodes has that shape,
// but logBody logs whatever JSON arrived before decoding, so any
// authenticated caller could pick the key.
func TestRedactMasksArraysUnderEmailPhoneAndContentKeys(t *testing.T) {
	in := map[string]any{
		"emails":     []any{"victim@x.com", "second@y.org"},
		"phones":     []any{"+919888000855"},
		"identifier": []any{"+15551234567"},
		"body":       []any{"the whole message text"},
	}
	out := Redact(in).(map[string]any)
	if got := out["emails"].([]any); got[0] != "v•••@x.com" || got[1] != "s•••@y.org" {
		t.Fatalf("emails = %v, want each address masked", got)
	}
	if got := out["phones"].([]any)[0]; got != "+91 98••• •855" {
		t.Fatalf("phones[0] = %v, want it masked", got)
	}
	if got := out["identifier"].([]any)[0]; got != "+1 55••• •567" {
		t.Fatalf("identifier[0] = %v, want it masked", got)
	}
	if got := out["body"].([]any)[0]; got != "[22 chars]" {
		t.Fatalf("body[0] = %v, want a length marker", got)
	}
	for _, leak := range []string{"victim@x.com", "second@y.org", "919888000855", "5551234567", "whole message"} {
		if strings.Contains(toString(out), leak) {
			t.Fatalf("redacted output leaked %q: %v", leak, out)
		}
	}
}

// An email with no "@" is not really an email; it is masked down entirely
// rather than partially revealed, and an empty phone/identifier value stays
// empty rather than becoming a mask of nothing.
func TestRedactMasksMalformedEmailAndEmptyPhone(t *testing.T) {
	in := map[string]any{"email": "not-an-email", "phone": ""}
	out := Redact(in).(map[string]any)
	if out["email"] != "•••" {
		t.Fatalf("malformed email = %v, want •••", out["email"])
	}
	if out["phone"] != "" {
		t.Fatalf("empty phone = %v, want empty", out["phone"])
	}
}

// toString is a crude stringifier good enough for a "did this leak"
// substring check across nested maps and slices.
func toString(v any) string {
	return fmt.Sprintf("%v", v)
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
