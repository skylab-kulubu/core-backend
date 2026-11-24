# An Event's capacity and waiting list

kontenjan-iptal-hatirlatma 06 and 07 (Yusuf's answers of 2026-11-24): an
Event's `capacity` bounds its active Tickets; once full, a registration goes
on its waiting list (a `waitlisted` Ticket, [ticket-cancellation.md](ticket-cancellation.md)),
and a place that frees goes to the longest waiting, by itself.

## The fields

| Field | Who sees it | |
|---|---|---|
| `capacity` | everyone | the bound; `0` is none (every Event before this, and the default) |
| `waitlistEnabled` | everyone | a full Event puts a registration on its waiting list while on (default `true`); `PATCH` sets it, `null` is `400` |
| `full` | everyone | `capacity > 0` and the active Tickets fill it |
| `ticketCount`, `waitlistCount` | who may edit the Event | its active and waitlisted Tickets; left out for anyone else |

A copy takes the source's `capacity` and `waitlistEnabled`
(migration `20261124110000` adds `events.waitlist_enabled`).

## Writing a Ticket

Every write that makes a Ticket active holds the Event's row
(`SELECT … FOR UPDATE`) while it counts the active Tickets and writes: two
registrations for the last place get one active and one waitlisted (or
refused), never two active.

| Way | Room | Full, waiting list on | Full, waiting list off |
|---|---|---|---|
| Member apply `POST /v1/events/{id}/applications/me` with `{"joinWaitlist": true}` | `201` active | `201` `status: "waitlisted"` | `409 event_full` |
| Member apply without the body (sky-app before the list) | `201` active | `409 event_full` | `409 event_full` |
| Guest apply without a token (the internal hop; the edge answers it `404`, [guest-apply.md](guest-apply.md)) with `"joinWaitlist": true` | `201 {"status":"applied"}` | `201 {"status":"waitlisted"}` | `409 event_full` |
| Guest apply without a token, without it | `201 {"status":"applied"}` | `409 event_full` | `409 event_full` |
| Apply-for-other, the panel's guest ("Katılımcı ekle") | `201` active | `201` active, `overCapacity: true` | same |
| Forms report, accepted | Ticket, `204` | waitlisted Ticket, `200 {"outcome":"waitlisted"}` | no Ticket, `200 {"outcome":"event_full"}` |

- `409 event_full` carries `waitlistEnabled`, so a client offers the list
  only while it is on. Nothing is written.
- A member already waiting who applies again gets `409 ticket_waitlisted`,
  never the bare `409` of an active Ticket (an older sky-app shows that as
  "already registered"). The forms report counts the waiting Ticket as
  there (`waitlisted`).
- The panel adding a person who waits (Apply-for-other, the panel's guest)
  promotes them, past the capacity if it must (`overCapacity: true`).
- The token-less Guest apply's answer is told from the Event, not from a Ticket
  the e-mail already holds ([guest-apply.md](guest-apply.md)): an e-mail
  that applied before gets what a new one would, so the answer still does
  not tell whether it had applied.
- A form listed by several Events: each decides on its own; the report's
  outcome is `recorded` when any wrote (or had) an active Ticket, else
  `waitlisted` when any put the answer on its list, else `event_full`.
- A cancelled Ticket registered again is reopened under the same rules
  (waitlisted on a full Event); the panel's restore goes past the capacity
  (`overCapacity: true`).
- A lower capacity takes nobody out: new registrations wait.
- Turning the waiting list off stops new entries; who already waits keeps
  the place and is still promoted when one frees.

## Promotion

The waiting list moves in the transaction that frees a place, under the
Event's row lock: the longest waiting first (`waitlisted_at`, then when the
Ticket was written), as many as there are free places (all of them once
`capacity` is `0`).

- A place frees when an active Ticket is cancelled (an operator; later the
  holder and the forms decline, tickets 03 and 05, through the same store
  call), when `capacity` rises or becomes `0`, when the waiting list is
  turned on, when a draft is published and when a cancelled Event is
  reinstated.
- Only while the Event is published, not archived and has not started.
  Once it starts the list stays as it is (kept, never deleted).
- Two cancellations at once promote two different people: each waits for
  the Event's lock and counts again.
- Account erasure (KVKK) cancels the person's waitlisted Tickets (a
  member's, and the guest Tickets of their addresses) before they lose
  their holder; promotion skips any row without an owner or an address.
- The Event's row is locked `FOR NO KEY UPDATE`: the writers of its Tickets
  queue, rows that only reference the Event do not. The promoted Tickets'
  mails are queued in one statement.
- The log says `event <id>: <n> promoted from the waitlist`, naming nobody.

### "Öne al": `POST /v1/tickets/{id}/promote`

Who may cancel and restore the Event's Tickets
([ticket-cancellation.md](ticket-cancellation.md)) may move a waitlisted
Ticket to active, past the capacity if it must (`overCapacity: true`).
`200` the Ticket; an active one answers as it is; a cancelled one is
`409 ticket_cancelled` (restore it); `409 event_ended` once the Event is
over; a cancelled Event is `409`. A Team audit row `ticket.promote`, like
`ticket.cancel` (the `event_editor` basis included); the log line is
`ticket_promoted`.

## Mails

`TICKET_STATUS_MAIL` (default `off`) turns the waiting list's mails on, and
the cancellation's ([ticket-status-mail.md](ticket-status-mail.md)). They
go through the "Biletin hazır" queue and worker
([ticket-created-notification.md](ticket-created-notification.md)):
`ticket_notifications.kind` is `created`, `waitlisted` or `promoted`, queued
in the transaction that writes the status.

| When | Type (SkyMail template, skymail-frontend #179) | Variables | Key |
|---|---|---|---|
| Put on the waiting list | `event.waitlist-joined` | `EventId`, `EventName`, `EventDate`, `OwnerTeam`, `Venue`, `TicketUrl` (the ticket page, where the holder may leave the list) | `core:event.waitlist-joined:<ticketId>:q<qrVersion>` |
| Promoted (by itself or "Öne al") | `event.waitlist-promoted` | `event.ticket-created`'s, the PDF too while `TICKET_PDF_ATTACHMENT` is on | `core:event.waitlist-promoted:<ticketId>:q<qrVersion>` |

- Off, nothing is queued for the list and the log says the mail did not go
  (`event <id>: a registration went on the waitlist; no mail …`); a
  promoted Ticket gets "Biletin hazır" while `TICKET_CREATED_NOTIFICATION`
  is on, under the Ticket's next resend number.
- A "listedesin" not yet sent gives way to "yer açıldı"; the worker sends
  "listedesin" only while the Ticket still waits, the others only while it
  is active.
- A waitlisted Ticket gets no "Biletin hazır", and its QR and PDF stay `410`
  until it is promoted.
- Turn it on only once SkyMail has seeded both types: before that SkyMail
  answers `422 notification.type_unknown` and the rows are dropped as
  refused.
- `event.registration-full` (a forms answer on a full Event with the list
  off) is not sent yet: there is no Ticket to queue it by, and the queue
  holds no address. The report's log line says `outcome: event_full`.

## Rollback

Rolling back the code needs no down migration: the release before ignores
both columns, and its worker would send a queued `waitlisted` or `promoted`
row as "Biletin hazır" (turn `TICKET_STATUS_MAIL` off and let the queue
drain first). Taking `capacity` to `0` on an Event lifts its bound now, and
promotes its waiting list. The down migration deletes the rows of the new
kinds, then drops `kind` and `waitlist_enabled`; waitlisted Tickets stay
waitlisted.
