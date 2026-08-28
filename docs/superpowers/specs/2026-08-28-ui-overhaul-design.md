# UI overhaul and Entropix website — design

Date: 2026-08-28. Status: approved in discussion, pending written review.

## Goal

Replace the seven server-rendered pages (each an inline HTML/CSS/JS blob in a
Go string) with one shared, embedded design system, fix the UX defects the
audit found, and add a public product website at `/` under the name
**Entropix**. No build step: everything stays in the single Go binary,
served under the existing CSP (`default-src 'self'`; inline script/style
allowed). Branding is web-surface only — the Go module, binary, README title,
env vars and DB names keep `unified-messaging`.

## Audit findings this design answers

- Six drifted copies of the CSS tokens/base styles; three copies each of
  `api()`, `escapeHtml`, `$`, error helpers, with real behavioural drift
  (mail's `api()` mishandles 204; login's dark mode misses `--accent`).
- No way from the dashboard to `/mail` or `/chat` without an account row;
  mail and chat do not link to each other; no `GET /`.
- Errors split between `alert()`, banners, inline text; no loading states for
  accounts, keys, folders or lists; resync gives no feedback; reconnect never
  refreshes the row; one-time API key has no copy button.
- Chat accounts show raw `connection.state` enums and never show `since`,
  `reconnects` or `last_error`.
- Mail/chat are desktop-only (`100vh`, fixed panes, no media queries); chat
  polls in background tabs and force-scrolls; no keyboard navigation; rows are
  non-focusable `<div>`s.
- `/connect` has hardcoded Microsoft copy for every provider, every terminal
  state says "reload the page", QR `expires_in` is never shown, result pages
  are unstyled with no dark mode and no way onward.
- Accessibility: placeholders as labels, no `aria-live` on status/errors, no
  `:focus-visible` styles, method chips below AA contrast.

## 1. Assets and shell

**Package `internal/web/`** holds `static/app.css`, `static/app.js`,
`static/site.css` (website-only additions), and `templates/*.html`, embedded
with `go:embed`. It exposes:

- `web.Static() http.Handler` — serves `/static/{file}` with
  `Cache-Control: public, max-age=31536000, immutable`. `web.Version` is the
  first 8 hex chars of the SHA-256 over all static files, computed at init;
  templates reference `/static/app.css?v={{.Version}}`. `/static/` is excluded
  from the `no-store` prefixes in `internal/api/middleware.go`. CSP unchanged.
- `web.Templates()` — one `html/template` set. `layout.html` defines the
  console shell; `layout_public.html` the hosted-auth/auth shell;
  `layout_site.html` the website shell. Pages are `{{define "content"}}`
  blocks and are executed by the existing handlers in `internal/api`, which
  keep their routes, auth checks and CSRF handling. Templates parse at init
  (`template.Must`) so a broken template fails startup, not a request.

**Console shell (`layout.html`)**: `<head>` with viewport, `color-scheme:
light dark`, title `"{Page} · Entropix"`; top bar with the Entropix wordmark
(links to `/dashboard`), nav `Dashboard · Mail · Chat · Docs` with
`aria-current="page"`, signed-in email, logout `<form>` with CSRF; `<main>`;
a notice region `<div id="notice" role="status" aria-live="polite">`.

**Public shell (`layout_public.html`)**: no nav, Entropix wordmark, one
centered card (max 28rem), larger base type (16px), mobile-first.

**Routes added**: `GET /` (website, section 7), `GET /static/{file}`.

**`app.js`** exposes one global `um`:

| helper | behaviour |
|---|---|
| `um.api(path, opts)` | JSON in/out; 204 → `null`; 401 → redirect to `/login?next=`; non-2xx → throws `Error(body.error.message \|\| status)` |
| `um.api.raw(path, opts)` | same auth/401 handling, returns the `Response` (attachment downloads) |
| `um.esc(s)` | HTML escape |
| `um.notice(kind, text, {timeout})` | renders into `#notice`; kinds `info/success/error`; error persists until dismissed, others auto-clear after 5 s |
| `um.confirm({title, body, action, danger})` | native `<dialog>`; resolves boolean |
| `um.copy(text, btn)` | clipboard write; flips button label to "Copied" for 1.5 s |
| `um.relTime(iso)` | "just now", "3 min ago", "2 h ago", "yesterday", else short date |
| `um.poll(fn, ms)` | interval that pauses while `document.hidden`, resumes immediately on visibility; returns `stop()` |
| `um.listNav(container)` | ↑/↓/Home/End move focus among `[role=option]` children; Enter/Space activate |

**`app.css` tokens** (light default, dark under `prefers-color-scheme:dark`):
`--bg --surface --surface-2 --border --text --muted --accent --accent-text
--ok --warn --danger --info` plus their `-bg` tints; spacing scale
`--s1..--s8` = 4/8/12/16/24/32/48/64px; type scale 13/14/16/20/24/32; radius
6/10; `:focus-visible` ring `2px solid var(--accent)` with 2px offset. One
font stack for everything: `ui-sans-serif, system-ui, -apple-system, "Segoe
UI", sans-serif`; code `ui-monospace, SFMono-Regular, Menlo, monospace`.

**Components**: `.btn` (`.primary .secondary .danger .ghost`, `.sm`),
`.card`, `.pill` (`.ok .warn .danger .info .muted`), `.field` (label, input,
hint, error), `.table`, `.empty` (icon-less: title, sub, optional action),
`.skeleton`, `.notice`, `.tabs`, `.split` (sidebar + main; sidebar becomes a
slide-over under 48rem; heights use `100dvh`), `dialog` styling.

## 2. Dashboard (`/dashboard`)

- Header: title "Accounts", primary **Connect account** (opens a `<dialog>`
  listing enabled providers from `GET /api/v1/providers`; choosing one calls
  `POST /api/v1/hosted-auth` and navigates to the returned URL).
- In-page `.tabs`: **Accounts · Webhooks · API keys · Settings** (settings =
  change password, redirect domains). Tabs are anchors (`#webhooks`), so deep
  links work and no client router is needed.
- Account row: provider initial, display name (name → email → masked
  identifier), kind badge, **status pill** with human labels mapped as:

  | condition | pill | secondary line |
  |---|---|---|
  | mail, `status=OK` | Connected (ok) | "synced {relTime(last_synced_at)}" or "first sync pending" |
  | mail, `status=CREDENTIALS` | Needs reconnect (danger) | "sign in again to resume" |
  | chat, `connection.state=connected` | Connected (ok) | "since {relTime}; {n} reconnects" when n>0 |
  | chat, `connecting` | Connecting (info) | — |
  | chat, `backoff` | Reconnecting (warn) | "attempt {reconnects}: {last_error}" |
  | chat, `stopped` or `status=CREDENTIALS` | Needs relink (danger) | last_error if any |
  | chat, `error` | Error (danger) | last_error |

  Actions: **Open** (→ `/mail?account_id=` or `/chat?account_id=`),
  **Resync** (mail) / **Reconnect** (chat), **Webhook** (jumps to the
  Webhooks tab filtered to this account), **Disconnect** (`um.confirm`,
  danger).
- After Resync/Reconnect: notice "Resync started" / "Reconnecting…", then
  `um.poll` that account (`GET /api/v1/accounts/{id}`) every 3 s for up to
  60 s, re-rendering the row; stop early when `last_synced_at` changes or
  `connection.state` becomes `connected`. Failure → error notice.
- API keys tab: table (name, prefix, created, last used, Revoke). Create →
  dialog with name; on success the full key is shown once in `<code>` with a
  **Copy** button and "This key won't be shown again". Revoke → confirm.
- Webhooks tab: one table for global and per-account hooks (columns: scope,
  kind, name, target, events, actions). Add/edit dialog: scope (global or an
  account), kind (webhook / discord / telegram; fields switch accordingly),
  name, events (checkbox list from a Go-provided constant list), URL/secret.
  Delete → confirm. No test-delivery button (no API for it).
- Loading: skeleton rows. Empty: `.empty` with the primary action inline
  ("No accounts yet" → Connect account; "No API keys" → Create key; "No
  webhooks" → Add webhook).
- All fetch errors → `um.notice('error')`; field validation errors inline.

## 3. Hosted auth (`/connect/{state}`)

- Public shell, mobile-first. Provider display name comes from the registry;
  the page never hardcodes a vendor.
- Three-step indicator at the top: **Review → {Authorize|Scan} → Done**.
- OAuth providers: "Connect your {Provider} account" with the scopes rendered
  as plain-English bullets (a small Go map from scope → sentence; unknown
  scopes fall back to the raw name), primary **Continue to {Provider}**,
  secondary **Cancel**. Cancel goes to the request's failure redirect if one
  was supplied, else renders the "You can close this page" state.
- WhatsApp: disclosure card (existing copy), consent checkbox with a visible
  focus ring, **Show QR code**. QR pane: the image, a countdown ring driven by
  `expires_in` from `GET /connect/{state}/qr`, instructions "WhatsApp →
  Settings → Linked devices → Link a device", and an `aria-live` status line.
  On expiry the code is refetched automatically (existing rotation); on
  `expired`/`failed` terminal states a **Try again** button re-POSTs consent
  instead of asking for a reload. Polling uses `um.poll` (pauses when hidden,
  stops on terminal states).
- Result pages (`renderMessage` in `handlers_connect.go`) move onto the public
  shell. When `success_redirect_url` is set the server keeps redirecting
  (302) straight to it — that is documented contract for both providers —
  so the success page only renders when there is no redirect: it shows the
  connected identity, "You can return to the app now", and the account id
  behind a `<details>` "Details" with copy. Failure pages show a human title
  and the provider's error text only under "Details".

## 4. Mail (`/mail`) and Chat (`/chat`)

Shared: `.split` layout; the sidebar starts with an account switcher
(`<select>` with a visible label) followed by folders (mail) or chats (chat).
Under 48rem the list is the full screen; opening an item shows the
reader/thread full-screen with a **Back** button; the sidebar is a slide-over
behind a menu button. Lists use `role="listbox"` with `role="option"` rows
that are real `<button>`s; `um.listNav` gives ↑/↓/Enter. Skeletons while
loading; `.empty` states keep today's copy.

Mail specifics: header search with 300 ms debounce and an "Unread only"
toggle; pager shows "1–50" / "51–100" with Older/Newer, Older disabled when
fewer than a page came back; opening a message marks it read via the existing
`PATCH /api/v1/emails/{id}` (`{"unread":false}`) and updates the row. Mail
adds `j/k`. Compose/reply stay out of scope: the viewer remains a viewer.

Chat specifics: connection pill in the header using the section-2 mapping;
composer with optimistic send — the bubble appears immediately marked
"sending", is replaced by the 201 response, or marked "failed" with a
**Retry** link on error; `um.poll` at 5 s pauses when hidden; on refresh the
thread does not scroll if the user has scrolled up more than 80px — a "New
messages ↓" chip appears instead; the error bar clears on the next successful
poll. Reactions/edit/delete/mark-read stay out of scope for this pass.

## 5. Auth (`/login`, `/signup`) and Docs (`/docs`)

Auth: public shell, real `<label>`s, error paragraph `role="alert"`, submit
button disabled after first click with "Signing in…", password visibility
toggle, links to `/docs` and the website. Copy: "Sign in to Entropix".

Docs (`/docs`) become a developer reference, not a restyled page. Same
route, same handler, content regenerated from the same `apiRoutes` table plus
new structured data in `internal/web/docs/`:

- **Layout**: three columns on wide screens — sticky left `<nav
  aria-label="Contents">` with a filter box (`/` focuses it; typing hides
  non-matching entries), main column (`max-width: 72ch`), sticky right rail
  with the current section's code sample. Two columns under 64rem, one under
  48rem with the TOC as a disclosure at the top.
- **Order**: Quickstart (sign up → create key → connect an account → receive
  the first event → send a message, each step with a runnable snippet) →
  Authentication → Hosted auth → Accounts → Mail → Chat → Webhooks & events →
  Errors → Rate limits & idempotency → Self-hosting → `llms.txt`.
- **Endpoint blocks**: every route renders as a block with method chip, path
  with `{params}` highlighted, one-line summary, a parameters table (name,
  in, type, required, description), and a request/response pair. Request
  samples have tabs `curl · Node · Go` sharing the snippet constants from
  section 7; responses are real JSON from the model types. Each block has a
  stable anchor (`#post-api-v1-chats-id-messages`) shown as `#` on hover.
- **Events reference**: one entry per event type with a sample payload,
  when it fires, and which webhook kinds carry it; the Discord/Telegram
  payload shapes are shown alongside the HTTP one.
- **Errors table**: every `error.code` the API emits with HTTP status and
  what to do about it.
- **Copy** on every code block (`um.copy`); method chips AA-compliant (dark
  text on tinted background); "Edit this page" is not added (no public repo
  link in the app config).
- Page uses the console shell when signed in (nav + email), otherwise the
  site shell's wordmark + Sign in, so the docs read the same from the
  website and from the console. Copy says **Entropix** throughout.
- Parameter/response data is declared in Go (`docs.Endpoint{Method, Path,
  Summary, Params, Request, Response, Anchor}`) next to `apiRoutes`; a test
  asserts every route in `apiRoutes` has a docs entry, so a new endpoint
  cannot ship undocumented.

## 6. Error handling, security, testing

- No `alert()`/`confirm()` remain; all failures go through `um.notice` or
  `um.confirm`. Text inserted into the DOM goes through `um.esc` or
  `textContent`, including ids in `data-` attributes and hrefs.
- CSP unchanged. Static assets are same-origin; no external fonts, scripts or
  images. Logout and every mutating form keep CSRF.
- Go tests (`internal/web`, `internal/api`): static handler sets immutable
  cache and correct content types; `web.Version` is stable across two calls;
  every page route renders 200 (or 302 to login) with the layout markers
  (`<nav`, `#notice`) and the CSP header; `GET /` renders the website with the
  enabled providers; `/connect/{state}` renders the provider's display name
  and no vendor name for a fake provider; docs still lists every `apiRoutes`
  entry. Existing handler tests updated where they assert on old markup.
- No JS test harness is added. `docs/ui-manual-checklist.md` lists what to
  verify by hand per page: keyboard-only navigation, 360px/768px/1280px
  widths, light/dark, every loading/empty/error state, copy buttons, chat
  optimistic send and failure, QR countdown and Try again.
- Rollout: six commits, each leaving the app fully working — (1) assets,
  shells, `/static`, `GET /` as a minimal website placeholder; (2) dashboard;
  (3) hosted auth; (4) mail + chat; (5) auth + docs reference; (6) website
  content. Each step replaces one Go string template with an embedded one.

## 7. Entropix website (`GET /`)

Public marketing page for the product, dark-first, developer-oriented,
sharing tokens with the console so signup → dashboard feels continuous.
Served by a new `handleSite` handler using `layout_site.html`.

Sections, in order, with an anchored top nav (**Features · Providers · Docs ·
Sign in** or **Dashboard** when a session cookie is present, plus a **Get API
key** primary CTA → `/signup`):

1. **Hero** — headline "One API for mail and chat.", two-line sub ("Connect
   Outlook and WhatsApp accounts with hosted auth, receive every message on a
   webhook, send with one endpoint. Self-host in a single binary."), CTAs
   **Get started** → `/signup` and **Read the docs** → `/docs`, and a code
   block with tabs `curl · Node · Go` showing "send a message" in ≤ 10 lines.
2. **How it works** — three numbered steps, each with a short snippet:
   connect via `POST /api/v1/hosted-auth`; receive `chat_received` /
   `mail_received` on your webhook (sample payload); send via
   `POST /api/v1/chats/{id}/messages` with an `Idempotency-Key`.
3. **Providers** — one card per provider from the registry (Outlook,
   WhatsApp) listing supported capabilities (read, send, webhooks, reactions,
   edit/delete where applicable) and a muted "More providers coming" card.
   WhatsApp's card carries the linked-device / ban-risk note in one sentence.
4. **Built for developers** — six cards: webhooks (HTTP, Discord, Telegram),
   idempotency keys, hosted auth, `llms.txt`, self-hostable (SQLite or
   Postgres), API keys with rotation.
5. **Events** — chips for every event type, each linking to its docs anchor.
6. **Footer** — Docs, llms.txt, Sign in, "Free while in beta · self-host any
   time". No pricing, analytics, cookie banner or external assets.

Snippets are Go constants shared with `/docs` (`internal/web/snippets.go`) so
the site and the docs cannot drift. Feature and provider lists are Go data
passed to the template; the providers section reflects what the registry has
enabled.

Branding: `<title>`s, wordmark, login/signup card, docs header and
`/llms.txt` say **Entropix**. Hosted-auth pages say "Entropix is connecting
your {Provider} account" (the developer's app name is not available in the
hosted-auth request today and is not added).

## Out of scope

Mail compose/reply, chat reactions/edit/delete UI, a frontend build
toolchain, pricing/billing, renaming the module/binary/README, a JS test
runner, analytics.
