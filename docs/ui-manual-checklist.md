# UI manual checklist

What the automated tests cannot see: focus order, contrast, reflow, and the
states that only appear while something is loading, empty, or broken. Run
this before shipping anything that touches `internal/web/templates`,
`internal/web/static`, or a handler that renders a page.

Budget: about 30 minutes for a full pass, 10 for the section you changed.

## Setup

```bash
set -a && source .env && set +a
go run ./cmd/server
export BASE=http://localhost:8080
```

Sign up a developer at `/signup`, connect one Outlook account and (if
`WHATSAPP_ENABLED=true`) one WhatsApp account, so the pages have real data.
Keep a second browser profile signed out, for the public pages.

Widths below mean the viewport width in devtools' responsive mode:
**360** (small phone), **768** (tablet / split screen), **1280** (laptop).
Theme means the OS/browser `prefers-color-scheme` setting.

## 0. Cross-cutting — do this on every page you touch

- [ ] **Keyboard only.** Unplug the mouse. Tab through the whole page: focus
      is always visible, order matches reading order, no focus trap, nothing
      reachable only by pointer. `Enter`/`Space` activate buttons and links.
      `Esc` closes any dialog and returns focus to whatever opened it.
- [ ] **360 wide.** No horizontal page scroll. Wide things (tables, code
      blocks, QR pane) scroll inside their own box, not the body.
- [ ] **768 wide.** Multi-column layouts collapse without overlap; nothing is
      clipped behind a sticky header.
- [ ] **1280 wide.** Line lengths stay readable (~65–75 characters), content
      is not stranded in one narrow column.
- [ ] **Light theme.** Text, muted text, borders, and status chips are all
      legible; nothing is white-on-white.
- [ ] **Dark theme.** Same, inverted. Check code blocks, method chips, alerts
      and disabled buttons specifically — they are the usual casualties.
- [ ] **Zoom to 200%** at 1280. Nothing overlaps or disappears.
- [ ] **Copy buttons.** Every `data-copy` button copies the right text, shows
      its confirmation state, and reverts. Paste somewhere to confirm.
- [ ] No console errors on load or on the page's main interaction.

## 1. Website — `/`

- [ ] Signed out: hero, nav, and every anchor (`#how`, `#providers`,
      `#features`, `#events`) scroll to the right section; the back button
      returns you.
- [ ] Signed in: the nav shows the account affordance instead of Sign in.
- [ ] Hero snippet tabs (curl / Node / Go) switch panes by click *and* by
      keyboard; the copy button copies the visible pane only.
- [ ] Provider cards list every registered provider; the "more coming" card
      is visibly muted, not mistakable for a real one.
- [ ] Event pills link into `/docs#event-…` and land on the right block.
- [ ] Footer links (Docs, llms.txt, Sign in) all resolve — no 404s.

## 2. Login and signup — `/login`, `/signup`

- [ ] Autofocus lands on Email. Tab reaches password, the Show toggle, and
      Submit in that order.
- [ ] Show/hide password toggles the field type and its `aria-pressed`.
- [ ] Submit disables the button and changes its label — double-submitting is
      impossible.
- [ ] Wrong password: the error renders in the alert region, is announced by
      a screen reader, and the email field keeps its value.
- [ ] Signup with a <10-character password: the hint and the browser's own
      validation both make the rule obvious before submitting.
- [ ] Signup with an already-registered email surfaces a human message, not a
      raw error code.
- [ ] The cross-link (login ↔ signup) and the Docs link work.

## 3. Dashboard — `/dashboard`

- [ ] **Empty state**, with a fresh developer: no accounts and no keys still
      renders a page that tells you what to do next, not a blank panel.
- [ ] **Loading state**: throttle to Slow 3G and reload — skeletons or
      spinners appear rather than a flash of empty content.
- [ ] Create an API key: the full key is shown once, is copyable, and the
      dialog warns it will not be shown again. Reload — it is gone.
- [ ] Revoke a key: confirmation is required; the row shows revoked.
- [ ] Connect dialog: provider list matches `/api/v1/providers`; submitting
      opens the hosted-auth flow.
- [ ] An account in `CREDENTIALS` status is visibly distinct and offers the
      reconnect action. (Force one by revoking consent provider-side, or edit
      the row in the DB.)
- [ ] Redirect-domain settings: saving a bad entry (scheme, port, IP) shows
      the server's message inline; saving a good one persists across reload.
- [ ] **Error state**: stop the server, then trigger any dashboard action —
      the failure is reported in the UI, not swallowed.

## 4. Connect, OAuth — `/connect/{state}`

- [ ] Step indicator shows Review as current, with Authorize and Done ahead.
- [ ] The copy names the provider from the registry (e.g. "Outlook"), never
      hardcodes a vendor the state does not point at.
- [ ] The scope list reads as sentences an end user understands, not raw
      scope strings.
- [ ] Continue goes to the provider's own sign-in page.
- [ ] Cancel (or the "you can close this page" fallback) is present.
- [ ] An expired or unknown `state` shows the styled result page, not a stack
      trace or a bare 404.

## 5. Connect, QR — `/connect/{state}` for a linkable provider

- [ ] The disclosure renders in full before anything else; **Show QR code**
      is disabled until the consent box is ticked.
- [ ] Ticking consent by keyboard enables the button.
- [ ] After consent: a QR image appears within a few seconds and the
      countdown ring starts, visibly draining.
- [ ] `#status` updates are inside an `aria-live` region — a screen reader
      announces "waiting", "connected", "expired" without needing focus.
- [ ] **Let the code expire** (do not scan). The ring reaches zero, the
      status says expired, polling stops (check the network tab), and
      **Try again** appears.
- [ ] Try again re-runs consent and shows a fresh code with a fresh
      countdown.
- [ ] Scan for real: the step indicator advances to Done and the page
      redirects when the state carries a success URL.
- [ ] Stop the server mid-poll: the page reports a failure rather than
      spinning forever.

## 6. Result pages

- [ ] Success: title, body naming the connected identity, the account id with
      a working copy button, and a Continue button when a redirect was
      configured (otherwise the "you can return to the app" line).
- [ ] Failure: human title, and the provider's raw error tucked inside
      `<details>` rather than shown by default.
- [ ] Both render correctly at 360 and in dark theme.

## 7. Mail — `/mail`

- [ ] **Empty**: an account with no messages (or a filter matching none) says
      so; it does not look like a loading failure.
- [ ] **Loading**: throttled reload shows a placeholder, and switching
      folders shows one too.
- [ ] Message list → message body: keyboard navigation between list and
      reading pane works, and the selected row is marked for assistive tech.
- [ ] Folder filter, unread filter and search each change the list and are
      reflected in the URL (so the back button works).
- [ ] A message with attachments shows them; the download link works.
- [ ] Long subjects, long sender names and a message with no subject all
      render without breaking the layout.
- [ ] **Error**: stop the server and change folder — the failure is visible
      and recoverable (a retry affordance, not a dead page).

## 8. Chat — `/chat`

- [ ] **Empty**: no chats, and a chat with no messages, both read as
      intentional.
- [ ] **Optimistic send**: type and send — the message appears immediately in
      a pending state, then settles to sent. The composer clears and keeps
      focus.
- [ ] **Send failure**: stop the server *after* typing, then send. The
      pending message is marked failed (or removed) with a visible reason and
      a way to retry — it must never sit silently in "sending" forever.
      Restart the server and confirm a retry succeeds.
- [ ] Send with `Enter`; insert a newline with `Shift+Enter`.
- [ ] Reactions, edit and delete are offered only on your own messages;
      someone else's message offers only what is legal (403 `not_own_message`
      must be unreachable from the UI).
- [ ] Incoming message while the pane is open appends without stealing focus
      or losing your scroll position mid-history.
- [ ] Scrolling up loads older messages (the `before` pager) without a jump.
- [ ] Very long messages, emoji and RTL text render without breaking the
      bubble layout.

## 9. Docs — `/docs`

- [ ] Pressing `/` anywhere on the page focuses the contents filter (and does
      not type "/" into it).
- [ ] Typing in the filter narrows the contents list; clearing it restores
      every entry.
- [ ] Every contents entry scrolls to its section, and the current section is
      marked as you scroll.
- [ ] Every endpoint block has a `#` anchor: click it, copy the URL, open it
      in a new tab, and land on that endpoint.
- [ ] Method chips (GET/POST/PATCH/PUT/DELETE) are readable in both themes.
- [ ] Snippet tabs (curl / Node / Go) switch by click and by keyboard; the
      copy button copies the visible pane.
- [ ] `#events` and `#errors` are reachable from the contents and from an
      external link.
- [ ] At 768 the right rail disappears without losing content; at 360 the
      contents collapse into a disclosure that opens and closes.
- [ ] Signed out, the page renders fully — nothing here requires an account.

## Reporting

Note failures with page, width, theme and steps. Anything that fails the
keyboard or contrast checks is a blocker, not a polish item.
