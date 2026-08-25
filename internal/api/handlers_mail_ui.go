package api

import (
	"net/http"
	"net/url"
)

// The mail viewer is the third human-facing screen: a read-only, three-pane
// client over the synced mirror. Like the dashboard it requires a browser
// session; the page's own fetches then ride the same cookie, so data stays
// gated exactly where the REST API already gates it. It has no
// developer-specific content, so unlike the dashboard it stays a plain string
// rather than a template.
func (s *Server) handleMailPage(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.sessionDeveloper(w, r); !ok {
		http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(mailHTML))
}

const mailHTML = `<!doctype html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Mail</title>
<style>
:root{--bg:#f7f7f8;--card:#fff;--text:#1a1a1a;--muted:#6b6b76;--border:#e6e6ea;--accent:#2563eb;--accent-text:#fff;
  --ok:#16a34a;--warn:#d97706;--danger:#dc2626;--danger-bg:#fef2f2;--hover:#f0f0f3;--sel:#e8effd}
@media (prefers-color-scheme:dark){:root{--bg:#0f1115;--card:#171a21;--text:#f0f0f2;--muted:#9a9aa5;--border:#2a2d36;
  --accent:#3b82f6;--accent-text:#fff;--ok:#4ade80;--warn:#fbbf24;--danger:#f87171;--danger-bg:#2a1418;--hover:#1d212b;--sel:#1e2a44}}
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
#search{flex:1;min-width:10rem;max-width:24rem}
label.toggle{display:inline-flex;align-items:center;gap:.35rem;font-size:.85rem;color:var(--muted);cursor:pointer}

main{display:flex;flex:1;min-height:0}
nav{width:220px;flex-shrink:0;overflow-y:auto;border-right:1px solid var(--border);padding:.6rem}
.folder{display:flex;justify-content:space-between;align-items:center;gap:.5rem;padding:.4rem .6rem;border-radius:8px;
  cursor:pointer;color:var(--text)}
.folder:hover{background:var(--hover)}
.folder.sel{background:var(--sel);font-weight:600}
.folder .n{overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.folder .c{font-size:.75rem;color:var(--muted)}

#list-pane{width:360px;flex-shrink:0;display:flex;flex-direction:column;border-right:1px solid var(--border);min-height:0}
#messages{flex:1;overflow-y:auto}
.msg{padding:.6rem .9rem;border-bottom:1px solid var(--border);cursor:pointer}
.msg:hover{background:var(--hover)}
.msg.sel{background:var(--sel)}
.msg .top{display:flex;justify-content:space-between;gap:.5rem;align-items:baseline}
.msg .from{font-weight:600;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.msg.read .from,.msg.read .subj{font-weight:400}
.msg .when{font-size:.75rem;color:var(--muted);flex-shrink:0}
.msg .subj{margin-top:.1rem;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.msg .snip{font-size:.8rem;color:var(--muted);overflow:hidden;text-overflow:ellipsis;white-space:nowrap;margin-top:.1rem}
.msg .marks{margin-left:.3rem;font-size:.75rem}
.pager{display:flex;justify-content:space-between;align-items:center;padding:.5rem .9rem;border-top:1px solid var(--border);
  font-size:.8rem;color:var(--muted)}

#reader{flex:1;min-width:0;display:flex;flex-direction:column;overflow:hidden}
#reader .placeholder{margin:auto;color:var(--muted)}
#reader-head{padding:1rem 1.25rem;border-bottom:1px solid var(--border)}
#reader-head h2{margin:0 0 .5rem;font-size:1.1rem}
.addr{font-size:.85rem;color:var(--muted)}
.addr b{color:var(--text);font-weight:600}
#atts{display:flex;gap:.5rem;flex-wrap:wrap;margin-top:.6rem}
.att{font-size:.8rem;border:1px solid var(--border);border-radius:8px;padding:.25rem .6rem;cursor:pointer;background:var(--card)}
.att:hover{border-color:var(--accent)}
#body-frame{flex:1;border:0;width:100%;background:#fff}
.empty{color:var(--muted);text-align:center;padding:3rem 1rem;font-size:.9rem}
</style></head>
<body>

<div id="app">
  <header>
    <h1>Mail</h1>
    <select id="accounts"></select>
    <input id="search" type="search" placeholder="Search mail&hellip;">
    <label class="toggle"><input id="unread-only" type="checkbox">Unread only</label>
    <a href="/dashboard">Accounts</a>
    <form method="post" action="/logout" style="display:inline"><button type="submit">Log out</button></form>
  </header>
  <p id="err" class="err hidden" style="margin:.6rem 1rem"></p>
  <main>
    <nav id="folders"></nav>
    <div id="list-pane">
      <div id="messages"></div>
      <div class="pager">
        <button id="prev" disabled>&larr; Newer</button>
        <span id="page-info"></span>
        <button id="next" disabled>Older &rarr;</button>
      </div>
    </div>
    <div id="reader"><div class="placeholder">Select a message</div></div>
  </main>
</div>

<script>
const PAGE = 50;
const $ = (id) => document.getElementById(id);

const state = { account: "", folder: "", q: "", unread: false, offset: 0, selected: "", count: 0 };

async function api(path, opts) {
  const res = await fetch(path, Object.assign({ credentials: "same-origin" }, opts));
  if (res.status === 401) { location.href = "/login?next=" + encodeURIComponent(location.pathname + location.search); throw new Error("unauthorized"); }
  if (!res.ok) {
    let msg = res.statusText;
    try { msg = (await res.json()).error.message; } catch (e) {}
    throw new Error(msg);
  }
  return res;
}
const apiJSON = (path, opts) => api(path, opts).then((r) => r.json());

function escapeHtml(s) {
  return String(s).replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
}

function showErr(msg) { $("err").textContent = msg; $("err").classList.remove("hidden"); }
function clearErr() { $("err").classList.add("hidden"); }

function fmtDate(iso) {
  const d = new Date(iso);
  const now = new Date();
  return d.toDateString() === now.toDateString()
    ? d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })
    : d.toLocaleDateString([], { month: "short", day: "numeric", year: d.getFullYear() === now.getFullYear() ? undefined : "numeric" });
}

// ---- accounts ----

async function loadAccounts() {
  const data = await apiJSON("/api/v1/accounts");
  const items = data.items || [];
  const sel = $("accounts");
  sel.innerHTML = items.map((a) =>
    '<option value="' + a.id + '">' + escapeHtml(a.email) + (a.status === "OK" ? "" : " (needs reconnect)") + "</option>"
  ).join("");
  if (!items.length) return false;
  const wanted = new URLSearchParams(location.search).get("account_id");
  state.account = items.some((a) => a.id === wanted) ? wanted : items[0].id;
  sel.value = state.account;
  return true;
}

// ---- folders ----

const ROLE_ORDER = { inbox: 0, drafts: 1, sent: 2, archive: 3, junk: 4, trash: 5 };

async function loadFolders() {
  const data = await apiJSON("/api/v1/folders?account_id=" + encodeURIComponent(state.account));
  const items = (data.items || []).slice().sort((a, b) => {
    const ra = a.role in ROLE_ORDER ? ROLE_ORDER[a.role] : 99;
    const rb = b.role in ROLE_ORDER ? ROLE_ORDER[b.role] : 99;
    return ra - rb || a.name.localeCompare(b.name);
  });
  if (!state.folder || !items.some((f) => f.id === state.folder)) {
    const inbox = items.find((f) => f.role === "inbox");
    state.folder = inbox ? inbox.id : (items[0] ? items[0].id : "");
  }
  $("folders").innerHTML = items.map((f) =>
    '<div class="folder' + (f.id === state.folder ? " sel" : "") + '" data-id="' + f.id + '">' +
      '<span class="n">' + escapeHtml(f.name) + "</span>" +
      (f.unread_count ? '<span class="c">' + f.unread_count + "</span>" : "") +
    "</div>"
  ).join("");
}

// ---- message list ----

async function loadMessages() {
  clearErr();
  let url = "/api/v1/emails?account_id=" + encodeURIComponent(state.account) +
    "&limit=" + PAGE + "&offset=" + state.offset;
  // A search runs across the whole account; folder scoping applies otherwise.
  if (state.q) url += "&q=" + encodeURIComponent(state.q);
  else if (state.folder) url += "&folder_id=" + encodeURIComponent(state.folder);
  if (state.unread) url += "&unread=true";

  const data = await apiJSON(url);
  const items = data.items || [];
  state.count = items.length;

  $("messages").innerHTML = items.length ? items.map((m) =>
    '<div class="msg' + (m.read ? " read" : "") + (m.id === state.selected ? " sel" : "") + '" data-id="' + m.id + '">' +
      '<div class="top">' +
        '<span class="from">' + escapeHtml(m.from && (m.from.name || m.from.email) || "(unknown)") + "</span>" +
        '<span class="when">' + fmtDate(m.date) + "</span>" +
      "</div>" +
      '<div class="subj">' + escapeHtml(m.subject || "(no subject)") +
        '<span class="marks">' + (m.flagged ? "&#9873;" : "") + (m.has_attachments ? "&#128206;" : "") + (m.draft ? " [draft]" : "") + "</span>" +
      "</div>" +
      '<div class="snip">' + escapeHtml(m.snippet || "") + "</div>" +
    "</div>"
  ).join("") : '<div class="empty">' + (state.q ? "No results." : "Nothing here yet.") + "</div>";

  $("prev").disabled = state.offset === 0;
  $("next").disabled = items.length < PAGE;
  $("page-info").textContent = items.length
    ? (state.offset + 1) + "–" + (state.offset + items.length)
    : "";
}

// ---- reading pane ----

async function openMessage(id) {
  state.selected = id;
  document.querySelectorAll(".msg").forEach((el) => el.classList.toggle("sel", el.dataset.id === id));
  const reader = $("reader");
  reader.innerHTML = '<div class="placeholder">Loading&hellip;</div>';
  let m;
  try {
    m = await apiJSON("/api/v1/emails/" + encodeURIComponent(id) +
      "?account_id=" + encodeURIComponent(state.account));
  } catch (e) {
    reader.innerHTML = '<div class="placeholder">Could not load message: ' + escapeHtml(e.message) + "</div>";
    return;
  }

  const line = (label, rs) => (rs && rs.length)
    ? '<div class="addr"><b>' + label + ":</b> " +
      rs.map((r) => escapeHtml(r.name ? r.name + " <" + r.email + ">" : r.email)).join(", ") + "</div>"
    : "";

  reader.innerHTML =
    '<div id="reader-head">' +
      "<h2>" + escapeHtml(m.subject || "(no subject)") + "</h2>" +
      line("From", m.from ? [m.from] : []) +
      line("To", m.to) + line("Cc", m.cc) +
      '<div class="addr">' + new Date(m.date).toLocaleString() + "</div>" +
      '<div id="atts"></div>' +
    "</div>" +
    '<iframe id="body-frame" sandbox=""></iframe>';

  // The iframe is sandboxed with no permissions at all, so HTML mail cannot run
  // scripts, navigate, or reach this page's localStorage.
  const frame = $("body-frame");
  frame.srcdoc = m.body_type === "html"
    ? m.body
    : "<pre style='font:13px/1.5 monospace;white-space:pre-wrap;margin:1rem'>" + escapeHtml(m.body || "") + "</pre>";

  if (m.has_attachments) loadAttachments(m.id);
}

async function loadAttachments(id) {
  let data;
  try {
    data = await apiJSON("/api/v1/emails/" + encodeURIComponent(id) +
      "/attachments?account_id=" + encodeURIComponent(state.account));
  } catch (e) { return; }
  const atts = (data.items || []).filter((a) => !a.is_inline);
  $("atts").innerHTML = atts.map((a) =>
    '<button class="att" data-aid="' + a.id + '" data-name="' + escapeHtml(a.name) + '">' +
      "&#128206; " + escapeHtml(a.name) + " (" + Math.max(1, Math.round(a.size / 1024)) + " KB)</button>"
  ).join("");
  $("atts").onclick = async (e) => {
    const btn = e.target.closest(".att");
    if (!btn) return;
    btn.disabled = true;
    try {
      // Downloads go through fetch so the Authorization header rides along.
      const res = await api("/api/v1/emails/" + encodeURIComponent(id) +
        "/attachments/" + encodeURIComponent(btn.dataset.aid) +
        "?account_id=" + encodeURIComponent(state.account));
      const blobURL = URL.createObjectURL(await res.blob());
      const a = document.createElement("a");
      a.href = blobURL;
      a.download = btn.dataset.name;
      a.click();
      URL.revokeObjectURL(blobURL);
    } catch (err) {
      showErr("Download failed: " + err.message);
    }
    btn.disabled = false;
  };
}

// ---- wiring ----

function resetAndLoad() {
  state.offset = 0;
  loadMessages().catch((e) => showErr(e.message));
}

$("folders").addEventListener("click", (e) => {
  const el = e.target.closest(".folder");
  if (!el) return;
  state.folder = el.dataset.id;
  state.q = "";
  $("search").value = "";
  document.querySelectorAll(".folder").forEach((f) => f.classList.toggle("sel", f.dataset.id === state.folder));
  resetAndLoad();
});

$("messages").addEventListener("click", (e) => {
  const el = e.target.closest(".msg");
  if (el) openMessage(el.dataset.id);
});

$("accounts").addEventListener("change", async () => {
  state.account = $("accounts").value;
  state.folder = "";
  state.selected = "";
  $("reader").innerHTML = '<div class="placeholder">Select a message</div>';
  history.replaceState(null, "", "?account_id=" + encodeURIComponent(state.account));
  await loadFolders().catch((e) => showErr(e.message));
  resetAndLoad();
});

let searchTimer;
$("search").addEventListener("input", () => {
  clearTimeout(searchTimer);
  searchTimer = setTimeout(() => {
    state.q = $("search").value.trim();
    resetAndLoad();
  }, 300);
});

$("unread-only").addEventListener("change", () => {
  state.unread = $("unread-only").checked;
  resetAndLoad();
});

$("prev").addEventListener("click", () => {
  state.offset = Math.max(0, state.offset - PAGE);
  loadMessages().catch((e) => showErr(e.message));
});
$("next").addEventListener("click", () => {
  state.offset += PAGE;
  loadMessages().catch((e) => showErr(e.message));
});

async function enter() {
  const any = await loadAccounts();
  if (!any) {
    $("messages").innerHTML = '<div class="empty">No accounts connected. <a href="/dashboard">Connect one</a> first.</div>';
    return;
  }
  await loadFolders();
  await loadMessages();
}

(function init() {
  enter().catch((e) => showErr(e.message));
})();
</script>
</body></html>`
