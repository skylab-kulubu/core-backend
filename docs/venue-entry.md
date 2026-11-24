# Venue entry and the door's mode

kapi-postalari tickets 01 and 02 (SPEC §4). CONTEXT: "Entry mode",
"Venue entry".

A **Venue entry** is a Ticket entering the venue on a day: one a Ticket a
day. A **Check-in** is attendance at a Session. They are separate: a Venue
entry is never counted for a certificate (the certificate rule reads
Check-ins alone), and a Check-in never needs a Venue entry first.

## The door's mode: `entryMode`

`events.entry_mode` is what a door scan writes:

| `entryMode` | `effectiveEntryMode` | A scan at a Session's door writes |
|---|---|---|
| `null` (automatic, every Event before this) | `venue` without a live Session, else `per_session` | as the effective mode |
| `"per_session"` | `per_session` with a live Session, else `venue` | the Check-in to that Session, and the day's Venue entry if none yet |
| `"venue"` | `venue` | the day's Venue entry alone; the Sessions take their attendance by the attendance QR |

A live Session is one that is not archived or cancelled, on a day that is
not archived. An Event with none can only work as `venue`: its door is the
Event's (below).

- The Event's answer carries both: `entryMode` (stored; `null` is
  automatic) and `effectiveEntryMode` (now). The public `Resource` and the
  door's Event list carry `effectiveEntryMode`.
- `POST /v1/events` and `PUT`/`PATCH /v1/events/{id}` take `entryMode`:
  `"per_session"`, `"venue"`, or `null` back to automatic. A body without
  it keeps it. Anything else is `400`.
- A copy keeps it (it is how the Event is run).

## The day

`entry_date` is a Europe/Istanbul calendar day.

- Written with a Session Check-in, or by a scan at a Session's door: the
  Check-in's EventDay's date (its start), else the Session's start, else
  the day of the write. `event_day_id` is that EventDay.
- Written at the Event's door: the day of the scan; `event_day_id` is the
  EventDay that starts that day, if any.

## Who writes what

| Way in | `per_session` | `venue` |
|---|---|---|
| Door scan at a Session, `POST /v1/sessions/{id}/check-in/scan` (and `/skypass`): Ticket QR, SkyPass, Wallet, card | Check-in + the day's entry if none (`scan`) | the day's entry alone (`scan`), `201 {…, "kind":"entry"}`; a second that day `409 already_entered` |
| Event door scan, `POST /v1/events/{id}/entries/scan` | the day's entry (`scan`) | the same |
| Attendance QR, `check-in/me`, `check-in/guest` | Check-in + the day's entry if none (`attendance`) | the same |
| Session desk, `check-in/resolve`, and `POST /v1/tickets/{t}/sessions/{s}/check-in` | Check-in + the day's entry if none (`desk`) | the same |
| Event desk, `POST /v1/events/{id}/entries/resolve` | the day's entry (`desk`) | the same |

- Nobody is refused attendance for not having been scanned at the door
  (Yusuf, 22 November): the Check-in writes the missing entry in the same
  statement.
- Only an `active` Ticket enters: a cancelled or waitlisted one gets
  neither a Check-in nor an entry (`409 ticket_cancelled` /
  `ticket_waitlisted`).
- `by_user_id` is the door staff who wrote it when that is an active
  account; a principal without one (a service) writes it with none.
  Account erasure clears it. The entry names no person: it lives as long as
  its Ticket (ADR-0062) and an erased person's Ticket keeps its entries,
  anonymous.

## Taking back

- **A Check-in** (`DELETE /v1/tickets/{t}/sessions/{s}/check-in`,
  `DELETE /v1/sessions/{s}/check-ins/{id}`): the entry that Check-in wrote
  (`attendance` or `desk`) goes, unless the Ticket has another Check-in that
  day, which then holds it (and takes it back when it is undone itself). A
  scanned entry (`scan`) stays.
- **A scan in the venue mode**: its answer's `id` is the entry's, and
  `DELETE /v1/sessions/{s}/check-ins/{id}` takes that entry back with the
  same rules, so the door's undo works in either mode.
- **An entry at the Event's desk**: `DELETE /v1/events/{id}/entries/{entryId}`,
  `204`; `404` when there is none (or it is another Event's). The day's
  Check-ins stay: attendance does not depend on the door.

## The Event's door (no Session picked)

For an Event without Sessions this is the only door; any Event has it.

| Route | Body / query | Answer |
|---|---|---|
| `POST /v1/events/{id}/entries/scan` | `{"token"}` (Ticket QR `tk-*`, SkyPass QR, Wallet `SPW1:…`) or `{"uid"}` (card) | `201 {"id","kind":"entry","entryDate","eventDayId"?,"source","name","createdAt"}` |
| `POST /v1/events/{id}/entries/resolve` | `{"query"}` (Ticket code `XXXX-XXXX`, name, e-mail) or `{"personId"}` | as scan, `source: "desk"` |
| `DELETE /v1/events/{id}/entries/{entryId}` | | `204` |
| `GET /v1/events/{id}/entries/summary` | | `{"days":[{"date","eventDayId"?,"total","bySource":{"scan","desk","attendance"}}]}` |
| `GET /v1/events/{id}/entries?date=YYYY-MM-DD` | today without `date` | `[{"id","kind","entryDate","eventDayId"?,"source","name","createdAt"}]`, newest first |

No answer carries the Ticket's id or an e-mail, as the door's other
answers. `attendance` in the summary is who came in without being scanned
at the door that day.

The order, as the Session door's, all before any code is read:

1. No such Event, or a draft the caller may not see: `404`.
2. To write: a draft, a cancelled or archived Event, or one that ended over
   a day ago (the Ticket QR's own window): `403 event_closed` with
   `reason` (`event_not_published`, `event_cancelled`, `event_archived`,
   `event_ended`).
3. To write, the caller must be able to take check-ins at the Event's door
   (door staff, the owning team's leaders, Privileged, team door scan):
   else `403`. To read (summary, list) who may read the Event's Tickets
   reads too (DK).

Then the code, with the Session scan's answers: `401 ticket_qr_invalid` /
`ticket_qr_expired`, `410 ticket_qr_superseded`, `404` for another Event's
Ticket or none. A second entry the same day is `409 already_entered` with
`name` (who is already in). The desk: `404` none, `409` (title "Ambiguous
Match") more than one.

Each entry and undo at the Event's door logs one line:
`{"event":"venue_entry","actorId":…,"eventId":…,"source":…}` and
`{"event":"venue_entry_undo","actorId":…,"eventId":…,"entryId":…}`; no
names.

## The Session's door activity

`GET /v1/sessions/{id}/check-ins` lists the Session's Check-ins
(`kind: "check_in"`). In the venue mode it lists the day's Venue entries
too (`kind: "entry"`, `sessionId` the Session asked for), newest first,
`total` counting both: the door shows who came in today.

## sky-app (kapi-postalari ticket 09)

- Today's sky-app picks a Session and calls `check-in/scan`; nothing breaks.
  In the venue mode the answer is `201` with `kind: "entry"` (its fields
  are the same) and `409` for a second scan that day; its texts stay right.
- An Event without Sessions cannot be scanned by today's sky-app (it needs
  a Session): the Event's door routes above are the contract for a new
  version, which reads `effectiveEntryMode` from the door's Event list.

## Metrics

`skylab_venue_entries_total{source="scan"|"desk"|"attendance"}` on
`/v1/metrics`: entries written. No ids, no names.
