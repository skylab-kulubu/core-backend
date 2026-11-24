# A Ticket's status and an operator's cancellation

kontenjan-iptal-hatirlatma ticket 02 (SPEC §3.1, §3.4). A Ticket is never
deleted to cancel it: it gets a status. The holder's own cancellation
(ticket 03), the cancellation and waiting-list mails (04), the forms
decline (05) and the waiting list itself (06, 07) build on this.

## Status

| `status` | Meaning | Door, QR, PDF | Counted, told, certificate, competition |
|---|---|---|---|
| `active` | the Ticket as before | yes | yes |
| `waitlisted` | on the Event's waiting list (ticket 06 writes it) | no (`409 ticket_waitlisted`; page QR/PDF `410`) | no |
| `cancelled` | cancelled | no (`409 ticket_cancelled`; page QR/PDF `410`) | no |

Columns (migration `20261124100000`, which gives up after a 5s
`lock_timeout` rather than queue Ticket reads behind a long one): `status` (default `active`; every
Ticket before the migration is active, and the release before writes active
Tickets on the new schema), `status_changed_at`, `waitlisted_at`,
`cancelled_at`, `cancelled_by` (`holder`, `operator`, `form`),
`cancel_reason` (at most 500 characters). A cancelled Ticket always has
`cancelled_at` and `cancelled_by`; any other has none of the three. Index
`(event_id, status)`.

Every answer that carries a Ticket carries `status`, and when cancelled
`cancelledAt`, `cancelledBy` and `cancelReason`.

## Who reads only active Tickets

Every reader that means "the Event's Tickets" or "has a Ticket" reads the
active ones (`status = 'active'`, not `<> 'cancelled'`, so the waiting list
stays out too):

- the Event's list (`GET /v1/events/{id}/tickets`, default), the holder's own
  list (`GET /v1/tickets/me`, default), the lookup (`GET /v1/tickets`) and
  "is this person registered" (`GET /v1/tickets/user/{userId}/event/{eventId}`:
  `404` for a cancelled or waitlisted Ticket, so sky-app, which shows
  "Kayıtlısın" on a `200`, offers to join again);
- the panel's resend and link renewal (`POST /v1/tickets/{id}/notification`,
  `/access-key/rotate`): `409 ticket_cancelled` (`ticket_waitlisted`), no
  audit row, no quota spent;
- unpublishing (`event_has_tickets` counts active Tickets only: an Event
  whose Tickets are all cancelled goes back to a draft);
- the door: the search lists no other; every way in (scan, SkyPass, me,
  resolve by name, e-mail, person or code, the guest's own check-in, the
  Ticket QR) finds the Ticket and refuses it with `409 ticket_cancelled` or
  `409 ticket_waitlisted`; the store itself writes a check-in only for an
  active Ticket;
- the mails: "Biletin hazır" (a queued one is dropped when the Ticket is
  cancelled, and the worker drops a Ticket that is no longer active), the
  publication's queue, the Event's cancellation and change mails, the
  feedback mails and their audience, the Event's SkyMail list;
- certificates: the Event's attendance (`EventAttendance`, both queries, and
  the memory store's) that the list, eligibility, the attendance snapshot,
  the automatic finalization and the rescan read; the manual issue (with a
  justification too, `404`) and a queued job whose Ticket was cancelled
  since; Competition candidates and Entrants (a cancelled Ticket is no
  candidate and cannot be added);
- the dashboard's counts (applications per Event and per day).

Every status is seen by: erasure and retention (a cancelled Ticket is still
personal data; erasure also clears the operator's `cancel_reason`, which may
name the person), the single Ticket reads (`GET /v1/tickets/{id}`, the panel's
`?status=`), the ticket page (to say `ticket_cancelled`) and the
re-registration below. Check-in counts need no filter: a Ticket with a
check-in cannot be cancelled.

## Lists

- `GET /v1/events/{id}/tickets?status=active|waitlisted|cancelled|all`,
  default `active`; another value `400`.
- `GET /v1/tickets/me?include=waitlisted,cancelled`: the active Tickets and
  those of the statuses named; without `include` only the active ones, so an
  older sky-app sees no cancelled Ticket. An unknown status `400`. Only an
  active Ticket carries `qrToken`.

## `POST /v1/tickets/{id}/cancel`

Body (optional): `{"reason": "…"}`, at most 500 characters (`400` over).

- **Who:** exactly who may edit the Event (Yusuf, 2026-11-24): Privileged
  `event:manage` (ADMIN, YK), the owning team's Leaders (and those of a Team
  above it), and the members of a team whose members edit its Events by the
  Event policy (GECEKODU), on that team's Events only. DK reads only
  (`403`), as do ordinary members, door staff, `ticket:manage` alone and the
  holder (the holder's own cancellation is ticket 03). A draft the caller
  may not see is `404`.
- **`200`** the Ticket, `status: "cancelled"`, `cancelledBy: "operator"`. A
  Ticket already cancelled answers `200` as it is (no second audit row).
- **`409 event_ended`** once the Event is over;
  **`409 ticket_checked_in`** for a Ticket with a check-in (undo it first,
  `DELETE /v1/tickets/{id}/sessions/{sessionId}/check-in`);
  **`409 ticket_in_competition`** for a Ticket of a Competition's current
  Entrant (ADR 0068; a withdrawn or disqualified Entrant's Ticket may go).
- The Ticket's "Biletin hazır" mail still waiting is dropped. Its link and
  QR stay: the page says `ticket_cancelled`, QR and PDF answer `410`.
- No mail to the holder yet (ticket 04, `TICKET_STATUS_MAIL`).

## `POST /v1/tickets/{id}/restore`

Same rights and `409 event_ended`. The Ticket is active again with a new
access key and the next QR version (the old link answers `404`, the old QR
`410`; the code stays), and its "Biletin hazır" mail is queued under the
next resend number (`…:r<n>`, [ticket-resend.md](ticket-resend.md); not
counted against the resend limits' purpose, though the row is in
`ticket_resends`) while `TICKET_CREATED_NOTIFICATION` is on. An active
Ticket answers `200` as it is; a waitlisted one is `409 ticket_waitlisted`.

## Registering again

Member apply, Apply-for-other, Guest apply and the forms report find a
cancelled Ticket of the same person and Event and reopen it, as a restore
does (new key and QR version, the mail again). Only an active (or
waitlisted) Ticket is "already there" (`409`, Guest apply's `existing`).
A guest Ticket reopened answers as written (`created`; the forms report
counts it).

## Audit and logs

Both actions are Team audit rows (`ticket.cancel`, `ticket.restore`,
[team-audit.md](team-audit.md)), as the resend: the Event's team, the
Ticket's and the Event's ids, never the reason or a person. The basis is
`privileged`, else `leader` (through the nearest Team the caller leads),
else `event_editor` through the Event's team: a member who edits the Event
by the Event policy (GECEKODU). A row that
cannot be opened stops the action (`503`). The log line
(`ticket_cancelled`, `ticket_restored`) names the request, the Ticket and
the Event only.

## Rollback

Rolling back the code needs no down migration: the release before ignores
the columns (and reads a cancelled Ticket as an ordinary one until the new
release is back). The down migration drops the columns, so every cancelled
Ticket becomes an ordinary one; the audit action and basis checks stay
wide.
