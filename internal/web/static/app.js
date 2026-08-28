/* Entropix shared browser helpers. Loaded on every page as window.um. */
(function () {
  "use strict";
  const $ = (id) => document.getElementById(id);

  function esc(s) {
    return String(s ?? "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
  }

  async function request(path, opts, raw) {
    const init = Object.assign({ credentials: "same-origin" }, opts || {});
    init.headers = Object.assign({}, init.headers || {});
    if (init.body && typeof init.body !== "string") { init.body = JSON.stringify(init.body); }
    if (init.body) { init.headers["Content-Type"] = "application/json"; }
    const res = await fetch(path, init);
    if (res.status === 401) {
      location.href = "/login?next=" + encodeURIComponent(location.pathname + location.search);
      throw new Error("signed out");
    }
    if (raw) return res;
    if (res.status === 204) return null;
    let body = null;
    try { body = await res.json(); } catch (_) { /* non-JSON */ }
    if (!res.ok) {
      const msg = (body && body.error && body.error.message) || ("request failed (" + res.status + ")");
      const err = new Error(msg); err.status = res.status; err.code = body && body.error && body.error.code; throw err;
    }
    return body;
  }
  const api = (path, opts) => request(path, opts, false);
  api.raw = (path, opts) => request(path, opts, true);

  function notice(kind, text, opts) {
    const host = $("notice"); if (!host) return;
    const el = document.createElement("div");
    el.className = "notice " + kind; el.setAttribute("role", kind === "error" ? "alert" : "status");
    el.innerHTML = "<span></span><button class=\"btn ghost sm\" type=\"button\" aria-label=\"Dismiss\">✕</button>";
    el.firstChild.textContent = text;
    el.lastChild.onclick = () => el.remove();
    host.appendChild(el);
    const timeout = (opts && opts.timeout) ?? (kind === "error" ? 0 : 5000);
    if (timeout) setTimeout(() => el.remove(), timeout);
    return el;
  }

  function confirm(o) {
    return new Promise((resolve) => {
      const d = document.createElement("dialog");
      d.innerHTML = "<div class=\"dlg-head\"></div><div class=\"dlg-body\"></div><div class=\"dlg-foot\">" +
        "<button class=\"btn\" type=\"button\" data-x>Cancel</button><button class=\"btn primary\" type=\"button\" data-ok></button></div>";
      d.querySelector(".dlg-head").textContent = o.title || "Are you sure?";
      d.querySelector(".dlg-body").textContent = o.body || "";
      const ok = d.querySelector("[data-ok]"); ok.textContent = o.action || "Confirm"; if (o.danger) ok.className = "btn danger";
      d.querySelector("[data-x]").onclick = () => { d.close(); resolve(false); };
      ok.onclick = () => { d.close(); resolve(true); };
      d.addEventListener("close", () => { d.remove(); resolve(false); });
      document.body.appendChild(d); d.showModal(); ok.focus();
    });
  }

  async function copy(text, btn) {
    try { await navigator.clipboard.writeText(text); } catch (_) {
      const ta = document.createElement("textarea"); ta.value = text; document.body.appendChild(ta); ta.select(); document.execCommand("copy"); ta.remove();
    }
    if (btn) { const old = btn.textContent; btn.textContent = "Copied"; setTimeout(() => { btn.textContent = old; }, 1500); }
  }

  function relTime(iso) {
    if (!iso) return "";
    const t = new Date(iso).getTime(), d = Date.now() - t;
    if (isNaN(t)) return "";
    const m = Math.round(d / 60000), h = Math.round(d / 3600000), days = Math.round(d / 86400000);
    if (m < 1) return "just now"; if (m < 60) return m + " min ago"; if (h < 24) return h + " h ago";
    if (days === 1) return "yesterday"; if (days < 7) return days + " days ago";
    return new Date(iso).toLocaleDateString(undefined, { month: "short", day: "numeric" });
  }

  function poll(fn, ms) {
    let timer = null, stopped = false;
    const tick = async () => { if (stopped || document.hidden) return; try { await fn(); } catch (_) { /* fn reports */ } };
    const start = () => { if (timer) clearInterval(timer); timer = setInterval(tick, ms); };
    document.addEventListener("visibilitychange", () => { if (!document.hidden && !stopped) { tick(); start(); } });
    start();
    return () => { stopped = true; clearInterval(timer); };
  }

  function listNav(container) {
    container.addEventListener("keydown", (e) => {
      const opts = [...container.querySelectorAll("[role=option]")];
      const i = opts.indexOf(document.activeElement);
      if (i < 0) return;
      const go = (n) => { e.preventDefault(); opts[Math.max(0, Math.min(opts.length - 1, n))].focus(); };
      if (e.key === "ArrowDown" || e.key === "j") go(i + 1);
      else if (e.key === "ArrowUp" || e.key === "k") go(i - 1);
      else if (e.key === "Home") go(0);
      else if (e.key === "End") go(opts.length - 1);
    });
  }

  window.um = { $, esc, api, notice, confirm, copy, relTime, poll, listNav };
})();
