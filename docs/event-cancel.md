# Cancelling an Event

ADR 0066 and its 2026-10-19 addendum (e-skylab), ticket 04. A cancelled Event stays visible with `status: "cancelled"`; nobody new registers or checks in and no certificate is issued, but **nothing is deleted or closed for good**: the cancellation can always be undone (`POST …/uncancel`, below).

## `POST /v1/events/{id}/cancel`

Body (optional, both fields too): `{"reason": "Hava koşulları nedeniyle", "notifyTicketHolders": true}`.

- `reason`: a short public text, trimmed, at most 500 characters (`400` beyond). Answered as `cancelReason`.
- `notifyTicketHolders`: whether core mails the Ticket holders (the panel's "Bilet sahiplerine bildir" box, ticked by default). `true` queues the cancellation mail in the same write and answers `cancelNotifiedAt`; left out or `false`, nothing is mailed (an older client starts no mail) and the organizer announces it through "Katılımcılara mail". See **Mails** below.

| Event | Answer |
|---|---|
| published, not ended (an undated one never ends) | `200`, the Event: `status: "cancelled"`, `active: false`, `cancelledAt`, `cancelReason` |
| cancelled already | `200`, the Event as it is (the first reason stays; edit it with `PATCH`, below) |
| a draft | `409`, `code: "event_not_published"` (a draft is deleted, not cancelled); `404` to who may not see it |
| ended | `409`, `code: "event_ended"`: cancelling it would blur its certificate history |

Who: whoever may edit the Event (`authz.Update`); DK and anyone else `403`. The write is one `UPDATE … WHERE status = 'published'` and not ended; a cancellation racing a change is decided again on what the Event is now. It writes the legacy `events.active` column `false`, so a release rolled back to hides the cancelled Event.

## What a cancelled Event does

| | |
|---|---|
| Reads | everyone sees it (`GET /v1/events/{id}`, the signed-in list, Tickets' summaries) with `status: "cancelled"` and `active: false`; it is not in the signed-out list, `?active=true` or `/v1/events/active` (it is not active); `/v1/door/events` leaves it out |
| `applications/me`, `applications/guest`, `applications/users/{userId}` (the panel too) | `409`, `code: "event_cancelled"` |
| Forms' report | no Ticket; `200` with `outcome: "event_cancelled"` when every Event of the form is cancelled ([form-response-tickets.md](form-response-tickets.md)) |
| `check-in`, `check-in/me`, `check-in/guest`, `check-in/resolve`, `door-qr` | `403`, `code: "session_closed"`, `reason: "event_cancelled"`; the site and the door apps show their "session closed" message unchanged |
| Certificates | finalize and issuing by hand `409 event_cancelled`; certificates issued before stay valid; jobs queued before wait untouched (the issuing worker does not take them; one claimed as the Event was cancelled is left to its lease) and go on when the cancellation is undone |
| Competitions | cancelled with the Event, their earlier status kept ([competitions.md](competitions.md)) |
| Tickets, applies, check-ins, door staff, Competitors | stay as they are; nothing is deleted or changes status |
| Form short links | keep working: the panel offers "SkyForms'ta formu kapat"; nothing closes the form by itself |
| Dashboard | not counted as upcoming or live |

**Editing the reason:** `PATCH /v1/events/{id}` with `cancelReason` (trimmed, at most 500 characters; `null` or `""` clears it) while the Event is cancelled. On an Event that is not cancelled the field is ignored, as `status` and `active` are; the write applies it only while the Event is cancelled, in the same `UPDATE`.

Publishing or unpublishing a cancelled Event is `409 event_cancelled`, as before. Deleting it is ticket 05's rule.

## `POST /v1/events/{id}/uncancel`

Undoes the cancellation, always (ADR 0066 addendum 2026-10-19, decision 2; ticket 15). Body (optional): `{"notifyTicketHolders": true}`, whether core mails who got the cancellation mail that the Event is back (the panel offers "Katılımcılara tekrar bildir", ticked by default, only while `cancelNotifiedAt` is set). Without a cancellation mail it does nothing; the undo always clears `cancelNotifiedAt`.

| Event | Answer |
|---|---|
| cancelled | `200`, the Event published again; an ended one too (it shows as ended, `active: false`) |
| published | `200`, the Event as it is |
| a draft | `409`, `code: "event_not_cancelled"`; `404` to who may not see it |

Who: whoever may edit the Event (`authz.Update`); DK and anyone else `403`. One `UPDATE … WHERE status = 'cancelled'`: `cancelledAt`, the actor and `cancelReason` are cleared (an old client does not show a reason on a published Event), `publishedAt` stays, the legacy `events.active` column gets the derived value. In the same transaction the Competitions the cancellation cancelled go back to their earlier status; one cancelled on its own stays cancelled.

Nothing else is written: since the cancellation deleted and closed nothing, registration, check-in and certificates work as before it, and the held certificate jobs are taken by the worker again by themselves.

## Mails

Ticket 14 (ADR 0066 addendum 2026-10-19, decision 1). Two SkyMail system templates, addressed by key:

| Mail | Key (`SKYMAIL_EVENT_CANCELLED_TEMPLATE_KEY`, `SKYMAIL_EVENT_REINSTATED_TEMPLATE_KEY`; unset: the key, empty: off) | Variables |
|---|---|---|
| The Event is cancelled | `core.event-cancelled` | `EventName`, `EventStart` (`02.01.2006 15:04`, Europe/Istanbul; empty when undated), `CancelReason` (may be empty), `FirstName` |
| The cancellation is undone | `core.event-reinstated` | `EventName`, `EventStart`, `FirstName` |

- **Queued, never sent inline.** The cancellation with `notifyTicketHolders` writes one row in `event_status_mails` in its own transaction and sets `events.cancel_notified_at`; a second cancel queues nothing. The undo with `notifyTicketHolders`, while the cancellation was mailed, queues one reinstated row that follows the cancellation's. Neither request waits for SkyMail or fails for it.
- **The worker** (`internal/eventstatusmail`, every 15 seconds) reads the Event again when it sends: a cancellation mail whose cancellation was undone meanwhile is dropped unsent, and a reinstated mail goes only while the Event is published.
- **Recipients**, read when the mail is sent: every Ticket of the Event, registered and guest, one mail per address. A registered holder gets core's primary e-mail of their account, not when it is erased, being erased or gone; a guest the address on the Ticket. The reinstated mail goes only to the Tickets the cancellation mail reached.
- **Once per person.** Each Ticket a mail reached is recorded (`event_status_mail_sends`, ids only), so a retry never writes twice. A send SkyMail may take later (404 template not seeded yet, 401/403, 5xx, unreachable) keeps the mail for a backoff (30 seconds, doubling to 30 minutes); a refusal of the address itself (400, 422) is counted and the others go on. A mail is given up after 72 hours. A template key set empty finishes the mail unsent.
- **No personal data** in the queue: ids and times only, so account erasure has nothing to do there; a Ticket deleted takes its record with it.
- **Seeding order:** seed `core.event-cancelled` and `core.event-reinstated` (skymail-frontend `emails/`) before the panel shows the box; a mail queued before then waits on SkyMail's 404 and goes once the template is there (within 72 hours).

