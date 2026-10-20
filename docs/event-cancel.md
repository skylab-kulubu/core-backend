# Cancelling an Event

ADR 0066 and its 2026-10-19 addendum (e-skylab), ticket 04. A cancelled Event stays visible with `status: "cancelled"`; nobody new registers or checks in and no certificate is issued, but **nothing is deleted or closed for good**: the cancellation can always be undone (`POST …/uncancel`, below).

## `POST /v1/events/{id}/cancel`

Body (optional, both fields too): `{"reason": "Hava koşulları nedeniyle", "notifyTicketHolders": true}`.

- `reason`: a short public text, trimmed, at most 500 characters (`400` beyond). Answered as `cancelReason`.
- `notifyTicketHolders`: whether core mails the Ticket holders. Read and, until the cancellation mail lands (ticket 14), does nothing: the organizer announces it through "Katılımcılara mail".

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

Undoes the cancellation, always (ADR 0066 addendum 2026-10-19, decision 2; ticket 15). Body (optional): `{"notifyTicketHolders": true}`, whether core mails who got the cancellation mail that the Event is back; both mails are ticket 14, so until then no cancellation mail went out and the field is read and does nothing.

| Event | Answer |
|---|---|
| cancelled | `200`, the Event published again; an ended one too (it shows as ended, `active: false`) |
| published | `200`, the Event as it is |
| a draft | `409`, `code: "event_not_cancelled"`; `404` to who may not see it |

Who: whoever may edit the Event (`authz.Update`); DK and anyone else `403`. One `UPDATE … WHERE status = 'cancelled'`: `cancelledAt`, the actor and `cancelReason` are cleared (an old client does not show a reason on a published Event), `publishedAt` stays, the legacy `events.active` column gets the derived value. In the same transaction the Competitions the cancellation cancelled go back to their earlier status; one cancelled on its own stays cancelled.

Nothing else is written: since the cancellation deleted and closed nothing, registration, check-in and certificates work as before it, and the held certificate jobs are taken by the worker again by themselves.
