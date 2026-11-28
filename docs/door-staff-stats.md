# Door staff statistics

`GET /v1/events/{eventId}/door-staff/stats`: who wrote what at an Event's
door. For the Event's organizers and YK after (or during) an Event: how
many people each door staff let in, how, when, and what they took back.
The same counts download as the attendee export's `door_staff` report
([attendee-export.md](attendee-export.md)).

## Who

- A reader of the Event's Tickets (`ticket.read`: the Privileged and
  Privileged readers, the organizer teams' Leaders, who may edit the Event,
  a Co-organizer person), as the attendee export: every row, and `others`.
- Who may only take check-ins at its door (its door staff, team door
  scan): their own row alone (`scope: "self"`, `others: null`). The door
  staff already read the door's counts and lists
  ([venue-entry.md](venue-entry.md)); another person's work is the
  organizers'.
- Anyone else `403`; a draft the caller may not see `404`.

## Answer

```json
{
  "scope": "all",
  "staff": [{
    "userId": "…", "name": "Elif Kaya",
    "entries": {"scan": 60, "desk": 4, "walkIn": 12, "total": 76},
    "checkIns": {"scan": 0, "desk": 3, "total": 3},
    "walkIns": 12, "undone": 1, "undid": 0,
    "firstAt": "2026-11-27T16:02:00Z", "lastAt": "2026-11-27T21:40:00Z",
    "hours": [{"hour": "2026-11-27T19:00:00+03:00", "count": 31}]
  }],
  "others": {"selfEntries": 20, "selfCheckIns": 0, "unattributedEntries": 0, "unattributedCheckIns": 5}
}
```

- A row is a person named as the writer of a mark (a Venue entry or a
  Session Check-in, `by_user_id`), of a Walk-in (the Team audit record's
  `ticket.walk_in` rows that wrote a Ticket) or of a mark taken back.
  `name` is the account's first and last name, never an e-mail;
  "Silinmiş kullanıcı" for an erased account, `""` for an account without
  a name. Rows are ordered by what they wrote (entries, Check-ins and
  Walk-ins), most first.
- `entries` and `checkIns` count what stands: a mark taken back is not in
  them. `entries.walkIn` is the entries written with a Walk-in (venue
  Entry mode); `walkIns` the Tickets the person wrote at the door, in
  either mode.
- `undone`: the person's marks the door took back; `undid`: the marks the
  person took back (`door_undos`, kept since 2026-11-28).
- `firstAt`, `lastAt`: the first and last of the person's marks and
  Walk-ins, by when the door took them (an offline scan's `scannedAt`).
- `hours`: the person's marks (entries and Check-ins) by Istanbul hour,
  `hour` its start, in order; hours without a mark are left out.
- `others`: the marks no door staff is named on: the people's own
  (`selfEntries`: entries written by an attendance QR Check-in;
  `selfCheckIns`: the attendance QR, `check-in/me`, `check-in/guest`), and
  the door's whose writer is not known (`unattributed…`: a Check-in written
  before core kept its writer, an erased account, a service).

## Before 2026-11-28

A Venue entry has always named its door staff. A Session Check-in did not
until migration `20261128120000`: an older one is counted by the Venue
entry it wrote in the same statement (an attendance QR's is the person's
own, a desk's is its door staff's); the others (a scan's, a second
Session's the same day) are `unattributedCheckIns`. Nothing is written
into the old rows. Marks taken back before then were deleted, so `undone`
and `undid` start on 2026-11-28.

## Not counted

A mark taken back is counted only in `undone` and `undid`. A failed scan or
a refused Walk-in writes nothing and is not counted (the door's metrics
count those, without names).
