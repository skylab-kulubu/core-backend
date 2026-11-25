# Problem codes

Core answers every error as an RFC 9457 problem (`application/problem+json`:
`type`, `title`, `status`, `detail`, `instance`). The extension member
`code` says why, in a word a client can switch on; the title is for people
and may change. A client reads `code` first and falls back on the status only
when there is none. Extension members next to `code` (`param`, `fields`,
`max`, `registrationUrl`, …) say what to fix.

Existing codes and statuses do not change: a new code is only added to an
answer that had none. Codes of other areas are listed in their own documents
(for example docs/certificate-template-errors.md,
docs/attendee-import.md, docs/event-capacity.md). This document grows area by
area; the Tickets and door section is the first.

## The request's shape

Any route, before a service reads the request.

| Status | `code` | Extension | Meaning |
|---|---|---|---|
| 400 | `invalid_id` | `param`: the path segment (`eventId`, `sessionId`, `id`, …) | a path segment that must be an id is not a UUID |
| 400 | `invalid_body` | | the body is not the route's JSON |
| 400 | `invalid_query` | `param`: the query parameter (`status`, `include`, `use`, `notify`, …) | a query parameter has a value the route does not take |

Used by the Tickets and door routes so far; the other routes follow.

## Tickets and door

Apply, Guest apply, the Ticket lists, Session check-ins (desk, scan, the
guest's and the member's own), the Event's door, cancel, resend, the
attendee import and the forms service's report.

| Status | `code` | Extension | Meaning |
|---|---|---|---|
| 400 | `guest_fields_required` | `fields`: the missing ones of `firstName`, `lastName`, `email` | Guest apply without a first name, last name or e-mail |
| 400 | `email_or_ticket_key_required` | | the guest's check-in (`check-in/guest`) with neither an e-mail nor a ticket key |
| 400 | `door_query_required` | | the desk (`check-in/resolve`, `entries/resolve`) with neither `personId` nor `query` |
| 400 | `door_target_invalid` | | the desk's `personId` is not a UUID |
| 400 | `entry_date_invalid` | | the Event door's `?date=` is not a `YYYY-MM-DD` day |
| 400 | `session_not_in_event` | | the Session belongs to another Event than the Ticket |
| 400 | `cancel_reason_too_long` | `max`: 500 | the cancel reason is longer than 500 characters |
| 400 | `request_key_too_long` | | the `Idempotency-Key` is longer than 128 characters |
| 400 | `ticket_query_required` | | the Ticket list (`GET /v1/tickets`, `POST /v1/tickets/search`) with neither an e-mail nor a person |
| 400 | `user_id_invalid` | | a person id that is not one: the list's `userId`, a zero `userId` in a report or for Apply for other |
| 400 | `token_subject_invalid` | | the token's subject is not a person's id (a service account on a person's route) |
| 400 | `form_id_required` | | the forms service's report for the zero form id |
| 400 | `form_status_invalid` | | the report's `status` is not `accepted`, `pending` or `declined` |
| 400 | `guest_name_required` | | the report's `guest` has an e-mail but no first or last name |
| 404 | `guest_ticket_not_found` | `registrationUrl` while the Event takes registrations | the guest's check-in: no Ticket for this e-mail or key on the Session's Event |
| 409 | `already_checked_in` | | the Ticket already has this Session's Check-in |
| 409 | `ambiguous_match` | | the desk's name or e-mail matches more than one Ticket (title "Ambiguous Match", unchanged) |
| 500 | `internal_error` | | Guest apply's answer when the error could not be written |

Already there before, unchanged: `ticket_cancelled`, `ticket_waitlisted`
(409, docs/ticket-cancellation.md), `already_entered` (409,
docs/venue-entry.md), `event_full` (409, docs/event-capacity.md),
`event_closed`, `session_closed` (403), `door_qr_required`,
`door_qr_invalid`, `door_qr_expired`, `door_qr_used_up` (403,
docs/guest-self-check-in.md), `guest_check_in_rate_limited`,
`ticket_resend_rate_limited` (429), `ticket_notification_off`,
`ticket_holder_erased`, `ticket_checked_in`, `ticket_in_competition`,
`ticket_cancel_closed`, `event_ended`, `event_cancelled`,
`event_not_published`, `registration_closed` (409), the `import_*` codes
(docs/attendee-import.md).

Not yet coded: `401`, `403` and `404` that the status already explains
(no token, not allowed, no such Ticket, Event or Session), and a `409` of a
Ticket that is already there; the general error handler gives them a code
next.
