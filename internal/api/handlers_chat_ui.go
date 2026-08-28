package api

import (
	"html/template"
	"net/http"
	"net/url"
)

// The chat viewer is the fourth human-facing screen: a live, two-pane client
// over the linked device's chats. Like the mail viewer it requires a browser
// session; the page's own fetches then ride the same cookie, so data stays
// gated exactly where the REST API already gates it. Its only server-rendered
// value is the CSRF token its Log out form has to carry.
func (s *Server) handleChatPage(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.sessionDeveloper(w, r); !ok {
		http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusFound)
		return
	}
	// Minted before anything is written; the Log out form below carries it.
	csrf := s.csrfToken(w, r)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = chatTmpl.Execute(w, struct{ CSRF string }{csrf})
}

var chatTmpl = template.Must(template.New("chat").Parse(chatHTML))

const chatHTML = `<!doctype html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Chat</title>
<style>
:root{--bg:#f7f7f8;--card:#fff;--text:#1a1a1a;--muted:#6b6b76;--border:#e6e6ea;--accent:#2563eb;--accent-text:#fff;
  --ok:#16a34a;--warn:#d97706;--danger:#dc2626;--danger-bg:#fef2f2;--hover:#f0f0f3;--sel:#e8effd;--bubble:#eef0f4;--bubble-me:#dbe7ff}
@media (prefers-color-scheme:dark){:root{--bg:#0f1115;--card:#171a21;--text:#f0f0f2;--muted:#9a9aa5;--border:#2a2d36;
  --accent:#3b82f6;--accent-text:#fff;--ok:#4ade80;--warn:#fbbf24;--danger:#f87171;--danger-bg:#2a1418;--hover:#1d212b;--sel:#1e2a44;
  --bubble:#20242e;--bubble-me:#22314f}}
*{box-sizing:border-box}
html,body{height:100%}
body{margin:0;background:var(--bg);color:var(--text);font:14px/1.5 -apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif}
button,select,input{font:inherit;color:var(--text)}
button{cursor:pointer;border:1px solid var(--border);background:var(--card);padding:.4rem .8rem;border-radius:8px}
button:hover{border-color:var(--accent)}
button:disabled{opacity:.5;cursor:default}
.primary{background:var(--accent);color:var(--accent-text);border-color:var(--accent)}
input,select{padding:.45rem .7rem;border:1px solid var(--border);border-radius:8px;background:var(--card)}
.err{color:var(--danger);background:var(--danger-bg);border-radius:8px;padding:.6rem .8rem;font-size:.85rem}
.hidden{display:none !important}

#app{display:flex;flex-direction:column;height:100vh}
header{display:flex;align-items:center;gap:.75rem;padding:.7rem 1rem;border-bottom:1px solid var(--border);
  background:var(--card);flex-wrap:wrap}
header h1{font-size:1rem;margin:0 .25rem 0 0}
header a{color:var(--muted);font-size:.85rem;text-decoration:none}
header a:hover{color:var(--accent)}

main{display:flex;flex:1;min-height:0}
#chats{width:280px;flex-shrink:0;overflow-y:auto;border-right:1px solid var(--border)}
.chat-row{display:flex;flex-direction:column;gap:.15rem;padding:.6rem .9rem;border-bottom:1px solid var(--border);cursor:pointer}
.chat-row:hover{background:var(--hover)}
.chat-row.sel{background:var(--sel)}
.chat-row .top{display:flex;justify-content:space-between;gap:.5rem;align-items:baseline}
.chat-row .name{font-weight:600;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.chat-row .badge{font-size:.7rem;background:var(--accent);color:var(--accent-text);border-radius:999px;padding:.05rem .4rem}

#thread{flex:1;min-width:0;display:flex;flex-direction:column;overflow:hidden}
#thread .placeholder{margin:auto;color:var(--muted)}
#thread-head{padding:.8rem 1.1rem;border-bottom:1px solid var(--border);font-weight:600}
#older{display:flex;justify-content:center;padding:.5rem}
#messages{flex:1;overflow-y:auto;padding:.75rem 1rem;display:flex;flex-direction:column;gap:.5rem}
.bubble{max-width:70%;padding:.5rem .75rem;border-radius:12px;background:var(--bubble);align-self:flex-start}
.bubble.me{background:var(--bubble-me);align-self:flex-end}
.bubble .text{white-space:pre-wrap;word-break:break-word}
.bubble .when{font-size:.7rem;color:var(--muted);margin-top:.2rem;text-align:right}
.bubble.deleted .text{color:var(--muted);font-style:italic}
#composer{display:flex;gap:.5rem;padding:.7rem;border-top:1px solid var(--border)}
#composer input{flex:1}
.empty{color:var(--muted);text-align:center;padding:3rem 1rem;font-size:.9rem}
</style></head>
<body>

<div id="app">
  <header>
    <h1>Chat</h1>
    <select id="accounts"></select>
    <a href="/dashboard">Accounts</a>
    <a href="/docs">API docs</a>
    <form method="post" action="/logout" style="display:inline"><input type="hidden" name="csrf" value="{{.CSRF}}"><button type="submit">Log out</button></form>
  </header>
  <p id="err" class="err hidden" style="margin:.6rem 1rem"></p>
  <main>
    <div id="chats"></div>
    <div id="thread">
      <div class="placeholder">Select a chat</div>
    </div>
  </main>
</div>

<script>
const $ = (id) => document.getElementById(id);
const REFRESH_MS = 5000;

const state = { account: "", chat: "", oldestBefore: "" };

async function api(path, opts) {
  const res = await fetch(path, Object.assign({ credentials: "same-origin" }, opts));
  if (res.status === 401) { location.href = "/login?next=" + encodeURIComponent(location.pathname + location.search); throw new Error("unauthorized"); }
  if (!res.ok) {
    let msg = res.statusText;
    try { msg = (await res.json()).error.message; } catch (e) {}
    throw new Error(msg);
  }
  if (res.status === 204) return null;
  return res.json();
}

function escapeHtml(s) {
  return String(s).replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
}

function showErr(msg) { $("err").textContent = msg; $("err").classList.remove("hidden"); }
function clearErr() { $("err").classList.add("hidden"); }

function fmtTime(iso) {
  if (!iso) return "";
  const d = new Date(iso);
  const now = new Date();
  return d.toDateString() === now.toDateString()
    ? d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })
    : d.toLocaleDateString([], { month: "short", day: "numeric" });
}

// ---- accounts (chat-kind only) ----

async function loadAccounts() {
  const data = await api("/api/v1/accounts");
  const items = (data.items || []).filter((a) => a.kind === "chat");
  const sel = $("accounts");
  sel.innerHTML = items.map((a) =>
    '<option value="' + a.id + '">' + escapeHtml(a.name || a.identifier || a.id) + "</option>"
  ).join("");
  if (!items.length) return false;
  const wanted = new URLSearchParams(location.search).get("account_id");
  state.account = items.some((a) => a.id === wanted) ? wanted : items[0].id;
  sel.value = state.account;
  return true;
}

// ---- chats ----

async function loadChats() {
  clearErr();
  const data = await api("/api/v1/chats?account_id=" + encodeURIComponent(state.account));
  const items = data.items || [];
  if (!state.chat || !items.some((c) => c.id === state.chat)) {
    state.chat = "";
  }
  $("chats").innerHTML = items.length ? items.map((c) =>
    '<div class="chat-row' + (c.id === state.chat ? " sel" : "") + '" data-id="' + c.id + '">' +
      '<div class="top">' +
        '<span class="name">' + escapeHtml(c.name || (c.kind === "group" ? "Group" : "Chat")) + "</span>" +
        (c.unread_count ? '<span class="badge">' + c.unread_count + "</span>" : "") +
      "</div>" +
    "</div>"
  ).join("") : '<div class="empty">No chats yet.</div>';
}

// ---- messages ----

function renderMessages(items, prepend) {
  const box = $("messages");
  const html = items.map((m) =>
    '<div class="bubble' + (m.is_from_me ? " me" : "") + (m.deleted ? " deleted" : "") + '" data-id="' + m.id + '">' +
      '<div class="text">' + (m.deleted ? "This message was deleted" : escapeHtml(m.text || "")) + "</div>" +
      '<div class="when">' + fmtTime(m.sent_at) + (m.edited_at ? " &middot; edited" : "") + "</div>" +
    "</div>"
  ).join("");
  if (prepend) {
    const atBottom = box.scrollHeight - box.scrollTop <= box.clientHeight + 40;
    box.insertAdjacentHTML("afterbegin", html);
    if (!atBottom) box.scrollTop = 0;
  } else {
    box.innerHTML = html || '<div class="empty">No messages yet.</div>';
    box.scrollTop = box.scrollHeight;
  }
}

async function loadMessages() {
  const data = await api("/api/v1/chats/" + encodeURIComponent(state.chat) +
    "/messages?account_id=" + encodeURIComponent(state.account) + "&limit=50");
  const items = (data.items || []).slice().reverse();
  state.oldestBefore = data.next_before || "";
  $("older").classList.toggle("hidden", !state.oldestBefore);
  renderMessages(items, false);
}

async function loadOlder() {
  if (!state.oldestBefore) return;
  const data = await api("/api/v1/chats/" + encodeURIComponent(state.chat) +
    "/messages?account_id=" + encodeURIComponent(state.account) +
    "&before=" + encodeURIComponent(state.oldestBefore) + "&limit=50");
  const items = (data.items || []).slice().reverse();
  state.oldestBefore = data.next_before || "";
  $("older").classList.toggle("hidden", !state.oldestBefore);
  renderMessages(items, true);
}

async function openChat(id) {
  state.chat = id;
  document.querySelectorAll(".chat-row").forEach((el) => el.classList.toggle("sel", el.dataset.id === id));
  $("thread").innerHTML =
    '<div id="thread-head"></div>' +
    '<div id="older" class="hidden"><button id="load-older">Load older</button></div>' +
    '<div id="messages"></div>' +
    '<form id="composer"><input id="compose-text" placeholder="Type a message" autocomplete="off"><button class="primary" type="submit">Send</button></form>';
  const row = document.querySelector('.chat-row[data-id="' + id + '"] .name');
  $("thread-head").textContent = row ? row.textContent : "Chat";
  $("load-older").addEventListener("click", () => loadOlder().catch((e) => showErr(e.message)));
  $("composer").addEventListener("submit", sendMessage);
  try { await loadMessages(); } catch (e) { showErr(e.message); }
}

async function sendMessage(e) {
  e.preventDefault();
  const input = $("compose-text");
  const text = input.value.trim();
  if (!text || !state.chat) return;
  const btn = e.target.querySelector("button");
  btn.disabled = true;
  try {
    await api("/api/v1/chats/" + encodeURIComponent(state.chat) +
      "/messages?account_id=" + encodeURIComponent(state.account), {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ text })
    });
    input.value = "";
    await loadMessages();
  } catch (err) {
    showErr("Could not send message: " + err.message);
  }
  btn.disabled = false;
}

// ---- wiring ----

$("chats").addEventListener("click", (e) => {
  const el = e.target.closest(".chat-row");
  if (el) openChat(el.dataset.id);
});

$("accounts").addEventListener("change", async () => {
  state.account = $("accounts").value;
  state.chat = "";
  $("thread").innerHTML = '<div class="placeholder">Select a chat</div>';
  history.replaceState(null, "", "?account_id=" + encodeURIComponent(state.account));
  await loadChats().catch((e) => showErr(e.message));
});

async function refresh() {
  if (!state.account) return;
  try {
    await loadChats();
    if (state.chat) await loadMessages();
  } catch (e) {
    showErr(e.message);
  }
}
setInterval(refresh, REFRESH_MS);

async function enter() {
  const any = await loadAccounts();
  if (!any) {
    $("chats").innerHTML = '<div class="empty">No chat accounts connected. <a href="/dashboard">Connect one</a> first.</div>';
    return;
  }
  await loadChats();
}

(function init() {
  enter().catch((e) => showErr(e.message));
})();
</script>
</body></html>`
