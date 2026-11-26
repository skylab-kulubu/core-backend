# The reminder the day before an Event (`event.reminder`)

kontenjan-iptal-hatirlatma 08 (SPEC §3.5); ADR 0070 (core says what happened
and to whom, SkyNotify says by which channel and with which text). At 18:00
Istanbul time the day before a published Event starts, everyone holding an
active Ticket of it gets one reminder: by the in-app inbox and by e-mail, as
SkyNotify's Notification type `event.reminder` says. It is off until
`EVENT_REMINDER_NOTIFICATION=on`.

## When and to whom

| Case | Reminded |
|---|---|
| A published Event, not archived, starting the next Istanbul day | every active Ticket's holder, once, from 18:00 the evening before |
| A draft, a cancelled or an archived Event | nobody |
| A waitlisted or cancelled Ticket at planning or at sending | nobody |
| A Ticket written after the evening's planning | nobody (its own "biletin hazır" mail is enough) |
| A member's and a guest Ticket under the same address | one mail, the member's, carrying the member's own Ticket |
| A multi-day Event | once, before its first day; no reminder per session |
| The start moved within the same day after planning | the planned reminder, with the new time |
| The start moved to another day | the old day's rows are dropped; the new day earns a reminder of its own |
| core down at 18:00 | planned on its first pass after, until 3 hours before the start; later, not at all |
| A row not sent by the Event's own day (00:00 Istanbul) | given up: the mail says "Yarın" |

This is a transactional mail about the holder's own Ticket, not an
announcement: the type is required (no preferences, no consent, no
unsubscribe link; Yusuf, 2026-11-24) and rides SkyNotify's `system` lane, so it
leaves from the transactional mailbox. Luma lets people turn its reminder
off; this is a deviation.

Istanbul keeps UTC+3 all year; the day is read from `Europe/Istanbul` all the
same.

## How

```text
pass (every 15 s, every replica)
  ├─ from 18:00: plan  ──(pg_try_advisory_xact_lock)──▶ event_reminders + event_reminder_mails
  └─ send ──(pg_advisory_xact_lock: count the last minute, claim one row)──▶ SkyNotify
```

- **Planning** takes `pg_try_advisory_xact_lock`: one replica plans, another
  skips that pass. It picks the published, unarchived Events starting in
  `[tomorrow 00:00, the day after 00:00)` Istanbul time and more than 3 hours
  after now that have no `event_reminders` row for that day
  (`(event_id, starts_on)` is the key), writes that row and one
  `event_reminder_mails` row for each Ticket active at that moment, in one
  transaction.
- **The queue holds ids** (Event, Ticket, start), never a person. The holder
  and the address are read when the mail goes, so a person erased or
  disabled meanwhile gets nothing.
- **Sending** claims the row due first under a 5-minute lease, then reads the
  Event and the Ticket again: an Event gone, cancelled, unpublished, archived,
  moved to another day or started drops the row; a Ticket no longer active
  skips it. The other open rows of the same reminder under the same address
  (a guest Ticket under it, a member whose primary e-mail it is) join the
  mail and are done with it.
- A registered holder is a Notification to their subject:

  ```http
  POST /v1/notifications
  Idempotency-Key: core:event.reminder:<eventId>:<startsUnix>:<ticketId>

  {"type": "event.reminder",
   "recipients": [{"sub": "<ownerId>"}],
   "variables": {"EventId": "…", "EventName": "…", "StartsAt": "25.11.2026 10:00", "Venue": "…",
                 "OwnerTeam": "…", "TicketUrl": "…", "TicketQrUrl": "…", "TicketCode": "…", "MapUrl": "…"},
   "source": {"kind": "event", "ref": "<eventId>"},
   "expires_at": "<the Event's end>"}
  ```

  The key names the type `event.reminder` whatever template key is set. The
  type is required, so no `allowed_channels`.
- A guest gets the template alone, by address: `POST /v1/mail_tasks/single`
  with `template_key` = `SKYNOTIFY_EVENT_REMINDER_TEMPLATE_KEY`, the same
  variables and the guest's name. No inbox item.
- **Variables** (skynotify-frontend `emails/event-reminder.tsx`, #179): the
  Event as it is when sent. `StartsAt` is `dd.MM.yyyy HH:mm` Istanbul time.
  `TicketUrl`, `TicketQrUrl` and `TicketCode` while `TICKET_PAGE_URL` is set
  (the ticket created mail's rule; `TicketQrUrl` also needs
  `PUBLIC_API_ORIGIN`); `MapUrl` is the ticket page's `directionsUrl`.
  `TicketNote`, `TicketNoteParts`, `TicketNoteLink` and
  `TicketNoteWhatsApp` are the organizer's announcement
  ([ticket-note.md](ticket-note.md)).
  Every variable is always sent, empty without a value: SkyNotify renders with
  `missingkey=error`. `EventId` fills the inbox item's link.

## Pace

The transactional mailbox (bilgi@) sends 30 mails a minute. The reminders of
an evening go out together, so the worker keeps its own bound,
`EVENT_REMINDER_PER_MINUTE` (unset: 20; 0: none), leaving room for the other
mails (new Tickets, cancellations, changes) in the same minute. The bound
counts the rows that led a mail in the last 60 seconds, under an advisory
lock, so it holds across replicas; a row dropped or skipped without a mail
gives its place back. At 20 a minute, 300 Tickets take 15 minutes, 1,200 an
hour; everything planned at 18:00 is out well before midnight unless a day
holds more than ~7,000 active Tickets.

## Retries

- A member's key is the same on every try: a retry after a lost answer is
  SkyNotify's `Idempotent-Replayed`.
- A guest's mail has no key. When it went out and no answer came, SkyNotify may
  have taken it: counted as uncertain and not tried again.
- `400` and `422` are permanent (refused), except
  `notification.type_unknown` (the type not seeded yet). That, and anything
  else (SkyNotify down, `401`, `403`, `409`, `404`), keeps the row, tried again
  30 seconds later, doubling to 30 minutes, until the Event's day begins;
  then it is given up.

Each pass that did something logs one line of counts, never a person:

```text
event reminder: 3 events and 212 tickets planned; 20 sent, 0 dropped, 1 skipped, 0 refused by SkyNotify, 0 expired, 0 guest mails unanswered and not tried again, 0 tried again later
```

## Erasure

Nothing to do: the tables hold ids and times. A Ticket deleted takes its row
with it (`ON DELETE CASCADE`); an Event deleted, its rows and its plan.

## Before turning it on

1. Keycloak: `core`'s service account holds `skynotify:notifications:send`
   (already, for `event.ticket-created`).
2. SkyNotify: the type `event.reminder` is seeded with its inbox text
   (skynotify-frontend #179: the template and
   `PUT /v1/notification_types/event.reminder`).
3. `EVENT_REMINDER_NOTIFICATION=on`; the startup line reads
   `event reminder: on (event.reminder, 18:00 Europe/Istanbul, 20/min)`.
   Turned on after 18:00, the next day's Events are planned on the first pass.

Rollback: `EVENT_REMINDER_NOTIFICATION=off` (or unset). The rows already
queued stay; turned on again the same evening they are sent, after the
Event's day began they are given up. The previous release reads none of the
tables, so the image can go back too; the down migration drops them.
