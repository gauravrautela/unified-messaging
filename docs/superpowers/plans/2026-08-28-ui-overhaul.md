# UI Overhaul + Entropix Website Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the seven inline-HTML pages with one embedded design system (no build step), fix the audited UX defects, turn `/docs` into a developer reference, and add the Entropix product website at `/`.

**Architecture:** A new `internal/web` package embeds `static/` (one CSS, one JS) and `templates/` (three layouts + one file per page) with `go:embed`, exposes `web.Static()` and `web.Templates()`, and the existing `internal/api` handlers keep their routes/auth/CSRF but execute the embedded templates instead of Go string constants. Content that must not drift (endpoint docs, code snippets, event and error lists) lives as Go data in `internal/web/docs`.

**Tech Stack:** Go 1.26 `html/template`, `embed`, vanilla CSS/JS (no framework, no bundler), existing `net/http` mux, `go test`.

**Spec:** `docs/superpowers/specs/2026-08-28-ui-overhaul-design.md`

## Global Constraints

- No build step, no Node toolchain, no external assets. CSP stays exactly: `default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; frame-src 'self'; frame-ancestors 'none'; form-action 'self'; base-uri 'self'` (`internal/api/middleware.go:11`).
- Product name on web surfaces is **Entropix**. Go module, binary, README title, env vars, DB names unchanged.
- Every route already in `browserRoutes` (`internal/api/api.go:137`) keeps its path and handler name; new routes are appended to `browserRoutes` AND `browserHandlers()` or `TestBrowserHandlersMatchBrowserRoutes` fails.
- No `alert()` / `confirm()` anywhere. All DOM text insertion via `um.esc` or `textContent`.
- Light default + dark via `prefers-color-scheme`. `:focus-visible` ring on every interactive element. `100dvh` never `100vh`.
- Do not log message text, emails or phone numbers (README "Logging"); templates render them, logs don't.
- Each task ends with `go build ./... && go vet ./... && go test ./...` green and a commit. Commit trailer on every commit:

  ```
  Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_01NDpTpqU4Bg64i2nipuXNAX
  ```

## File structure

```
internal/web/
  web.go              embed.FS, Version(), Static(), Templates()
  web_test.go
  static/app.css      tokens + components (console + public shells)
  static/site.css     website-only additions (hero, feature grid)
  static/app.js       window.um helpers
  templates/layout.html          console shell   {{define "layout"}}
  templates/layout_public.html   hosted-auth/auth shell
  templates/layout_site.html     website shell
  templates/dashboard.html, mail.html, chat.html, login.html (login+signup),
            connect_oauth.html, connect_qr.html, connect_result.html,
            docs.html, site.html
internal/web/docs/
  docs.go             Endpoint/Param/Event/ErrorCode types + the data
  snippets.go         curl/Node/Go snippet constants shared by site + docs
  docs_test.go        every apiRoutes entry has an Endpoint
docs/ui-manual-checklist.md
```

Handlers stay where they are (`internal/api/handlers_*.go`); each task deletes one page's `const ...HTML` string and `template.Must(...)` var and calls `web.Templates().ExecuteTemplate(w, "<page>", data)` instead.

---

### Task 1: `internal/web` package — embed, version, static handler, layouts, `GET /` placeholder

**Files:**
- Create: `internal/web/web.go`, `internal/web/web_test.go`, `internal/web/static/app.css`, `internal/web/static/app.js`, `internal/web/static/site.css`, `internal/web/templates/layout.html`, `internal/web/templates/layout_public.html`, `internal/web/templates/layout_site.html`, `internal/web/templates/site.html`
- Modify: `internal/api/api.go:137-160` (browserRoutes), `internal/api/api.go:168` (browserHandlers), `internal/api/middleware.go:32` (noStorePrefixes unchanged — `/static/` is not listed, verify), `internal/api/handlers_misc.go` (add `handleSite`)
- Test: `internal/web/web_test.go`, `internal/api/api_test.go`

**Interfaces:**
- Produces: `web.Version string` (8 hex chars), `web.Static() http.Handler` (mount at `/static/`), `web.Templates() *template.Template` (all templates parsed once; page names: `dashboard`, `mail`, `chat`, `login`, `connect_oauth`, `connect_qr`, `connect_result`, `docs`, `site`), `web.Shell` struct passed to every layout:

```go
// Shell is what every layout needs regardless of page.
type Shell struct {
    Title   string // "<Title> · Entropix"
    Version string // web.Version, for cache-busting static URLs
    Email   string // signed-in developer, "" when anonymous
    CSRF    string // logout form; "" on public pages
    Nav     string // "dashboard" | "mail" | "chat" | "docs" | "" for aria-current
}
```

- [ ] **Step 1: Write the failing tests**

`internal/web/web_test.go`:

```go
package web

import (
    "net/http"
    "net/http/httptest"
    "regexp"
    "strings"
    "testing"
)

func TestVersionIsStableShortHash(t *testing.T) {
    if !regexp.MustCompile(`^[0-9a-f]{8}$`).MatchString(Version) {
        t.Fatalf("Version = %q, want 8 hex chars", Version)
    }
    if Version != version() {
        t.Fatal("Version changed between calls")
    }
}

func TestStaticServesImmutableCSS(t *testing.T) {
    rec := httptest.NewRecorder()
    Static().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/static/app.css?v="+Version, nil))
    if rec.Code != http.StatusOK {
        t.Fatalf("status = %d", rec.Code)
    }
    if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/css") {
        t.Fatalf("content-type = %q", ct)
    }
    if cc := rec.Header().Get("Cache-Control"); cc != "public, max-age=31536000, immutable" {
        t.Fatalf("cache-control = %q", cc)
    }
    if !strings.Contains(rec.Body.String(), "--accent") {
        t.Fatal("app.css does not define tokens")
    }
}

func TestStaticRejectsTraversalAndMissing(t *testing.T) {
    for _, p := range []string{"/static/../web.go", "/static/nope.css"} {
        rec := httptest.NewRecorder()
        Static().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
        if rec.Code == http.StatusOK {
            t.Fatalf("%s served (%d)", p, rec.Code)
        }
    }
}

func TestTemplatesParseAndLayoutsRender(t *testing.T) {
    for _, name := range []string{"site"} {
        var sb strings.Builder
        err := Templates().ExecuteTemplate(&sb, name, map[string]any{
            "Shell": Shell{Title: "T", Version: Version},
        })
        if err != nil {
            t.Fatalf("%s: %v", name, err)
        }
        out := sb.String()
        for _, want := range []string{"<!doctype html>", "Entropix", "/static/app.css?v=" + Version, `id="notice"`} {
            if !strings.Contains(out, want) {
                t.Fatalf("%s: missing %q", name, want)
            }
        }
    }
}
```

`internal/api/api_test.go` (append):

```go
func TestRootServesWebsiteAndStaticIsCacheable(t *testing.T) {
    s, _ := newTestServer(t)
    h := s.Routes()

    rec := httptest.NewRecorder()
    h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
    if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Entropix") {
        t.Fatalf("GET / = %d %q", rec.Code, rec.Body.String()[:min(200, rec.Body.Len())])
    }
    if rec.Header().Get("Content-Security-Policy") == "" {
        t.Fatal("website served without CSP")
    }

    rec = httptest.NewRecorder()
    h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/static/app.js", nil))
    if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" {
        t.Fatalf("static: %d cache=%q", rec.Code, rec.Header().Get("Cache-Control"))
    }
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/web/ ./internal/api/ -run 'TestVersion|TestStatic|TestTemplates|TestRootServes' 2>&1 | tail -5`
Expected: build failure `package web` not found / undefined `Static`.

- [ ] **Step 3: Implement `internal/web/web.go`**

```go
// Package web embeds the browser-facing assets and templates so the binary
// ships its UI with no build step, and serves them with immutable caching.
package web

import (
    "crypto/sha256"
    "embed"
    "encoding/hex"
    "html/template"
    "io/fs"
    "net/http"
    "sort"
    "strings"
)

//go:embed static/*
var staticFS embed.FS

//go:embed templates/*.html
var templateFS embed.FS

// Version is a short content hash of every static file. Templates append it
// as ?v= so a deploy invalidates browser caches without any build tooling.
var Version = version()

func version() string {
    h := sha256.New()
    names, _ := fs.Glob(staticFS, "static/*")
    sort.Strings(names)
    for _, n := range names {
        b, _ := staticFS.ReadFile(n)
        h.Write([]byte(n))
        h.Write(b)
    }
    return hex.EncodeToString(h.Sum(nil))[:8]
}

// Shell is what every layout needs regardless of page.
type Shell struct {
    Title   string
    Version string
    Email   string
    CSRF    string
    Nav     string
}

// Static serves /static/{file}. Everything under it is content-addressed by
// Version, so it is safe to cache forever.
func Static() http.Handler {
    sub, err := fs.Sub(staticFS, "static")
    if err != nil {
        panic(err)
    }
    files := http.FileServer(http.FS(sub))
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        if strings.Contains(r.URL.Path, "..") {
            http.NotFound(w, r)
            return
        }
        w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
        http.StripPrefix("/static/", files).ServeHTTP(w, r)
    })
}

var tmpl = template.Must(template.New("").Funcs(template.FuncMap{
    "lower": strings.ToLower,
    "initial": func(s string) string {
        if s == "" {
            return "?"
        }
        return strings.ToUpper(s[:1])
    },
}).ParseFS(templateFS, "templates/*.html"))

// Templates is the parsed set. Pages are executed by name ("dashboard",
// "mail", ...); each page {{template}}s one of the three layouts.
func Templates() *template.Template { return tmpl }
```

Note: `http.FileServer` 404s missing files and `Content-Type` comes from the extension. Go's mime table maps `.css`→`text/css; charset=utf-8`, `.js`→`text/javascript; charset=utf-8`.

- [ ] **Step 4: Write `static/app.css`** (tokens + components; this file is the design system — write it fully)

```css
/* Entropix design system. Light default, dark via prefers-color-scheme. */
:root{
  color-scheme:light dark;
  --bg:#f6f7f9;--surface:#ffffff;--surface-2:#eef0f4;--border:#dfe3ea;
  --text:#14161a;--muted:#5f6675;--accent:#2563eb;--accent-text:#ffffff;--accent-bg:#e8effd;
  --ok:#15803d;--ok-bg:#e6f6ec;--warn:#b45309;--warn-bg:#fdf1e0;--danger:#b91c1c;--danger-bg:#fdeaea;--info:#1d4ed8;--info-bg:#e8effd;
  --code-bg:#f1f3f7;
  --s1:4px;--s2:8px;--s3:12px;--s4:16px;--s5:24px;--s6:32px;--s7:48px;--s8:64px;
  --r1:6px;--r2:10px;
  --font:ui-sans-serif,system-ui,-apple-system,"Segoe UI",sans-serif;
  --mono:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;
  --shadow:0 1px 2px rgba(0,0,0,.06);
}
@media (prefers-color-scheme:dark){:root{
  --bg:#0c0e12;--surface:#14171d;--surface-2:#1b1f27;--border:#262b35;
  --text:#eceef2;--muted:#9aa2b1;--accent:#5b8def;--accent-text:#0c0e12;--accent-bg:#172343;
  --ok:#4ade80;--ok-bg:#12291b;--warn:#fbbf24;--warn-bg:#2d2310;--danger:#f87171;--danger-bg:#2f1616;--info:#93b4ff;--info-bg:#172343;
  --code-bg:#0f1218;--shadow:none;
}}
*,*::before,*::after{box-sizing:border-box}
html{-webkit-text-size-adjust:100%}
body{margin:0;background:var(--bg);color:var(--text);font:14px/1.5 var(--font)}
a{color:var(--accent)} a:hover{text-decoration-thickness:2px}
code,pre,kbd{font-family:var(--mono);font-size:.92em}
pre{background:var(--code-bg);border:1px solid var(--border);border-radius:var(--r2);padding:var(--s4);overflow:auto;position:relative}
:focus-visible{outline:2px solid var(--accent);outline-offset:2px;border-radius:var(--r1)}
.sr-only{position:absolute;width:1px;height:1px;overflow:hidden;clip:rect(0 0 0 0);white-space:nowrap}
.hidden{display:none!important}

/* shell */
.topbar{position:sticky;top:0;z-index:10;background:var(--surface);border-bottom:1px solid var(--border);display:flex;align-items:center;gap:var(--s4);padding:0 var(--s5);height:52px}
.wordmark{font-weight:700;letter-spacing:-.01em;color:var(--text);text-decoration:none;font-size:15px}
.wordmark b{color:var(--accent)}
.topbar nav{display:flex;gap:var(--s1);margin-left:var(--s4)}
.topbar nav a{color:var(--muted);text-decoration:none;padding:var(--s2) var(--s3);border-radius:var(--r1)}
.topbar nav a[aria-current=page]{color:var(--text);background:var(--surface-2)}
.topbar .spacer{flex:1}
.topbar .who{color:var(--muted);font-size:13px}
main.page{max-width:64rem;margin:0 auto;padding:var(--s5)}
main.page.wide{max-width:none;padding:0}
.page-head{display:flex;align-items:center;justify-content:space-between;gap:var(--s4);margin-bottom:var(--s5);flex-wrap:wrap}
h1{font-size:24px;margin:0;letter-spacing:-.01em} h2{font-size:18px;margin:0 0 var(--s3)} h3{font-size:15px;margin:0 0 var(--s2)}
.muted{color:var(--muted)} .small{font-size:13px}

/* buttons */
.btn{display:inline-flex;align-items:center;gap:var(--s2);border:1px solid var(--border);background:var(--surface);color:var(--text);
  padding:7px 12px;border-radius:var(--r1);font:inherit;font-weight:500;cursor:pointer;text-decoration:none;line-height:1.2}
.btn:hover{background:var(--surface-2)} .btn:disabled{opacity:.55;cursor:default}
.btn.primary{background:var(--accent);border-color:var(--accent);color:var(--accent-text)} .btn.primary:hover{filter:brightness(1.08)}
.btn.danger{color:var(--danger);border-color:color-mix(in srgb,var(--danger) 40%,var(--border))} .btn.danger:hover{background:var(--danger-bg)}
.btn.ghost{border-color:transparent;background:transparent} .btn.ghost:hover{background:var(--surface-2)}
.btn.sm{padding:4px 9px;font-size:13px}
.actions{display:flex;gap:var(--s2);flex-wrap:wrap;align-items:center}

/* cards, tables, lists */
.card{background:var(--surface);border:1px solid var(--border);border-radius:var(--r2);box-shadow:var(--shadow)}
.card > .card-head{display:flex;align-items:center;justify-content:space-between;gap:var(--s3);padding:var(--s3) var(--s4);border-bottom:1px solid var(--border)}
.card > .card-body{padding:var(--s4)}
.table{width:100%;border-collapse:collapse} .table th{text-align:left;color:var(--muted);font-weight:500;font-size:13px;padding:var(--s2) var(--s4);border-bottom:1px solid var(--border)}
.table td{padding:var(--s3) var(--s4);border-bottom:1px solid var(--border);vertical-align:middle} .table tr:last-child td{border-bottom:0}
.row{display:flex;align-items:center;gap:var(--s3);padding:var(--s3) var(--s4);border-bottom:1px solid var(--border)} .row:last-child{border-bottom:0}
.row .grow{flex:1;min-width:0} .row .title{font-weight:600} .row .sub{color:var(--muted);font-size:13px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.avatar{width:32px;height:32px;border-radius:50%;background:var(--accent-bg);color:var(--accent);display:grid;place-items:center;font-weight:700;flex:none}

/* pills */
.pill{display:inline-flex;align-items:center;gap:6px;padding:2px 8px;border-radius:999px;font-size:12px;font-weight:600;background:var(--surface-2);color:var(--muted)}
.pill::before{content:"";width:6px;height:6px;border-radius:50%;background:currentColor}
.pill.ok{background:var(--ok-bg);color:var(--ok)} .pill.warn{background:var(--warn-bg);color:var(--warn)}
.pill.danger{background:var(--danger-bg);color:var(--danger)} .pill.info{background:var(--info-bg);color:var(--info)}
.badge{font-size:11px;text-transform:uppercase;letter-spacing:.04em;color:var(--muted);border:1px solid var(--border);border-radius:4px;padding:1px 5px}

/* forms */
.field{display:flex;flex-direction:column;gap:6px;margin-bottom:var(--s4)}
.field label{font-weight:500} .field .hint{color:var(--muted);font-size:13px} .field .error{color:var(--danger);font-size:13px}
input,select,textarea{font:inherit;color:var(--text);background:var(--surface);border:1px solid var(--border);border-radius:var(--r1);padding:8px 10px;width:100%}
input:focus,select:focus,textarea:focus{border-color:var(--accent);outline:none;box-shadow:0 0 0 3px var(--accent-bg)}
.check{display:flex;gap:var(--s2);align-items:flex-start} .check input{width:auto;margin-top:3px}

/* states */
.empty{text-align:center;padding:var(--s7) var(--s4);color:var(--muted)} .empty .title{color:var(--text);font-weight:600;margin-bottom:var(--s1)}
.skeleton{background:linear-gradient(90deg,var(--surface-2) 25%,var(--border) 50%,var(--surface-2) 75%);background-size:200% 100%;animation:sk 1.2s infinite;border-radius:var(--r1);height:14px}
@keyframes sk{to{background-position:-200% 0}}
#notice{position:fixed;left:50%;bottom:var(--s5);transform:translateX(-50%);z-index:50;display:flex;flex-direction:column;gap:var(--s2);max-width:min(40rem,90vw)}
.notice{display:flex;gap:var(--s3);align-items:center;padding:var(--s3) var(--s4);border-radius:var(--r2);border:1px solid var(--border);background:var(--surface);box-shadow:0 8px 24px rgba(0,0,0,.18)}
.notice.success{border-color:var(--ok)} .notice.error{border-color:var(--danger)} .notice button{margin-left:auto}
.alert{padding:var(--s3) var(--s4);border-radius:var(--r1);border:1px solid var(--border);background:var(--surface-2)}
.alert.error{background:var(--danger-bg);border-color:var(--danger);color:var(--danger)} .alert.success{background:var(--ok-bg);border-color:var(--ok);color:var(--ok)}

/* tabs */
.tabs{display:flex;gap:var(--s1);border-bottom:1px solid var(--border);margin-bottom:var(--s5);overflow-x:auto}
.tabs a{padding:var(--s2) var(--s3);color:var(--muted);text-decoration:none;border-bottom:2px solid transparent;white-space:nowrap}
.tabs a[aria-current=true]{color:var(--text);border-bottom-color:var(--accent)}

/* dialog */
dialog{border:1px solid var(--border);border-radius:var(--r2);background:var(--surface);color:var(--text);padding:0;max-width:min(32rem,92vw);width:100%}
dialog::backdrop{background:rgba(0,0,0,.45)} dialog .dlg-head{padding:var(--s4);border-bottom:1px solid var(--border);font-weight:600}
dialog .dlg-body{padding:var(--s4)} dialog .dlg-foot{padding:var(--s3) var(--s4);border-top:1px solid var(--border);display:flex;justify-content:flex-end;gap:var(--s2)}

/* split app layout (mail, chat) */
.split{display:grid;grid-template-columns:18rem minmax(0,1fr);height:calc(100dvh - 52px)}
.split .side{border-right:1px solid var(--border);background:var(--surface);display:flex;flex-direction:column;min-height:0}
.split .main{display:flex;flex-direction:column;min-height:0;min-width:0}
.side .side-head{padding:var(--s3);border-bottom:1px solid var(--border)} .side .side-list{overflow:auto;flex:1}
.list[role=listbox]{margin:0;padding:0;list-style:none}
.list [role=option]{display:block;width:100%;text-align:left;background:none;border:0;border-bottom:1px solid var(--border);padding:var(--s3) var(--s4);color:var(--text);font:inherit;cursor:pointer}
.list [role=option]:hover{background:var(--surface-2)} .list [role=option][aria-selected=true]{background:var(--accent-bg)}
.list .unread .title{font-weight:700}
.menu-btn,.back-btn{display:none}
@media (max-width:48rem){
  .split{grid-template-columns:1fr} .split .side{position:fixed;inset:52px 0 0 0;z-index:20;transform:translateX(-100%);transition:transform .2s}
  .split .side.open{transform:none} .menu-btn,.back-btn{display:inline-flex}
  .topbar{padding:0 var(--s3)} .topbar nav{gap:0} .topbar .who{display:none} main.page{padding:var(--s4)}
}
```

- [ ] **Step 5: Write `static/app.js`**

```js
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
```

- [ ] **Step 6: Write the three layouts and the site placeholder**

`templates/layout.html`:

```html
{{define "layout_head"}}<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>{{.Shell.Title}} · Entropix</title>
<link rel="stylesheet" href="/static/app.css?v={{.Shell.Version}}">
<script src="/static/app.js?v={{.Shell.Version}}" defer></script>
</head><body>{{end}}

{{define "layout"}}{{template "layout_head" .}}
<header class="topbar">
  <a class="wordmark" href="/dashboard">Entro<b>pix</b></a>
  <nav aria-label="Primary">
    <a href="/dashboard" {{if eq .Shell.Nav "dashboard"}}aria-current="page"{{end}}>Dashboard</a>
    <a href="/mail" {{if eq .Shell.Nav "mail"}}aria-current="page"{{end}}>Mail</a>
    <a href="/chat" {{if eq .Shell.Nav "chat"}}aria-current="page"{{end}}>Chat</a>
    <a href="/docs" {{if eq .Shell.Nav "docs"}}aria-current="page"{{end}}>Docs</a>
  </nav>
  <span class="spacer"></span>
  {{if .Shell.Email}}<span class="who">{{.Shell.Email}}</span>
  <form method="post" action="/logout"><input type="hidden" name="csrf" value="{{.Shell.CSRF}}"><button class="btn ghost sm" type="submit">Sign out</button></form>
  {{else}}<a class="btn sm" href="/login">Sign in</a>{{end}}
</header>
{{template "content" .}}
<div id="notice" aria-live="polite"></div>
</body></html>{{end}}
```

`templates/layout_public.html`:

```html
{{define "layout_public"}}{{template "layout_head" .}}
<main class="page" style="max-width:30rem;padding-top:var(--s7)">
  <p style="text-align:center;margin:0 0 var(--s5)"><a class="wordmark" href="/">Entro<b>pix</b></a></p>
  <div class="card"><div class="card-body" style="font-size:16px">{{template "content" .}}</div></div>
</main>
<div id="notice" aria-live="polite"></div>
</body></html>{{end}}
```

`templates/layout_site.html`:

```html
{{define "layout_site"}}{{template "layout_head" .}}
<link rel="stylesheet" href="/static/site.css?v={{.Shell.Version}}">
<header class="topbar site-bar">
  <a class="wordmark" href="/">Entro<b>pix</b></a>
  <nav aria-label="Site"><a href="#features">Features</a><a href="#providers">Providers</a><a href="/docs">Docs</a></nav>
  <span class="spacer"></span>
  {{if .Shell.Email}}<a class="btn sm" href="/dashboard">Dashboard</a>{{else}}<a class="btn ghost sm" href="/login">Sign in</a><a class="btn primary sm" href="/signup">Get API key</a>{{end}}
</header>
{{template "content" .}}
<div id="notice" aria-live="polite"></div>
</body></html>{{end}}
```

`templates/site.html` (placeholder — Task 6 replaces the content block):

```html
{{define "site"}}{{template "layout_site" .}}{{end}}
{{define "content"}}<main class="page"><h1>One API for mail and chat.</h1><p class="muted">Entropix connects Outlook and WhatsApp accounts with hosted auth, delivers every message to your webhook, and sends with one endpoint.</p><p><a class="btn primary" href="/signup">Get started</a> <a class="btn" href="/docs">Read the docs</a></p></main>{{end}}
```

**Important on `{{define "content"}}`**: `html/template` has one namespace per template set, so every page file must define its content block under a page-specific name and the layout must be invoked with that name. Use this pattern instead of a shared `"content"`: each page defines `{{define "site_content"}}…{{end}}` and the page entry is `{{define "site"}}{{template "layout_site_start" .}}{{template "site_content" .}}{{template "layout_site_end" .}}{{end}}`. Split each layout into `_start` (everything before the content) and `_end` (notice div + closing tags). Apply this split to all three layouts in this step; the snippets above show the intended markup.

`static/site.css` can be an empty file with a comment for now.

- [ ] **Step 7: Wire routes**

In `internal/api/api.go` append to `browserRoutes`: `"GET /"`, `"GET /static/"`. In `browserHandlers()` add:

```go
"GET /":        s.handleSite,
"GET /static/": web.Static().ServeHTTP,
```

Note `GET /` in Go 1.22+ mux matches only the exact root; keep `"GET /{$}"` if the isolation test or 404 behaviour for unknown paths matters — use `"GET /{$}"` so `/nonexistent` still 404s. Add to `handlers_misc.go`:

```go
// handleSite is the public product website. It is the only page that
// renders for both anonymous and signed-in visitors on the same route.
func (s *Server) handleSite(w http.ResponseWriter, r *http.Request) {
    email := ""
    if dev, ok := s.sessionDeveloper(w, r); ok {
        email = dev.Email
    }
    w.Header().Set("Content-Type", "text/html; charset=utf-8")
    w.WriteHeader(http.StatusOK)
    _ = web.Templates().ExecuteTemplate(w, "site", map[string]any{
        "Shell": web.Shell{Title: "Entropix", Version: web.Version, Email: email},
    })
}
```

Check `isolation_test.go:389 TestBrowserHandlersMatchBrowserRoutes` and `TestBrowserRoutesIsolation` — if the latter enumerates every route and needs a case, add cases for `GET /{$}` (expects 200, no tenant data) and `GET /static/` (200 for `/static/app.css`).

- [ ] **Step 8: Run tests**

Run: `go build ./... && go vet ./... && go test ./... 2>&1 | grep -v "^ok\|no test files"`
Expected: no output (all green).

- [ ] **Step 9: Commit**

```bash
git add internal/web internal/api/api.go internal/api/handlers_misc.go internal/api/api_test.go internal/api/isolation_test.go
git commit -m "feat(web): embedded design system, static assets, layouts, Entropix site placeholder"
```

---

### Task 2: Dashboard

**Files:**
- Create: `internal/web/templates/dashboard.html`
- Modify: `internal/api/handlers_ui.go:67-79` (handler), delete `dashboardTmpl` and `dashboardHTML` (lines 80–577)
- Test: `internal/api/api_test.go` (`TestDashboardShowsDeveloperAndKeysPanel:605`, `TestDashboardLinksToMailPage:644`, `TestDashboardShowsProviderPickerAndChatCards:658` — update assertions to the new markup)

**Interfaces:**
- Consumes: `web.Templates()`, `web.Shell`, `um.*` from Task 1. API endpoints (unchanged): `GET /api/v1/providers`, `GET /api/v1/accounts`, `GET /api/v1/accounts/{id}`, `POST /api/v1/accounts/{id}/resync`, `POST /api/v1/accounts/{id}/reconnect`, `DELETE /api/v1/accounts/{id}`, `POST /api/v1/hosted-auth`, `GET|POST|DELETE /api/v1/accounts/{id}/webhooks`, `GET|POST|DELETE /api/v1/webhooks` (verify the global-hook route names in `apiRoutes` before writing JS), `GET|POST|DELETE /api/v1/api-keys`, `POST /api/v1/me/password`, `GET /api/v1/me`, `PUT /api/v1/me/redirect-domains`.
- Produces: the status mapping function `stateOf(a)` in `dashboard.html`'s script, reused verbatim by chat (Task 4):

```js
function stateOf(a) {
  const c = a.connection;
  if (a.kind === "chat") {
    if (a.status === "CREDENTIALS" || (c && c.state === "stopped")) return { cls: "danger", label: "Needs relink", sub: c && c.last_error || "" };
    if (!c) return { cls: "info", label: "Connecting", sub: "" };
    if (c.state === "connected") return { cls: "ok", label: "Connected", sub: "since " + um.relTime(c.since) + (c.reconnects ? " · " + c.reconnects + " reconnect" + (c.reconnects > 1 ? "s" : "") : "") };
    if (c.state === "backoff") return { cls: "warn", label: "Reconnecting", sub: "attempt " + c.reconnects + (c.last_error ? ": " + c.last_error : "") };
    if (c.state === "error") return { cls: "danger", label: "Error", sub: c.last_error || "" };
    return { cls: "info", label: "Connecting", sub: "" };
  }
  if (a.status === "CREDENTIALS") return { cls: "danger", label: "Needs reconnect", sub: "sign in again to resume" };
  return { cls: "ok", label: "Connected", sub: a.last_synced_at ? "synced " + um.relTime(a.last_synced_at) : "first sync pending" };
}
```

- [ ] **Step 1: Update the failing tests**

Replace the bodies of the three dashboard tests so they assert on the new markup:

```go
func TestDashboardShowsDeveloperAndKeysPanel(t *testing.T) {
    s, _ := newTestServer(t)
    dev, _ := seedDev(t, s, "a@x.com")
    rec := httptest.NewRecorder()
    s.Routes().ServeHTTP(rec, withSession(t, s, httptest.NewRequest(http.MethodGet, "/dashboard", nil), dev.ID))
    body := rec.Body.String()
    for _, want := range []string{"a@x.com", `href="#api-keys"`, `id="api-keys"`, `id="webhooks"`, `id="settings"`, "/static/app.js", `aria-current="page"`, "Entropix"} {
        if !strings.Contains(body, want) {
            t.Fatalf("dashboard missing %q", want)
        }
    }
    for _, never := range []string{"alert(", "confirm(", "localStorage"} {
        if strings.Contains(body, never) {
            t.Fatalf("dashboard still uses %q", never)
        }
    }
}

func TestDashboardLinksToMailPage(t *testing.T) {
    s, _ := newTestServer(t)
    dev, _ := seedDev(t, s, "a@x.com")
    rec := httptest.NewRecorder()
    s.Routes().ServeHTTP(rec, withSession(t, s, httptest.NewRequest(http.MethodGet, "/dashboard", nil), dev.ID))
    body := rec.Body.String()
    // Top-level nav reaches mail and chat even with zero accounts.
    for _, want := range []string{`href="/mail"`, `href="/chat"`, `href="/docs"`} {
        if !strings.Contains(body, want) {
            t.Fatalf("dashboard nav missing %q", want)
        }
    }
}
```

For `TestDashboardShowsProviderPickerAndChatCards` keep its intent (providers rendered, chat rows) but assert on `id="connect-dialog"` and the `stateOf(` function being present, plus the absence of raw `connection.state` interpolation (`c.state` must only appear inside `stateOf`).

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/api -run TestDashboard -v 2>&1 | tail -8`
Expected: FAIL on missing `id="api-keys"` etc.

- [ ] **Step 3: Write `templates/dashboard.html`**

Structure (write the full file; the script must implement everything listed):

```html
{{define "dashboard"}}{{template "layout_start" .}}{{template "dashboard_content" .}}{{template "layout_end" .}}{{end}}
{{define "dashboard_content"}}
<main class="page">
  <div class="page-head">
    <div><h1>Accounts</h1><p class="muted small" id="provider-line">Loading providers…</p></div>
    <div class="actions"><button class="btn primary" type="button" id="connect-btn">Connect account</button></div>
  </div>
  <nav class="tabs" aria-label="Sections">
    <a href="#accounts" aria-current="true">Accounts</a><a href="#webhooks">Webhooks</a><a href="#api-keys">API keys</a><a href="#settings">Settings</a>
  </nav>
  <section id="accounts" class="card"><div id="accounts-list"><div class="row"><div class="skeleton" style="width:40%"></div></div><div class="row"><div class="skeleton" style="width:55%"></div></div></div></section>
  <section id="webhooks" class="card hidden"> … table + "Add webhook" button + empty state … </section>
  <section id="api-keys" class="card hidden"> … table + "Create key" + one-time key panel with <button data-copy> … </section>
  <section id="settings" class="hidden"> … change-password form + redirect-domains form, each with .field/label … </section>
  <dialog id="connect-dialog">…provider radio list…</dialog>
  <dialog id="key-dialog">…name field…</dialog>
  <dialog id="webhook-dialog">…scope select, kind select (webhook|discord|telegram), name, url, secret, events checkboxes…</dialog>
</main>
<script>
(() => {
  const { $, esc, api, notice, relTime } = um;
  // stateOf(a) exactly as in the Interfaces block above.
  // tabs: on hashchange show the matching section, set aria-current; default #accounts.
  // loadProviders(): GET /api/v1/providers → fill #provider-line ("Outlook · WhatsApp") and the connect dialog radios.
  // loadAccounts(): GET /api/v1/accounts → renderAccount(a) rows; empty → .empty with a button that opens the connect dialog.
  // renderAccount(a): avatar (initial of provider), .title (a.name || a.email || masked identifier — never print a.identifier raw; server already masks), badge kind, pill from stateOf, .sub line, actions: Open (href /mail?account_id= or /chat?account_id=), Resync|Reconnect, Webhook (sets hash #webhooks and filters), Disconnect.
  // resync/reconnect: POST, notice("info", …), then watch(a.id): um.poll(GET /api/v1/accounts/{id}, 3000) up to 60s; stop when last_synced_at changed or connection.state==="connected"; notice("success", "Resynced"|"Reconnected").
  // disconnect: await um.confirm({title:"Disconnect "+name+"?", body:"Mirrored mail/chats for this account are deleted. WhatsApp devices are logged out.", action:"Disconnect", danger:true}) → DELETE → reload list.
  // keys: loadKeys(); create dialog → POST /api/v1/api-keys {name} → show #new-key with <code> and data-copy button (um.copy); revoke → confirm → DELETE.
  // webhooks: loadWebhooks() merges global + per-account; add/edit dialog; delete → confirm.
  // settings: password form → POST /api/v1/me/password; domains → PUT /api/v1/me/redirect-domains; success → notice("success").
  // All catch(e) → notice("error", e.message).
  // Every id/href interpolation goes through esc().
})();
</script>
{{end}}
```

Write the real markup and JS — the comments above are the required behaviours, not placeholders to leave in.

- [ ] **Step 4: Rewrite `handleDashboard`**

```go
func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
    dev, ok := s.sessionDeveloper(w, r)
    if !ok {
        http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusFound)
        return
    }
    csrf := s.csrfToken(w, r)
    w.Header().Set("Content-Type", "text/html; charset=utf-8")
    w.WriteHeader(http.StatusOK)
    _ = web.Templates().ExecuteTemplate(w, "dashboard", map[string]any{
        "Shell": web.Shell{Title: "Dashboard", Version: web.Version, Email: dev.Email, CSRF: csrf, Nav: "dashboard"},
    })
}
```

Delete `dashboardTmpl`, `dashboardHTML`. Keep `landingTmpl`/`landingData` for now (Task 3 moves them).

- [ ] **Step 5: Run tests, build, and smoke in a browser**

Run: `go build ./... && go test ./internal/api -run 'TestDashboard|TestPages' -v 2>&1 | tail -6`
Expected: PASS. Then `set -a && source .env && set +a && WHATSAPP_ROSTER_GROUPS=0 go run ./cmd/server` and open `/dashboard`: tabs switch, connect dialog lists providers, key create shows a Copy button, no console errors.

- [ ] **Step 6: Commit**

```bash
git add internal/web/templates/dashboard.html internal/api/handlers_ui.go internal/api/api_test.go
git commit -m "feat(ui): dashboard on the shared design system — tabs, human status, dialogs, copyable keys"
```

---

### Task 3: Hosted auth — connect landing, QR page, result pages

**Files:**
- Create: `internal/web/templates/connect_oauth.html`, `connect_qr.html`, `connect_result.html`
- Modify: `internal/api/handlers_ui.go:22-60` (delete `landingData`, `landingTmpl`, keep the handler that renders it and switch it to the new template), `internal/api/handlers_link.go:686-791` (`linkPageData`, `linkTmpl`, render call), `internal/api/handlers_connect.go:505-515` (`messageTmpl`, `renderMessage`), `internal/provider/provider.go` (add `DisplayName`)
- Test: `internal/api/api_test.go` (`TestConnectRejectsUnknownState:689`, add `TestConnectPagesNameProviderFromRegistry`)

**Interfaces:**
- Produces: `provider.DisplayName(name string) string` — maps `"OUTLOOK"`→`"Outlook"`, `"WHATSAPP"`→`"WhatsApp"`, anything else → title-cased name. Also `provider.ScopeSentences(scopes []string) []string` in `internal/provider/outlook` is NOT needed — put the scope→sentence map in `handlers_ui.go`:

```go
var scopeText = map[string]string{
    "Mail.Read": "Read your mail", "Mail.ReadWrite": "Read, move and mark your mail", "Mail.Send": "Send mail as you",
    "User.Read": "See your name and email address", "offline_access": "Stay connected without asking again",
}
```

- `renderMessage(w, status, title, body)` keeps its signature but gains an optional `next` link: change to `renderResult(w http.ResponseWriter, status int, r resultPage)` with

```go
type resultPage struct {
    Title, Body, Detail string // Detail goes under a <details>; may be empty
    NextURL, NextLabel  string // optional Continue button
    Copy                string // optional value shown with a copy button (account id)
}
```

and keep `renderMessage` as a thin wrapper calling it so existing call sites compile.

- [ ] **Step 1: Write the failing test**

```go
func TestConnectPagesNameProviderFromRegistry(t *testing.T) {
    s, db := newTestServerWithProviders(t, providertest.NewFakeChat("FAKECHAT"))
    dev, _ := seedDev(t, s, "a@x.com")
    if err := db.SaveOAuthState(store.OAuthState{State: "st_qr", DeveloperID: dev.ID, Provider: "FAKECHAT", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
        t.Fatal(err)
    }
    rec := httptest.NewRecorder()
    s.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/connect/st_qr", nil))
    body := rec.Body.String()
    if rec.Code != http.StatusOK || !strings.Contains(body, "Fakechat") {
        t.Fatalf("code=%d, body lacks provider display name", rec.Code)
    }
    for _, never := range []string{"Microsoft", "Reload the page"} {
        if strings.Contains(body, never) {
            t.Fatalf("connect page still says %q", never)
        }
    }
    for _, want := range []string{`aria-live`, `id="try-again"`, `id="countdown"`, "Entropix"} {
        if !strings.Contains(body, want) {
            t.Fatalf("connect page missing %q", want)
        }
    }
}
```

- [ ] **Step 2: Run to verify it fails** — `go test ./internal/api -run TestConnectPagesName -v` → FAIL.

- [ ] **Step 3: Add `provider.DisplayName`**

```go
// DisplayName is how a provider is named to a human. Names() are stable
// identifiers, not copy.
func DisplayName(name string) string {
    switch name {
    case "OUTLOOK":
        return "Outlook"
    case "WHATSAPP":
        return "WhatsApp"
    }
    if name == "" {
        return ""
    }
    return strings.ToUpper(name[:1]) + strings.ToLower(name[1:])
}
```

- [ ] **Step 4: Write the three templates**

`connect_oauth.html` (public layout): step indicator (`<ol class="steps"><li aria-current="step">Review</li><li>Authorize</li><li>Done</li></ol>` — add `.steps` CSS to `app.css`), `h1` "Connect your {{.Provider}} account", `<p>` "Entropix is connecting your {{.Provider}} account. You'll sign in on {{.Provider}}'s own page; Entropix never sees your password.", `<ul>` of `{{range .Scopes}}<li>{{.}}</li>{{end}}`, `<a class="btn primary" href="{{.AuthorizeURL}}">Continue to {{.Provider}}</a>`, `{{if .CancelURL}}<a class="btn" href="{{.CancelURL}}">Cancel</a>{{else}}<p class="muted small">Changed your mind? You can close this page.</p>{{end}}`.

`connect_qr.html`: steps Review → Scan → Done; disclosure text (move the existing copy from `linkTmpl` verbatim); `<label class="check"><input type="checkbox" id="consent"> I understand…</label>`; `<button id="show" class="btn primary" disabled>Show QR code</button>`; QR pane (hidden until consent): `<img id="qr" alt="QR code to scan with WhatsApp">`, `<svg id="countdown" …>` ring with `<circle>` whose `stroke-dashoffset` is driven by JS from `expires_in`, `<p id="status" aria-live="polite">`, instructions list, `<button id="try-again" class="btn hidden">Try again</button>`. Script: consent → POST `/connect/{state}/consent` (CSRF as today) → `um.poll(fetchQR, 2000)`; `fetchQR` GETs `/connect/{{.State}}/qr`; on `{status:"pending", qr, expires_in}` set image + restart countdown; on `connected` stop, mark step Done, redirect if the response carries `redirect`; on `expired`/`failed` stop, show `#try-again` which re-runs consent. Keep every existing status string the server emits — read `handleLinkQR` (`handlers_link.go:382`) for the exact JSON fields before writing this.

`connect_result.html`: `{{.Title}}`, `{{.Body}}`, `{{if .Copy}}<p><code>{{.Copy}}</code> <button class="btn sm" data-copy="{{.Copy}}">Copy</button></p>{{end}}`, `{{if .Detail}}<details><summary>Details</summary><pre>{{.Detail}}</pre></details>{{end}}`, `{{if .NextURL}}<a class="btn primary" href="{{.NextURL}}">{{.NextLabel}}</a>{{else}}<p class="muted">You can return to the app now.</p>{{end}}`. Inline script: `document.querySelectorAll("[data-copy]").forEach(b => b.onclick = () => um.copy(b.dataset.copy, b))`.

- [ ] **Step 5: Rewire handlers**

In `handlers_ui.go` the landing handler builds `map[string]any{"Shell": web.Shell{Title: "Connect " + display, Version: web.Version}, "Provider": display, "AuthorizeURL": url, "Scopes": sentences, "CancelURL": cancel}` where `display := provider.DisplayName(st.Provider)`, `sentences` maps `s.cfg.Scopes` through `scopeText` (fallback: the raw scope), and `cancel` is the state's failure redirect if the store keeps one (check `store.OAuthState` fields; if none exists, leave `CancelURL` empty — do not add a field).

In `handlers_link.go` replace `linkTmpl.Execute(w, d)` with `web.Templates().ExecuteTemplate(w, "connect_qr", map[string]any{"Shell": …, "Provider": provider.DisplayName(d.Provider), "State": d.State, "CSRF": …})`. Delete `linkTmpl`.

In `handlers_connect.go` replace `messageTmpl`/`renderMessage` with `renderResult`, and update the success call site to pass `NextURL` = the success redirect when set, `Copy` = account id, `Body` = "{email} is now connected." Update the four error call sites: `Title` human, `Detail` = provider error text (was inline in the body).

- [ ] **Step 6: Run all tests** — `go test ./... 2>&1 | grep -v "^ok\|no test files"` → empty. Fix any test asserting the old "Account ID:" body text (`grep -rn "Account ID" internal/api/*_test.go`).

- [ ] **Step 7: Manual smoke** — with the server running, `POST /api/v1/hosted-auth` for WHATSAPP from the dashboard's Connect dialog, open the URL: consent → QR → countdown visible → let it expire → Try again works.

- [ ] **Step 8: Commit**

```bash
git add internal/web/templates/connect_*.html internal/api/handlers_ui.go internal/api/handlers_link.go internal/api/handlers_connect.go internal/provider/provider.go internal/api/api_test.go internal/web/static/app.css
git commit -m "feat(ui): hosted-auth pages — provider-agnostic copy, QR countdown and retry, styled results"
```

---

### Task 4: Mail and Chat viewers

**Files:**
- Create: `internal/web/templates/mail.html`, `internal/web/templates/chat.html`
- Modify: `internal/api/handlers_mail_ui.go` (handler at :14; delete `mailTmpl`/`mailHTML`), `internal/api/handlers_chat_ui.go` (handler at :14; delete `chatTmpl`/`chatHTML`)
- Test: `internal/api/api_test.go` — add `TestMailAndChatPagesUseSharedShell`

**Interfaces:**
- Consumes: `um.listNav`, `um.poll`, `um.api.raw` (attachments), `stateOf(a)` copied verbatim from Task 2 into `chat.html`. Endpoints unchanged from the audit: mail `GET /api/v1/accounts`, `/folders`, `/emails?account_id&q&folder_id&unread&limit&offset`, `/emails/{id}`, `/emails/{id}/attachments[/{aid}]`, `PATCH /api/v1/emails/{id}` with `{"unread":false}` (confirm the exact route/body in `handlers_mail.go` — if the API uses a different shape, use that); chat `GET /api/v1/chats?account_id`, `GET|POST /api/v1/chats/{id}/messages?account_id` (`limit`, `before`).

- [ ] **Step 1: Write the failing test**

```go
func TestMailAndChatPagesUseSharedShell(t *testing.T) {
    s, _ := newTestServer(t)
    dev, _ := seedDev(t, s, "a@x.com")
    for _, path := range []string{"/mail", "/chat"} {
        rec := httptest.NewRecorder()
        s.Routes().ServeHTTP(rec, withSession(t, s, httptest.NewRequest(http.MethodGet, path, nil), dev.ID))
        body := rec.Body.String()
        if rec.Code != http.StatusOK {
            t.Fatalf("%s: %d", path, rec.Code)
        }
        for _, want := range []string{`class="split"`, `role="listbox"`, `class="menu-btn`, `class="back-btn`, "um.listNav", "/static/app.js", `aria-current="page"`} {
            if !strings.Contains(body, want) {
                t.Fatalf("%s missing %q", path, want)
            }
        }
        if strings.Contains(body, "100vh") || strings.Contains(body, "alert(") {
            t.Fatalf("%s uses 100vh or alert()", path)
        }
    }
}
```

- [ ] **Step 2: Run to verify it fails** — `go test ./internal/api -run TestMailAndChatPages -v` → FAIL.

- [ ] **Step 3: Write `mail.html`**

Markup: `<main class="page wide"><div class="split"><aside class="side" id="side"><div class="side-head"><label class="sr-only" for="acct">Account</label><select id="acct"></select><input id="q" type="search" placeholder="Search mail" aria-label="Search mail"><label class="check small"><input type="checkbox" id="unread"> Unread only</label></div><div class="side-list"><ul class="list" role="listbox" id="folders" aria-label="Folders"></ul></div></aside><section class="main"><div class="side-head actions"><button class="btn ghost sm menu-btn" aria-label="Folders">☰</button><span id="page-info" class="muted small"></span><span class="spacer"></span><button class="btn sm" id="newer">Newer</button><button class="btn sm" id="older">Older</button></div><div style="display:grid;grid-template-columns:22rem minmax(0,1fr);min-height:0;flex:1" id="panes"><ul class="list" role="listbox" id="list" aria-label="Messages"></ul><article id="reader"><div class="empty">Select a message</div></article></div></section></div></main>`. Add CSS in `app.css`: `@media (max-width:48rem){#panes{grid-template-columns:1fr} #panes.reading #list{display:none} #panes:not(.reading) #reader{display:none}}`.

Script requirements: account switcher from `/api/v1/accounts` filtered `kind==="mail"`, honouring `?account_id=`; folders sorted by the existing `ROLE_ORDER` (copy the array from the old template); list rows are `<li><button role="option" class="…unread">` with `.title` sender, `.sub` subject + `um.relTime(date)`; skeleton rows while loading; 300 ms debounce search; pager with Older disabled when `items.length < 50`; open → `GET /emails/{id}`, render headers, body into `<iframe sandbox="" srcdoc>` exactly as before, attachments list with download via `um.api.raw` → blob; after open, if unread: `PATCH` to mark read and drop `.unread`; on `< 48rem` add `reading` class to `#panes` and show `.back-btn` inside reader; `um.listNav` on both listboxes; errors → `um.notice("error")`; `j/k` handled by `listNav`.

- [ ] **Step 4: Write `chat.html`**

Markup mirrors mail: side = account select + chats listbox; main = head (`menu-btn`, chat name, `<span id="conn" class="pill">`), `#thread` scroll region with `<button id="older" class="btn sm">Load older</button>` at top, bubbles, `<button id="newmsgs" class="btn sm hidden">New messages ↓</button>` floating, composer `<form id="composer"><label class="sr-only" for="text">Message</label><input id="text" autocomplete="off"><button class="btn primary">Send</button></form>`.

Script requirements: `stateOf(a)` verbatim from Task 2 drives `#conn` (`pill ok|warn|danger|info` + label); `um.poll(refresh, 5000)`; `refresh` reloads chats and, if a thread is open, its latest page — merge by message id, do not re-render the whole thread; scroll logic: `const atBottom = thread.scrollHeight - thread.scrollTop - thread.clientHeight < 80` captured before render; if `atBottom` scroll to bottom, else show `#newmsgs` (click → scroll to bottom, hide); optimistic send: push `{id:"tmp-"+Date.now(), text, is_from_me:true, status:"sending"}` render immediately, POST with `Idempotency-Key: crypto.randomUUID()`; on 201 replace the temp bubble with the response; on error mark bubble `.failed` with a Retry link that re-sends with the same idempotency key; error bar: `um.notice` only on send failure; poll failures set `#conn` sub text "offline" and clear on next success — no persistent banner.

- [ ] **Step 5: Rewire handlers**

`handleMailPage` / `handleChatPage`: same session + CSRF logic as today, then `web.Templates().ExecuteTemplate(w, "mail"|"chat", map[string]any{"Shell": web.Shell{Title: "Mail"|"Chat", Version: web.Version, Email: dev.Email, CSRF: csrf, Nav: "mail"|"chat"}})`. Delete the two `const …HTML` strings and `template.Must` vars.

- [ ] **Step 6: Run tests + manual smoke** — `go test ./... 2>&1 | grep -v "^ok\|no test files"` empty; then in the browser: mail list/reader at 1280px and 375px (DevTools device mode), keyboard ↑/↓/Enter; chat send shows "sending" then settles; scroll up, wait for poll, "New messages" chip appears instead of a jump.

- [ ] **Step 7: Commit**

```bash
git add internal/web/templates/mail.html internal/web/templates/chat.html internal/web/static/app.css internal/api/handlers_mail_ui.go internal/api/handlers_chat_ui.go internal/api/api_test.go
git commit -m "feat(ui): mail and chat viewers — responsive split layout, keyboard nav, optimistic send, gentle polling"
```

---

### Task 5: Auth pages + docs reference

**Files:**
- Create: `internal/web/templates/login.html`, `internal/web/templates/docs.html`, `internal/web/docs/docs.go`, `internal/web/docs/snippets.go`, `internal/web/docs/docs_test.go`
- Modify: `internal/api/handlers_auth.go:120-186` (`authTmpl`, `renderAuth`), `internal/api/handlers_docs.go` (all), `internal/api/handlers_llms.go` (say Entropix in the title line only)
- Test: `internal/web/docs/docs_test.go`, `internal/api/api_test.go` (add `TestDocsListsEveryRouteWithAnchors`)

**Interfaces:**
- Produces (`internal/web/docs`):

```go
package docs

type Param struct{ Name, In, Type, Desc string; Required bool } // In: "path"|"query"|"body"|"header"
type Endpoint struct {
    Method, Path, Summary, Group string
    Params            []Param
    Request, Response string // JSON samples; Request may be ""
    Anchor            string // e.g. "post-api-v1-chats-id-messages"
}
type Event struct{ Type, When, Sample string; Kinds []string }
type ErrorCode struct{ Code string; Status int; Fix string }

var Endpoints []Endpoint   // one per apiRoutes entry
var Events []Event         // one per model.Event* constant
var Errors []ErrorCode     // every error.code the API emits (grep `errorCode|writeError` in internal/api)
func Anchor(method, path string) string // lowercases, strips "/", "{" "}" → "-"; used by tests and templates
```

`snippets.go`: `type Snippet struct{ Curl, Node, Go string }` and `var SendMessage, HostedAuth, WebhookPayload, CreateKey Snippet` — real, runnable text using `$API_KEY`, `$BASE`, `{chat_id}`, `{account_id}`.

- [ ] **Step 1: Write the failing tests**

`internal/web/docs/docs_test.go`:

```go
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
        if seen[e.Anchor] { t.Fatalf("duplicate anchor %s", e.Anchor) }
        seen[e.Anchor] = true
    }
    if len(Events) == 0 || len(Errors) == 0 { t.Fatal("events/errors empty") }
}
```

`internal/api/api_test.go`:

```go
func TestDocsListsEveryRouteWithAnchors(t *testing.T) {
    s, _ := newTestServer(t)
    rec := httptest.NewRecorder()
    s.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/docs", nil))
    body := rec.Body.String()
    for _, p := range apiRoutes {
        method, path, _ := strings.Cut(p, " ")
        if !strings.Contains(body, `id="`+docs.Anchor(method, path)+`"`) {
            t.Fatalf("docs missing endpoint block for %s", p)
        }
    }
    for _, want := range []string{"Quickstart", `id="events"`, `id="errors"`, "data-copy", `aria-label="Contents"`, "Entropix"} {
        if !strings.Contains(body, want) { t.Fatalf("docs missing %q", want) }
    }
}
func TestDocsDataCoversApiRoutes(t *testing.T) {
    have := map[string]bool{}
    for _, e := range docs.Endpoints { have[e.Method+" "+e.Path] = true }
    for _, p := range apiRoutes {
        if !have[p] { t.Fatalf("no docs.Endpoint for %s", p) }
    }
}
```

- [ ] **Step 2: Run to verify they fail** — `go test ./internal/web/docs ./internal/api -run 'TestAnchor|TestEndpoints|TestDocs' 2>&1 | tail -4` → build failures.

- [ ] **Step 3: Write `docs.go` and `snippets.go`**

Populate `Endpoints` for every entry in `internal/api/api.go:84 apiRoutes`. Source of truth for params and bodies: the handler and request structs in `handlers_*.go` (e.g. `createKeyRequest` at `handlers_auth.go:317`, `sendPayload` at `handlers_mail.go:306`, hosted-auth request in `handlers_connect.go`). Responses are JSON of the model types with realistic values (`acc_…`, `wh_…`, ISO timestamps). `Group` reuses `routeGroup()` names ("Developer & keys", "Connecting mailboxes", "Accounts", "Mail", "Chat", "Webhooks"). `Events` lists every `model.EventXxx` constant (`grep -n "Event[A-Z][A-Za-z]* *=" internal/model/*.go`) with a sample payload. `Errors` comes from `grep -rn '"[a-z_]*"' internal/api/errors.go internal/api/*.go | grep -i code` — enumerate what `writeError`/`apiError` emits.

- [ ] **Step 4: Write `docs.html`**

Layout: `<main class="page docs">` with a 3-column grid (`grid-template-columns:15rem minmax(0,1fr) 22rem`; `@media (max-width:64rem)` → 2 columns hiding the right rail; `@media (max-width:48rem)` → 1 column with the TOC inside `<details>`). Left: `<nav aria-label="Contents"><input id="toc-filter" type="search" placeholder="Filter  /" aria-label="Filter contents"><ul>…</ul></nav>`. Main sections in spec order: Quickstart (5 steps using the snippets), Authentication, Hosted auth, then `{{range groups}}` with each endpoint as `<section class="endpoint" id="{{.Anchor}}"><h3><span class="m {{.Method}}">{{.Method}}</span> <code>{{.Path}}</code> <a class="anchor" href="#{{.Anchor}}">#</a></h3><p>{{.Summary}}</p>{{if .Params}}<table class="table">…</table>{{end}}<div class="tabs sm" role="tablist">curl/Node/Go</div><pre>…</pre><h4>Response</h4><pre>{{.Response}}</pre></section>`, then `#events`, `#errors`, Self-hosting, llms.txt. Every `<pre>` gets `<button class="btn sm copy" data-copy>Copy</button>` injected by script (copy uses the `<pre>`'s `textContent`). Script: `/` focuses `#toc-filter`; filter hides `li`s whose text doesn't include the query; snippet tabs toggle `hidden` on sibling `<pre data-lang>`; `IntersectionObserver` marks the current TOC entry. Method chip CSS: `.m{font:600 11px var(--mono);padding:2px 6px;border-radius:4px}.m.GET{background:var(--ok-bg);color:var(--ok)}.m.POST{background:var(--info-bg);color:var(--info)}.m.PUT,.m.PATCH{background:var(--warn-bg);color:var(--warn)}.m.DELETE{background:var(--danger-bg);color:var(--danger)}` (AA: dark-on-tint in light, light-on-dark-tint in dark).

- [ ] **Step 5: Rewrite `handleDocs`**

```go
func (s *Server) handleDocs(w http.ResponseWriter, r *http.Request) {
    email, csrf := "", ""
    if dev, ok := s.sessionDeveloper(w, r); ok {
        email, csrf = dev.Email, s.csrfToken(w, r)
    }
    groups := docs.Grouped() // []struct{Name string; Endpoints []docs.Endpoint} in routeGroup order
    w.Header().Set("Content-Type", "text/html; charset=utf-8")
    w.WriteHeader(http.StatusOK)
    _ = web.Templates().ExecuteTemplate(w, "docs", map[string]any{
        "Shell":  web.Shell{Title: "API reference", Version: web.Version, Email: email, CSRF: csrf, Nav: "docs"},
        "Groups": groups, "Events": docs.Events, "Errors": docs.Errors,
        "Snippets": map[string]docs.Snippet{"send": docs.SendMessage, "hosted": docs.HostedAuth, "hook": docs.WebhookPayload, "key": docs.CreateKey},
        "Base": s.baseURL(r),
    })
}
```

Add `docs.Grouped()` returning endpoints grouped in the order Developer & keys, Connecting mailboxes, Accounts, Mail, Chat, Webhooks. Delete `docsTmpl`/`docsHTML`; keep `routeGroup` only if `docs.go` doesn't replicate it (prefer moving it into `docs` and deleting here).

- [ ] **Step 6: Write `login.html` and rewire `renderAuth`**

Template renders `authPage` fields on the public layout: `<h1>{{.Title}}</h1><p class="muted">{{.Lead}}</p>{{if .Error}}<p class="alert error" role="alert">{{.Error}}</p>{{end}}<form method="post" action="{{.Action}}" id="auth"><input type="hidden" name="csrf" value="{{.CSRF}}"><div class="field"><label for="email">Email</label><input id="email" name="email" type="email" autocomplete="email" required value="{{.Email}}" autofocus></div><div class="field"><label for="password">Password</label><div style="display:flex;gap:var(--s2)"><input id="password" name="password" type="password" autocomplete="{{if .Signup}}new-password{{else}}current-password{{end}}" required {{if .Signup}}minlength="10"{{end}}><button class="btn" type="button" id="toggle" aria-pressed="false">Show</button></div>{{if .Signup}}<p class="hint">At least 10 characters.</p>{{end}}</div>{{if .Signup}}<div class="field"><label for="name">Name <span class="muted">(optional)</span></label><input id="name" name="name"></div>{{end}}<button class="btn primary" type="submit" id="submit">{{.Button}}</button></form><p class="muted small">{{.AltLead}} <a href="{{.AltHref}}">{{.AltText}}</a> · <a href="/docs">Docs</a></p>` plus a 6-line inline script for the toggle and `submit.disabled=true; submit.textContent="Signing in…"` on submit. Titles: "Sign in to Entropix" / "Create your Entropix account" (update the page constructors in `handlers_auth.go`). `renderAuth` passes `map[string]any{"Shell": web.Shell{Title: p.Title, Version: web.Version}, "P": p}` — adjust field references to `.P.Title` etc.

- [ ] **Step 7: Run everything** — `go test ./... 2>&1 | grep -v "^ok\|no test files"` empty. Fix tests asserting old auth copy (`grep -rn "Sign in\|Create account" internal/api/*_test.go`).

- [ ] **Step 8: Commit**

```bash
git add internal/web internal/api/handlers_auth.go internal/api/handlers_docs.go internal/api/handlers_llms.go internal/api/api_test.go
git commit -m "feat(ui): auth pages on the shared shell; docs become a developer reference generated from Go data"
```

---

### Task 6: Entropix website content + manual checklist

**Files:**
- Modify: `internal/web/templates/site.html`, `internal/web/static/site.css`, `internal/api/handlers_misc.go` (`handleSite` passes providers + snippets)
- Create: `docs/ui-manual-checklist.md`
- Test: `internal/api/api_test.go` (extend `TestRootServesWebsiteAndStaticIsCacheable`)

**Interfaces:**
- Consumes: `docs.SendMessage`, `docs.HostedAuth`, `docs.WebhookPayload`, `docs.Events`, `provider.DisplayName`, `s.registry.Names()` (confirm the field name for the registry on `Server`).

- [ ] **Step 1: Extend the failing test**

Add to `TestRootServesWebsiteAndStaticIsCacheable` after the first request:

```go
for _, want := range []string{`id="features"`, `id="providers"`, "Outlook", "WhatsApp", `href="/signup"`, `href="/docs"`, "curl", "Idempotency-Key", "llms.txt", "self-host"} {
    if !strings.Contains(strings.ToLower(rec.Body.String()), strings.ToLower(want)) {
        t.Fatalf("site missing %q", want)
    }
}
if strings.Contains(rec.Body.String(), "Pricing") { t.Fatal("site must not have pricing") }
```

(`newTestServer` registers OUTLOOK + a fake chat provider — check and, if WhatsApp isn't registered there, assert on the fake's display name instead.)

- [ ] **Step 2: Run to verify it fails.**

- [ ] **Step 3: Write `site.html` content and `site.css`**

Sections with ids `hero`, `how`, `providers`, `features`, `events`, footer, per spec §7. Hero code block uses tabs `curl · Node · Go` over `docs.SendMessage`. "How it works" three cards with `docs.HostedAuth`, `docs.WebhookPayload`, `docs.SendMessage`. Providers: `{{range .Providers}}` cards with `Name`, `Kind`, capability list (`Read · Send · Webhooks` for mail; `Receive · Send · Reactions · Edit · Delete · Webhooks` for chat — pass as `[]string` from Go per kind), plus the muted "More providers coming" card; WhatsApp card shows the one-sentence linked-device note. Features grid of six per spec. Events: `{{range .Events}}<a class="pill" href="/docs#event-{{.Type}}">{{.Type}}</a>{{end}}` (add matching `id="event-…"` in docs.html Task 5 — do it here if missed). Footer: Docs, llms.txt, Sign in, "Free while in beta · self-host any time". `site.css`: dark-first hero (`.hero{background:#0c0e12;color:#eceef2}` regardless of scheme, accent gradient text on the headline, `.grid-3`/`.grid-2` responsive grids, `.feature` cards).

- [ ] **Step 4: Update `handleSite`** to pass `Providers` (from the registry, mapped to `{Name: provider.DisplayName(n), Kind, Caps []string}`), `Events: docs.Events`, `Snippets`.

- [ ] **Step 5: Write `docs/ui-manual-checklist.md`** — checkboxes per page: keyboard-only pass, 360/768/1280 widths, light/dark, every loading/empty/error state, copy buttons, chat optimistic send + failure (stop the server mid-send), QR countdown + Try again, docs `/` filter and anchors, site nav anchors.

- [ ] **Step 6: Run everything and commit**

```bash
go build ./... && go vet ./... && go test ./... 2>&1 | grep -v "^ok\|no test files"
git add internal/web internal/api/handlers_misc.go internal/api/api_test.go docs/ui-manual-checklist.md
git commit -m "feat(site): Entropix product website at / with shared snippets, providers from the registry"
```

---

### Task 7: End-to-end user-journey verification

**Files:** none new (fixes land as follow-up commits `fix(ui): …`).

- [ ] **Step 1: Run the server** — `set -a && source .env && set +a && DEBUG=1 WHATSAPP_ROSTER_GROUPS=0 go run ./cmd/server` in the background, poll `/healthz`.
- [ ] **Step 2: Walk the journey in a browser** (Chrome tools if connected; otherwise `curl` the HTML + read it critically) following `docs/ui-manual-checklist.md` in order: `/` → Get API key → `/signup` → `/dashboard` (empty state) → Create key + Copy → Connect account → `/connect/{state}` (both providers) → back on dashboard with status pill → Webhooks tab add Discord hook → `/chat` send a message (optimistic → sent) → `/mail` open, mark read → `/docs` filter + copy → Sign out → `/login` error state.
- [ ] **Step 3: For every friction point** (unclear label, missing feedback, extra click, unreadable contrast, layout break at 375px) fix it, run `go test ./...`, commit `fix(ui): <what>`.
- [ ] **Step 4: Stop the server**, confirm port 8080 free, and write the journey results (what was checked, what was fixed, what remains) into `docs/smoke-report.md` under a dated heading.
- [ ] **Step 5: Push** — `git push origin feature/multi-tenancy`.

---

## Self-review

- **Spec coverage**: §1 → Task 1; §2 → Task 2; §3 → Task 3; §4 → Task 4; §5 auth+docs → Task 5; §6 tests/checklist/rollout → Tasks 1–6 tests + Task 6 checklist; §7 → Task 6; user-journey verification requested after approval → Task 7. `GET /` redirect from spec §1 is superseded by §7 (website at `/`).
- **Placeholders**: Task 2/4 template steps list required behaviours as comments; the implementer writes the markup/JS — the behaviours are fully specified, not deferred. Task 5 tells the implementer where to source every data value.
- **Type consistency**: `web.Shell{Title,Version,Email,CSRF,Nav}` used identically in Tasks 1–6; `stateOf(a)` defined in Task 2, copied verbatim in Task 4; `docs.Anchor`, `docs.Endpoints`, `docs.Events`, `docs.Errors`, `docs.Snippet` defined in Task 5 and consumed in Task 6; `renderResult`/`resultPage` in Task 3 only. Layout template names are `layout_start`/`layout_end` (and `_public_`, `_site_` variants) per the note in Task 1 Step 6 — every page must use those names.
