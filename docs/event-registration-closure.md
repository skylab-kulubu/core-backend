# Registration closure

ADR 0066 addendum (2026-10-19, decision 10), ticket 12. The organizer closes or opens an Event's applying by hand, and may set a deadline, apart from the Event's status: a published Event may be closed to new applications and still be shown everywhere.

## Fields

| Field | Written by | Meaning |
|---|---|---|
| `registrationClosed` | `POST /v1/events`, `PUT`/`PATCH /v1/events/{id}` | closed by hand; default `false` |
| `registrationClosesAt` | the same | the deadline: applying closes at this moment; none: no deadline |
| `registrationClosedAt` | the store, never the body | when applying was closed by hand: set when `registrationClosed` turns on, kept while it stays on, cleared when it turns off |
| `registrationOpen` | derived, never written | `status = published`, not ended (as `active`), not closed by hand, and before the deadline |

Writing either needs what any other field of the Event needs (`authz.Update`: the editors; DK `403`). An update that leaves them out keeps them ([event-updates.md](event-updates.md)); `registrationClosed: null` is `400`, `registrationClosesAt: null` removes the deadline. The body's `active` is still ignored: the legacy "Başvurular açık" switch of sky-app can be bound to `registrationClosed` instead.

## What a closure refuses

| Path | Applying not open |
|---|---|
| `POST /v1/events/{id}/applications/me`, `…/applications/guest` (a person's own application, the editor's own too) | `409`, `code: "registration_closed"` (a draft still answers `event_not_published` to who may see it, `404` to anyone else) |
| `POST /v1/events/{id}/applications/users/{userId}`, and `…/applications/guest` called by an operator (who may update the Event or assign its Tickets: the panel's "Katılımcı ekle") | allowed: the editor's call (Yusuf, 2026-10-20) |
| `POST /v1/forms/{formId}/responses` (Forms' report) | **not refused by the closure**: the Ticket is written. Only a report carrying the optional `submittedAt` is judged by it: an answer sent at or after `registrationClosedAt` or `registrationClosesAt` gets no Ticket on that Event; when no Event is left, `200` with `outcome: "registration_closed"` ([form-response-tickets.md](form-response-tickets.md)) |

Self-apply is refused whenever `registrationOpen` is false: on an Event closed by hand or past its deadline, and also on an ended or cancelled one.

**The Forms report does not follow `registrationOpen`, and closing applying does not refuse it** (Yusuf, 2026-10-20; review B1). Forms reports an answer when it is accepted, and an approval can come after the closure for an answer sent in time; Forms also retries for days. Refusing on arrival would lose those answers silently, and Luma and Eventbrite give a Ticket to an application approved after registration closed. So the closure stops a person's own application only. Once Forms sends `submittedAt` (the moment Forms first took the answer, UTC RFC 3339 with `Z`; an unreadable value is ignored, never refused, and a time to come counts as now), core refuses an answer sent after the closure: by hand (`registrationClosedAt`, stored for that) or by the deadline. A report without it is not judged. A cancelled Event refuses Forms' answers first, as ticket 04 says. An ended Event keeps taking its forms' answers (feedback forms are filled in after the Event): it answers `registrationOpen: false` and still writes the Ticket; a draft (ADR 0066) does too.

Forms' worker sends a report again for seven days on any answer that is not `2xx`, and the answer was already accepted in Forms. So core answers a refusal by `submittedAt` with `200`, which Forms takes as done, until Forms treats a lasting refusal of its own (proposed: `409`, `code: "registration_closed"`) and shows it to the form's owner and the person who answered. Sending `submittedAt` and that answer are Fatih's call; core moves to the `409` in a separate change after that Forms version is live.

## Clients

- sky-app: may show the Katıl button on `registrationOpen` instead of `active`, and bind its "Başvurular açık" switch to `registrationClosed`. Nothing breaks if it does neither: a self-apply on a closed Event is `409`.
- The panel (core-frontend) writes the two fields; its UI is a separate ticket.
