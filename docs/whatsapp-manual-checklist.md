# WhatsApp manual checklist

Everything below needs a real phone with WhatsApp installed, and cannot be
automated: it exercises Meta's actual servers, a real linked-device pairing,
and (deliberately) a real unlink from the phone's own UI. Run it once after
any change that touches `internal/provider/whatsapp`, `internal/chatsync`, or
the connect/link handlers, before shipping.

Do this against a throwaway or secondary phone number where possible — not a
number you depend on — and re-read the disclosure in the README ("Setup" §6)
and `/docs` §7 before starting: an account that automates WhatsApp can be
banned by Meta, and this checklist involves sending several messages in a row
against a real number.

## Setup

```bash
set -a && source .env && set +a
WHATSAPP_ENABLED=true go run ./cmd/server
```

Confirm the startup line shows `whatsapp=true`. Sign up a developer at
`/signup`, create an API key on the dashboard, and export it:

```bash
export API_KEY=um_...
export BASE=http://localhost:8080
```

## 1. Link a real number

- [ ] `curl -s -X POST $BASE/api/v1/hosted-auth -H "Authorization: Bearer $API_KEY" -H 'Content-Type: application/json' -d '{"provider":"WHATSAPP","notify_url":"..."}' | jq -r .url`
- [ ] Open the URL in a browser. The disclosure text renders (linked-device
      model, ban risk, "you can disconnect at any time") — read it as the end
      user would.
- [ ] Tick the consent checkbox and click **Show QR code**. A QR image
      appears within a few seconds.
- [ ] On the phone: **WhatsApp → Settings → Linked devices → Link a device**,
      scan the code.
- [ ] The page shows "Connected." and redirects (or shows the account id if
      no `success_redirect_url` was set). `notify_url`, if set, received
      `{"status":"CREATED","account_id":"acc_…","identifier":"+E164…","provider":"WHATSAPP"}`.
- [ ] `GET /api/v1/accounts/{id}` shows `status: "OK"` and
      `connection: {"state":"connected", ...}`.
- [ ] On the phone, confirm the linked device appears under **Linked
      devices** with the name from `WHATSAPP_DEVICE_NAME`.

Export the account id for the rest of this checklist:

```bash
export ACC=acc_...
```

## 2. Receive a message

- [ ] From a **different** phone number, send a WhatsApp message to the
      linked number.
- [ ] `GET $BASE/api/v1/chats?account_id=$ACC` lists the chat within a few
      seconds, `unread_count` incremented.
- [ ] `GET $BASE/api/v1/chats/{chat_id}/messages?account_id=$ACC` shows the
      message with the correct `text`, `sender`, and `is_from_me: false`.
- [ ] If a developer-wide or per-account webhook is registered, it received a
      `chat_received` event carrying the same message.

## 3. Send a message via the API

```bash
curl -s -X POST "$BASE/api/v1/chats/$CHAT_ID/messages?account_id=$ACC" \
  -H "Authorization: Bearer $API_KEY" -H 'Content-Type: application/json' \
  -H "Idempotency-Key: $(uuidgen)" -d '{"text":"manual checklist ping"}'
```

- [ ] `201` with a `ChatMessage`, `status: "sent"` (or `"delivered"` shortly
      after).
- [ ] The message actually arrives on the other phone.
- [ ] `chat_sent` fires on any registered webhook.
- [ ] Repeat the exact same request with the **same** `Idempotency-Key` and
      body: the second call returns the same response, does **not** send a
      second WhatsApp message, and the other phone shows only one new
      message.

## 4. React, edit, delete

- [ ] React to the message just sent:
      `PUT $BASE/api/v1/chats/$CHAT_ID/messages/$MID/reaction` body
      `{"emoji":"👍"}` → `204`; the reaction shows up on the phone that sent
      it (yours) as a reaction from you, and `chat_reaction` fires.
- [ ] Edit it: `PATCH .../messages/$MID` body `{"text":"edited"}` → `200`; the
      other phone shows the edited text; `chat_updated` fires.
- [ ] Delete it: `DELETE .../messages/$MID` → `204`; the message shows
      "This message was deleted" on both phones; `chat_deleted` fires.
- [ ] Attempt to edit or delete a message the **other** phone sent (one from
      §2): both return `403 not_own_message`, and nothing changes on WhatsApp.

## 5. Unlink from the phone

- [ ] On the phone: **Settings → Linked devices**, tap the linked device,
      **Log out**.
- [ ] Within roughly a poll cycle, `GET /api/v1/accounts/{id}` shows
      `status: "CREDENTIALS"`.
- [ ] A registered webhook received `account_status` with the new status —
      unprompted by any request you made.
- [ ] Any further chat call against this account id (e.g. list chats) either
      still serves the local mirror (reads) or fails cleanly on writes; there
      is no way to relink the same account id — confirm a fresh
      `hosted-auth` call is required to pair the number again.

## 6. Restart and auto-reconnect

- [ ] With the account still `status: "OK"` (relink it first if step 5 left
      it `CREDENTIALS`), stop the server (`Ctrl-C`, clean shutdown) and start
      it again with the same `DB_PATH`.
- [ ] Without any manual action, `GET /api/v1/accounts/{id}` returns to
      `connection: {"state":"connected"}` within a minute or so — the runtime
      reattaches every stored chat account at startup.
- [ ] Send another message through the API (§3) and confirm it still arrives.

## 7. Delete the account unlinks the device

- [ ] `DELETE $BASE/api/v1/accounts/$ACC` → `204`.
- [ ] On the phone, the entry under **Linked devices** for this connection is
      gone (whatsmeow logged the device out server-side as part of the
      delete) — confirm this rather than assuming it from the API response
      alone.
- [ ] `GET /api/v1/accounts/$ACC` now returns `404`.
- [ ] `GET /api/v1/chats?account_id=$ACC` also `404`s (or the account no
      longer resolves) — the local mirror was removed with the account.

## Afterwards

- [ ] Grep the server log for the number you tested with, and for any
      message text sent above — neither should appear (see README
      "Logging"). If either does, that is a regression worth its own bug
      report, not something to wave off as this checklist's fault.
- [ ] Turn `WHATSAPP_ENABLED` back off in any environment where WhatsApp
      support isn't intentionally being kept on, and consider logging the
      test number out of "Linked devices" from the phone side too if you
      don't intend to keep the connection around.
