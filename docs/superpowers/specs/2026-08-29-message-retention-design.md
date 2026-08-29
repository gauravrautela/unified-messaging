# Message content retention — design

**Status:** approved in conversation, 2026-08-29
**Problem:** the local mirror stores full message content (`emails.body`, `emails.subject`, `chat_messages.text`) with no bound. That is data-at-rest liability for a tenant's end users, unbounded DB growth, and — for WhatsApp — retention the provider's terms do not invite.
**Shape:** message content is evicted when it has been forwarded to every subscribing webhook, or when it ages past a per-tenant maximum, whichever comes first. The message row survives with its identifiers, timestamps and flags so sync stays correct. Eviction is opt-in per developer and off by default.

**Out of scope:** contact-level data in `attendees` (phone, name) and `chats.name` — that is address-book state, not message state, and a chat is unusable without it. Attachment *bytes* were never stored (they stream from the provider at `handlers_mail.go:463`), so there is nothing to evict. Encryption at rest for the mirror.

---

## 1. What the code already gives us

Three facts from the existing implementation shaped the design and are worth stating, because each one removed a component from an earlier draft.

**Retries carry their own copy.** `webhook_deliveries.payload` is the fully serialized event, body included (`store/aux.go:279`), and `retryDue` replays it with `json.Unmarshal(dl.Payload, &ev)` (`events.go:425`) rather than re-reading the mirror. **Evicting `emails.body` cannot break a pending retry.** No delivery-preservation machinery is needed.

**The delivery table is self-cleaning.** A row is created only after the first attempt *fails* (`events.go:277`) and deleted on eventual success (`events.go:377`). Content lingers there only for retrying and dead deliveries.

**`deliver()` already knows what a counter would reconstruct.** It fans out to the matching hooks and waits on them (`events.go:237`). How many hooks matched, and whether any of them failed, are in-process facts at the end of that function. An earlier draft added `forwards_pending`/`forwarded` columns and a `resource_id` join on `webhook_deliveries` to recover that state after the fact; all of it is unnecessary. §4 uses the in-process signal instead.

---

## 2. Schema

Added through the existing additive `sqliteMigrations` list in `store/schema.go`, so an upgrade is a no-op.

**`developers`** — one column:

```sql
ALTER TABLE developers ADD COLUMN retention_max_age_secs BIGINT NOT NULL DEFAULT 0
```

`0` means keep forever: today's behaviour, and the default every existing tenant lands on. Any value `> 0` turns on **both** eviction triggers — delivery (§4) and max-age (§5). One knob, stated to the developer as *"content is dropped as soon as it has been delivered to your webhooks, and in no case kept longer than N."*

**`emails` and `chat_messages`** — two columns each:

```sql
ALTER TABLE emails        ADD COLUMN stored_at BIGINT NOT NULL DEFAULT 0
ALTER TABLE emails        ADD COLUMN content_evicted_at BIGINT
ALTER TABLE chat_messages ADD COLUMN stored_at BIGINT NOT NULL DEFAULT 0
ALTER TABLE chat_messages ADD COLUMN content_evicted_at BIGINT
```

`stored_at` is ingestion time and cannot be folded into the existing timestamps: `date` and `sent_at` are *provider* timestamps, so a bounded backfill of an old mailbox would evict every message on the first sweep and present the developer with an empty-looking mirror. Set in `UpsertEmail`/`UpsertChatMessage` when the row is first inserted; left alone on update.

The migration follows each `ADD COLUMN` with a one-time `UPDATE ... SET stored_at = <migration clock> WHERE stored_at = 0`, so pre-existing rows start their clock at upgrade rather than at the epoch.

`content_evicted_at` is NULL until evicted. It is the flag the API reports on (§6) and the two guards in §7 test.

---

## 3. What eviction blanks

Eviction is an `UPDATE`. It removes content *and participants*, keeping only what sync and threading need.

**`emails`** — blanked to `''`: `subject`, `snippet`, `body`, `body_type`, `attachments_json` (→ `'[]'`), `from_name`, `from_email`, `to_json`, `cc_json`, `bcc_json`, `reply_to_json` (→ `'[]'`). Sender and recipient addresses are personal data; retaining them indefinitely would make "we do not store your messages" untrue.

**Kept:** `account_id`, `id`, `thread_id`, `folder_id`, `date`, `read`, `flagged`, `draft`, `has_attachments`, `internet_message_id`. `has_attachments` stays truthful even with the names gone.

**`chat_messages`** — blanked: `text` (→ `''`). **Kept:** everything else, including `sender_id`.

The mail/chat asymmetry is deliberate and should be documented for developers rather than papered over: a chat message's participants are not on the message row, they are in `attendees`, which eviction does not touch (see Out of scope). So chat eviction removes message content but not who was in the conversation. Mail eviction removes both.

**Both:** `content_evicted_at` set to the eviction clock.

Two invariants this preserves, and the reason the row is blanked rather than deleted:

- `store.EmailExists` (`syncer.go:365`) still answers correctly. It is the only thing stopping a resync from re-firing `mail_received` for the entire mailbox — at the same endpoint that was just sent those messages.
- A late WhatsApp reaction or receipt still finds its message via `GetChatMessage` (`sink.go:106`, `:169`) instead of being dropped as unknown.

---

## 4. Trigger 1 — delivered

In `Dispatcher.deliver()`, after the fan-out completes:

- a local `matched int` counts hooks that passed the `subscribes(h, ev.Type)` filter;
- a local `failed atomic.Bool` is set in the branch that calls `d.enqueue(dl, err)`;
- the deferred `wg.Wait()` becomes an explicit `wg.Wait()` at the end of the function, with the defer kept for the early-return paths;
- if `matched > 0 && !failed.Load()` and the event names a message, evict it.

The developer's `retention_max_age_secs` is read from `h.DeveloperID`, already in hand (`events.go:375`); zero means no eviction.

Which events name a message: `mail_received`/`mail_sent` via `ev.Email.ID`, `chat_received`/`chat_sent` via `ev.Message.ID`. `chat_updated`, `chat_reaction`, `chat_deleted` and `account_status` do not trigger eviction — they reference a message whose own event already governed it.

Three cases, all handled by that one condition:

| Situation | Result |
|---|---|
| Every hook accepted on the first attempt | Evicted within milliseconds. This is effectively all real traffic. |
| Any hook enqueued a retry | Not evicted. Falls through to max-age — which is exactly the case where the content may still be needed. |
| No hook matched (none configured, or all filtered out) | Not evicted. Max-age only. |

The third row is the zero-webhook trap: a developer with no webhooks has never forwarded anything, and inferring "delivered" from the *absence* of a delivery row would destroy content they never received — irreversibly, for WhatsApp. Deciding at fan-out time rather than reconstructing later avoids it without a column.

Eviction is best-effort and never fails the delivery: an error is logged and the sweep picks the message up later.

---

## 5. Trigger 2 — max age

The hourly loop in `cmd/server/main.go:83` already runs `PurgeExpiredOAuthStates` and `PurgeDeadDeliveries`. It gains one more call, no new goroutine:

```go
n, err := db.EvictExpiredContent(time.Now())
```

which, for every developer with `retention_max_age_secs > 0`, blanks (per §3) both message tables where `content_evicted_at IS NULL AND now - stored_at > retention_max_age_secs`, joined to the developer through `accounts.developer_id`.

Hourly granularity is adequate because §4 covers the fast path; max-age is a backstop for undelivered and unforwarded content, not the mechanism anyone measures. The sweep logs counts only — never content, matching the existing discipline at `aux.go:274` ("payload is a byte count, never content").

**Dead deliveries are capped too.** A dead delivery holds a full serialized body and is bounded only by the global `DELIVERY_RETENTION_DAYS` (default 7, `config.go:123`). For a tenant whose policy says one hour, a week-old dead delivery makes that policy false. `PurgeDeadDeliveries` takes a per-developer cutoff of `min(global, tenant max_age)`, joining `webhook_deliveries → webhooks → developers`.

---

## 6. API surface

**`model.Email` and `model.ChatMessage`** gain:

```go
ContentAvailable bool `json:"content_available"`
```

Always emitted, deliberately not `omitempty`: an absent field cannot be distinguished from an older server. It appears on both the full and list forms — list responses already omit `body` (`handlers_llms.go:86`), so without it a client cannot tell "not included here" from "destroyed."

An evicted email returns its envelope with `subject`, `snippet`, `body`, `body_plain`, `from`, `to`, `cc`, `bcc`, `attachments` empty and `content_available: false`.

`emailSelect` (`store/store.go:595`) and the chat message select gain `content_evicted_at`; `scanEmail` and its chat counterpart set `ContentAvailable` from it.

**Settings**, following the shape of `PUT /api/v1/me/redirect-domains`:

- `GET /api/v1/me` includes `retention_max_age_secs`.
- `PUT /api/v1/me/retention` sets it. Rejects negatives; `0` disables.
- The dashboard gets a retention field with copy that states plainly that eviction is permanent and that for WhatsApp there is no way to recover the content, because there is no history API to re-fetch from.

**`/docs` and `/llms.txt`** describe the field and the two triggers.

**Two consequences that must be documented rather than fixed:**

*Search stops matching evicted mail.* `ListEmails` matches `Search` against `subject`, `snippet` and `from_email` (`store/store.go:537`) — eviction blanks all three, so an evicted message can never be a search hit. This is correct behaviour (the text is gone; there is nothing to match) but it is surprising, and a developer who enables retention will see their search results shrink over time. It belongs in the docs next to the knob, not in a footnote.

*The mail and chat viewers need an evicted state.* They render whatever the API returns, so without a change an evicted message shows as a blank message with no sender — indistinguishable from a bug. They read `content_available` and render an explicit "content removed by your retention policy" state instead.

---

## 7. Two guards against content coming back

Eviction is not idempotent against the write paths, which will happily repopulate a blanked row. Both cases are one `if`, and both are silent leaks if missed.

**Attachment cache-fill** — `handlers_mail.go:251-258` lazily fills `attachments_json` from the provider on `GET /emails/{id}` when it is empty. On an evicted email that re-creates the filenames at rest, and unlike an edit there is no subsequent event to trigger re-eviction, so they persist until max-age. Skip the cache-fill when `content_evicted_at` is set. The two attachment *endpoints* are unaffected — `handleListAttachments` and `handleDownloadAttachment` go live to the provider (`handlers_mail.go:446`, `:463`) and never read the column.

**WhatsApp edits** — `sink.go:169` suppresses replayed edits with `prev.EditedAt != nil && prev.Text == text`. On an evicted row `prev.Text` is `''`, so a replay no longer matches, re-applies, and re-emits a duplicate `chat_updated` — a bug eviction would introduce. The edit path returns early when `content_evicted_at` is set, logging `decision=content-evicted`. This also removes the repopulate-then-re-evict cycle that storing the new text would create.

The cost is that a genuine late edit to an evicted message is dropped rather than forwarded. WhatsApp's edit window is 15 minutes, so this cannot fire in practice for any max-age above that, and the content it would carry is content the tenant's policy has already said to discard. Reactions are unaffected: `ApplyReaction` writes `reactions_json`, not `text`.

---

## 8. Testing

- **Store:** eviction blanks exactly the columns in §3 and leaves the rest intact; is idempotent; is a no-op when `retention_max_age_secs = 0`.
- **`deliver()` trigger:** all hooks succeed first try → evicted; one hook enqueues a retry → not evicted; zero hooks match → not evicted; `chat_reaction` does not evict its target.
- **Sweep:** a message past max-age with no webhook configured is evicted; one inside the window is not; the developer join scopes it to the right tenant.
- **Guards:** `GET` on an evicted email does not refill `attachments_json`; a replayed edit on an evicted chat message is suppressed and emits no second `chat_updated`.
- **Sync invariants — the regressions that matter most:** `EmailExists` returns true for an evicted email, and a resync after eviction fires no `mail_received`; `GetChatMessage` still resolves an evicted message so a late reaction is applied.
- **Dead deliveries:** purged at the tenant cutoff when it is shorter than the global one.
- **Migration:** existing rows land at `retention_max_age_secs = 0` with `stored_at` stamped at the migration clock — an upgrade evicts nothing.
- **API:** evicted email serializes with `content_available: false` and empty participants; a non-evicted one with `true`.
- **Search:** an evicted email is not returned for a search that matched its subject before eviction — asserted so the behaviour change is pinned rather than discovered.

---

## 9. Rollout

Additive migration, `0` default, no behaviour change on deploy. `stored_at` is maintained for every tenant from the first boot regardless of setting, so a developer switching retention on has accurate ages immediately rather than a cold start in which everything looks freshly stored.
