# "Biletin iptal edildi": the cancellation mail

kontenjan-iptal-hatirlatma ticket 04 (SPEC §3.6). When a Ticket is
cancelled, its holder is told by `event.ticket-cancelled` (SkyNotify
template, skynotify-frontend #179) while `TICKET_STATUS_MAIL` is on, the
switch that also turns on the waiting list's mails
([event-capacity.md](event-capacity.md)). Default `off`; the start line says
`ticket status mail: on (event.waitlist-joined, event.waitlist-promoted,
event.ticket-cancelled)` or `ticket status mail: off (…)`.

## When

| Cancelled by | Told |
|---|---|
| the holder (member route or ticket page, [ticket-cancellation.md](ticket-cancellation.md)) | always (`CancelledBy=holder`, `Reason` empty) |
| an operator | only with `notify: true` (`CancelledBy=operator`, `Reason` the operator's, possibly empty) |
| a declined forms answer | never: Forms sends its own ([form-response-tickets.md](form-response-tickets.md)) |

A Ticket already cancelled is not told again.

## The queue

No new table: the row goes to the "Biletin hazır" queue
([ticket-created-notification.md](ticket-created-notification.md)),
`ticket_notifications.kind = 'cancelled'` (migration `20261124130000`
widens the kind check), in the transaction that cancels the Ticket. The
queue keeps one row per Ticket (`ticket_id` its key): a mail of the Ticket
still waiting ("Biletin hazır", "Bekleme listesindesin") becomes the
cancellation's, since it is no longer news; a cancellation that tells
nobody drops it, as before. The row keeps no person: the address is read
when the mail goes. SPEC §3.6 proposed a separate `ticket_mails` table;
the waiting list (#409) had already given this queue its `kind`, so the
cancellation is one more kind of it.

The worker is the same one, with the same retries and giving up (30 s
doubling to 30 min, a day): see ticket-created-notification.md.

## Sending

- **Re-read at sending:** the mail goes only while the Ticket is still
  cancelled (restored or registered again since, the row is dropped); a
  deleted Ticket's row went with it (`ON DELETE CASCADE`); an erased
  member, a member whose account is not active, and a guest without an
  address are dropped as for "Biletin hazır"; so is a row of a draft's, a
  cancelled or archived Event's, or one over (only a published Event that
  is not over is told of).
- Member: `POST /v1/notifications` (inbox and e-mail); guest:
  `POST /v1/mail_tasks/single`. Key
  `core:event.ticket-cancelled:<ticketId>:q<qrVersion>`: a Ticket restored
  and cancelled again (a new QR version) is told again.
- Variables, every one always (SkyNotify renders with `missingkey=error`):
  `EventId`, `EventName`, `EventDate`, `OwnerTeam`, `CancelledBy`
  (`holder` or `operator`), `Reason`. No link, QR, code or PDF: a
  cancelled Ticket opens nothing.
- Off, nothing is queued and the log says the mail did not go, naming the
  Event alone (`event <id>: a ticket was cancelled; no mail
  (TICKET_STATUS_MAIL is not on)`).
- The worker's lines count the mails; no name or address.

## Turning it on

Only once SkyNotify has seeded `event.waitlist-joined`,
`event.waitlist-promoted` and `event.ticket-cancelled`: before that SkyNotify
answers `422 notification.type_unknown` and the rows are dropped as
refused.

## Rollback

Rolling the code back needs no down migration: the release before drops a
queued `cancelled` row unsent (its worker mails only an active Ticket), so
unsent cancellation mails are lost. Let the queue drain first to keep them
(turn `TICKET_STATUS_MAIL` off a few minutes before). The down migration
deletes those rows, narrows the kind check again and drops
`tickets.form_response_id`; cancelled Tickets stay cancelled.
