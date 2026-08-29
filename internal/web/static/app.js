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

  // accountState turns an account into the one badge a human needs: a pill
  // class, a label in words, and a single line of supporting detail. It is the
  // only place the raw socket state is read, so the dashboard and the chat page
  // cannot drift into describing the same account differently.
  function accountState(a) {
    const c = a.connection;
    if (a.kind === "chat") {
      if (a.status === "CREDENTIALS" || (c && c.state === "stopped")) return { cls: "danger", label: "Needs relink", sub: (c && c.last_error) || "" };
      if (!c) return { cls: "info", label: "Connecting", sub: "" };
      if (c.state === "connected") return { cls: "ok", label: "Connected", sub: "since " + relTime(c.since) + (c.reconnects ? " · " + c.reconnects + " reconnect" + (c.reconnects > 1 ? "s" : "") : "") };
      if (c.state === "backoff") return { cls: "warn", label: "Reconnecting", sub: "attempt " + c.reconnects + (c.last_error ? ": " + c.last_error : "") };
      if (c.state === "error") return { cls: "danger", label: "Error", sub: c.last_error || "" };
      return { cls: "info", label: "Connecting", sub: "" };
    }
    if (a.status === "CREDENTIALS") return { cls: "danger", label: "Needs reconnect", sub: "sign in again to resume" };
    return { cls: "ok", label: "Connected", sub: a.last_synced_at ? "synced " + relTime(a.last_synced_at) : "first sync pending" };
  }

  // maskPhone keeps the country code plus the first two and last three digits
  // of a phone number, masking the rest — e.g. "+91 88••• •855". The account
  // JSON carries the real identifier; this function's whole job is making sure
  // a console page in a screenshot or a screen share never shows it in full.
  function maskPhone(p) {
    if (!p) return "";
    const s = String(p);
    if (s.indexOf("@") > -1) return s; // an email address, not a phone number
    const m = /^(\+\d{1,3})(\d+)$/.exec(s.replace(/[^\d+]/g, ""));
    if (!m || m[2].length < 5) {
      const n = s.length;
      if (n <= 4) return s;
      const keep = Math.max(1, Math.floor(n / 4));
      return s.slice(0, keep) + "•".repeat(Math.max(3, n - keep * 2)) + s.slice(n - keep);
    }
    const rest = m[2], midLen = Math.min(3, Math.max(1, rest.length - 5));
    return m[1] + " " + rest.slice(0, 2) + "•".repeat(midLen) + " •" + rest.slice(-3);
  }

  function poll(fn, ms) {
    let timer = null, stopped = false;
    const tick = async () => { if (stopped || document.hidden) return; try { await fn(); } catch (_) { /* fn reports */ } };
    const start = () => { if (timer) clearInterval(timer); timer = setInterval(tick, ms); };
    // Named so stop() can take it back off document: a page that re-polls on
    // every selection change (the mail and chat viewers do) would otherwise
    // leave one live listener per call behind, each one still ticking its own
    // closure every time the tab is revealed.
    const onVisible = () => { if (!document.hidden && !stopped) { tick(); start(); } };
    document.addEventListener("visibilitychange", onVisible);
    start();
    return () => {
      stopped = true;
      clearInterval(timer);
      document.removeEventListener("visibilitychange", onVisible);
    };
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

  window.um = { $, esc, api, notice, confirm, copy, relTime, accountState, maskPhone, poll, listNav };
})();
