# Sending a Ticket again and renewing its link

ADR 0082 (e-skylab); bilet-qr-ve-pdf ticket 05. The panel's attendee row
has two actions: "Bileti yeniden gönder" sends the Ticket's "Biletin hazır"
mail again; "Bağlantıyı yenile" gives the Ticket a new link and QR, ends the
old ones and sends the mail with the new link.

## Who

Who may give out the Event's Tickets (the rule of Apply-for-other): the
owning team's Leaders and Privileged with `ticket:manage` (ADMIN, YK). Not
door staff, not DK (a reader), not the holder. A draft the caller may not
see is `404`. A Ticket whose holder was erased is `409
ticket_holder_erased`: nobody to send it to.

## `POST /v1/tickets/{id}/notification`

Queues the Ticket's Notification (`event.ticket-created`,
[ticket-created-notification.md](ticket-created-notification.md)) again,
with the variables of the moment (its page, QR and code).

- **`202`** `{"resend": n, "replayed": false, "queued": true}`: the n-th
  resend of the Ticket. Its Notification's key is
  `core:event.ticket-created:<ticketId>:r<n>`, its own, so SkyMail sends it
  instead of answering the first mail's replay (the first mail's key stays
  `…:v1`). A row still waiting (the first mail, an earlier resend) is taken
  over and sends this one.
- **`Idempotency-Key`** (optional, at most 128 characters): the same key
  for the same Ticket answers as the first time (`"replayed": true`) and
  writes nothing again; a double click sends one mail. Only its SHA-256 is
  kept.
- **`429 ticket_resend_rate_limited`** (`Retry-After: 3600`): three resends
  of the Ticket in the last hour, or six in the last 24 hours (a renewal
  with its mail counts).
- **`503 audit_unavailable`**: the Team audit row could not be opened
  (below); nothing was sent.
- **`409`**: `ticket_notification_off` while `TICKET_CREATED_NOTIFICATION` is
  off; `event_not_published` (a draft), `event_cancelled`, `event_ended`: a
  Ticket is sent again only for a published Event that is not over.

## `POST /v1/tickets/{id}/access-key/rotate`

A new access key and the next QR version, in one transaction; the code
stays.

- The old ticket page answers `404` and the old Ticket QR `410
  ticket_qr_superseded` at the door ([ticket-qr.md](ticket-qr.md)). A
  scanner offline can still take the old QR until it syncs (ADR 0022).
- The mail with the new link is queued in the same transaction, as above
  (its own `…:r<n>`, the same limit, the same `409`s, in which case nothing
  is renewed: a new link nobody is told of would lock the holder out).
- `?notify=false`: the link alone, on any Event, with the Notifications
  off too: an operator ending a leaked link at once. No mail, no
  `Idempotency-Key`.
- **`200`**: the Ticket as the panel reads it (its `code`, never its key)
  with `notificationQueued` and `resend`. A repeated `Idempotency-Key` with
  the mail answers the Ticket as it is, renewed once.
- The holder cannot renew their own link (a later "biletimi tekrar gönder"
  page only sends it again, ticket 14).

## Record

- **Who: the Team audit record** ([team-audit.md](team-audit.md)). Each
  resend is a `ticket.resend` row, each renewal a `ticket.link.renew` row,
  against the Event's team (`group_id`, `group_path`): the caller and their
  basis (`privileged`, or `leader` through the nearest Team they lead the
  Event's team in or under), the request's correlation id and client, and
  after `{"ticketId", "eventId", "resend"}`, never a person. A refused
  attempt (the hour's or the day's limit) is a `failed` row with its code
  (`rate_limited`, `daily_limited`). A row that cannot be opened stops the
  action: `503 audit_unavailable`, nothing sent or renewed. Kept three
  years like every row of the record.
- **How often: `ticket_resends`** keeps each resend: the Ticket, its number,
  whether it renewed the link, when, and the hash of the request's
  `Idempotency-Key`. No person; the row goes with its Ticket. It bounds a
  Ticket to three resends an hour and six a day.
- **Log lines**, never a person: `ticket_resent` (correlation id, Ticket,
  resend number) and `ticket_link_renewed` (correlation id, Ticket, Event,
  whether the mail went).
- A renewal with `notify=false` takes no `Idempotency-Key`: a repeated
  request renews once more. No mail goes, so nothing reaches the holder
  twice.

## Rollback

The down migration drops `ticket_resends` and the queue's `resend` column.
A resend still queued then goes under the first mail's key, which SkyMail
answers as a replay: nothing is sent. The release before reads neither.

## Industry practice

Matches: an organizer resends a ticket from the attendee list (Eventbrite
"Resend tickets", Tito); a new barcode makes the old one void (Ticketmaster
transfer, Eventbrite "reissue"). Deviation: renewing here does not move the
Ticket to someone else (no transfer, ADR 0082); and a resend is bounded to
three an hour and six a day per Ticket.
