# The door's mails

kapi-postalari tickets 04 and 05 (SPEC §5; Yusuf's decisions of 2026-11-23/24).
SkyMail templates `event.attendance-welcome` and `event.attendance-recorded`
(skymail-frontend, seeded in production 2026-11-24).

A Ticket's holder hears from the door:

| Mail | Type (template key) | When |
|---|---|---|
| Hoş geldin | `event.attendance-welcome` | the Ticket's first Venue entry of a day ([venue-entry.md](venue-entry.md)): a door scan, the desk, or a Session Check-in that wrote the day's entry; once a day, every day of a several-day Event |
| Katılımın alındı | `event.attendance-recorded` | each Session Check-in, however written: the door's scan, the attendance QR, `check-in/me`, a guest's own (the attendance form), the desk |

Both go two minutes after the scan (`ticket.AttendanceMailDelay`). They are
**transactional mails** of the holder's own scan (ADR-0072 decision 2): `bilgi@`,
SkyMail's `system` lane, required types (the person cannot turn them off), no
unsubscribe header. A member gets a Notification (`POST /v1/notifications`:
inbox + e-mail) under an Idempotency-Key; a guest the template by address
(`POST /v1/mail_tasks/single`).

The day-end summary (`event.attendance-day-summary`, ticket 06) and the
organizer's venue note (`attendeeNote`, ticket 03) are not in core yet: the
welcome sends `AttendeeNote` empty.

## Switches

| Setting | Default | Effect |
|---|---|---|
| `ATTENDANCE_MAIL` | `off` | `on` queues the mails. Off, no scan queues anything; turned on later, the scans of meanwhile are not told. Anything but `on`/`off` stops core. |
| `SKYMAIL_ATTENDANCE_WELCOME_TEMPLATE_KEY` | `event.attendance-welcome` | empty: no welcome is queued |
| `SKYMAIL_ATTENDANCE_RECORDED_TEMPLATE_KEY` | `event.attendance-recorded` | empty: no receipt is queued |
| `ATTENDANCE_MAIL_PER_MINUTE` | `20` | mails a minute across replicas; `0` no bound |
| Event `attendanceWelcomeMail` | `true` | the organizer's switch for the welcome |
| Event `attendanceRecordedMail` | `true` | the organizer's switch for the receipt |

The Event's two switches are on the Event JSON (anyone who sees the Event sees
them); `POST /v1/events` takes them (none is `true`), `PATCH`/`PUT
/v1/events/{id}` turn them on or off (left out keeps them, `null` is `400`). A
copy keeps them. A switch turned off drops the rows still waiting.

Startup logs `attendance mail: on (welcome …, recorded …; 20/min)` or why it is
off.

## Queue

`attendance_mails` (migration `20261125203000`) holds ids only: `kind`
(`welcome` | `recorded`), `ticket_id`, `event_id`, `entry_date` (Istanbul day),
`session_id` (a receipt's), the attempt columns and `outcome`.

- **Same statement.** While on, the statement that writes the day's Venue entry
  writes the welcome row, and the one that writes a Check-in the receipt row
  (`ticket.PostgresStore.NotifyAttendance`): neither is there without the other.
- **Once.** One welcome a Ticket a day (`ticket_id, entry_date`), one receipt a
  Ticket a Session (`ticket_id, session_id`). A second scan is refused (`409`)
  and queues nothing; a scan after an undo finds the row and queues nothing, so
  nobody is told twice. A row that was **dropped** (its scan undone before it
  went) is queued again by the next scan.
- **Undo.** Just before it sends, the worker reads again whether the Venue entry
  or the Check-in is still there; undone within the two minutes, nothing goes
  (`dropped`). A mail already sent is not taken back.
- **Merging.** The worker claims welcomes first. A welcome takes the same
  Ticket's waiting receipts of that day into one mail (`Recorded` lists them); a
  receipt takes the same Ticket's other due receipts (`Sessions` lists them).
  The Idempotency-Key is `core:attendance-mail:<kind>:<hash of the rows' Ticket,
  day and Session>:v1`: the same rows always carry the same key.
- **Pace.** One replica at a time counts the minute's mails under an advisory
  lock (the reminder's pattern, [event-reminder.md](event-reminder.md)); a pass
  runs every 15 seconds. 300 people checking in at a Session's start go out in
  about 15 minutes at 20 a minute (Yusuf accepted, S4).
- **Read again** when it sends: the Event published and not archived, the
  Ticket active, the holder's account active, the switch on. Otherwise the row
  is `dropped` (or `skipped`: nobody to write to).
- **Failures.** SkyMail's `400`/`422` refusal ends the row (`rejected`);
  `422 notification.type_unknown` and every other failure are tried again,
  30 seconds doubling up to 30 minutes; a row a day old is `expired`. A guest's
  mail that went out with no answer is `uncertain` and not tried again.
- **Keep.** A done row stays 30 days (it keeps the "once"), then the worker
  deletes it. Rows name no person: an erased account's Tickets keep only ids.

## Variables

Every variable always goes (SkyMail renders with `missingkey=error`).

Counts are the certificate rule's as it stands when the mail goes
([certificate-rule.md](certificate-rule.md)): N = live counted Sessions, x =
the Ticket's Check-ins on them, M = the rule's threshold. With no rule, or
N = 0, there are none: `HasRule` false, the numbers 0, the lists empty.
Required Sessions not attended are split: still to come (`MissingRequired`)
and over (`MissedRequired`).

**Welcome:** `FirstName`, `EventName`, `EventId`, `OwnerTeam`, `DayLabel`
("14 Mart Cuma"; "2. gün · 15 Mart Cumartesi" on an Event of several days),
`Program` (`[{Time, Title}]`, the day's live Sessions, "10:00–10:45"),
`AttendeeNote` (empty for now), `CertificateLine` ("Sertifika için 16 oturumdan
en az 12 tanesine katılmalısın. Zorunlu: Açılış, Final."; empty without a
rule), `Recorded` (`[{Title}]`, the receipts that went along), `Attended`,
`CountedTotal`, `MinRequired`, `MissingRequired`, `MissedRequired`.

**Receipt:** `FirstName`, `EventName`, `EventId`, `OwnerTeam`, `Sessions`
(`[{Title}]`), `HasRule`, `Attended`, `CountedTotal`, `MinRequired`,
`MissingRequired`, `MissedRequired`, `NotCounted` (the mail's Sessions that do
not count).

## Turning it on

1. The two templates and Notification types are seeded in SkyMail (done in
   production 2026-11-24).
2. Core with this release; then `ATTENDANCE_MAIL=on`.
3. Rollback: `ATTENDANCE_MAIL=off` (waiting rows stay and are not sent while
   off; they expire after a day). Rolling the migration back drops the queue
   and the two switches.
