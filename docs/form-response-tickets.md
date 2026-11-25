# Form response tickets

**Core writes the Ticket an answer to an Event's form earns; the forms service
only reports its answers.** Forms knows nothing about Events. It reports every
answer to every form, and core decides from its own Events which answers are
applications.

An Event lists a form through `formUrl` or one of its `extraFormUrls`: the
form is the first UUID path segment of the address (`event.FormIDFromURL`),
with or without an alias. An Event's feedback form (`feedbackFormUrl`) is
not listed: its answers write no Ticket ([event-feedback.md](event-feedback.md)).
Archived Events list nothing. A form is listed by one current Event only, a
draft or an ended one too: binding it to a second is `409 form_in_use`
(ADR 0069, addendum of 2026-11-22; [event-feedback.md](event-feedback.md#one-place-per-form)).
A form two Events listed before that rule stays on both until one lets it
go; its answers write the Tickets of those of them still open (below).

Two ways in, one rule: the HTTP report below, and Forms' `core.forms`
events (`forms.response.created` and `forms.response.status_changed`, with
`EVENTS_FORMS_ENABLED=true`; [integration-events.md](integration-events.md#coreforms-forms-events)),
which carry no guest's name or e-mail (core reads them from Forms) and the
invite code as the `davet` hidden field. A third, the pull from Forms'
list (an organizer's backfill and the daily catch-up,
[form-pulls.md](form-pulls.md)), takes the same rule. All end in
`ticket.Service.RecordFormResponse`; what this page says of an answer holds
for each. The HTTP report goes when Forms stops sending it, after at most
one to two days of both (hub ticket R16).

## Endpoint

`POST /v1/forms/{formId}/responses` needs the forms service account
(`MEDIA_SERVICE_CLIENTS` maps its client to the `forms` product) holding the
`core` client role `ticket:forms`. No person may call it, whatever roles they
hold.

| Body field | Meaning |
|---|---|
| `responseId` | the answer's id in Forms, for tracing only |
| `status` | `pending` (waits for review), `accepted` (needs no review, or approved), `declined` |
| `userId` | the person who answered signed in; wins over `guest` |
| `guest` | `{ firstName, lastName, email }` a guest typed into the form's identity fields; a guest without an e-mail counts as no respondent |
| `invite` | optional: the feedback invite's code the answer came with (the `?davet=` of the mail's link), for a feedback form only; an unknown one, another form's, or one longer than 64 characters is ignored; never logged ([event-feedback.md](event-feedback.md)) |
| `submittedAt` | optional: the moment Forms first took the answer, in UTC as RFC 3339 with `Z` (`2026-10-20T09:15:00Z`); judged against the Event's registration closure, see below. A value that is not such a time (no zone, not a string) is ignored as if not sent, with a warning line that names the form only (`form_response_submitted_at_ignored`); the report is never refused for it. A time later than core's now counts as now |

The answer is `204` whether or not an Event lists the form. A pending
report is not read further; a declined one only cancels what the same
answer earned (below). Core neither stores nor logs a report
that no Event lists: it writes nothing for it, and the report's log line
(below) carries only ids, the status and the outcome, never who answered.

| Answer | Meaning for Forms |
|---|---|
| `204` | done; stop sending the report. A report to a current Event's feedback form is `204` too, whatever its status and whoever answered: no Ticket is written (logged as `outcome: feedback`) |
| `200` `{"outcome": "event_cancelled"}` | done, and no Ticket was written: every Event that lists the form is cancelled ([event-cancel.md](event-cancel.md)); stop sending the report |
| `200` `{"outcome": "event_ended"}` | done, and no Ticket was written: every Event that lists the form and is not cancelled has ended (below); stop sending the report |
| `200` `{"outcome": "waitlisted"}` | done: every Event of the form that took the answer is full, and the answer is on the waiting list of at least one ([event-capacity.md](event-capacity.md)); core tells the person (while `TICKET_STATUS_MAIL` is on); stop sending the report |
| `200` `{"outcome": "event_full"}` | done, and no Ticket was written: every Event of the form is full with its waiting list off; stop sending the report |
| `200` `{"outcome": "registration_closed"}` | done, and no Ticket was written: the report said when the answer was sent (`submittedAt`), and by then every Event of the form not cancelled had closed applying ([event-registration-closure.md](event-registration-closure.md)); stop sending the report |
| `400` | a malformed report: a form id or body core cannot read, an unknown `status`, a zero `userId`, or a `guest` with an e-mail but without a first name or last name; sending it again will not help |
| `401` | no valid token |
| `403` | the token is not the forms service account's with `ticket:forms`; core refuses it before reading the request, so a malformed report from such a caller is `403` too |
| `5xx` | core could not finish; send the report again |

## What a report writes

A report to a feedback form writes no Ticket (checked before anything
else, after the status is read). Otherwise only an `accepted` answer writes,
on every current Event that lists the form
and is neither cancelled nor ended. When every such Event is cancelled, nothing is written
and the answer is `200` with `outcome: "event_cancelled"` (checked first);
when every one left has ended, `200` with `outcome: "event_ended"`.

**An ended Event takes no new application** (ADR 0069, addendum of
2026-11-22). Ended is the Event's end (`endDate`, else a day after
`startDate`) at the moment core reads the report: an answer approved after
the Event ended earns no Ticket, since the door and the certificates need a
Check-in by then. The organizer adds a late person from the panel. An Event
without dates never ends. Feedback forms, answered after the Event, are
apart and write no Ticket anyway.

**Closing applying does not refuse a report** (Yusuf, 2026-10-20). An answer
sent in time and approved after the organizer closed applying, or reported
late by Forms, still earns its Ticket, as an approved application does on
Luma or Eventbrite. Only a report that says when the answer was sent, with
the optional `submittedAt` (above), is judged by it: an Event whose
organizer had closed applying by then, by hand (`registrationClosedAt`) or by
its deadline (`registrationClosesAt`), gets no Ticket, and when no Event is
left the answer is `200` with `outcome: "registration_closed"`. Forms sends
no `submittedAt` today; core takes it once Forms does (Fatih's call). A
draft takes a form's answers as a published one does, although
`registrationOpen` is `false` for it; an ended Event takes none (above).

| Answer from | Ticket |
|---|---|
| a person signed in (`userId`) | REGISTERED, as `POST /v1/events/{id}/applications/users/{userId}` writes it |
| a guest (`guest`) | GUEST under the e-mail, as Guest apply writes it for an untrusted caller |
| neither, or a guest without an e-mail | none (`no_respondent`) |

A report carries what an anonymous form filler typed, so like the forms hop it
replaces it fills only a detail the guest Ticket lacks and never renames a
guest; an operator corrects names from the Event hub.

A pending answer writes nothing until Forms reports it accepted, so a form
that needs review tickets on approval. A declined answer writes nothing and
**cancels nothing** (Yusuf, K6 2026-11-25; before it, kontenjan-iptal-hatirlatma
ticket 05 cancelled): it marks the Tickets **the same answer** earned, and
the panel shows "Forms'ta reddedildi" with a one-click cancel (the
operator's cancellation, [ticket-cancellation.md](ticket-cancellation.md)).

- A Ticket keeps the answer that wrote or reopened it
  (`tickets.form_response_id`, Forms' `responseId`; migration
  `20261124130000`). A Ticket written another way (sky-app, the panel, Guest
  apply) has none, and an accepted answer that finds a Ticket already there
  does not claim it. A Ticket reopened another way (the panel's restore,
  registering again) loses it. Tickets written before the column have none
  and are never marked.
- Core keeps the form of the Ticket's answer and where Forms has it
  (`form_response_form_id`, `form_response_status`: `accepted`, `pending`,
  `declined`; migration `20261127110000`). An accepted answer that finds the
  Ticket takes it over (`form_response_id` becomes the new answer, the mark
  is cleared) only when it is of **the same form**, by **the same holder**
  (the signed-in owner; a guest Ticket for a guest's answer), and the
  Ticket's answer is **no longer accepted** (declined, a correction asked,
  pending again). So when Forms asks for a correction (`Returned`, sent as
  `declined` today, `CoreResponseFeed.cs`) and the corrected answer is
  approved, the Ticket follows the corrected answer and loses its mark. An
  extra form's answer never takes over the application's Ticket (the
  application's decline still marks it, the extra form's does not), and
  someone typing the holder's e-mail into the form while the holder's
  answer stands gets nothing of the Ticket. The Ticket's own answer
  accepted again clears its mark. A Ticket whose answer was kept before the
  migration (no form) is not taken over until its answer's decline names
  the form (the decline fills it in).
- Known limit: when the corrected answer's acceptance (R2) is handled
  before the first answer's decline (R1), R2 does not take the Ticket over
  (R1 still stands then), and R1's decline arriving afterwards marks the
  Ticket. Forms sends `Returned` before the person can answer again, so
  this needs reports out of order (a retry); the mark is a warning the
  organizer clears by hand, never a cancellation.
- A `pending` report of the Ticket's answer (Forms' review opened again)
  sets it `pending` and clears the mark; nothing else changes.
- `declined` marks, on the Events that list the form and have not ended,
  each active or waitlisted Ticket whose answer is the report's, with a
  check-in too: `formsRejectedAt` (migration `20261127110000`), in every
  Ticket answer the panel reads (the Event's Tickets). No status change, no
  mail from core (Forms sends its own). The answer is still `204`; the
  log's `outcome` is `forms_rejected`, else `not_accepted`.
- A later `accepted` report of the same answer clears the mark.
- A Ticket Forms' decline cancelled before K6 (`cancelledBy: form`) is
  reopened by its answer's acceptance, as below.
- Account erasure clears `form_response_id` with the person's other data;
  the mark is a time only.

An operator cancels a Ticket
from the panel ([ticket-cancellation.md](ticket-cancellation.md)). **A report
(HTTP or a `core.forms` event) reopens only a Ticket Forms' own decline
cancelled** (`cancelledBy: form`), with a new link and QR, counted in
`ticketsWritten`: the holder's own cancellation and an operator's stand,
and an accepted report finds such a Ticket as one already there, writing
nothing and mailing nobody (the holder registering again from the app, or
the panel's restore, still reopens it). A
person core cannot find, or whose account is blocked or no longer active in
core, gets no Ticket.

Reports are safe to repeat: a second report finds the Tickets already there.
Forms keeps a report in its outbox until core answers `2xx` and retries it for
seven days, so reports sent before this route or the role existed arrive once
they do. For the same reason Forms must drop the queued reports of a person it
erases: a guest report delivered after account erasure would write the guest
Ticket again. Forms removes only the verified address from a guest report
([account-erasure-command.md §10](account-erasure-command.md#10-forms)), so
such a report arrives with the name and `email: null`; core answers it `204`
as a report without a respondent and writes nothing.

## Log

Core writes one JSON line per report it reads (`"event":"form_response"`):
`correlation_id`, `form_id`, `response_id`, `status` (`unknown` for a value
other than the three), `outcome`, `tickets_written` (new Tickets only),
`tickets_marked` (the Tickets a declined answer marked, K6) and
`listing_events`: how many current Events list the form, whatever their
status (`0` when the report ended before they were read). More than one is a
form shared before the one-place rule; such a form is left as it is and the
line is how it shows.
`outcome` is `recorded`, `not_accepted`, `forms_rejected`, `cancelled` (before K6), `no_respondent`, `not_listed`,
`event_cancelled`, `event_ended`, `registration_closed`, `waitlisted`, `event_full`, `person_unavailable`, `invalid` or `failed`. The line never carries the
`userId`, a name or an e-mail.

## Setup

Keycloak (follows in e-skylab-keycloak): the `core` client role
`ticket:forms`, granted to the forms service account only. Forms' tokens
already carry core's roles (`resource_access.core.roles`, audience `core`) for
`media:attach`. Deploy core with this route, then the Forms version that
reports answers; that version no longer calls Guest apply
([guest-apply.md](guest-apply.md)).
