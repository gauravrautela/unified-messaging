# Delivery targets — manual checklist

Needs: a running server, one connected account (mail or WhatsApp), a Discord
channel you administer, a Telegram group with a bot you created.

Run the server with `DEBUG=1` for this checklist: the `delivery attempt` /
`delivery response` lines step 2 looks for, and the request-body line step 7
greps, are DEBUG-level and absent otherwise.

1. Discord: Server settings → Integrations → Webhooks → New webhook → Copy URL.
   `POST /api/v1/accounts/{id}/webhooks {"kind":"discord","url":"<url>"}` → 201, `kind: discord`.
2. Trigger an event (send a WhatsApp message to the linked number / send yourself a mail).
   Expect the notification in the Discord channel within seconds; server log shows
   `delivery attempt … kind=discord target=https://discord.com/api/webhooks/<id>/•••`.
3. Telegram: @BotFather → /newbot → copy the token; add the bot to a group; post a message in the
   group; `curl https://api.telegram.org/bot<token>/getUpdates` → note `chat.id`.
   `POST /api/v1/accounts/{id}/webhooks {"kind":"telegram","bot_token":"<token>","chat_id":"<id>"}` → 201,
   response has `telegram.chat_id` and no token.
4. Trigger an event → notification in the Telegram group (bold sender, escaped text).
5. Wrong chat id → `POST` answers 400 with Telegram's "chat not found".
6. Point a Discord hook at a deleted Discord webhook → `GET /api/v1/webhooks/{id}/deliveries` shows
   attempts with `last_error: discord: status 404 …` and no token in the URL.
7. With `DEBUG=1` on for every step above: `grep -c '<bot token>' server.log` → 0, and every
   `api/webhooks/` line — the `delivery attempt` target, the `webhook delivery failed` error and
   the DEBUG `request body` line from the create call — ends in `/•••`, never the token.
8. Dashboard: the Set-webhook form switches fields per kind; the card shows the kind badge, and a
   discord hook's URL is displayed as `…/api/webhooks/<id>/•••`.
