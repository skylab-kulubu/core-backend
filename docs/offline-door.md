# The offline door: `scannedAt`

ADR-0022's Queued check-in from core's side (CONTEXT: "Queued check-in",
"Staff scanner"). A door app that lost the network keeps scanning: it
verifies a Ticket QR (`kid` `tk-*`) or a SkyPass QR on the device with the
keys of `GET /v1/door/jwks`, lets the person in, and queues the scan. When
the network is back it sends the queue to the same routes as ever, each
scan with the moment the device took it:

| Route | Body |
|---|---|
| `POST /v1/sessions/{sessionId}/check-in/scan` (and `/check-in/skypass`) | `{"token": "…", "scannedAt": "2026-12-12T19:42:05+03:00"}` or `{"uid": "…", "scannedAt": …}` |
| `POST /v1/events/{eventId}/entries/scan` | the same |

`scannedAt` is RFC 3339 with its offset (a fraction is kept). Without it
nothing changes: core decides at its own clock, as before.

## What `scannedAt` changes

Core decides the scan **as of `scannedAt`**:

- the Ticket QR's expiry (the Event's end plus a day, by its dates now)
  and a SkyPass QR's (a minute after it was shown);
- the Event's door window (`403 event_closed` `event_ended`: an Event that
  ended over a day before the scan);
- the venue day (`entryDate`, `eventDayId`: [venue-entry.md](venue-entry.md),
  "The day"), and the day of a Session with no start.

Everything else is read **now**: who may scan (door staff, Leaders,
Privileged), the Event's status (a draft, cancelled or archived Event is
refused whenever the scan was taken), the Ticket's status (a Ticket
cancelled since is `409 ticket_cancelled`), the QR version (a renewed code
is `410 ticket_qr_superseded`: the door let the person in, the record is
not written; ADR-0022's accepted cost), and the holder behind a card UID.
A Google Wallet code (`SPW1:…`) is a one-time code core checks online
only: the door cannot queue it.

The record keeps both moments: `created_at` is when core wrote it,
`scanned_at` (migration `20261126102700`, on `ticket_checkins` and
`venue_entries`) when the device took it; `NULL` for a scan core took as it
came. The answer carries `scannedAt` beside `createdAt`.

## Bounds

| `scannedAt` | Answer |
|---|---|
| not RFC 3339 | `400 scanned_at_invalid` |
| more than 5 minutes ahead of core's clock (`ticket.ScanFutureSkew`) | `422 scanned_at_in_future`: the device's clock is wrong; it fixes it and sends again |
| more than 72 hours ago (`ticket.ScanMaxAge`) | `422 scanned_at_too_old`: core takes no scan that old; the door app drops it and says so |

72 hours: a weekend's Event offline from Friday to Sunday still syncs. The
door's mails have their own, shorter limit: a scan an hour or more old
sends none ([Mails](#mails)).

## Sent twice: one record

A queue can go twice (an answer lost, the app killed mid-sync) and two
doors can each have let the same Ticket in offline. With `scannedAt`, a scan
whose Ticket is already in answers **`200`** with
`{"duplicate": true, "kind": "entry" | "check_in", "id", "scannedAt", "createdAt", "name"?}`
instead of `409`, describing **the record that is there**:

- `kind` `entry`: the Ticket has that venue day's entry (the Event's door,
  or a Session's door in the `venue` mode); `name` is who is in;
- `kind` `check_in`: the Ticket is checked in at that Session;
- `id`, `createdAt`: that record's; `scannedAt`: the moment its scan was
  taken, `null` when core took it as it came (not the moment of the scan
  just sent).

Nothing is written; the first record stays. Core counts each scan sent
later in `skylab_door_offline_scans_total{outcome}`: `written`, `resent`
(the record's `scannedAt` is this scan's: the same scan sent again) and
`other_door` (the record is another scan's: another door's, a second
reading, or one core took as it came). Many `other_door` at one Event mean
two doors let the same people in while offline. The door app marks the queued scan done on `201` or on `200`
`duplicate`. Any other answer is the scan's own refusal: the app shows it
in its sync list and drops the scan (no retry), except `5xx`, `429` and a
network error, which it retries later. Without `scannedAt` a second scan is
`409` (`already_entered`, or the Check-in's), as before.

## Mails

The door's mails ([attendance-mail.md](attendance-mail.md): "hoş geldin",
"katılımın alındı") go for a scan sent later only when it was taken less
than an hour before it reached core (`ticket.ScanMailLate`): a welcome with
the Wi-Fi password the day after is no use. The record is written either
way.

## Compared with other products

- **Eventbrite Organizer** checks attendees in offline once the attendee
  list is on the device and syncs the check-ins when the device is online
  again. **Matches:** offline scan, later sync, one check-in per ticket.
- **Deviation 1:** our device holds no guest list: it verifies the signed
  code with the published key (ADR-0022, ADR-0082). It cannot tell a
  renewed or cancelled Ticket offline; core refuses those at the sync.
- **Deviation 2:** core bounds the device's moment (5 minutes ahead, 72
  hours back) and decides the expiry, the door window and the day at it;
  anything about rights and statuses is decided at the sync.
