# Walk-in at the door (`POST /v1/events/{id}/entries/walk-in`)

"Kapıda kayıt" (Yusuf, 2026-11-26, ARTLAB): a person comes to the door
without a Ticket. Who may take check-ins at the Event's door registers
them there: core writes their guest Ticket, its mail goes as any new
Ticket's, and in the `venue` Entry mode their entry for the day is written
in the same request. CONTEXT "Walk-in"; ADR 0093.

## Who

Exactly who may write the Event's door (`eventDoor`, as `entries/resolve`
and `entries/scan`): the Event's Door staff, a member of a team with Team
door scan, its Leaders and Co-organizer people, `ticket:validate`. Anybody
else `403`; a draft (to who may see it), a cancelled or archived Event, or
one that has ended `403 event_closed` with `reason`; no Event `404`. A
Walk-in is refused once the Event has ended (`event_ended`), not only a day
after as the door's scans.

## Body

```json
{"firstName": "Ayşe", "lastName": "Yılmaz", "email": "ayse@example.com",
 "phoneNumber": "0532 123 45 67", "university": "Yıldız Teknik Üniversitesi",
 "department": "Bilgisayar Mühendisliği", "grade": "2"}
```

- `firstName`, `lastName`, `email` are required (`400 guest_fields_required`
  with `fields`), names up to 100 characters (`guest_name_too_long`). The
  e-mail is trimmed and lower-cased, and must look like a mailbox: one `@`,
  a domain with a dot, no spaces (`email_invalid`). Whether it receives
  mail is not asked: the Ticket's mail tells.
- The rest is optional, for the Event's statistics (KVKK: only what is
  asked for, nothing required):
  - `phoneNumber`: spaces, dashes, dots and parentheses are dropped. A
    Turkish number, `+90`, `90` or `0` and its ten digits, or the ten digits
    alone, is stored `+90XXXXXXXXXX`; another country's, `+` (or `00`) and 8
    to 15 digits, `+` and its digits. Anything else `phone_invalid`.
  - `university`, `department`: up to 200 characters
    (`university_too_long`, `department_too_long`).
  - `grade`: one of `prep`, `1`, `2`, `3`, `4`, `masters`, `phd`,
    `graduate`, `other` (`grade_invalid`); the panel shows Hazırlık, 1–4,
    Yüksek lisans, Doktora, Mezun, Diğer.
- They are stored on the guest Ticket (`guest_phone_number`,
  `guest_university`, `guest_department`, `guest_grade`; JSON
  `guestPhoneNumber`, `guestUniversity`, `guestDepartment`, `guestGrade`),
  read by who reads the Event's Tickets, never shown at the door. The phone
  number goes 90 days after the Event (`guest_phone`), the rest with the
  guest's identity (`guest_identity` v4, [retention-sweep.md](retention-sweep.md)),
  and on account erasure of the address.

## What happens

1. The person's Ticket is looked for by the e-mail: a guest Ticket written
   for it, then the Ticket of the member whose account e-mail (primary or
   school) it is.
   - Active: nothing is written to it, `200` with `"result": "existing"`.
   - Waitlisted: `409 ticket_waitlisted` (an operator promotes it).
   - Cancelled by an operator or by its form: `409 ticket_cancelled` (an
     operator restores it). Cancelled by its holder: reopened under the
     capacity, as their registering again would, `201 "created"`.
2. Without one, a **guest** Ticket is written (always guest, as CONTEXT's
   Walk-in: even when the e-mail is a member's account's, the door writes
   no Ticket into an account) under the Event's capacity: a full Event is
   `409 event_full`, never its waiting list. Two requests for the same
   e-mail write one Ticket; the second is answered `existing`.
3. Its mail goes as any new Ticket's (`event.ticket-created` while
   `TICKET_CREATED_NOTIFICATION=on`, queued in the statement that writes
   it; [ticket-created-notification.md](ticket-created-notification.md)):
   the person needs the Ticket for the Sessions' attendance QR and their
   certificate. A draft does not get here, so the mail is never dropped for
   one.
4. In the `venue` Entry mode the day's entry is written with the Ticket:
   source `walk_in` for a Ticket the Walk-in wrote, `desk` for one it
   found. A Ticket already in today is `409 already_entered` with `name`.
   In `per_session` no entry and no Check-in is written: the door then
   checks the person in to a Session as anybody else.

The Ticket and the entry are two writes of one request: if the entry
fails after the Ticket was written, sending the form again finds the
Ticket (`existing`) and writes the entry (`desk`).

## Answer

```json
{"result": "created", "name": "Ayşe Yılmaz", "ticketCode": "K7M2-QX9F", "ticketMail": true,
 "entry": {"id": "…", "kind": "entry", "entryDate": "2026-11-27", "source": "walk_in", "name": "Ayşe Yılmaz", "createdAt": "…"}}
```

`201` when a Ticket was written, `200` when it was already there.
`ticketMail` says the Ticket's mail was queued with this write (never for
`existing`). `entry` is `null` in `per_session`. No e-mail, phone number or
the rest of what the guest gave is answered.

## Record and limits

- A written Ticket is a Team audit row `ticket.walk_in` against the Event's
  team ([team-audit.md](team-audit.md)): `after` the Event and the Ticket,
  never a name, an e-mail or a phone number. The basis is `privileged`,
  `leader` (a Leader of the Event's team or of one of its Co-organizer
  teams, or a Team above), else `door_staff` through the Event's team
  (migration `20261127220000`). The row is opened before anything is
  written; if it cannot be, `503 audit_unavailable` and nothing is written.
  A Walk-in that found the Ticket writes no row (the entry carries
  `by_user_id`, as every door write).
- One log line per written Ticket, `{"event":"walk_in","actorId":…,"eventId":…,"ticketId":…}`,
  naming nobody else.
- Budget per signed-in subject: 60 requests in ten minutes, 300 answered a
  day; past it `429 walk_in_rate_limited` with `retryAfterSeconds`.
