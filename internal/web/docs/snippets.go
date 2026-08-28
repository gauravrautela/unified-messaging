package docs

// Snippet is one operation shown three ways. The docs page and the product
// website both render these, which is the point: a curl line that works on
// the marketing page and a different one in the reference would be worse
// than having no marketing page.
//
// Every snippet is runnable as written once $BASE and $API_KEY are set, with
// the {braced} placeholders replaced by real ids.
type Snippet struct {
	Curl, Node, Go string
}

// SendMessage is the one call the whole product is for: send a chat message
// as one of your users.
var SendMessage = Snippet{
	Curl: `curl -X POST "$BASE/api/v1/chats/{chat_id}/messages?account_id={account_id}" \
  -H "Authorization: Bearer $API_KEY" \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: $(uuidgen)" \
  -d '{"text":"Hi Grace — following up on the roadmap."}'`,

	Node: `const res = await fetch(
  ` + "`" + `${BASE}/api/v1/chats/${chatId}/messages?account_id=${accountId}` + "`" + `,
  {
    method: "POST",
    headers: {
      "Authorization": ` + "`" + `Bearer ${API_KEY}` + "`" + `,
      "Content-Type": "application/json",
      // Same key + same body on a retry replays the first response
      // instead of sending the message twice.
      "Idempotency-Key": crypto.randomUUID(),
    },
    body: JSON.stringify({ text: "Hi Grace — following up on the roadmap." }),
  },
);
if (!res.ok) throw new Error((await res.json()).error.code);
const message = await res.json();
console.log(message.id, message.status);`,

	Go: `package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
)

func main() {
	body, _ := json.Marshal(map[string]string{
		"text": "Hi Grace — following up on the roadmap.",
	})
	url := os.Getenv("BASE") + "/api/v1/chats/" + chatID + "/messages?account_id=" + accountID
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+os.Getenv("API_KEY"))
	req.Header.Set("Content-Type", "application/json")
	// Optional, but the cheapest way to make a retry safe.
	req.Header.Set("Idempotency-Key", idempotencyKey)

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		panic(err)
	}
	defer res.Body.Close()

	var msg struct {
		ID     string ` + "`json:\"id\"`" + `
		Status string ` + "`json:\"status\"`" + `
	}
	if err := json.NewDecoder(res.Body).Decode(&msg); err != nil {
		panic(err)
	}
	fmt.Println(res.StatusCode, msg.ID, msg.Status)
}`,
}

// HostedAuth mints the link you hand to an end user so they can connect
// their own mailbox or number. Called from your server, with your API key —
// never from a browser.
var HostedAuth = Snippet{
	Curl: `curl -X POST "$BASE/api/v1/hosted-auth" \
  -H "Authorization: Bearer $API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "provider": "WHATSAPP",
    "notify_url": "https://api.example.com/hooks/connected",
    "success_redirect_url": "https://app.example.com/connected"
  }'
# → {"url":"...","state":"...","provider":"WHATSAPP","expires_at":"..."}
# Send the user to .url. Your notify_url is POSTed the account_id the
# moment they finish, so you never depend on the browser coming back.`,

	Node: `const res = await fetch(` + "`" + `${BASE}/api/v1/hosted-auth` + "`" + `, {
  method: "POST",
  headers: {
    "Authorization": ` + "`" + `Bearer ${API_KEY}` + "`" + `,
    "Content-Type": "application/json",
  },
  body: JSON.stringify({
    provider: "WHATSAPP",
    notify_url: "https://api.example.com/hooks/connected",
    success_redirect_url: "https://app.example.com/connected",
  }),
});
const { url, state, expires_at } = await res.json();
// Redirect your user to url; keep state if you want to correlate the
// notify_url callback with the user who started the flow.`,

	Go: `body, _ := json.Marshal(map[string]any{
	"provider":             "WHATSAPP",
	"notify_url":           "https://api.example.com/hooks/connected",
	"success_redirect_url": "https://app.example.com/connected",
})
req, _ := http.NewRequest(http.MethodPost,
	os.Getenv("BASE")+"/api/v1/hosted-auth", bytes.NewReader(body))
req.Header.Set("Authorization", "Bearer "+os.Getenv("API_KEY"))
req.Header.Set("Content-Type", "application/json")

res, err := http.DefaultClient.Do(req)
if err != nil {
	panic(err)
}
defer res.Body.Close()

var link struct {
	URL       string    ` + "`json:\"url\"`" + `
	State     string    ` + "`json:\"state\"`" + `
	ExpiresAt time.Time ` + "`json:\"expires_at\"`" + `
}
_ = json.NewDecoder(res.Body).Decode(&link)
// Hand link.URL to the end user before link.ExpiresAt.`,
}

// WebhookPayload is the other direction: what lands on your endpoint. The
// "curl" pane shows the request we make to you, so you can replay it against
// your own handler while building.
var WebhookPayload = Snippet{
	Curl: `# This is the request WE send to YOUR endpoint. Replay it locally to
# develop your handler without waiting for a real message:
curl -X POST "https://api.example.com/hooks/messages" \
  -H "Content-Type: application/json" \
  -d '{
    "type": "chat_received",
    "account_id": "acc_0fdaacb3ecb24d83b83b1f6d",
    "timestamp": "2026-08-28T09:38:21Z",
    "webhook": { "id": "wh_5c1de77a90b4426f9c0b12a7", "name": "prod" },
    "message": {
      "id": "3EB0C767D26A1D9B7A2E",
      "account_id": "acc_0fdaacb3ecb24d83b83b1f6d",
      "chat_id": "16505550123@s.whatsapp.net",
      "sender": { "id": "16505550123@s.whatsapp.net", "phone": "+16505550123", "name": "Grace Hopper", "is_self": false },
      "is_from_me": false,
      "kind": "text",
      "text": "Thursday works.",
      "sent_at": "2026-08-28T09:38:20Z",
      "deleted": false,
      "reactions": []
    },
    "chat": {
      "id": "16505550123@s.whatsapp.net",
      "account_id": "acc_0fdaacb3ecb24d83b83b1f6d",
      "kind": "direct",
      "name": "Grace Hopper",
      "unread_count": 1,
      "last_message_at": "2026-08-28T09:38:20Z",
      "archived": false,
      "muted": false
    }
  }'`,

	Node: `// Your endpoint. Answer 2xx fast — anything slow or failing is retried
// with backoff and shows up under GET /api/v1/webhooks/{id}/deliveries.
app.post("/hooks/messages", express.json(), (req, res) => {
  const event = req.body;
  switch (event.type) {
    case "chat_received":
      console.log(event.chat.name, "→", event.message.text);
      break;
    case "mail_received":
      console.log(event.email.from.email, "→", event.email.subject);
      break;
    case "account_status":
      console.warn("needs reconnect:", event.account.id, event.account.status);
      break;
  }
  res.sendStatus(204);
});`,

	Go: `// Your endpoint. Answer 2xx fast — anything slow or failing is retried
// with backoff and shows up under GET /api/v1/webhooks/{id}/deliveries.
http.HandleFunc("/hooks/messages", func(w http.ResponseWriter, r *http.Request) {
	var ev struct {
		Type      string ` + "`json:\"type\"`" + `
		AccountID string ` + "`json:\"account_id\"`" + `
		Message   *struct {
			ChatID string ` + "`json:\"chat_id\"`" + `
			Text   string ` + "`json:\"text\"`" + `
		} ` + "`json:\"message\"`" + `
		Email *struct {
			Subject string ` + "`json:\"subject\"`" + `
		} ` + "`json:\"email\"`" + `
	}
	if err := json.NewDecoder(r.Body).Decode(&ev); err != nil {
		http.Error(w, "bad payload", http.StatusBadRequest)
		return
	}
	log.Printf("%s on %s", ev.Type, ev.AccountID)
	w.WriteHeader(http.StatusNoContent)
})`,
}

// CreateKey is the first thing a new integrator does. It is deliberately
// session-only, so it cannot be shown as a copy-pasteable curl with a
// bearer token — the snippet says so rather than pretending otherwise.
var CreateKey = Snippet{
	Curl: `# API keys are minted from the dashboard only: this endpoint accepts a
# signed-in session, never a bearer token, so a leaked key cannot mint more
# keys. Sign in at $BASE/login → Dashboard → New key.
#
# For reference, the request the dashboard makes:
POST $BASE/api/v1/api-keys
Content-Type: application/json

{"name":"production"}

# The response carries the full key exactly once — it is never stored, so
# copy it now:
# {"id":"key_…","name":"production","prefix":"um_7Kd2LpQx","key":"um_7Kd2…"}
#
# Then use it on every API call:
curl "$BASE/api/v1/me" -H "Authorization: Bearer $API_KEY"`,

	Node: `// Session-only: this runs in the dashboard, with the session cookie —
// an API key is refused with 403 session_required.
const res = await fetch("/api/v1/api-keys", {
  method: "POST",
  headers: { "Content-Type": "application/json" },
  credentials: "same-origin",
  body: JSON.stringify({ name: "production" }),
});
const key = await res.json();
// key.key is the full secret and is returned exactly once. Store it now;
// afterwards only key.prefix is recoverable.
console.log(key.id, key.prefix, key.key);

// Every later call authenticates with it:
await fetch(` + "`" + `${BASE}/api/v1/me` + "`" + `, {
  headers: { "Authorization": ` + "`" + `Bearer ${key.key}` + "`" + ` },
});`,

	Go: `// Keys are minted from the dashboard (session-only), not from your
// server. Once you have one, this is how every call uses it:
req, _ := http.NewRequest(http.MethodGet, os.Getenv("BASE")+"/api/v1/me", nil)
req.Header.Set("Authorization", "Bearer "+os.Getenv("API_KEY"))

res, err := http.DefaultClient.Do(req)
if err != nil {
	panic(err)
}
defer res.Body.Close()

var me struct {
	ID    string ` + "`json:\"id\"`" + `
	Email string ` + "`json:\"email\"`" + `
	Auth  string ` + "`json:\"auth\"`" + `
}
_ = json.NewDecoder(res.Body).Decode(&me)
fmt.Println(me.Email, "authenticated via", me.Auth)`,
}
