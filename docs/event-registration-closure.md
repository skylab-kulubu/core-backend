# Registration closure

ADR 0066 addendum (2026-10-19, decision 10), ticket 12. The organizer closes or opens an Event's applying by hand, and may set a deadline, apart from the Event's status: a published Event may be closed to new applications and still be shown everywhere.

## Fields

| Field | Written by | Meaning |
|---|---|---|
| `registrationClosed` | `POST /v1/events`, `PUT`/`PATCH /v1/events/{id}` | closed by hand; default `false` |
| `registrationClosesAt` | the same | the deadline: applying closes at this moment; none: no deadline |
| `registrationOpen` | derived, never written | `status = published`, not ended (as `active`), not closed by hand, and before the deadline |

Writing either needs what any other field of the Event needs (`authz.Update`: the editors; DK `403`). An update that leaves them out keeps them ([event-updates.md](event-updates.md)); `registrationClosed: null` is `400`, `registrationClosesAt: null` removes the deadline. The body's `active` is still ignored: the legacy "Başvurular açık" switch of sky-app can be bound to `registrationClosed` instead.

## What a closure refuses

| Path | Applying not open |
|---|---|
| `POST /v1/events/{id}/applications/me`, `…/applications/guest` | `409`, `code: "registration_closed"` (a draft still answers `event_not_published` to who may see it, `404` to anyone else) |
| `POST /v1/events/{id}/applications/users/{userId}` (adding a person from the panel) | allowed: the editor's call |
| `POST /v1/forms/{formId}/responses` (Forms' report) | no Ticket on an Event whose organizer closed applying, by hand or by the deadline; when every Event of the form has, `200` with `outcome: "registration_closed"` ([form-response-tickets.md](form-response-tickets.md)) |

Self-apply is refused whenever `registrationOpen` is false: on an Event closed by hand or past its deadline, and also on an ended or cancelled one. The Forms report looks at the organizer's closure alone: a draft (ADR 0066), an ended Event and a cancelled one take a form's answers as before, unless closed by hand or by the deadline.

Forms' worker sends a report again for seven days on any answer that is not `2xx`, and the answer was already accepted in Forms. So core answers the refusal `200`, which Forms takes as done, until Forms treats a lasting refusal of its own (proposed: `409`, `code: "registration_closed"`) and shows it to the form's owner and the person who answered; core moves to that answer in a separate change after that Forms version is live. That part is Fatih's call.

## Clients

- sky-app: may show the Katıl button on `registrationOpen` instead of `active`, and bind its "Başvurular açık" switch to `registrationClosed`. Nothing breaks if it does neither: a self-apply on a closed Event is `409`.
- The panel (core-frontend) writes the two fields; its UI is a separate ticket.
