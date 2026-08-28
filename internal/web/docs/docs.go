// Package docs is the reference data behind the developer documentation
// page: every endpoint the API registers, every event it emits, and every
// error code it can answer with.
//
// It is deliberately a leaf package with no dependency on internal/api.
// internal/api imports this (the docs handler renders it); if this imported
// internal/api back, the cycle would be immediate. The cost is that the
// method/path strings here are copies of api.apiRoutes rather than
// references — which is exactly what the api-side test
// TestDocsDataCoversApiRoutes exists to police.
package docs

import "strings"

// Param is one input an endpoint takes. In is "path", "query", "body" or
// "header".
type Param struct {
	Name, In, Type, Desc string
	Required             bool
}

// Endpoint is one row of the reference: what it does, what it takes, and
// what it answers with.
type Endpoint struct {
	Method, Path, Summary, Group string
	Params                       []Param
	// Request and Response are JSON samples. Request is "" for endpoints
	// that take no body.
	Request, Response string
	// Anchor is the in-page id, always Anchor(Method, Path).
	Anchor string
}

// Event is one webhook event type.
type Event struct {
	Type, When, Sample string
	// Kinds are the webhook kinds that carry this event.
	Kinds []string
}

// ErrorCode is one value of the `error.code` field in an error body.
type ErrorCode struct {
	Code   string
	Status int
	Fix    string
}

// Group is a set of endpoints under one heading, in reference order.
type Group struct {
	Name      string
	Endpoints []Endpoint
}

// anchorReplacer turns a method+path into an id: path separators and the
// braces around a path parameter all become hyphens or nothing.
var anchorReplacer = strings.NewReplacer("{", "", "}", "", "/", "-")

// Anchor is the in-page id for one endpoint. It must stay a pure function of
// method and path: the docs template, the table of contents and the api-side
// test all compute it independently and must agree.
//
//	Anchor("POST", "/api/v1/chats/{id}/messages") == "post-api-v1-chats-id-messages"
func Anchor(method, path string) string {
	return strings.Trim(anchorReplacer.Replace(strings.ToLower(method+path)), "-")
}

// groupOrder is the order groups appear on the page: what a new integrator
// needs first (a key), then connecting a mailbox, then the data.
//
// It mirrors the buckets api.routeGroup assigns. The two lists are asserted
// to agree by the api-side docs test.
var groupOrder = []string{
	"Developer & keys",
	"Connecting mailboxes",
	"Accounts",
	"Mail",
	"Chat",
	"Webhooks",
}

// Grouped buckets Endpoints for rendering, in groupOrder. Endpoint order
// within a group is the order they are declared, which is the order the
// server registers them. A group with no endpoints is omitted rather than
// rendered empty.
func Grouped() []Group {
	out := make([]Group, 0, len(groupOrder))
	for _, name := range groupOrder {
		g := Group{Name: name}
		for _, e := range Endpoints {
			if e.Group == name {
				g.Endpoints = append(g.Endpoints, e)
			}
		}
		if len(g.Endpoints) > 0 {
			out = append(out, g)
		}
	}
	return out
}

// withAnchors fills in every Endpoint.Anchor. Computing them rather than
// writing them out by hand is the only way 44 anchors stay consistent with
// the template's `href="#..."` and with the api-side test.
func withAnchors(in []Endpoint) []Endpoint {
	for i := range in {
		in[i].Anchor = Anchor(in[i].Method, in[i].Path)
	}
	return in
}

// Shared parameters, so the same wording cannot drift between the twenty
// routes that take them.
var (
	accountQuery = Param{Name: "account_id", In: "query", Type: "string", Required: true,
		Desc: "The connected account to act on. Every mail and chat route is scoped to one account."}
	limitQuery = Param{Name: "limit", In: "query", Type: "integer",
		Desc: "Page size, 1–200. Default 50."}
	offsetQuery = Param{Name: "offset", In: "query", Type: "integer",
		Desc: "Rows to skip. Default 0."}
	idempotencyHeader = Param{Name: "Idempotency-Key", In: "header", Type: "string",
		Desc: "Optional. Retrying with the same key and the same body replays the first response instead of sending twice; the same key with a different body is 409 idempotency_conflict."}
)

// Endpoints is one entry per pattern in api.apiRoutes. They are declared
// grouped, because Grouped is what the page renders and reading them in
// rendered order is the only way to notice a gap.
var Endpoints = withAnchors([]Endpoint{
	// ---- Connecting mailboxes ----
	{
		Method: "POST", Path: "/api/v1/hosted-auth", Group: "Connecting mailboxes",
		Summary: "Mint a single-use connect link to hand to one of your end users. Your API key never leaves your server and the end user never sees your provider credentials.",
		Params: []Param{
			{Name: "provider", In: "body", Type: "string", Desc: `Which backend to connect, e.g. "OUTLOOK" or "WHATSAPP". Optional only while exactly one provider is registered.`},
			{Name: "success_redirect_url", In: "body", Type: "string", Desc: "Where the end user's browser lands once connected. The host must be this server's own origin or on your redirect-domain allowlist."},
			{Name: "failure_redirect_url", In: "body", Type: "string", Desc: "Where the browser lands if the user cancels or the provider refuses. Same allowlist rule."},
			{Name: "notify_url", In: "body", Type: "string", Desc: "Fetched server-to-server the moment the account connects, so you learn the account_id without depending on the browser completing its redirect. Must be a public http(s) URL."},
			{Name: "webhook", In: "body", Type: "object", Desc: "Optional hook to register against the new account the instant it connects — same body as POST /api/v1/webhooks. Saves a second call."},
			{Name: "expires_in_minutes", In: "body", Type: "integer", Desc: "Link lifetime. Default 30."},
			{Name: "force_consent", In: "body", Type: "boolean", Desc: "Re-prompt for consent even when the provider would sign the user in silently. Use after changing scopes."},
		},
		Request: `{
  "provider": "OUTLOOK",
  "success_redirect_url": "https://app.example.com/connected",
  "failure_redirect_url": "https://app.example.com/connect-failed",
  "notify_url": "https://api.example.com/hooks/connected",
  "webhook": {
    "name": "prod",
    "url": "https://api.example.com/hooks/messages",
    "events": ["mail_received"]
  },
  "expires_in_minutes": 30
}`,
		Response: `{
  "url": "https://api.entropix.dev/connect/9tQ2mQhBqk0vY4pW1sR7nZ3c",
  "state": "9tQ2mQhBqk0vY4pW1sR7nZ3c",
  "provider": "OUTLOOK",
  "expires_at": "2026-08-28T10:11:12Z"
}`,
	},
	{
		Method: "GET", Path: "/api/v1/providers", Group: "Connecting mailboxes",
		Summary: "List the backends this deployment can connect, what kind of account each produces, how it is authenticated, and whether it pushes or must be polled.",
		Response: `{
  "items": [
    { "name": "OUTLOOK",  "kind": "mail", "push_notifications": true,  "auth": "oauth" },
    { "name": "WHATSAPP", "kind": "chat", "push_notifications": true,  "auth": "link" }
  ],
  "limit": 0,
  "offset": 0
}`,
	},

	// ---- Developer & keys ----
	{
		Method: "GET", Path: "/api/v1/me", Group: "Developer & keys",
		Summary: "The developer this credential belongs to, plus how you authenticated. The cheapest way to check a key is live.",
		Response: `{
  "id": "dev_3a7c19f04be2418d9f0b6c55",
  "email": "ada@example.com",
  "name": "Ada Lovelace",
  "created_at": "2026-08-21T09:58:03Z",
  "redirect_domains": ["app.example.com", "*.staging.example.com"],
  "auth": "api_key"
}`,
	},
	{
		Method: "POST", Path: "/api/v1/me/password", Group: "Developer & keys",
		Summary: "Change your dashboard password. Session-only: an API key cannot call this. Every other session is signed out; the calling browser stays in.",
		Params: []Param{
			{Name: "current_password", In: "body", Type: "string", Required: true, Desc: "Your existing password."},
			{Name: "new_password", In: "body", Type: "string", Required: true, Desc: "At least 10 characters."},
		},
		Request: `{
  "current_password": "the-old-one",
  "new_password": "a-much-longer-new-one"
}`,
		Response: `204 No Content`,
	},
	{
		Method: "PUT", Path: "/api/v1/me/redirect-domains", Group: "Developer & keys",
		Summary: "Replace the allowlist a hosted-auth success/failure redirect URL must match. Session-only, so a leaked key cannot widen where end users can be bounced to.",
		Params: []Param{
			{Name: "domains", In: "body", Type: "string[]", Required: true,
				Desc: `Bare hosts — no scheme, port or path — optionally prefixed "*." to cover subdomains. At most 20. Sending [] clears the list.`},
		},
		Request: `{
  "domains": ["app.example.com", "*.staging.example.com"]
}`,
		Response: `{
  "redirect_domains": ["app.example.com", "*.staging.example.com"]
}`,
	},
	{
		Method: "GET", Path: "/api/v1/api-keys", Group: "Developer & keys",
		Summary: "List your API keys. Only the 12-character prefix is stored, so a key you have lost cannot be recovered here — revoke it and create another.",
		Response: `{
  "items": [
    {
      "id": "key_4b2f9a1c77d84e0aa0c31f52",
      "name": "production",
      "prefix": "um_7Kd2LpQx",
      "created_at": "2026-08-21T10:03:31Z",
      "last_used_at": "2026-08-28T09:41:12Z"
    },
    {
      "id": "key_88ba0d4e12f34c6b91ad7e03",
      "name": "laptop",
      "prefix": "um_Za91mVbR",
      "created_at": "2026-07-02T14:20:09Z",
      "revoked_at": "2026-08-14T08:00:00Z"
    }
  ],
  "limit": 0,
  "offset": 0
}`,
	},
	{
		Method: "POST", Path: "/api/v1/api-keys", Group: "Developer & keys",
		Summary: "Create an API key. The full key is returned exactly once and never stored — copy it now. Session-only, so a leaked key cannot mint more keys.",
		Params: []Param{
			{Name: "name", In: "body", Type: "string", Required: true, Desc: "A label you will recognise when revoking, e.g. \"production\"."},
		},
		Request: `{
  "name": "production"
}`,
		Response: `{
  "id": "key_4b2f9a1c77d84e0aa0c31f52",
  "name": "production",
  "prefix": "um_7Kd2LpQx",
  "created_at": "2026-08-28T09:41:12Z",
  "key": "um_7Kd2LpQxvR3nTgY8wJhF2sMcB5aZ0eDqXuKlNpO1"
}`,
	},
	{
		Method: "DELETE", Path: "/api/v1/api-keys/{id}", Group: "Developer & keys",
		Summary: "Revoke a key. It stops authenticating immediately and stays listed with a revoked_at so you can audit it. Session-only.",
		Params: []Param{
			{Name: "id", In: "path", Type: "string", Required: true, Desc: "The key id (key_…), not the key itself."},
		},
		Response: `204 No Content`,
	},

	// ---- Accounts ----
	{
		Method: "GET", Path: "/api/v1/accounts", Group: "Accounts",
		Summary: "Every mailbox and chat number your end users have connected to you. Chat accounts also carry live socket state.",
		Response: `{
  "items": [
    {
      "id": "acc_0fdaacb3ecb24d83b83b1f6d",
      "provider": "OUTLOOK",
      "email": "ada@example.com",
      "kind": "mail",
      "identifier": "ada@example.com",
      "name": "Ada Lovelace",
      "status": "OK",
      "created_at": "2026-08-21T10:02:44Z",
      "updated_at": "2026-08-28T09:41:12Z",
      "last_synced_at": "2026-08-28T09:40:57Z"
    },
    {
      "id": "acc_7b19e4c2a5f34d16b0c9e831",
      "provider": "WHATSAPP",
      "email": "",
      "kind": "chat",
      "identifier": "+15550100123",
      "name": "Support line",
      "status": "OK",
      "created_at": "2026-08-24T16:11:02Z",
      "updated_at": "2026-08-28T09:39:50Z",
      "connection": { "state": "connected", "since": "2026-08-28T06:12:00Z", "reconnects": 0 }
    }
  ],
  "limit": 0,
  "offset": 0
}`,
	},
	{
		Method: "GET", Path: "/api/v1/accounts/{id}", Group: "Accounts",
		Summary: "One connected account. An id belonging to another developer answers 404, never 403 — ids are not enumerable across tenants.",
		Params: []Param{
			{Name: "id", In: "path", Type: "string", Required: true, Desc: "The account id (acc_…)."},
		},
		Response: `{
  "id": "acc_0fdaacb3ecb24d83b83b1f6d",
  "provider": "OUTLOOK",
  "email": "ada@example.com",
  "kind": "mail",
  "identifier": "ada@example.com",
  "name": "Ada Lovelace",
  "status": "OK",
  "created_at": "2026-08-21T10:02:44Z",
  "updated_at": "2026-08-28T09:41:12Z",
  "last_synced_at": "2026-08-28T09:40:57Z"
}`,
	},
	{
		Method: "DELETE", Path: "/api/v1/accounts/{id}", Group: "Accounts",
		Summary: "Disconnect an account: the stored grant is destroyed, any live socket is stopped, and mirrored mail and chat rows go with it. Not reversible — the end user connects again from scratch.",
		Params: []Param{
			{Name: "id", In: "path", Type: "string", Required: true, Desc: "The account id (acc_…)."},
		},
		Response: `204 No Content`,
	},
	{
		Method: "POST", Path: "/api/v1/accounts/{id}/resync", Group: "Accounts",
		Summary: "Queue a full resync of a mail account. Answers immediately; the sync runs in the background. Mail only — chat accounts use /reconnect.",
		Params: []Param{
			{Name: "id", In: "path", Type: "string", Required: true, Desc: "The account id (acc_…) of a kind:\"mail\" account."},
		},
		Response: `{
  "status": "queued"
}`,
	},
	{
		Method: "POST", Path: "/api/v1/accounts/{id}/reconnect", Group: "Accounts",
		Summary: "Ask the chat runtime to bring a chat account's socket back up. Chat only — mail accounts use /resync.",
		Params: []Param{
			{Name: "id", In: "path", Type: "string", Required: true, Desc: "The account id (acc_…) of a kind:\"chat\" account."},
		},
		Response: `{
  "status": "reconnecting"
}`,
	},

	// ---- Webhooks (per-account) ----
	{
		Method: "GET", Path: "/api/v1/accounts/{id}/webhooks", Group: "Webhooks",
		Summary: "The hooks scoped to one account. Secrets are never echoed on a listing.",
		Params: []Param{
			{Name: "id", In: "path", Type: "string", Required: true, Desc: "The account id (acc_…)."},
		},
		Response: `{
  "items": [
    {
      "id": "wh_5c1de77a90b4426f9c0b12a7",
      "name": "prod",
      "account_id": "acc_0fdaacb3ecb24d83b83b1f6d",
      "kind": "webhook",
      "url": "https://api.example.com/hooks/messages",
      "events": ["mail_received", "mail_sent"],
      "created_at": "2026-08-21T10:04:02Z"
    }
  ],
  "limit": 0,
  "offset": 0
}`,
	},
	{
		Method: "POST", Path: "/api/v1/accounts/{id}/webhooks", Group: "Webhooks",
		Summary: "Register a hook that only receives one account's events. Events default to that account's kind (mail_received for mail, chat_received for chat).",
		Params: []Param{
			{Name: "id", In: "path", Type: "string", Required: true, Desc: "The account id (acc_…)."},
			{Name: "kind", In: "body", Type: "string", Desc: `"webhook" (default), "discord" or "telegram".`},
			{Name: "url", In: "body", Type: "string", Desc: "Your endpoint (kind=webhook) or the Discord incoming-webhook URL (kind=discord). Must be a public http(s) URL."},
			{Name: "secret", In: "body", Type: "string", Desc: "kind=webhook only. Signs each delivery; echoed back once at creation and never again."},
			{Name: "bot_token", In: "body", Type: "string", Desc: "kind=telegram only. Your bot's token; sealed at rest and never serialised."},
			{Name: "chat_id", In: "body", Type: "string", Desc: "kind=telegram only. The Telegram chat to post into."},
			{Name: "events", In: "body", Type: "string[]", Desc: `Event types to receive, or ["*"] for all.`},
		},
		Request: `{
  "name": "prod",
  "kind": "webhook",
  "url": "https://api.example.com/hooks/messages",
  "secret": "whsec_2f1e9c74d0a3",
  "events": ["mail_received", "mail_sent"]
}`,
		Response: `{
  "id": "wh_5c1de77a90b4426f9c0b12a7",
  "name": "prod",
  "account_id": "acc_0fdaacb3ecb24d83b83b1f6d",
  "kind": "webhook",
  "url": "https://api.example.com/hooks/messages",
  "secret": "whsec_2f1e9c74d0a3",
  "events": ["mail_received", "mail_sent"],
  "created_at": "2026-08-28T09:41:12Z"
}`,
	},
	{
		Method: "DELETE", Path: "/api/v1/accounts/{id}/webhooks/{wid}", Group: "Webhooks",
		Summary: "Delete one of an account's hooks. Deliveries already queued for it stop.",
		Params: []Param{
			{Name: "id", In: "path", Type: "string", Required: true, Desc: "The account id (acc_…)."},
			{Name: "wid", In: "path", Type: "string", Required: true, Desc: "The webhook id (wh_…)."},
		},
		Response: `204 No Content`,
	},

	// ---- Mail ----
	{
		Method: "GET", Path: "/api/v1/folders", Group: "Mail",
		Summary: "An account's folders, with unread and total counts. role is set for the well-known ones (inbox, sentitems, drafts…), which is what folder_role on the message listing matches against.",
		Params:  []Param{accountQuery},
		Response: `{
  "items": [
    {
      "id": "AQMkAGI2NmY4ZTk1AAAI",
      "account_id": "acc_0fdaacb3ecb24d83b83b1f6d",
      "name": "Inbox",
      "role": "inbox",
      "total_count": 1284,
      "unread_count": 7
    },
    {
      "id": "AQMkAGI2NmY4ZTk1AAAJ",
      "account_id": "acc_0fdaacb3ecb24d83b83b1f6d",
      "name": "Sent Items",
      "role": "sentitems",
      "total_count": 903,
      "unread_count": 0
    }
  ],
  "limit": 0,
  "offset": 0
}`,
	},
	{
		Method: "GET", Path: "/api/v1/threads", Group: "Mail",
		Summary: "Conversation threads for an account, newest activity first.",
		Params:  []Param{accountQuery, limitQuery, offsetQuery},
		Response: `{
  "items": [
    {
      "id": "AAQkAGI2NmY4ZTk1LTk4",
      "account_id": "acc_0fdaacb3ecb24d83b83b1f6d",
      "subject": "Q3 roadmap",
      "count": 4,
      "last_date": "2026-08-28T09:12:44Z",
      "unread": 1
    }
  ],
  "limit": 50,
  "offset": 0
}`,
	},
	{
		Method: "GET", Path: "/api/v1/emails", Group: "Mail",
		Summary: "List messages in an account, newest first. Bodies are omitted here because they make list responses enormous — fetch one message to get its body.",
		Params: []Param{
			accountQuery,
			{Name: "folder_id", In: "query", Type: "string", Desc: "Restrict to one folder, by its provider id."},
			{Name: "folder_role", In: "query", Type: "string", Desc: `Restrict to a well-known folder without knowing its id, e.g. "inbox". Ignored when folder_id is also given; an unmatched role is 400 unknown_folder_role.`},
			{Name: "thread_id", In: "query", Type: "string", Desc: "Restrict to one conversation."},
			{Name: "q", In: "query", Type: "string", Desc: "Full-text search over subject, sender and body."},
			{Name: "unread", In: "query", Type: "boolean", Desc: `"true"/"1" for unread only, "false"/"0" for read only. Omit for both.`},
			limitQuery, offsetQuery,
		},
		Response: `{
  "items": [
    {
      "id": "AAMkAGI2NmY4ZTk1LTk4YzUtNDQ",
      "account_id": "acc_0fdaacb3ecb24d83b83b1f6d",
      "thread_id": "AAQkAGI2NmY4ZTk1LTk4",
      "folder_id": "AQMkAGI2NmY4ZTk1AAAI",
      "subject": "Q3 roadmap",
      "from": { "name": "Grace Hopper", "email": "grace@example.org" },
      "to": [{ "name": "Ada Lovelace", "email": "ada@example.com" }],
      "date": "2026-08-28T09:12:44Z",
      "snippet": "Pulling the migration forward a week — see the attached plan.",
      "body_type": "html",
      "read": false,
      "flagged": false,
      "draft": false,
      "has_attachments": true,
      "internet_message_id": "<CAJ8n0=abc123@mail.example.org>"
    }
  ],
  "limit": 50,
  "offset": 0
}`,
	},
	{
		Method: "POST", Path: "/api/v1/emails", Group: "Mail",
		Summary: "Send a message. Answers 202 as soon as the provider accepts it; the matching mail_sent event follows.",
		Params: []Param{
			{Name: "account_id", In: "body", Type: "string", Required: true, Desc: "The sending account. May also be given as ?account_id, which wins if both are present."},
			{Name: "to", In: "body", Type: "object[]", Required: true, Desc: "Recipients, {name?, email}. Required unless reply_to_email_id is set."},
			{Name: "subject", In: "body", Type: "string", Desc: "Subject line."},
			{Name: "body", In: "body", Type: "string", Desc: "Message body."},
			{Name: "body_type", In: "body", Type: "string", Desc: `"html" (default) or "text".`},
			{Name: "attachments", In: "body", Type: "object[]", Desc: "{name, mime_type, content} with content base64-encoded. 3 MB total across all attachments."},
		},
		Request: `{
  "account_id": "acc_0fdaacb3ecb24d83b83b1f6d",
  "to": [{ "name": "Grace Hopper", "email": "grace@example.org" }],
  "cc": [{ "email": "team@example.org" }],
  "subject": "Re: Q3 roadmap",
  "body": "<p>Works for me — shipping Thursday.</p>",
  "body_type": "html"
}`,
		Response: `{
  "status": "sent",
  "message_id": "AAMkAGI2NmY4ZTk1LTk4YzUtOTk"
}`,
	},
	{
		Method: "GET", Path: "/api/v1/emails/{id}", Group: "Mail",
		Summary: "One message, with its body. Served from the local mirror, falling back to the provider for a message the sync engine has not reached yet.",
		Params: []Param{
			{Name: "id", In: "path", Type: "string", Required: true, Desc: "The provider message id."},
			accountQuery,
		},
		Response: `{
  "id": "AAMkAGI2NmY4ZTk1LTk4YzUtNDQ",
  "account_id": "acc_0fdaacb3ecb24d83b83b1f6d",
  "thread_id": "AAQkAGI2NmY4ZTk1LTk4",
  "folder_id": "AQMkAGI2NmY4ZTk1AAAI",
  "subject": "Q3 roadmap",
  "from": { "name": "Grace Hopper", "email": "grace@example.org" },
  "to": [{ "name": "Ada Lovelace", "email": "ada@example.com" }],
  "date": "2026-08-28T09:12:44Z",
  "snippet": "Pulling the migration forward a week — see the attached plan.",
  "body": "<p>Pulling the migration forward a week — see the attached plan.</p>",
  "body_type": "html",
  "body_plain": "Pulling the migration forward a week — see the attached plan.",
  "read": false,
  "flagged": false,
  "draft": false,
  "has_attachments": true,
  "internet_message_id": "<CAJ8n0=abc123@mail.example.org>",
  "attachments": [
    { "id": "AAMkAtt1", "name": "plan.pdf", "mime_type": "application/pdf", "size": 184320, "is_inline": false }
  ]
}`,
	},
	{
		Method: "PATCH", Path: "/api/v1/emails/{id}", Group: "Mail",
		Summary: "Mark a message read/unread or flagged/unflagged. The change is written through locally too, so it is visible before the next sync round. Supply at least one field.",
		Params: []Param{
			{Name: "id", In: "path", Type: "string", Required: true, Desc: "The provider message id."},
			accountQuery,
			{Name: "read", In: "body", Type: "boolean", Desc: "Omit to leave unchanged."},
			{Name: "flagged", In: "body", Type: "boolean", Desc: "Omit to leave unchanged."},
		},
		Request: `{
  "read": true
}`,
		Response: `{
  "id": "AAMkAGI2NmY4ZTk1LTk4YzUtNDQ",
  "account_id": "acc_0fdaacb3ecb24d83b83b1f6d",
  "thread_id": "AAQkAGI2NmY4ZTk1LTk4",
  "folder_id": "AQMkAGI2NmY4ZTk1AAAI",
  "subject": "Q3 roadmap",
  "from": { "name": "Grace Hopper", "email": "grace@example.org" },
  "to": [{ "name": "Ada Lovelace", "email": "ada@example.com" }],
  "date": "2026-08-28T09:12:44Z",
  "snippet": "Pulling the migration forward a week — see the attached plan.",
  "read": true,
  "flagged": false,
  "draft": false,
  "has_attachments": true
}`,
	},
	{
		Method: "POST", Path: "/api/v1/emails/{id}/reply", Group: "Mail",
		Summary: "Reply to a message. The provider builds the reply, so conversation id, In-Reply-To and References are populated correctly rather than hand-rolled.",
		Params: []Param{
			{Name: "id", In: "path", Type: "string", Required: true, Desc: "The message being replied to."},
			{Name: "account_id", In: "body", Type: "string", Required: true, Desc: "The sending account. May also be given as ?account_id."},
			{Name: "body", In: "body", Type: "string", Desc: "Your reply text."},
			{Name: "reply_all", In: "body", Type: "boolean", Desc: "Include every original recipient. Default false."},
		},
		Request: `{
  "account_id": "acc_0fdaacb3ecb24d83b83b1f6d",
  "body": "<p>Thursday works.</p>",
  "body_type": "html",
  "reply_all": true
}`,
		Response: `{
  "status": "sent",
  "message_id": "AAMkAGI2NmY4ZTk1LTk4YzUtQjE"
}`,
	},
	{
		Method: "POST", Path: "/api/v1/emails/{id}/forward", Group: "Mail",
		Summary: "Forward a message, attachments included, to new recipients.",
		Params: []Param{
			{Name: "id", In: "path", Type: "string", Required: true, Desc: "The message being forwarded."},
			{Name: "account_id", In: "body", Type: "string", Required: true, Desc: "The sending account. May also be given as ?account_id."},
			{Name: "to", In: "body", Type: "object[]", Required: true, Desc: "Recipients, {name?, email}."},
			{Name: "body", In: "body", Type: "string", Desc: "A note above the forwarded content."},
		},
		Request: `{
  "account_id": "acc_0fdaacb3ecb24d83b83b1f6d",
  "to": [{ "email": "legal@example.org" }],
  "body": "<p>FYI — the plan Grace mentioned.</p>"
}`,
		Response: `{
  "status": "sent",
  "message_id": "AAMkAGI2NmY4ZTk1LTk4YzUtQzc"
}`,
	},
	{
		Method: "GET", Path: "/api/v1/emails/{id}/attachments", Group: "Mail",
		Summary: "List a message's attachments — names, types and sizes, without the bytes.",
		Params: []Param{
			{Name: "id", In: "path", Type: "string", Required: true, Desc: "The provider message id."},
			accountQuery,
		},
		Response: `{
  "items": [
    { "id": "AAMkAtt1", "name": "plan.pdf", "mime_type": "application/pdf", "size": 184320, "is_inline": false },
    { "id": "AAMkAtt2", "name": "logo.png", "mime_type": "image/png", "size": 8241, "is_inline": true, "content_id": "logo@cid" }
  ],
  "limit": 0,
  "offset": 0
}`,
	},
	{
		Method: "GET", Path: "/api/v1/emails/{id}/attachments/{aid}", Group: "Mail",
		Summary: "Download one attachment's bytes. This is the only endpoint that does not answer JSON: the body is the file, with Content-Type and Content-Disposition set from the attachment.",
		Params: []Param{
			{Name: "id", In: "path", Type: "string", Required: true, Desc: "The provider message id."},
			{Name: "aid", In: "path", Type: "string", Required: true, Desc: "The attachment id from the listing."},
			accountQuery,
		},
		Response: `200 OK
Content-Type: application/pdf
Content-Disposition: attachment; filename="plan.pdf"

<the file bytes>`,
	},
	{
		Method: "POST", Path: "/api/v1/drafts", Group: "Mail",
		Summary: "Create a draft in the account's Drafts folder without sending it. Same body as POST /api/v1/emails.",
		Params: []Param{
			{Name: "account_id", In: "body", Type: "string", Required: true, Desc: "The owning account. May also be given as ?account_id."},
			{Name: "to", In: "body", Type: "object[]", Desc: "Recipients, {name?, email}."},
		},
		Request: `{
  "account_id": "acc_0fdaacb3ecb24d83b83b1f6d",
  "to": [{ "email": "grace@example.org" }],
  "subject": "Q3 roadmap — revised",
  "body": "<p>Draft, not sent yet.</p>"
}`,
		Response: `{
  "id": "AAMkAGI2NmY4ZTk1LTk4YzUtRDE",
  "account_id": "acc_0fdaacb3ecb24d83b83b1f6d",
  "thread_id": "",
  "folder_id": "AQMkAGI2NmY4ZTk1AAAL",
  "subject": "Q3 roadmap — revised",
  "from": { "name": "Ada Lovelace", "email": "ada@example.com" },
  "to": [{ "email": "grace@example.org" }],
  "date": "2026-08-28T09:41:12Z",
  "snippet": "Draft, not sent yet.",
  "body": "<p>Draft, not sent yet.</p>",
  "body_type": "html",
  "read": true,
  "flagged": false,
  "draft": true,
  "has_attachments": false
}`,
	},
	{
		Method: "POST", Path: "/api/v1/drafts/{id}/send", Group: "Mail",
		Summary: "Send a draft that already exists in the mailbox — yours or one the user wrote in Outlook.",
		Params: []Param{
			{Name: "id", In: "path", Type: "string", Required: true, Desc: "The draft's provider message id."},
			accountQuery,
		},
		Response: `{
  "status": "sent"
}`,
	},

	// ---- Webhooks (global) ----
	{
		Method: "GET", Path: "/api/v1/webhooks", Group: "Webhooks",
		Summary: "All your hooks, global and per-account. Secrets are never echoed on a listing.",
		Response: `{
  "items": [
    {
      "id": "wh_5c1de77a90b4426f9c0b12a7",
      "name": "prod",
      "kind": "webhook",
      "url": "https://api.example.com/hooks/messages",
      "events": ["*"],
      "created_at": "2026-08-21T10:04:02Z"
    },
    {
      "id": "wh_a30c92be5f7d4b1c81e60d44",
      "name": "ops-alerts",
      "kind": "discord",
      "url": "https://discord.com/api/webhooks/1234567890/AbCdEf",
      "events": ["account_status"],
      "created_at": "2026-08-25T11:30:18Z"
    }
  ],
  "limit": 0,
  "offset": 0
}`,
	},
	{
		Method: "POST", Path: "/api/v1/webhooks", Group: "Webhooks",
		Summary: "Register a hook that receives events from every account you own. Deliveries are signed and retried with backoff.",
		Params: []Param{
			{Name: "name", In: "body", Type: "string", Desc: "A label echoed in every delivery, so one endpoint fed by several hooks can tell them apart."},
			{Name: "kind", In: "body", Type: "string", Desc: `"webhook" (default) posts the JSON event; "discord" and "telegram" post a formatted notification instead.`},
			{Name: "url", In: "body", Type: "string", Desc: "Required for kind=webhook and kind=discord. Must be a public http(s) URL; a Discord hook must be a discord.com incoming-webhook URL."},
			{Name: "secret", In: "body", Type: "string", Desc: "kind=webhook only. Used to sign deliveries; returned once here and never again."},
			{Name: "bot_token", In: "body", Type: "string", Desc: "kind=telegram only. Validated against Telegram at registration, so a bad token fails here rather than silently at delivery time."},
			{Name: "chat_id", In: "body", Type: "string", Desc: "kind=telegram only."},
			{Name: "events", In: "body", Type: "string[]", Desc: `Event types, or ["*"] for all. Defaults by account kind.`},
		},
		Request: `{
  "name": "prod",
  "kind": "webhook",
  "url": "https://api.example.com/hooks/messages",
  "secret": "whsec_2f1e9c74d0a3",
  "events": ["mail_received", "chat_received"]
}`,
		Response: `{
  "id": "wh_5c1de77a90b4426f9c0b12a7",
  "name": "prod",
  "kind": "webhook",
  "url": "https://api.example.com/hooks/messages",
  "secret": "whsec_2f1e9c74d0a3",
  "events": ["mail_received", "chat_received"],
  "created_at": "2026-08-28T09:41:12Z"
}`,
	},
	{
		Method: "DELETE", Path: "/api/v1/webhooks/{id}", Group: "Webhooks",
		Summary: "Delete a hook. Queued deliveries for it stop.",
		Params: []Param{
			{Name: "id", In: "path", Type: "string", Required: true, Desc: "The webhook id (wh_…)."},
		},
		Response: `204 No Content`,
	},
	{
		Method: "GET", Path: "/api/v1/webhooks/{id}/deliveries", Group: "Webhooks",
		Summary: "What is still waiting for a retry and what was abandoned, so you can measure an outage's cost. dead:true means every attempt was used up.",
		Params: []Param{
			{Name: "id", In: "path", Type: "string", Required: true, Desc: "The webhook id (wh_…)."},
			{Name: "limit", In: "query", Type: "integer", Desc: "Page size, 1–200. Default 50; a larger value clamps to 200 rather than falling back to the default."},
			offsetQuery,
		},
		Response: `{
  "items": [
    {
      "id": "dl_9c02f4a71be34d5f8a11c706",
      "webhook_id": "wh_5c1de77a90b4426f9c0b12a7",
      "account_id": "acc_0fdaacb3ecb24d83b83b1f6d",
      "event_type": "mail_received",
      "attempts": 3,
      "next_attempt_at": "2026-08-28T09:49:00Z",
      "last_error": "Post \"https://api.example.com/hooks/messages\": dial tcp: connection refused",
      "dead": false,
      "created_at": "2026-08-28T09:41:12Z"
    }
  ],
  "limit": 50,
  "offset": 0
}`,
	},

	// ---- Chat ----
	{
		Method: "GET", Path: "/api/v1/chats", Group: "Chat",
		Summary: "Conversations on a chat account, most recent first.",
		Params: []Param{
			accountQuery,
			{Name: "kind", In: "query", Type: "string", Desc: `"direct" or "group".`},
			{Name: "q", In: "query", Type: "string", Desc: "Search chat names."},
			{Name: "unread", In: "query", Type: "boolean", Desc: `"true"/"1" for chats with unread messages only.`},
			limitQuery, offsetQuery,
		},
		Response: `{
  "items": [
    {
      "id": "16505550123@s.whatsapp.net",
      "account_id": "acc_7b19e4c2a5f34d16b0c9e831",
      "kind": "direct",
      "name": "Grace Hopper",
      "unread_count": 2,
      "last_message_at": "2026-08-28T09:38:20Z",
      "archived": false,
      "muted": false
    }
  ],
  "limit": 50,
  "offset": 0
}`,
	},
	{
		Method: "POST", Path: "/api/v1/chats", Group: "Chat",
		Summary: "Start a conversation with someone you have not messaged before and send the first message, in one call. Identify them by phone or by an attendee_id you already know.",
		Params: []Param{
			{Name: "account_id", In: "body", Type: "string", Required: true, Desc: "The chat account to send from."},
			{Name: "phone", In: "body", Type: "string", Desc: "Recipient in E.164. Either this or attendee_id is required."},
			{Name: "attendee_id", In: "body", Type: "string", Desc: "An existing attendee id. Either this or phone is required."},
			{Name: "text", In: "body", Type: "string", Required: true, Desc: "The first message. Must not be blank."},
			idempotencyHeader,
		},
		Request: `{
  "account_id": "acc_7b19e4c2a5f34d16b0c9e831",
  "phone": "+16505550123",
  "text": "Hi Grace — following up on the roadmap."
}`,
		Response: `{
  "chat": {
    "id": "16505550123@s.whatsapp.net",
    "account_id": "acc_7b19e4c2a5f34d16b0c9e831",
    "kind": "direct",
    "name": "Grace Hopper",
    "unread_count": 0,
    "last_message_at": "2026-08-28T09:41:12Z",
    "archived": false,
    "muted": false
  },
  "message": {
    "id": "3EB0C767D26A1D9B7A2E",
    "account_id": "acc_7b19e4c2a5f34d16b0c9e831",
    "chat_id": "16505550123@s.whatsapp.net",
    "sender": { "id": "15550100123@s.whatsapp.net", "phone": "+15550100123", "name": "Support line", "is_self": true },
    "is_from_me": true,
    "kind": "text",
    "text": "Hi Grace — following up on the roadmap.",
    "sent_at": "2026-08-28T09:41:12Z",
    "deleted": false,
    "status": "sent",
    "reactions": []
  }
}`,
	},
	{
		Method: "GET", Path: "/api/v1/chats/{id}", Group: "Chat",
		Summary: "One conversation, with its members for a group chat.",
		Params: []Param{
			{Name: "id", In: "path", Type: "string", Required: true, Desc: "The chat id."},
			accountQuery,
		},
		Response: `{
  "id": "120363041234567890@g.us",
  "account_id": "acc_7b19e4c2a5f34d16b0c9e831",
  "kind": "group",
  "name": "Roadmap crew",
  "unread_count": 0,
  "last_message_at": "2026-08-28T09:38:20Z",
  "archived": false,
  "muted": false,
  "members": [
    { "id": "16505550123@s.whatsapp.net", "phone": "+16505550123", "name": "Grace Hopper", "is_self": false },
    { "id": "15550100123@s.whatsapp.net", "phone": "+15550100123", "name": "Support line", "is_self": true }
  ]
}`,
	},
	{
		Method: "PATCH", Path: "/api/v1/chats/{id}", Group: "Chat",
		Summary: "Mark a chat read, archive it or mute it. Supply at least one field.",
		Params: []Param{
			{Name: "id", In: "path", Type: "string", Required: true, Desc: "The chat id."},
			accountQuery,
			{Name: "read", In: "body", Type: "boolean", Desc: "true clears the unread count."},
			{Name: "archived", In: "body", Type: "boolean", Desc: "Omit to leave unchanged."},
			{Name: "muted", In: "body", Type: "boolean", Desc: "Omit to leave unchanged."},
		},
		Request: `{
  "read": true,
  "archived": false
}`,
		Response: `{
  "id": "16505550123@s.whatsapp.net",
  "account_id": "acc_7b19e4c2a5f34d16b0c9e831",
  "kind": "direct",
  "name": "Grace Hopper",
  "unread_count": 0,
  "last_message_at": "2026-08-28T09:38:20Z",
  "archived": false,
  "muted": false
}`,
	},
	{
		Method: "GET", Path: "/api/v1/chats/{id}/messages", Group: "Chat",
		Summary: "A chat's messages, newest first. Page backwards by passing the previous page's next_before.",
		Params: []Param{
			{Name: "id", In: "path", Type: "string", Required: true, Desc: "The chat id."},
			accountQuery,
			{Name: "before", In: "query", Type: "string", Desc: "Return messages older than this message id. An id that is not in this chat is 404 not_found."},
			limitQuery,
		},
		Response: `{
  "items": [
    {
      "id": "3EB0C767D26A1D9B7A2E",
      "account_id": "acc_7b19e4c2a5f34d16b0c9e831",
      "chat_id": "16505550123@s.whatsapp.net",
      "sender": { "id": "16505550123@s.whatsapp.net", "phone": "+16505550123", "name": "Grace Hopper", "is_self": false },
      "is_from_me": false,
      "kind": "text",
      "text": "Thursday works.",
      "sent_at": "2026-08-28T09:38:20Z",
      "deleted": false,
      "reactions": [
        { "attendee_id": "15550100123@s.whatsapp.net", "emoji": "👍", "at": "2026-08-28T09:39:01Z" }
      ]
    }
  ],
  "next_before": "3EB0C767D26A1D9B71FA"
}`,
	},
	{
		Method: "POST", Path: "/api/v1/chats/{id}/messages", Group: "Chat",
		Summary: "Send a text message into an existing chat. A failed send leaves no trace — you never see a message stuck in \"sending\".",
		Params: []Param{
			{Name: "id", In: "path", Type: "string", Required: true, Desc: "The chat id."},
			accountQuery,
			{Name: "text", In: "body", Type: "string", Required: true, Desc: "The message. Must not be blank."},
			{Name: "quoted_message_id", In: "body", Type: "string", Desc: "Reply to a specific message in the chat."},
			idempotencyHeader,
		},
		Request: `{
  "text": "On it — sending the revised plan now.",
  "quoted_message_id": "3EB0C767D26A1D9B7A2E"
}`,
		Response: `{
  "id": "3EB0C767D26A1D9B8F04",
  "account_id": "acc_7b19e4c2a5f34d16b0c9e831",
  "chat_id": "16505550123@s.whatsapp.net",
  "sender": { "id": "15550100123@s.whatsapp.net", "phone": "+15550100123", "name": "Support line", "is_self": true },
  "is_from_me": true,
  "kind": "text",
  "text": "On it — sending the revised plan now.",
  "quoted_message_id": "3EB0C767D26A1D9B7A2E",
  "sent_at": "2026-08-28T09:41:12Z",
  "deleted": false,
  "status": "sent",
  "reactions": []
}`,
	},
	{
		Method: "GET", Path: "/api/v1/chats/{id}/messages/{mid}", Group: "Chat",
		Summary: "One message. A message id that exists but belongs to a different chat answers 404, like any other id you do not own.",
		Params: []Param{
			{Name: "id", In: "path", Type: "string", Required: true, Desc: "The chat id."},
			{Name: "mid", In: "path", Type: "string", Required: true, Desc: "The message id."},
			accountQuery,
		},
		Response: `{
  "id": "3EB0C767D26A1D9B7A2E",
  "account_id": "acc_7b19e4c2a5f34d16b0c9e831",
  "chat_id": "16505550123@s.whatsapp.net",
  "sender": { "id": "16505550123@s.whatsapp.net", "phone": "+16505550123", "name": "Grace Hopper", "is_self": false },
  "is_from_me": false,
  "kind": "text",
  "text": "Thursday works.",
  "sent_at": "2026-08-28T09:38:20Z",
  "deleted": false,
  "reactions": []
}`,
	},
	{
		Method: "PATCH", Path: "/api/v1/chats/{id}/messages/{mid}", Group: "Chat",
		Summary: "Edit a message you sent. Editing someone else's is 403 not_own_message.",
		Params: []Param{
			{Name: "id", In: "path", Type: "string", Required: true, Desc: "The chat id."},
			{Name: "mid", In: "path", Type: "string", Required: true, Desc: "The message id. Must be one of yours."},
			accountQuery,
			{Name: "text", In: "body", Type: "string", Required: true, Desc: "The replacement text. Must not be blank."},
		},
		Request: `{
  "text": "On it — revised plan attached instead."
}`,
		Response: `{
  "id": "3EB0C767D26A1D9B8F04",
  "account_id": "acc_7b19e4c2a5f34d16b0c9e831",
  "chat_id": "16505550123@s.whatsapp.net",
  "sender": { "id": "15550100123@s.whatsapp.net", "phone": "+15550100123", "name": "Support line", "is_self": true },
  "is_from_me": true,
  "kind": "text",
  "text": "On it — revised plan attached instead.",
  "sent_at": "2026-08-28T09:41:12Z",
  "edited_at": "2026-08-28T09:42:30Z",
  "deleted": false,
  "status": "sent",
  "reactions": []
}`,
	},
	{
		Method: "DELETE", Path: "/api/v1/chats/{id}/messages/{mid}", Group: "Chat",
		Summary: "Revoke a message you sent, for everyone. Deleting someone else's is 403 not_own_message.",
		Params: []Param{
			{Name: "id", In: "path", Type: "string", Required: true, Desc: "The chat id."},
			{Name: "mid", In: "path", Type: "string", Required: true, Desc: "The message id. Must be one of yours."},
			accountQuery,
		},
		Response: `204 No Content`,
	},
	{
		Method: "PUT", Path: "/api/v1/chats/{id}/messages/{mid}/reaction", Group: "Chat",
		Summary: `Set your reaction on a message. Send "" to remove it — the field being absent entirely is a client bug and is refused.`,
		Params: []Param{
			{Name: "id", In: "path", Type: "string", Required: true, Desc: "The chat id."},
			{Name: "mid", In: "path", Type: "string", Required: true, Desc: "The message id."},
			accountQuery,
			{Name: "emoji", In: "body", Type: "string", Required: true, Desc: `The emoji, or "" to remove an existing reaction. Omitting the field is 400 missing_emoji.`},
		},
		Request: `{
  "emoji": "👍"
}`,
		Response: `204 No Content`,
	},
	{
		Method: "GET", Path: "/api/v1/attendees", Group: "Chat",
		Summary: "People in a chat account's conversations. is_self marks the connected number itself.",
		Params: []Param{
			accountQuery,
			{Name: "q", In: "query", Type: "string", Desc: "Search names and phone numbers."},
			limitQuery, offsetQuery,
		},
		Response: `{
  "items": [
    { "id": "16505550123@s.whatsapp.net", "phone": "+16505550123", "name": "Grace Hopper", "is_self": false },
    { "id": "15550100123@s.whatsapp.net", "phone": "+15550100123", "name": "Support line", "is_self": true }
  ],
  "limit": 50,
  "offset": 0
}`,
	},
	{
		Method: "GET", Path: "/api/v1/attendees/{id}", Group: "Chat",
		Summary: "One attendee. Use this to turn a message's sender id into a name and phone number.",
		Params: []Param{
			{Name: "id", In: "path", Type: "string", Required: true, Desc: "The attendee id."},
			accountQuery,
		},
		Response: `{
  "id": "16505550123@s.whatsapp.net",
  "phone": "+16505550123",
  "name": "Grace Hopper",
  "is_self": false
}`,
	},
})

// allKinds is every delivery kind. Every event type is rendered by
// internal/notify for Discord and Telegram as well as being posted raw to a
// kind=webhook endpoint, so no event is webhook-only.
var allKinds = []string{"webhook", "discord", "telegram"}

// Events is one entry per model.Event* constant.
var Events = []Event{
	{
		Type:  "mail_received",
		When:  "A new message arrived in a connected mailbox — the event most integrations are built on.",
		Kinds: allKinds,
		Sample: `{
  "type": "mail_received",
  "account_id": "acc_0fdaacb3ecb24d83b83b1f6d",
  "timestamp": "2026-08-28T09:12:45Z",
  "webhook": { "id": "wh_5c1de77a90b4426f9c0b12a7", "name": "prod" },
  "email": {
    "id": "AAMkAGI2NmY4ZTk1LTk4YzUtNDQ",
    "account_id": "acc_0fdaacb3ecb24d83b83b1f6d",
    "thread_id": "AAQkAGI2NmY4ZTk1LTk4",
    "folder_id": "AQMkAGI2NmY4ZTk1AAAI",
    "subject": "Q3 roadmap",
    "from": { "name": "Grace Hopper", "email": "grace@example.org" },
    "to": [{ "name": "Ada Lovelace", "email": "ada@example.com" }],
    "date": "2026-08-28T09:12:44Z",
    "snippet": "Pulling the migration forward a week — see the attached plan.",
    "body_plain": "Pulling the migration forward a week — see the attached plan.",
    "role": "inbox",
    "read": false,
    "flagged": false,
    "draft": false,
    "has_attachments": true
  }
}`,
	},
	{
		Type:  "mail_sent",
		When:  "A message left a connected mailbox — whether you sent it through this API or the user sent it from Outlook.",
		Kinds: allKinds,
		Sample: `{
  "type": "mail_sent",
  "account_id": "acc_0fdaacb3ecb24d83b83b1f6d",
  "timestamp": "2026-08-28T09:41:13Z",
  "webhook": { "id": "wh_5c1de77a90b4426f9c0b12a7", "name": "prod" },
  "email": {
    "id": "AAMkAGI2NmY4ZTk1LTk4YzUtOTk",
    "account_id": "acc_0fdaacb3ecb24d83b83b1f6d",
    "thread_id": "AAQkAGI2NmY4ZTk1LTk4",
    "folder_id": "AQMkAGI2NmY4ZTk1AAAJ",
    "subject": "Re: Q3 roadmap",
    "from": { "name": "Ada Lovelace", "email": "ada@example.com" },
    "to": [{ "name": "Grace Hopper", "email": "grace@example.org" }],
    "date": "2026-08-28T09:41:12Z",
    "snippet": "Works for me — shipping Thursday.",
    "role": "sentitems",
    "read": true,
    "flagged": false,
    "draft": false,
    "has_attachments": false
  }
}`,
	},
	{
		Type:  "mail_updated",
		When:  "A message's read or flagged state changed, from either side.",
		Kinds: allKinds,
		Sample: `{
  "type": "mail_updated",
  "account_id": "acc_0fdaacb3ecb24d83b83b1f6d",
  "timestamp": "2026-08-28T09:44:02Z",
  "webhook": { "id": "wh_5c1de77a90b4426f9c0b12a7", "name": "prod" },
  "email": {
    "id": "AAMkAGI2NmY4ZTk1LTk4YzUtNDQ",
    "account_id": "acc_0fdaacb3ecb24d83b83b1f6d",
    "thread_id": "AAQkAGI2NmY4ZTk1LTk4",
    "folder_id": "AQMkAGI2NmY4ZTk1AAAI",
    "subject": "Q3 roadmap",
    "from": { "name": "Grace Hopper", "email": "grace@example.org" },
    "to": [{ "name": "Ada Lovelace", "email": "ada@example.com" }],
    "date": "2026-08-28T09:12:44Z",
    "snippet": "Pulling the migration forward a week — see the attached plan.",
    "read": true,
    "flagged": true,
    "draft": false,
    "has_attachments": true
  }
}`,
	},
	{
		Type:  "mail_deleted",
		When:  "A message was removed from the mailbox. Only the id survives, so keep your own copy if you need the content.",
		Kinds: allKinds,
		Sample: `{
  "type": "mail_deleted",
  "account_id": "acc_0fdaacb3ecb24d83b83b1f6d",
  "timestamp": "2026-08-28T09:46:30Z",
  "webhook": { "id": "wh_5c1de77a90b4426f9c0b12a7", "name": "prod" },
  "email_id": "AAMkAGI2NmY4ZTk1LTk4YzUtNDQ"
}`,
	},
	{
		Type:  "account_status",
		When:  "A connected account needs attention — usually status CREDENTIALS, meaning the grant died and only the end user can fix it by connecting again.",
		Kinds: allKinds,
		Sample: `{
  "type": "account_status",
  "account_id": "acc_0fdaacb3ecb24d83b83b1f6d",
  "timestamp": "2026-08-28T09:50:00Z",
  "webhook": { "id": "wh_5c1de77a90b4426f9c0b12a7", "name": "prod" },
  "account": {
    "id": "acc_0fdaacb3ecb24d83b83b1f6d",
    "provider": "OUTLOOK",
    "email": "ada@example.com",
    "kind": "mail",
    "identifier": "ada@example.com",
    "name": "Ada Lovelace",
    "status": "CREDENTIALS",
    "created_at": "2026-08-21T10:02:44Z",
    "updated_at": "2026-08-28T09:50:00Z"
  }
}`,
	},
	{
		Type:  "chat_received",
		When:  "Someone else sent a message into one of the account's chats.",
		Kinds: allKinds,
		Sample: `{
  "type": "chat_received",
  "account_id": "acc_7b19e4c2a5f34d16b0c9e831",
  "timestamp": "2026-08-28T09:38:21Z",
  "webhook": { "id": "wh_5c1de77a90b4426f9c0b12a7", "name": "prod" },
  "message": {
    "id": "3EB0C767D26A1D9B7A2E",
    "account_id": "acc_7b19e4c2a5f34d16b0c9e831",
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
    "account_id": "acc_7b19e4c2a5f34d16b0c9e831",
    "kind": "direct",
    "name": "Grace Hopper",
    "unread_count": 1,
    "last_message_at": "2026-08-28T09:38:20Z",
    "archived": false,
    "muted": false
  }
}`,
	},
	{
		Type:  "chat_sent",
		When:  "A message went out from the connected number — through this API, or from the user's own phone.",
		Kinds: allKinds,
		Sample: `{
  "type": "chat_sent",
  "account_id": "acc_7b19e4c2a5f34d16b0c9e831",
  "timestamp": "2026-08-28T09:41:13Z",
  "webhook": { "id": "wh_5c1de77a90b4426f9c0b12a7", "name": "prod" },
  "message": {
    "id": "3EB0C767D26A1D9B8F04",
    "account_id": "acc_7b19e4c2a5f34d16b0c9e831",
    "chat_id": "16505550123@s.whatsapp.net",
    "sender": { "id": "15550100123@s.whatsapp.net", "phone": "+15550100123", "name": "Support line", "is_self": true },
    "is_from_me": true,
    "kind": "text",
    "text": "On it — sending the revised plan now.",
    "sent_at": "2026-08-28T09:41:12Z",
    "deleted": false,
    "status": "sent",
    "reactions": []
  },
  "chat": {
    "id": "16505550123@s.whatsapp.net",
    "account_id": "acc_7b19e4c2a5f34d16b0c9e831",
    "kind": "direct",
    "name": "Grace Hopper",
    "unread_count": 0,
    "last_message_at": "2026-08-28T09:41:12Z",
    "archived": false,
    "muted": false
  }
}`,
	},
	{
		Type:  "chat_updated",
		When:  `A message changed after the fact. "change":"edited" carries the new text; "change":"receipt" carries a delivery or read receipt in "status".`,
		Kinds: allKinds,
		Sample: `{
  "type": "chat_updated",
  "account_id": "acc_7b19e4c2a5f34d16b0c9e831",
  "timestamp": "2026-08-28T09:42:31Z",
  "webhook": { "id": "wh_5c1de77a90b4426f9c0b12a7", "name": "prod" },
  "change": "edited",
  "message_ids": ["3EB0C767D26A1D9B8F04"],
  "message": {
    "id": "3EB0C767D26A1D9B8F04",
    "account_id": "acc_7b19e4c2a5f34d16b0c9e831",
    "chat_id": "16505550123@s.whatsapp.net",
    "sender": { "id": "15550100123@s.whatsapp.net", "phone": "+15550100123", "name": "Support line", "is_self": true },
    "is_from_me": true,
    "kind": "text",
    "text": "On it — revised plan attached instead.",
    "sent_at": "2026-08-28T09:41:12Z",
    "edited_at": "2026-08-28T09:42:30Z",
    "deleted": false,
    "status": "delivered",
    "reactions": []
  }
}`,
	},
	{
		Type:  "chat_reaction",
		When:  `Someone reacted to a message, or removed their reaction — an empty "emoji" means removed.`,
		Kinds: allKinds,
		Sample: `{
  "type": "chat_reaction",
  "account_id": "acc_7b19e4c2a5f34d16b0c9e831",
  "timestamp": "2026-08-28T09:39:02Z",
  "webhook": { "id": "wh_5c1de77a90b4426f9c0b12a7", "name": "prod" },
  "message_ids": ["3EB0C767D26A1D9B7A2E"],
  "reaction": {
    "attendee_id": "15550100123@s.whatsapp.net",
    "emoji": "👍",
    "at": "2026-08-28T09:39:01Z"
  }
}`,
	},
	{
		Type:  "chat_deleted",
		When:  "A message was revoked for everyone. Only the ids survive.",
		Kinds: allKinds,
		Sample: `{
  "type": "chat_deleted",
  "account_id": "acc_7b19e4c2a5f34d16b0c9e831",
  "timestamp": "2026-08-28T09:47:10Z",
  "webhook": { "id": "wh_5c1de77a90b4426f9c0b12a7", "name": "prod" },
  "message_ids": ["3EB0C767D26A1D9B8F04"]
}`,
	},
}

// Errors is every value the `error.code` field can take, with the status it
// arrives on and what to do about it.
//
// Ordered by how likely an integrator is to hit it, not alphabetically: the
// credential and body mistakes first, then the routing ones, then the
// provider- and connection-level failures.
var Errors = []ErrorCode{
	{Code: "unauthorized", Status: 401,
		Fix: "No credential, a revoked key, or an expired session. Send Authorization: Bearer $API_KEY (X-API-Key also works)."},
	{Code: "session_required", Status: 403,
		Fix: "This endpoint is dashboard-session-only so a leaked key cannot mint keys or widen your redirect allowlist. Do it from the dashboard."},
	{Code: "json_required", Status: 415,
		Fix: "A state-changing request riding the session cookie must send Content-Type: application/json. API-key callers are unaffected."},
	{Code: "invalid_body", Status: 400,
		Fix: "Malformed JSON, an unknown field, or a field of the wrong type. The message names the offending field."},
	{Code: "body_too_large", Status: 413,
		Fix: "64 KB for ordinary routes, 8 MB for the send family. Shrink the payload or attach fewer files."},
	{Code: "missing_account_id", Status: 400,
		Fix: "Every mail and chat route is scoped to one account. Add ?account_id=acc_… (or account_id in the body where the route documents it)."},
	{Code: "account_not_found", Status: 404,
		Fix: "No such account for this developer. Cross-tenant ids answer 404 rather than 403, so check you are using the right key."},
	{Code: "not_found", Status: 404,
		Fix: "The chat, message, attendee, webhook or before-cursor does not exist under this account."},
	{Code: "unsupported_for_kind", Status: 400,
		Fix: "A mail route was called on a chat account, or vice versa. Check the account's kind and use /api/v1/emails or /api/v1/chats accordingly."},
	{Code: "unknown_folder_role", Status: 400,
		Fix: "This mailbox has no folder with that role. List /api/v1/folders and use folder_id instead."},
	{Code: "empty_patch", Status: 400,
		Fix: "A PATCH with nothing to change. Supply at least one of the documented fields."},
	{Code: "missing_text", Status: 400,
		Fix: "A chat message needs non-blank text."},
	{Code: "missing_emoji", Status: 400,
		Fix: `Send "emoji": "" to remove a reaction. Leaving the field out entirely is refused, because it is far more likely a dropped field than a deliberate removal.`},
	{Code: "missing_recipient", Status: 400,
		Fix: "Starting a chat needs either phone (E.164) or attendee_id."},
	{Code: "missing_recipients", Status: 400,
		Fix: "Sending or forwarding mail needs a non-empty to. Only a reply may omit it."},
	{Code: "missing_name", Status: 400,
		Fix: "An API key needs a name you will recognise when revoking it."},
	{Code: "attachment_too_large", Status: 400,
		Fix: "Attachments total more than 3 MB. Send fewer, or link to the file instead."},
	{Code: "not_own_message", Status: 403,
		Fix: "Only the sender can edit or delete a message. Check is_from_me before offering the action."},
	{Code: "invalid_credentials", Status: 400,
		Fix: "The current password given to the password-change endpoint is wrong."},
	{Code: "invalid_url", Status: 400,
		Fix: "notify_url must be a public http(s) URL; redirect URLs must be absolute http(s) and their host must be this origin or on your redirect-domain allowlist (Settings → Redirect domains)."},
	{Code: "invalid_webhook", Status: 400,
		Fix: "The hook body does not match its kind: webhook needs a public url, discord a discord.com incoming-webhook URL, telegram both bot_token and chat_id and neither url nor secret."},
	{Code: "unknown_provider", Status: 400,
		Fix: "No provider by that name is registered on this deployment. GET /api/v1/providers to see what is. (The same code appears as 404 on the provider push endpoints.)"},
	{Code: "idempotency_conflict", Status: 409,
		Fix: "This Idempotency-Key was already used for a different operation or body, or the first request with it is still running. Use a fresh key per logical send, and retry the same one only with an identical body."},
	{Code: "account_not_ok", Status: 409,
		Fix: "The account is not in status OK — usually CREDENTIALS. Mint a fresh hosted-auth link and have the end user connect again."},
	{Code: "reconnect_required", Status: 409,
		Fix: "The account exists and you own it, but its live session is down. Wait for the reconnect, POST /reconnect, or relink."},
	{Code: "consent_required", Status: 409,
		Fix: "The QR pairing page asked for a code before the end user accepted the linked-device disclosure. Accept it first."},
	{Code: "expired", Status: 410,
		Fix: "The connect link's window elapsed. Mint another with POST /api/v1/hosted-auth."},
	{Code: "link_browser_mismatch", Status: 403,
		Fix: "A connect link is bound to the browser that opened it. Reopen the link in that browser, or mint a new one."},
	{Code: "capacity", Status: 503,
		Fix: "This deployment is already holding as many live chat sockets as it is configured for, or the chat runtime is disabled. Retry later or raise the limit."},
	{Code: "provider_error", Status: 502,
		Fix: "The upstream provider refused or failed. The message carries their reason; it is usually safe to retry."},
	{Code: "internal", Status: 500,
		Fix: "A bug or an unavailable dependency on our side. Retry; if it persists, quote the timestamp and the request path."},
}
