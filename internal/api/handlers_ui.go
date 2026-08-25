package api

import (
	"html/template"
	"net/http"
)

// This file holds the two screens a human being actually looks at.
//
// The landing page is end-user facing: it appears once, mid-OAuth, and has no
// login of its own — its only authority is the single-use state token already
// embedded in its URL. The dashboard is operator/developer facing: it is a
// static shell with no server-side session, and everything it shows is gated
// by the caller pasting in the API key, which is held in the browser only.
//
// Both are plain html/template + vanilla JS. No build step, no framework, no
// external assets — this ships inside the Go binary.

// ---------- landing page ----------

type landingData struct {
	Provider     string
	AuthorizeURL string
}

var landingTmpl = template.Must(template.New("landing").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Connect your {{.Provider}} account</title>
<style>
:root{--bg:#f7f7f8;--card:#fff;--text:#1a1a1a;--muted:#6b6b76;--border:#e6e6ea;--accent:#2563eb;--accent-text:#fff}
@media (prefers-color-scheme:dark){:root{--bg:#0f1115;--card:#171a21;--text:#f0f0f2;--muted:#9a9aa5;--border:#2a2d36;--accent:#3b82f6;--accent-text:#fff}}
*{box-sizing:border-box}
body{margin:0;min-height:100vh;display:flex;align-items:center;justify-content:center;
  background:var(--bg);color:var(--text);font:16px/1.5 -apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif;padding:1.5rem}
.card{background:var(--card);border:1px solid var(--border);border-radius:16px;padding:2.5rem 2rem;
  max-width:26rem;width:100%;text-align:center;box-shadow:0 1px 3px rgba(0,0,0,.06)}
.badge{width:56px;height:56px;border-radius:14px;background:var(--accent);color:var(--accent-text);
  display:flex;align-items:center;justify-content:center;margin:0 auto 1.25rem;font-size:1.5rem;font-weight:600}
h1{font-size:1.25rem;margin:0 0 .5rem}
p{color:var(--muted);margin:0 0 1.75rem;font-size:.95rem}
.btn{display:inline-block;width:100%;padding:.75rem 1rem;border-radius:10px;background:var(--accent);
  color:var(--accent-text);text-decoration:none;font-weight:600;font-size:.95rem}
.btn:hover{opacity:.92}
.fine{margin-top:1.25rem;font-size:.8rem;color:var(--muted)}
</style></head>
<body>
  <div class="card">
    <div class="badge">{{slice .Provider 0 1}}</div>
    <h1>Connect your {{.Provider}} account</h1>
    <p>You'll sign in on Microsoft's own page and choose what to share. We never see your password.</p>
    <a class="btn" href="{{.AuthorizeURL}}">Continue with {{.Provider}}</a>
    <p class="fine">You can disconnect this at any time.</p>
  </div>
</body></html>`))

func renderLanding(w http.ResponseWriter, d landingData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = landingTmpl.Execute(w, d)
}

// ---------- dashboard ----------

// handleDashboard serves the static shell. It is deliberately not behind
// requireAPIKey: the HTML itself carries nothing sensitive. Every fetch the
// page makes carries the key the visitor pastes in, so the account data stays
// gated exactly where the REST API already gates it.
func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(dashboardHTML))
}

const dashboardHTML = `<!doctype html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Connected accounts</title>
<style>
:root{--bg:#f7f7f8;--card:#fff;--text:#1a1a1a;--muted:#6b6b76;--border:#e6e6ea;--accent:#2563eb;--accent-text:#fff;
  --ok:#16a34a;--warn:#d97706;--danger:#dc2626;--danger-bg:#fef2f2}
@media (prefers-color-scheme:dark){:root{--bg:#0f1115;--card:#171a21;--text:#f0f0f2;--muted:#9a9aa5;--border:#2a2d36;
  --accent:#3b82f6;--accent-text:#fff;--ok:#4ade80;--warn:#fbbf24;--danger:#f87171;--danger-bg:#2a1418}}
*{box-sizing:border-box}
body{margin:0;background:var(--bg);color:var(--text);font:15px/1.5 -apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif}
.wrap{max-width:44rem;margin:0 auto;padding:2.5rem 1.5rem}
header{display:flex;align-items:center;justify-content:space-between;margin-bottom:1.5rem;gap:1rem;flex-wrap:wrap}
h1{font-size:1.35rem;margin:0}
.sub{color:var(--muted);font-size:.85rem;margin-top:.15rem}
button,.btn{font:inherit;cursor:pointer;border:1px solid var(--border);background:var(--card);color:var(--text);
  padding:.5rem .9rem;border-radius:8px;text-decoration:none;font-size:.85rem}
button:hover{border-color:var(--accent)}
.primary{background:var(--accent);color:var(--accent-text);border-color:var(--accent)}
.primary:hover{opacity:.92}
.danger{color:var(--danger)}
input{font:inherit;padding:.6rem .8rem;border:1px solid var(--border);border-radius:8px;background:var(--card);
  color:var(--text);width:100%}
.card{background:var(--card);border:1px solid var(--border);border-radius:12px;padding:1.25rem}
.gate{max-width:24rem;margin:4rem auto;text-align:center}
.gate p{color:var(--muted);font-size:.9rem}
.gate form{display:flex;flex-direction:column;gap:.75rem;margin-top:1rem}
.row{display:flex;flex-wrap:wrap;align-items:center;justify-content:space-between;gap:1rem;padding:.9rem 0;border-bottom:1px solid var(--border)}
.row:last-child{border-bottom:none}
.who{min-width:0}
.email{font-weight:600;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.meta{color:var(--muted);font-size:.8rem;margin-top:.15rem}
.status{display:inline-flex;align-items:center;gap:.4rem;font-size:.8rem;font-weight:600;padding:.2rem .55rem;border-radius:999px}
.status.ok{color:var(--ok);background:color-mix(in srgb, var(--ok) 15%, transparent)}
.status.bad{color:var(--warn);background:color-mix(in srgb, var(--warn) 15%, transparent)}
.actions{display:flex;gap:.4rem;flex-shrink:0}
.hook{flex-basis:100%;display:flex;flex-wrap:wrap;align-items:center;gap:.4rem;margin-top:.6rem;
  padding-top:.6rem;border-top:1px dashed var(--border);font-size:.8rem;color:var(--muted)}
.hook code{color:var(--text);word-break:break-all}
.hook input{font:inherit;padding:.35rem .5rem;border:1px solid var(--border);border-radius:6px;
  background:var(--bg);color:var(--text);min-width:0}
.hook input[name=url]{flex:1 1 14rem}
.hook input[name=secret]{flex:0 1 9rem}
.hook button{padding:.35rem .7rem;font-size:.8rem}
.empty{color:var(--muted);text-align:center;padding:3rem 1rem;font-size:.9rem}
.err{color:var(--danger);background:var(--danger-bg);border-radius:8px;padding:.6rem .8rem;font-size:.85rem;margin-bottom:1rem}
.hidden{display:none}
.signout{font-size:.8rem;color:var(--muted);background:none;border:none;padding:0;text-decoration:underline}
</style></head>
<body>
<div class="wrap">

  <div id="gate" class="gate">
    <h1>Connected accounts</h1>
    <p>Paste the service's API key to manage connections.</p>
    <form id="gate-form">
      <input id="key-input" type="password" placeholder="API key" autocomplete="off" required>
      <button class="primary" type="submit">Continue</button>
    </form>
    <p id="gate-err" class="err hidden"></p>
  </div>

  <div id="app" class="hidden">
    <header>
      <div>
        <h1>Connected accounts</h1>
        <div class="sub" id="provider-line"></div>
      </div>
      <div style="display:flex;gap:.5rem;align-items:center">
        <button id="connect-btn" class="primary">+ Connect account</button>
        <button id="signout-btn" class="signout">Sign out</button>
      </div>
    </header>
    <p id="banner" class="err hidden" style="color:var(--ok);background:color-mix(in srgb, var(--ok) 12%, transparent)"></p>
    <p id="list-err" class="err hidden"></p>
    <div class="card">
      <div id="list"></div>
    </div>
  </div>

</div>

<script>
const KEY_STORAGE = "um_api_key";
const $ = (id) => document.getElementById(id);

function apiKey() { return localStorage.getItem(KEY_STORAGE) || ""; }

async function api(path, opts) {
  const res = await fetch(path, Object.assign({}, opts, {
    headers: Object.assign({ "Authorization": "Bearer " + apiKey() }, (opts && opts.headers) || {})
  }));
  if (res.status === 401) { signOut(); throw new Error("unauthorized"); }
  if (!res.ok) {
    let msg = res.statusText;
    try { msg = (await res.json()).error.message; } catch (e) {}
    throw new Error(msg);
  }
  if (res.status === 204) return null;
  return res.json();
}

function signOut() {
  localStorage.removeItem(KEY_STORAGE);
  $("app").classList.add("hidden");
  $("gate").classList.remove("hidden");
}

function statusBadge(status) {
  const ok = status === "OK";
  return '<span class="status ' + (ok ? "ok" : "bad") + '">' + (ok ? "Connected" : "Needs reconnect") + "</span>";
}

function fmtTime(iso) {
  if (!iso) return "never synced";
  const d = new Date(iso);
  return "synced " + d.toLocaleString();
}

async function loadProviders() {
  const data = await api("/api/v1/providers");
  const names = data.items.map((p) => p.name.charAt(0) + p.name.slice(1).toLowerCase());
  $("provider-line").textContent = names.length
    ? "Providers: " + names.join(", ")
    : "No providers configured";
}

async function loadAccounts() {
  $("list-err").classList.add("hidden");
  try {
    const data = await api("/api/v1/accounts");
    renderAccounts(data.items || []);
  } catch (e) {
    $("list-err").textContent = "Could not load accounts: " + e.message;
    $("list-err").classList.remove("hidden");
  }
}

function renderAccounts(items) {
  const list = $("list");
  if (!items.length) {
    list.innerHTML = '<div class="empty">No accounts connected yet.</div>';
    return;
  }
  list.innerHTML = items.map((a) => (
    '<div class="row" data-id="' + a.id + '">' +
      '<div class="who">' +
        '<div class="email">' + escapeHtml(a.email) + "</div>" +
        '<div class="meta">' + escapeHtml(a.provider) + " &middot; " + fmtTime(a.last_synced_at) + "</div>" +
      "</div>" +
      statusBadge(a.status) +
      '<div class="actions">' +
        '<a class="btn" href="/mail?account_id=' + a.id + '">View mail</a>' +
        '<button data-action="resync">Resync</button>' +
        '<button data-action="disconnect" class="danger">Disconnect</button>' +
      "</div>" +
      '<div class="hook" data-hook>Loading webhook&hellip;</div>' +
    "</div>"
  )).join("");
  items.forEach((a) => loadWebhook(a.id));
}

// Each account has at most one webhook from this UI: new mail for that user
// is POSTed there. The API allows several; the dashboard keeps it simple.
async function loadWebhook(id) {
  const el = document.querySelector('.row[data-id="' + id + '"] [data-hook]');
  if (!el) return;
  try {
    const data = await api("/api/v1/accounts/" + id + "/webhooks");
    renderWebhook(el, (data.items || [])[0]);
  } catch (e) {
    el.textContent = "Could not load webhook: " + e.message;
  }
}

function renderWebhook(el, hook) {
  if (hook) {
    el.innerHTML =
      "Webhook: <code>" + escapeHtml(hook.url) + "</code>" +
      '<button data-action="remove-webhook" data-wid="' + hook.id + '" class="danger">Remove</button>';
    return;
  }
  el.innerHTML =
    '<input name="url" type="url" placeholder="https://your-app.example.com/hooks/mail" required>' +
    '<input name="secret" type="text" placeholder="secret (optional)">' +
    '<button data-action="set-webhook">Set webhook</button>';
}

function escapeHtml(s) {
  return String(s).replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
}

$("list").addEventListener("click", async (e) => {
  const btn = e.target.closest("button[data-action]");
  if (!btn) return;
  const id = btn.closest(".row").dataset.id;
  const action = btn.dataset.action;

  if (action === "resync") {
    btn.disabled = true;
    try { await api("/api/v1/accounts/" + id + "/resync", { method: "POST" }); }
    catch (e) { alert("Resync failed: " + e.message); }
    btn.disabled = false;
    return;
  }
  if (action === "set-webhook") {
    const box = btn.closest("[data-hook]");
    const url = box.querySelector('input[name=url]').value.trim();
    const secret = box.querySelector('input[name=secret]').value.trim();
    if (!url) return;
    btn.disabled = true;
    try {
      await api("/api/v1/accounts/" + id + "/webhooks", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ url, secret })
      });
      loadWebhook(id);
    } catch (e) {
      alert("Could not set webhook: " + e.message);
      btn.disabled = false;
    }
    return;
  }
  if (action === "remove-webhook") {
    btn.disabled = true;
    try {
      await api("/api/v1/accounts/" + id + "/webhooks/" + btn.dataset.wid, { method: "DELETE" });
      loadWebhook(id);
    } catch (e) {
      alert("Could not remove webhook: " + e.message);
      btn.disabled = false;
    }
    return;
  }
  if (action === "disconnect") {
    if (!confirm("Disconnect this account? Any app using it will lose access immediately.")) return;
    btn.disabled = true;
    try {
      await api("/api/v1/accounts/" + id, { method: "DELETE" });
      loadAccounts();
    } catch (e) {
      alert("Disconnect failed: " + e.message);
      btn.disabled = false;
    }
  }
});

$("connect-btn").addEventListener("click", async () => {
  $("connect-btn").disabled = true;
  try {
    const dest = location.origin + location.pathname + "?connected=1";
    const data = await api("/api/v1/hosted-auth", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ success_redirect_url: dest })
    });
    location.href = data.url;
  } catch (e) {
    alert("Could not start connect flow: " + e.message);
    $("connect-btn").disabled = false;
  }
});

$("signout-btn").addEventListener("click", signOut);

$("gate-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  $("gate-err").classList.add("hidden");
  const key = $("key-input").value.trim();
  if (!key) return;
  localStorage.setItem(KEY_STORAGE, key);
  try {
    await enter();
  } catch (err) {
    localStorage.removeItem(KEY_STORAGE);
    $("gate-err").textContent = "That key was rejected.";
    $("gate-err").classList.remove("hidden");
  }
});

async function enter() {
  await loadProviders();
  $("gate").classList.add("hidden");
  $("app").classList.remove("hidden");
  if (new URLSearchParams(location.search).get("connected")) {
    $("banner").textContent = "Account connected.";
    $("banner").classList.remove("hidden");
    history.replaceState(null, "", location.pathname);
  }
  await loadAccounts();
}

(function init() {
  if (apiKey()) {
    enter().catch(() => {
      $("gate").classList.remove("hidden");
      $("app").classList.add("hidden");
    });
  }
})();
</script>
</body></html>`
