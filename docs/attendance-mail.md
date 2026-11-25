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

The day-end summary (`event.attendance-day-summary`, ticket 06) is not in
core yet.

## The attendee note

`attendeeNote` (ticket 03; Yusuf, 2026-11-24 S3) is the organizer's note for
whoever enters: the venue's Wi-Fi, the way to the room. Plain text, at most
1000 characters (`events.attendee_note`, a database check too), trimmed, line
breaks kept; a longer text, a control character other than a line break or a
tab, a direction override or isolate (U+202A–U+202E, U+2066–U+2069) or a
zero-width character (U+200B–U+200D, U+2060, U+FEFF) is
`400 event_attendee_note_invalid`.

- `POST /v1/events` and `PUT /v1/events/{id}` take it (left out keeps it);
  `PATCH` writes it, `null` or `""` clears it. A copy does not take it (the
  Wi-Fi changes).
- **Only who may edit the Event, or read its Tickets (DK, read-only), reads
  it**: their Event answer carries `attendeeNote` (`""` for none). Every other answer, the public ones, the
  lists, the door's and sky-app's, leaves the key out: the stored field never
  goes into JSON by itself (`Event.AttendeeNote` is `json:"-"`; `ProjectFor`
  copies it for an editor).
- It goes in the welcome mail alone (`AttendeeNote`; SkyMail masks no
  variable, ADR-0074 2026-11-24 ek: it is mail content whoever holds
  `mails:read` reads, and its links are tracked as any other), with
  `AttendeeNoteParts`: the same text cut at its `http(s)://`
  addresses (`[{Text, Url}]`, trailing punctuation left out) for the template
  to link them. Empty, the template leaves the section out. Logs and metrics
  never carry it.

## Switches

| Setting | Default | Effect |
|---|---|---|
| `ATTENDANCE_MAIL` | `off` | `on` queues the mails. Off, no scan queues anything; turned on later, the scans of meanwhile are not told. Anything but `on`/`off` stops core. |
| `SKYMAIL_ATTENDANCE_WELCOME_TEMPLATE_KEY` | `event.attendance-welcome` | empty: no welcome is queued |
| `SKYMAIL_ATTENDANCE_RECORDED_TEMPLATE_KEY` | `event.attendance-recorded` | empty: no receipt is queued |
| `ATTENDANCE_MAIL_PER_MINUTE` | `20` | mails a minute across replicas; `0` no bound |
| Event `attendanceWelcomeMail` | `true` | the organizer's switch for the welcome |
| Event `attendanceRecordedMail` | `true` | the organizer's switch for the receipt |

Whether a scan queues each mail now, with every setting above but the Event's
switches applied, is in `GET /v1/users/me/capabilities` as
`features.attendanceWelcomeMail` and `features.attendanceRecordedMail`
(`ticket.PostgresStore.AttendanceMailOn`): the panel greys the Event's switches
out while they are `false`.

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
  or the Check-in is still there, and a receipt's Session still live (not
  cancelled or archived); otherwise that row goes `dropped`. A mail already
  sent is not taken back.
- **Merging.** The worker claims welcomes first. A welcome takes the same
  Ticket's waiting receipts of that day into one mail (`Recorded` lists them); a
  receipt takes the same Ticket's other due receipts (`Sessions` lists them).
  The Idempotency-Key is `core:attendance-mail:<kind>:<hash of the rows' Ticket,
  day and Session>:v1`: the same rows always carry the same key.
- **Retry.** The first try writes its key on its rows (`mail_key`). A retry
  takes only the rows of that try and sends under that key, whatever dropped
  out since: SkyMail may have taken the first try without an answer, and then
  answers the retry from it instead of mailing again; SkyMail's
  `idempotency_key_reused` (fewer rows since) means the first try went, and the
  rows are `sent`. A row queued since goes in a mail of its own.
- **Pace.** One replica at a time counts the minute's mails under an advisory
  lock (the reminder's pattern, [event-reminder.md](event-reminder.md)); a pass
  runs every 15 seconds. A mail counts once in the minute, also when the row
  that led it dropped out and another row of it went. 300 people checking in at a Session's start go out in
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
`AttendeeNote` (the organizer's note, empty for none), `AttendeeNoteParts`
(`[{Text, Url}]`, the note cut at its web addresses), `CertificateLine` ("Sertifika için 16 oturumdan
en az 12 tanesine katılmalısın. Zorunlu: Açılış, Final."; on an Event with
no live Session and a rule, "Girişin sertifika için sayıldı."; empty without a
rule), `Recorded` (`[{Title}]`, the receipts that went along), `Attended`,
`CountedTotal`, `MinRequired`, `MissingRequired`, `MissedRequired`.

**Receipt:** `FirstName`, `EventName`, `EventId`, `OwnerTeam`, `Sessions`
(`[{Title}]`), `HasRule`, `Attended`, `CountedTotal`, `MinRequired`,
`MissingRequired`, `MissedRequired`, `NotCounted` (the mail's Sessions that do
not count).

A scan an offline door sends later queues these mails only when it was
taken less than an hour before it reached core ([offline-door.md](offline-door.md#mails)).

## Turning it on

1. Core with this release (`ATTENDANCE_MAIL` stays off).
2. The welcome template's seed with `AttendeeNoteParts` (skymail-frontend
   #186); the templates and types themselves are in production since
   2026-11-24.
3. `ATTENDANCE_MAIL=on`.
3. Rollback: `ATTENDANCE_MAIL=off` (waiting rows stay and are not sent while
   off; they expire after a day). Rolling the migration back drops the queue
   and the two switches.
