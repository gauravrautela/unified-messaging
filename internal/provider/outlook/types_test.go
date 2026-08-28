package outlook

import (
	"encoding/json"
	"testing"
)

func TestMessageToModel(t *testing.T) {
	raw := `{
	  "id": "AAMkAGI2",
	  "conversationId": "CONV1",
	  "parentFolderId": "F_INBOX",
	  "subject": "Quarterly numbers",
	  "bodyPreview": "Here they are",
	  "body": {"contentType": "html", "content": "<p>Here they are</p>"},
	  "from": {"emailAddress": {"name": "Ada", "address": "ada@example.com"}},
	  "toRecipients": [{"emailAddress": {"name": "Bob", "address": "bob@example.com"}}],
	  "ccRecipients": [],
	  "receivedDateTime": "2026-08-20T09:30:00Z",
	  "isRead": false,
	  "isDraft": false,
	  "hasAttachments": true,
	  "flag": {"flagStatus": "flagged"},
	  "internetMessageId": "<abc@example.com>"
	}`
	var m graphMessage
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatal(err)
	}

	e := m.toModel("acc_1")
	if e.AccountID != "acc_1" || e.ID != "AAMkAGI2" || e.ThreadID != "CONV1" {
		t.Fatalf("identity mapped wrong: %+v", e)
	}
	if e.From.Email != "ada@example.com" || e.From.Name != "Ada" {
		t.Fatalf("from = %+v", e.From)
	}
	if len(e.To) != 1 || e.To[0].Email != "bob@example.com" {
		t.Fatalf("to = %+v", e.To)
	}
	// Subscribers should not have to strip HTML themselves.
	if e.BodyPlain != "Here they are" {
		t.Fatalf("body_plain = %q, want plain text of the html body", e.BodyPlain)
	}
	if e.Read || !e.Flagged || !e.HasAttachments {
		t.Fatalf("flags wrong: read=%v flagged=%v att=%v", e.Read, e.Flagged, e.HasAttachments)
	}
	if e.Date.Format("2006-01-02T15:04:05Z") != "2026-08-20T09:30:00Z" {
		t.Fatalf("date = %v", e.Date)
	}
	if e.BodyType != "html" {
		t.Fatalf("body type = %q", e.BodyType)
	}
}

// Drafts and items in Sent Items often carry `sender` but a null `from`.
// Falling back keeps the author from rendering blank.
func TestMessageToModelFallsBackToSender(t *testing.T) {
	raw := `{
	  "id": "M2",
	  "from": null,
	  "sender": {"emailAddress": {"name": "Me", "address": "me@outlook.com"}},
	  "sentDateTime": "2026-08-21T10:00:00Z",
	  "isDraft": true
	}`
	var m graphMessage
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatal(err)
	}
	e := m.toModel("acc_1")
	if e.From.Email != "me@outlook.com" {
		t.Fatalf("expected sender fallback, got %+v", e.From)
	}
	if e.Date.IsZero() {
		t.Fatal("expected sentDateTime fallback for a draft with no receivedDateTime")
	}
	if !e.Draft {
		t.Fatal("draft flag lost")
	}
}

func TestRemovedMessageIsDetected(t *testing.T) {
	var m graphMessage
	if err := json.Unmarshal([]byte(`{"id":"M3","@removed":{"reason":"deleted"}}`), &m); err != nil {
		t.Fatal(err)
	}
	if m.Removed == nil || m.Removed.Reason != "deleted" {
		t.Fatalf("@removed not parsed: %+v", m.Removed)
	}
}

func TestPlainTextStripsMarkupAndScripts(t *testing.T) {
	got := plainText(`<style>p{color:red}</style><p>Hello&nbsp;&amp; welcome</p><script>x()</script>`, "html")
	want := "Hello & welcome"
	if got != want {
		t.Fatalf("snippet = %q, want %q", got, want)
	}
}

func TestTextToHTMLEscapes(t *testing.T) {
	got := textToHTML("a < b\nnext")
	want := "<div>a &lt; b<br>next</div>"
	if got != want {
		t.Fatalf("textToHTML = %q, want %q", got, want)
	}
}
