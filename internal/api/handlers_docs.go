package api

import (
	"html/template"
	"net/http"
	"strings"
)

// handleDocs serves the integration guide. It is public on purpose: an
// integrator reads it before they have an account, and nothing on it is
// secret. The endpoint table is rendered from apiRoutes, so the page cannot
// describe a route the server does not register.
func (s *Server) handleDocs(w http.ResponseWriter, r *http.Request) {
	type route struct{ Method, Path, Group string }
	routes := make([]route, 0, len(apiRoutes))
	for _, p := range apiRoutes {
		method, path, _ := strings.Cut(p, " ")
		routes = append(routes, route{Method: method, Path: path, Group: routeGroup(path)})
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = docsTmpl.Execute(w, struct {
		Routes []route
		Base   string
	}{routes, s.baseURL(r)})
}

// routeGroup buckets a path for the endpoint table.
func routeGroup(path string) string {
	switch {
	case strings.HasPrefix(path, "/api/v1/api-keys"), path == "/api/v1/me":
		return "Developer & keys"
	case path == "/api/v1/hosted-auth", path == "/api/v1/providers":
		return "Connecting mailboxes"
	case strings.Contains(path, "/webhooks"):
		return "Webhooks"
	case strings.HasPrefix(path, "/api/v1/accounts"):
		return "Accounts"
	default:
		return "Mail"
	}
}

var docsTmpl = template.Must(template.New("docs").Parse(docsHTML))

const docsHTML = `<!doctype html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>API integration guide</title>
<style>
:root{--bg:#f7f7f8;--card:#fff;--text:#1a1a1a;--muted:#6b6b76;--border:#e6e6ea;--accent:#2563eb;--accent-text:#fff;
  --ok:#16a34a;--warn:#d97706;--danger:#dc2626;--code:#f1f3f7}
@media (prefers-color-scheme:dark){:root{--bg:#0f1115;--card:#171a21;--text:#f0f0f2;--muted:#9a9aa5;--border:#2a2d36;--code:#0b0d12}}
*{box-sizing:border-box}
body{margin:0;font-family:system-ui,-apple-system,Segoe UI,Roboto,sans-serif;background:var(--bg);color:var(--text);line-height:1.55}
.layout{display:grid;grid-template-columns:16rem minmax(0,1fr);gap:2rem;max-width:72rem;margin:0 auto;padding:2rem 1.5rem}
@media (max-width:52rem){.layout{grid-template-columns:1fr}nav.toc{position:static}}
nav.toc{position:sticky;top:1.5rem;align-self:start;font-size:.9rem}
nav.toc a{display:block;color:var(--muted);text-decoration:none;padding:.15rem 0}
nav.toc a:hover{color:var(--accent)}
nav.toc .top{margin-bottom:1rem}
nav.toc .top a{color:var(--accent);font-weight:600}
main{min-width:0}
h1{font-size:1.75rem;margin:0 0 .25rem}
h2{font-size:1.25rem;margin:2.5rem 0 .75rem;padding-top:.5rem;border-top:1px solid var(--border)}
h3{font-size:1rem;margin:1.5rem 0 .5rem}
p,li{max-width:64ch}
.sub{color:var(--muted);margin-bottom:1.5rem}
code{font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:.88em;background:var(--code);padding:.1em .35em;border-radius:4px}
pre{background:var(--code);border:1px solid var(--border);border-radius:10px;padding:.9rem 1rem;overflow-x:auto;font-size:.84rem;line-height:1.5}
pre code{background:none;padding:0}
table{border-collapse:collapse;width:100%;font-size:.9rem;margin:.75rem 0 1rem}
th,td{text-align:left;padding:.45rem .6rem;border-bottom:1px solid var(--border);vertical-align:top}
th{color:var(--muted);font-weight:600;font-size:.8rem;text-transform:uppercase;letter-spacing:.03em}
.m{display:inline-block;min-width:4.2rem;font-family:ui-monospace,Menlo,monospace;font-size:.78rem;font-weight:700;padding:.1rem .4rem;border-radius:4px;color:var(--accent-text);background:var(--muted)}
.m.GET{background:#16a34a}.m.POST{background:var(--accent)}.m.PATCH{background:#d97706}.m.DELETE{background:var(--danger)}
.note{border-left:3px solid var(--accent);background:var(--card);padding:.6rem .9rem;border-radius:0 8px 8px 0;margin:1rem 0;font-size:.92rem}
.note.warn{border-color:var(--warn)}
.steps{counter-reset:s;padding:0;list-style:none}
.steps>li{counter-increment:s;position:relative;padding-left:2.2rem;margin:.5rem 0}
.steps>li::before{content:counter(s);position:absolute;left:0;top:.05rem;width:1.5rem;height:1.5rem;border-radius:50%;background:var(--accent);color:var(--accent-text);font-size:.8rem;font-weight:700;display:flex;align-items:center;justify-content:center}
footer{margin-top:3rem;color:var(--muted);font-size:.85rem}
</style></head>
<body>
<div class="layout">
<nav class="toc">
  <div class="top"><a href="/dashboard">&larr; Dashboard</a></div>
  <a href="#overview">1. Overview</a>
  <a href="#auth">2. Authentication</a>
  <a href="#connect">3. Connect a mailbox</a>
  <a href="#read">4. Read mail</a>
  <a href="#write">5. Send &amp; update</a>
  <a href="#webhooks">6. Webhooks</a>
  <a href="#lifecycle">7. Account lifecycle</a>
  <a href="#errors">8. Errors &amp; debugging</a>
  <a href="#walkthrough">9. Full walkthrough</a>
  <a href="#reference">10. Endpoint reference</a>
  <a href="#limits">11. Known limits</a>
</nav>
<main>

<h1>API integration guide</h1>
<p class="sub">Everything an integrator needs to connect end users&rsquo; mailboxes, read and send mail through one API, and receive webhooks when new mail arrives. Base URL for this deployment: <code>{{.Base}}</code>. Building with an AI assistant? Give it <a href="/llms.txt"><code>/llms.txt</code></a> &mdash; the same guide as exact, machine-readable Markdown.</p>

<h2 id="overview">1. Overview</h2>
<p>The service sits between your application and your users&rsquo; mail providers (Outlook / Microsoft 365 today). You never see a provider payload &mdash; every message, folder and event comes back in one normalized shape.</p>
<table>
<tr><th>Concept</th><th>What it is</th></tr>
<tr><td><b>Developer</b></td><td>You. One login (email + password) on the <a href="/dashboard">dashboard</a>, owning everything below.</td></tr>
<tr><td><b>API key</b></td><td>A bearer token your server sends on every API call. Create as many as you like; revoke individually.</td></tr>
<tr><td><b>Account</b></td><td>One connected mailbox belonging to one of <i>your</i> end users. Identified by <code>account_id</code>. Everything mail-related is scoped to an account.</td></tr>
<tr><td><b>Webhook</b></td><td>A URL you own that we POST normalized events to (<code>mail_received</code> and friends), signed and retried.</td></tr>
</table>
<div class="note">Tenancy is strict: an account, webhook or email that belongs to another developer answers <b>404</b>, never 403 &mdash; ids are not enumerable across tenants.</div>

<h2 id="auth">2. Authentication</h2>
<ol class="steps">
<li>Sign up at <a href="/signup"><code>/signup</code></a> and open the <a href="/dashboard">dashboard</a>.</li>
<li>Under <b>API keys</b>, create a key (e.g. <code>production</code>). The full key is shown <b>once</b>. Store it in your secret manager.</li>
<li>Send it on every request:</li>
</ol>
<pre><code>Authorization: Bearer um_XXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX</code></pre>
<p><code>X-API-Key: &lt;key&gt;</code> is accepted as an alternative header. Check who you are with:</p>
<pre><code>curl -s {{.Base}}/api/v1/me -H "Authorization: Bearer $API_KEY"
# {"id":"dev_…","email":"you@example.com","name":"…","created_at":"…","auth":"api_key"}</code></pre>
<div class="note warn">Creating and revoking keys is <b>session-only</b> (dashboard). Calling <code>POST /api/v1/api-keys</code> with an API key returns <code>403 session_required</code>, so a leaked key cannot mint more keys.</div>

<h2 id="connect">3. Connect a mailbox</h2>
<p>You never handle your users&rsquo; provider credentials. You mint a single-use <b>connect link</b>, hand it to the user, and we run the OAuth consent flow. When it completes, the account exists under your developer and syncing starts immediately.</p>
<h3>3.1 Mint a connect link</h3>
<pre><code>curl -s -X POST {{.Base}}/api/v1/hosted-auth \
  -H "Authorization: Bearer $API_KEY" -H "Content-Type: application/json" \
  -d '{
    "success_redirect_url": "https://app.example.com/mail/connected",
    "failure_redirect_url": "https://app.example.com/mail/failed",
    "notify_url":           "https://api.example.com/hooks/mail-connected",
    "webhook": { "url": "https://api.example.com/hooks/mail", "secret": "…", "name": "prod" },
    "expires_in_minutes": 30
  }'
# {"url":"{{.Base}}/connect/&lt;state&gt;","state":"…","provider":"OUTLOOK","expires_at":"…"}</code></pre>
<table>
<tr><th>Field</th><th>Purpose</th></tr>
<tr><td><code>success_redirect_url</code></td><td>Where the user&rsquo;s browser lands afterwards; we append <code>?account_id=…</code>. Optional.</td></tr>
<tr><td><code>failure_redirect_url</code></td><td>Where the browser lands on cancel/error; we append <code>?error=…&amp;error_description=…</code>. Optional.</td></tr>
<tr><td><code>notify_url</code></td><td>Server-to-server POST the moment the connection completes, so your backend learns the <code>account_id</code> without depending on the browser. Optional but recommended. Must be a public http(s) URL.</td></tr>
<tr><td><code>webhook</code></td><td>Register this account&rsquo;s webhook <i>before its first sync</i>, so nothing is missed. <code>events</code> defaults to <code>["mail_received"]</code>. Optional.</td></tr>
<tr><td><code>provider</code></td><td>Only needed when several providers are configured; see <code>GET /api/v1/providers</code>.</td></tr>
<tr><td><code>force_consent</code></td><td>Re-prompt consent even if the provider would sign in silently (e.g. after a scope change).</td></tr>
</table>
<h3>3.2 Send the user there</h3>
<p>Open <code>url</code> in the user&rsquo;s browser (redirect, new tab, or an email link). They see a branded confirmation page, then the provider&rsquo;s own sign-in and consent screens. The link is single-use and expires.</p>
<h3>3.3 Learn the result</h3>
<p><code>notify_url</code> receives one JSON POST:</p>
<pre><code>{"status":"CREATED","account_id":"acc_…","email":"user@outlook.com","provider":"OUTLOOK"}
{"status":"FAILED","error":"access_denied","message":"…"}</code></pre>
<p>Persist <code>account_id</code> against your user. Reconnecting the same mailbox later keeps the same id. Backfill (the last 30 days by default) runs in the background; <code>GET /api/v1/accounts/{id}</code> shows <code>last_synced_at</code> once the first pass completes.</p>

<h2 id="read">4. Read mail</h2>
<p>All mail routes take <code>?account_id=acc_…</code>. Reads are served from our local mirror, so they are fast and never count against the provider.</p>
<h3>4.1 List messages</h3>
<pre><code>curl -s "{{.Base}}/api/v1/emails?account_id=$ACC&amp;folder_role=inbox&amp;unread=true&amp;limit=50&amp;offset=0" \
  -H "Authorization: Bearer $API_KEY"</code></pre>
<table>
<tr><th>Filter</th><th>Meaning</th></tr>
<tr><td><code>folder_role</code></td><td><code>inbox</code>, <code>sentitems</code>, <code>drafts</code>, <code>deleteditems</code>, <code>junkemail</code>, <code>archive</code> &mdash; no need to know provider folder ids.</td></tr>
<tr><td><code>folder_id</code></td><td>An id from <code>GET /api/v1/folders</code>.</td></tr>
<tr><td><code>thread_id</code></td><td>All messages in one conversation.</td></tr>
<tr><td><code>unread=true</code></td><td>Unread only.</td></tr>
<tr><td><code>q</code></td><td>Substring match on subject, snippet and sender.</td></tr>
<tr><td><code>limit</code>, <code>offset</code></td><td>Paging; <code>limit</code> max 200.</td></tr>
</table>
<p>List items omit <code>body</code>; fetch one message for the content.</p>
<h3>4.2 Get one message</h3>
<pre><code>curl -s "{{.Base}}/api/v1/emails/$MSG_ID?account_id=$ACC" -H "Authorization: Bearer $API_KEY"</code></pre>
<pre><code>{
  "id": "…", "account_id": "acc_…", "thread_id": "…", "folder_id": "…", "role": "inbox",
  "subject": "Quarterly numbers",
  "from": {"name": "Ada", "email": "ada@example.com"},
  "to": [{"name": "Bob", "email": "bob@example.com"}], "cc": [], "bcc": [], "reply_to": [],
  "date": "2026-08-20T09:30:00Z",
  "snippet": "Here they are…",
  "body": "&lt;html&gt;…&lt;/html&gt;", "body_type": "html",
  "body_plain": "Here they are …",
  "read": false, "flagged": false, "draft": false,
  "has_attachments": true,
  "attachments": [{"id": "…", "name": "q3.pdf", "mime_type": "application/pdf", "size": 140994, "is_inline": false}],
  "internet_message_id": "&lt;abc@example.com&gt;"
}</code></pre>
<p>One call returns the complete message: full HTML <code>body</code>, <code>body_plain</code> with markup stripped, the well-known folder <code>role</code>, and attachment metadata. Download an attachment&rsquo;s bytes with <code>GET /api/v1/emails/{id}/attachments/{aid}</code> (streams with the right <code>Content-Type</code> and filename).</p>
<h3>4.3 Threads and folders</h3>
<pre><code>curl -s "{{.Base}}/api/v1/threads?account_id=$ACC"  -H "Authorization: Bearer $API_KEY"
curl -s "{{.Base}}/api/v1/folders?account_id=$ACC"  -H "Authorization: Bearer $API_KEY"</code></pre>

<h2 id="write">5. Send &amp; update</h2>
<h3>5.1 Send a new message</h3>
<pre><code>curl -s -X POST {{.Base}}/api/v1/emails \
  -H "Authorization: Bearer $API_KEY" -H "Content-Type: application/json" \
  -d '{
    "account_id": "acc_…",
    "to":  [{"email": "someone@example.com", "name": "Someone"}],
    "cc":  [], "bcc": [],
    "subject": "Hello",
    "body": "&lt;p&gt;Hi there&lt;/p&gt;", "body_type": "html",
    "attachments": [{"name": "brief.pdf", "mime_type": "application/pdf", "content": "&lt;base64&gt;"}]
  }'
# 202 {"status":"sent"}</code></pre>
<p>Inline attachments are capped around 3&nbsp;MB by the provider. The sent message appears in <code>sentitems</code> on the next sync and emits a <code>mail_sent</code> event.</p>
<h3>5.2 Reply and forward in-thread</h3>
<pre><code>curl -s -X POST "{{.Base}}/api/v1/emails/$MSG_ID/reply?account_id=$ACC" \
  -H "Authorization: Bearer $API_KEY" -H "Content-Type: application/json" \
  -d '{"body": "&lt;p&gt;Thanks!&lt;/p&gt;", "reply_all": false}'

curl -s -X POST "{{.Base}}/api/v1/emails/$MSG_ID/forward?account_id=$ACC" \
  -H "Authorization: Bearer $API_KEY" -H "Content-Type: application/json" \
  -d '{"to": [{"email": "colleague@example.com"}], "body": "&lt;p&gt;FYI&lt;/p&gt;"}'</code></pre>
<p>Threading headers are generated by the provider, so replies land in the right conversation in every client. <code>POST /api/v1/emails</code> with <code>"reply_to_email_id"</code> is equivalent to the reply route.</p>
<h3>5.3 Drafts</h3>
<pre><code>curl -s -X POST {{.Base}}/api/v1/drafts -H "Authorization: Bearer $API_KEY" -H "Content-Type: application/json" \
  -d '{"account_id":"acc_…","to":[{"email":"x@example.com"}],"subject":"Draft","body":"…"}'
# returns the draft as an email object; later:
curl -s -X POST "{{.Base}}/api/v1/drafts/$DRAFT_ID/send?account_id=$ACC" -H "Authorization: Bearer $API_KEY"</code></pre>
<h3>5.4 Mark read / flag</h3>
<pre><code>curl -s -X PATCH "{{.Base}}/api/v1/emails/$MSG_ID?account_id=$ACC" \
  -H "Authorization: Bearer $API_KEY" -H "Content-Type: application/json" \
  -d '{"read": true, "flagged": false}'</code></pre>

<h2 id="webhooks">6. Webhooks</h2>
<p>Register a URL and we POST normalized events to it. Two scopes:</p>
<table>
<tr><th>Scope</th><th>Register with</th><th>Receives</th></tr>
<tr><td>Per account</td><td><code>POST /api/v1/accounts/{id}/webhooks</code> or the <code>webhook</code> field at connect time</td><td>Events for that account only. <code>events</code> defaults to <code>["mail_received"]</code>.</td></tr>
<tr><td>Developer-wide</td><td><code>POST /api/v1/webhooks</code></td><td>Events for every account you own. Empty <code>events</code> means everything.</td></tr>
</table>
<pre><code>curl -s -X POST "{{.Base}}/api/v1/accounts/$ACC/webhooks" \
  -H "Authorization: Bearer $API_KEY" -H "Content-Type: application/json" \
  -d '{"url":"https://api.example.com/hooks/mail","secret":"choose-a-long-random-secret","name":"prod","events":["mail_received"]}'</code></pre>
<p>Event names: <code>mail_received</code>, <code>mail_sent</code>, <code>mail_updated</code> (read/flag changes), <code>mail_deleted</code>, <code>account_status</code>. Use <code>"*"</code> for all. URLs must be public http(s); loopback, link-local and private addresses are rejected.</p>
<h3>6.1 Payload</h3>
<pre><code>POST https://api.example.com/hooks/mail
Content-Type: application/json
X-Outlook-Event: mail_received
X-Outlook-Delivery: 1                      # attempt number
X-Outlook-Signature: sha256=&lt;hex hmac&gt;    # when a secret is set

{
  "type": "mail_received",
  "account_id": "acc_…",
  "timestamp": "2026-08-25T11:30:02Z",
  "webhook": {"id": "wh_…", "name": "prod"},
  "email": { …the complete message object from §4.2, including attachments… }
}</code></pre>
<p><code>mail_deleted</code> carries <code>email_id</code> instead of <code>email</code>; <code>account_status</code> carries <code>account</code> with the new <code>status</code>.</p>
<h3>6.2 Verify the signature</h3>
<p>Compute HMAC-SHA256 of the <b>raw request body</b> with your secret and compare it, constant-time, to the header:</p>
<pre><code>// Go
mac := hmac.New(sha256.New, []byte(secret))
mac.Write(rawBody)
want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
ok := hmac.Equal([]byte(want), []byte(r.Header.Get("X-Outlook-Signature")))

# Node
const want = "sha256=" + crypto.createHmac("sha256", secret).update(rawBody).digest("hex");
const ok = crypto.timingSafeEqual(Buffer.from(want), Buffer.from(req.get("X-Outlook-Signature")));</code></pre>
<h3>6.3 Delivery guarantees</h3>
<ul>
<li>Respond with any <b>2xx</b> within 15&nbsp;s. Anything else &mdash; or a timeout &mdash; is a failure.</li>
<li>Failures are retried on a fixed schedule after the immediate first attempt: <b>30s, 2m, 10m, 30m, 2h, 6h, 12h</b>. After the last retry the delivery is marked <code>dead</code> and kept.</li>
<li>Delivery is <b>at-least-once</b>. Dedupe on <code>(type, email.id)</code>; the <code>X-Outlook-Delivery</code> attempt number tells you a redelivery from a first attempt.</li>
<li>Inspect the queue for a hook: <code>GET /api/v1/webhooks/{id}/deliveries</code> lists pending and dead deliveries with <code>attempts</code>, <code>next_attempt_at</code> and <code>last_error</code>.</li>
</ul>

<h2 id="lifecycle">7. Account lifecycle</h2>
<table>
<tr><th>Status</th><th>Meaning</th><th>What to do</th></tr>
<tr><td><code>OK</code></td><td>Token valid, syncing.</td><td>Nothing.</td></tr>
<tr><td><code>CREDENTIALS</code></td><td>The provider rejected the refresh token (password change, consent revoked, policy).</td><td>Mint a new connect link for the same user; reconnecting keeps the <code>account_id</code>. You also receive an <code>account_status</code> event.</td></tr>
</table>
<ul>
<li><code>POST /api/v1/accounts/{id}/resync</code> forces a sync now (otherwise incremental sync runs on a poll interval, or instantly via provider push when the deployment has a public HTTPS origin).</li>
<li><code>DELETE /api/v1/accounts/{id}</code> disconnects: tokens, mirror, subscriptions and per-account webhooks are removed.</li>
</ul>

<h2 id="errors">8. Errors &amp; debugging</h2>
<p>Every error is JSON with a stable machine-readable code:</p>
<pre><code>{"error":{"code":"account_not_found","message":"no such account: acc_…"}}</code></pre>
<table>
<tr><th>Status</th><th>Codes</th></tr>
<tr><td>400</td><td><code>invalid_body</code>, <code>missing_account_id</code>, <code>missing_recipients</code>, <code>invalid_webhook</code>, <code>invalid_url</code>, <code>unknown_folder_role</code>, <code>missing_name</code></td></tr>
<tr><td>401</td><td><code>unauthorized</code> &mdash; missing, invalid or revoked key</td></tr>
<tr><td>403</td><td><code>session_required</code> &mdash; key management needs the dashboard session</td></tr>
<tr><td>404</td><td><code>account_not_found</code>, <code>not_found</code> &mdash; including anything owned by another developer</td></tr>
<tr><td>409</td><td><code>account_not_ok</code>, <code>reconnect_required</code></td></tr>
<tr><td>415</td><td><code>json_required</code> &mdash; dashboard-session writes must send <code>Content-Type: application/json</code></td></tr>
<tr><td>502</td><td><code>provider_error</code> &mdash; the mail provider failed; the message carries its error code</td></tr>
</table>
<p>Every response carries <code>X-Request-Id</code>. Send your own value in the request header to have it echoed and used in our logs; quote it when reporting a problem.</p>

<h2 id="walkthrough">9. Full walkthrough</h2>
<pre><code>export API_KEY=um_…            # from the dashboard
export BASE={{.Base}}

# 1. who am I
curl -s $BASE/api/v1/me -H "Authorization: Bearer $API_KEY"

# 2. mint a connect link with a webhook attached, open it in the user's browser
curl -s -X POST $BASE/api/v1/hosted-auth -H "Authorization: Bearer $API_KEY" -H "Content-Type: application/json" \
  -d '{"notify_url":"https://api.example.com/hooks/connected","webhook":{"url":"https://api.example.com/hooks/mail","secret":"s3cret"}}' | jq -r .url

# 3. after the user consents, notify_url received {"status":"CREATED","account_id":"acc_…"}
export ACC=acc_…

# 4. wait for backfill, then read
curl -s "$BASE/api/v1/accounts/$ACC" -H "Authorization: Bearer $API_KEY" | jq .last_synced_at
curl -s "$BASE/api/v1/emails?account_id=$ACC&amp;folder_role=inbox&amp;limit=5" -H "Authorization: Bearer $API_KEY" | jq '.items[] | {id, subject, from}'
export MSG=…
curl -s "$BASE/api/v1/emails/$MSG?account_id=$ACC" -H "Authorization: Bearer $API_KEY" | jq '{subject, body_plain, attachments}'

# 5. reply
curl -s -X POST "$BASE/api/v1/emails/$MSG/reply?account_id=$ACC" -H "Authorization: Bearer $API_KEY" \
  -H "Content-Type: application/json" -d '{"body":"&lt;p&gt;Got it, thanks.&lt;/p&gt;"}'

# 6. from now on, every new inbound message is POSTed to https://api.example.com/hooks/mail
#    verify X-Outlook-Signature, dedupe on (type, email.id), respond 2xx.</code></pre>

<h2 id="reference">10. Endpoint reference</h2>
<p>Generated from the routes this server registers. All require a developer credential; mail routes additionally require <code>?account_id</code> (or <code>account_id</code> in the JSON body for sends and drafts).</p>
<table>
<tr><th>Group</th><th>Method</th><th>Path</th></tr>
{{range .Routes}}<tr data-route="{{.Method}} {{.Path}}"><td>{{.Group}}</td><td><span class="m {{.Method}}">{{.Method}}</span></td><td><code>{{.Path}}</code></td></tr>
{{end}}</table>

<h2 id="limits">11. Known limits</h2>
<ul>
<li>Outlook / Microsoft 365 is the only provider today. Calendar and contacts are not exposed.</li>
<li>Backfill is bounded (30 days by default); older mail is fetched on demand by <code>GET /api/v1/emails/{id}</code>.</li>
<li>Inline attachment uploads are capped by the provider (~3&nbsp;MB).</li>
<li>Webhook delivery is at-least-once; the very first attempt is in-memory, so a crash between an event and its first POST can lose it &mdash; the next sync re-converges the mirror but does not replay the event.</li>
<li>Paging is offset-based; a mailbox that changes during pagination can shift items between pages.</li>
<li>Webhook URLs are checked for literal private/loopback addresses only; hostnames are not resolved.</li>
</ul>

<footer>Signed in? Manage keys and connected accounts on the <a href="/dashboard">dashboard</a>. Problems? Include the <code>X-Request-Id</code> from the failing response.</footer>
</main>
</div>
</body></html>`
