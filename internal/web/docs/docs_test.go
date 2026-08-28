package docs

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestAnchor(t *testing.T) {
	if got := Anchor("POST", "/api/v1/chats/{id}/messages"); got != "post-api-v1-chats-id-messages" {
		t.Fatalf("Anchor = %q", got)
	}
}

func TestEndpointsHaveAnchorsSummariesAndResponses(t *testing.T) {
	seen := map[string]bool{}
	for _, e := range Endpoints {
		if e.Summary == "" || e.Response == "" || e.Anchor != Anchor(e.Method, e.Path) {
			t.Fatalf("%s %s incomplete: %+v", e.Method, e.Path, e)
		}
		if seen[e.Anchor] {
			t.Fatalf("duplicate anchor %s", e.Anchor)
		}
		seen[e.Anchor] = true
	}
	if len(Events) == 0 || len(Errors) == 0 {
		t.Fatal("events/errors empty")
	}
}

// TestGroupedCoversAllEndpoints is the guard against an endpoint whose Group
// is a typo: it would silently vanish from the page, since Grouped only ever
// emits endpoints whose group is one of the known names.
func TestGroupedCoversAllEndpoints(t *testing.T) {
	groups := Grouped()
	total := 0
	for _, g := range groups {
		total += len(g.Endpoints)
	}
	if total != len(Endpoints) {
		t.Fatalf("Grouped covers %d endpoints, have %d", total, len(Endpoints))
	}

	want := []string{"Developer & keys", "Connecting mailboxes", "Accounts", "Mail", "Chat", "Webhooks"}
	if len(groups) != len(want) {
		t.Fatalf("got %d groups, want %d", len(groups), len(want))
	}
	for i, g := range groups {
		if g.Name != want[i] {
			t.Fatalf("group %d = %q, want %q", i, g.Name, want[i])
		}
	}
}

// TestEndpointsHaveGroupsAndSaneParams keeps the reference honest about its
// own vocabulary: an unknown "In" would render an empty column in the params
// table, and a body on a GET is a copy-paste mistake.
func TestEndpointsHaveGroupsAndSaneParams(t *testing.T) {
	for _, e := range Endpoints {
		if e.Method == "" || !strings.HasPrefix(e.Path, "/api/v1/") {
			t.Fatalf("bad route %q %q", e.Method, e.Path)
		}
		if (e.Method == "GET" || e.Method == "DELETE") && e.Request != "" {
			t.Fatalf("%s %s: %s must not document a request body", e.Method, e.Path, e.Method)
		}
		for _, p := range e.Params {
			switch p.In {
			case "path", "query", "body", "header":
			default:
				t.Fatalf("%s %s: param %q has In=%q", e.Method, e.Path, p.Name, p.In)
			}
			if p.Name == "" || p.Desc == "" {
				t.Fatalf("%s %s: param %+v is incomplete", e.Method, e.Path, p)
			}
			if p.In == "path" && !strings.Contains(e.Path, "{"+p.Name+"}") {
				t.Fatalf("%s %s: path param %q is not in the path", e.Method, e.Path, p.Name)
			}
		}
	}
}

// TestJSONSamplesParse catches a stray comma or an unbalanced brace in the
// hand-written samples, which would otherwise ship as broken JSON on the
// docs page. Non-JSON responses (204s, the attachment download) are skipped
// deliberately — they are documented as raw HTTP.
func TestJSONSamplesParse(t *testing.T) {
	check := func(t *testing.T, what, sample string) {
		t.Helper()
		if !strings.HasPrefix(strings.TrimSpace(sample), "{") {
			return
		}
		if !json.Valid([]byte(sample)) {
			t.Errorf("%s: sample is not valid JSON:\n%s", what, sample)
		}
	}
	for _, e := range Endpoints {
		check(t, e.Method+" "+e.Path+" request", e.Request)
		check(t, e.Method+" "+e.Path+" response", e.Response)
	}
	for _, ev := range Events {
		check(t, "event "+ev.Type, ev.Sample)
	}
}

func TestEventsAndErrorsAreComplete(t *testing.T) {
	// Every model.Event* constant. Named literally rather than imported so
	// that renaming a constant without updating the docs is a test failure
	// here, not a silent gap on the page.
	want := []string{
		"mail_received", "mail_sent", "mail_updated", "mail_deleted", "account_status",
		"chat_received", "chat_sent", "chat_updated", "chat_reaction", "chat_deleted",
	}
	have := map[string]bool{}
	for _, e := range Events {
		if e.When == "" || e.Sample == "" || len(e.Kinds) == 0 {
			t.Fatalf("event %q incomplete: %+v", e.Type, e)
		}
		for _, k := range e.Kinds {
			switch k {
			case "webhook", "discord", "telegram":
			default:
				t.Fatalf("event %q names unknown webhook kind %q", e.Type, k)
			}
		}
		have[e.Type] = true
	}
	for _, w := range want {
		if !have[w] {
			t.Errorf("no docs.Event for %s", w)
		}
	}
	if len(Events) != len(want) {
		t.Errorf("got %d events, want %d", len(Events), len(want))
	}

	seen := map[string]bool{}
	for _, e := range Errors {
		if e.Code == "" || e.Fix == "" || e.Status < 400 || e.Status > 599 {
			t.Fatalf("error %+v is incomplete", e)
		}
		if seen[e.Code] {
			t.Fatalf("duplicate error code %s", e.Code)
		}
		seen[e.Code] = true
	}
}

func TestSnippetsAreRunnable(t *testing.T) {
	for name, s := range map[string]Snippet{
		"SendMessage": SendMessage, "HostedAuth": HostedAuth,
		"WebhookPayload": WebhookPayload, "CreateKey": CreateKey,
	} {
		if s.Curl == "" || s.Node == "" || s.Go == "" {
			t.Fatalf("%s: a language pane is empty", name)
		}
	}
	// The send snippet is the one every integrator copies first; it must
	// carry the account scope and the idempotency header in all three.
	for lang, code := range map[string]string{
		"curl": SendMessage.Curl, "node": SendMessage.Node, "go": SendMessage.Go,
	} {
		for _, want := range []string{"account_id", "Idempotency-Key"} {
			if !strings.Contains(code, want) {
				t.Errorf("SendMessage.%s does not mention %q", lang, want)
			}
		}
	}
}
