# Form response tickets

**Core writes the Ticket an answer to an Event's form earns; the forms service
only reports its answers.** Forms knows nothing about Events. It reports every
answer to every form, and core decides from its own Events which answers are
applications.

An Event lists a form through `formUrl` or one of its `extraFormUrls`: the
form is the first UUID path segment of the address (`event.FormIDFromURL`),
with or without an alias. Archived Events list nothing. A form an older Event
still lists keeps writing that Event's Tickets, so remove it from the older
Event when the form is reused.

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
| `guest` | `{ firstName, lastName, email }` a guest typed into the form's identity fields |

The answer is `204` whether or not an Event lists the form. A pending or
declined report is not read further. Core neither stores nor logs a report
that no Event lists: it writes nothing for it, and the report's log line
(below) carries only ids, the status and the outcome, never who answered.

| Answer | Meaning for Forms |
|---|---|
| `204` | done; stop sending the report |
| `400` | a malformed report: a form id or body core cannot read, an unknown `status`, a zero `userId`, or a `guest` without a first name, last name or e-mail; sending it again will not help |
| `401` | no valid token |
| `403` | the token is not the forms service account's with `ticket:forms`; core refuses it before reading the request, so a malformed report from such a caller is `403` too |
| `5xx` | core could not finish; send the report again |

## What a report writes

Only an `accepted` answer writes, on every current Event that lists the form:

| Answer from | Ticket |
|---|---|
| a person signed in (`userId`) | REGISTERED, as `POST /v1/events/{id}/applications/users/{userId}` writes it |
| a guest (`guest`) | GUEST under the e-mail, as Guest apply writes it for an untrusted caller |
| neither | none |

A report carries what an anonymous form filler typed, so like the forms hop it
replaces it fills only a detail the guest Ticket lacks and never renames a
guest; an operator corrects names from the Event hub.

A pending answer writes nothing until Forms reports it accepted, so a form
that needs review tickets on approval. A declined answer writes nothing and
**does not take back a Ticket written earlier**: Tickets have no cancel. A
person core cannot find, or whose account is blocked or no longer active in
core, gets no Ticket.

Reports are safe to repeat: a second report finds the Tickets already there.
Forms keeps a report in its outbox until core answers `2xx` and retries it for
seven days, so reports sent before this route or the role existed arrive once
they do. For the same reason Forms must drop the queued reports of a person it
erases: a guest report delivered after account erasure would write the guest
Ticket again.

## Log

Core writes one JSON line per report it reads (`"event":"form_response"`):
`correlation_id`, `form_id`, `response_id`, `status` (`unknown` for a value
other than the three), `outcome` and `tickets_written` (new Tickets only).
`outcome` is `recorded`, `not_accepted`, `no_respondent`, `not_listed`,
`person_unavailable`, `invalid` or `failed`. The line never carries the
`userId`, a name or an e-mail.

## Setup

Keycloak (follows in e-skylab-keycloak): the `core` client role
`ticket:forms`, granted to the forms service account only. Forms' tokens
already carry core's roles (`resource_access.core.roles`, audience `core`) for
`media:attach`. Deploy core with this route, then the Forms version that
reports answers; that version no longer calls Guest apply
([guest-apply.md](guest-apply.md)).
