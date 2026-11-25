# A person added to the door staff (`event.door-staff-added`)

etkinlik-bildirim-secenekleri (c); Yusuf, 2026-11-25: a person added to an
Event's door staff is always told, nobody is asked. ADR 0070: core says what
happened and to whom; SkyNotify picks the channels (inbox and e-mail) and the
text. Off until `DOOR_STAFF_MAIL=on`.

## When

| Change | Told |
|---|---|
| `POST /v1/events` with `doorStaffIds`, `PATCH`/`PUT /v1/events/{id}` adding a person to `doorStaffIds`, a copy with `includeDoorStaff` | each person new to the Event's door staff, once, two minutes later |
| A person who was already on the door staff (the panel sends the whole list on every save) | nobody |
| Added and taken off again within the two minutes | nobody |
| A draft's door staff | waits: told once the Event is published (looked at again every hour), for 30 days |
| Cancelled, archived or ended Event; taken off meanwhile; account not active | nobody |
| Taken off and added again later | told again (a new row) |

Only a person with a `users` row is queued (door staff are members). It is a
transactional mail about their own task: a required type, SkyNotify's
`system` lane, no preferences, no unsubscribe link.

## How

```text
Event write ──(same transaction)──▶ door_staff_notifications ──(2 min)──▶ worker ──▶ SkyNotify
```

- While `DOOR_STAFF_MAIL=on` (and SkyNotify is configured), the write that
  replaces the Event's door staff compares it with what it was and queues a
  row for each person added (`event_id`, `user_id`, due two minutes later).
  One row waits per Event and person. Off, nothing is queued: turning it on
  later tells none of the people added meanwhile.
- The worker reads the Event and the person when it sends:

  ```http
  POST /v1/notifications
  Idempotency-Key: core:event.door-staff-added:<rowId>:v1

  {"type": "event.door-staff-added",
   "recipients": [{"sub": "<userId>"}],
   "variables": {"FirstName": "Ada", "EventId": "…", "EventName": "…",
                 "StartsAt": "04.12.2026 18:00", "EndsAt": "04.12.2026 21:00",
                 "Venue": "…", "DoorUrl": "https://…/events/<id>/door"},
   "source": {"kind": "event", "ref": "<eventId>"},
   "expires_at": "<the Event's end>"}
  ```

  Every variable is always sent (SkyNotify renders the inbox text with
  `missingkey=error`); times are Europe/Istanbul, empty without one.
  `DoorUrl` is the panel's door page under `PANEL_URL`; empty without it,
  and the template leaves the button out.

## Retries

`400` and `422` are permanent (the row is done with, `refused`), except
`notification.type_unknown` (the type not seeded yet). That, and anything
else (SkyNotify down, `401`, `403`, `409`), keeps the row, tried again 30
seconds later, doubling to 30 minutes, until 30 days after the person was
added. Several cores share the queue: a claim skips the row another holds.
A pass that did something logs one line of counts, never a person:

```text
door staff mail: 1 sent, 0 dropped, 2 waiting for publication, 0 given up, 0 refused by SkyNotify, 0 tried again later
```

## Erasure

The row holds ids. A `users` row deleted outright takes it (`ON DELETE
CASCADE`); an erased (anonymized) account is no longer active, and the worker
drops its row when it comes due.

## Before turning it on

1. Keycloak: `core`'s service account holds `skynotify:notifications:send`
   (already, for `event.ticket-created`).
2. SkyNotify: the type `event.door-staff-added` is seeded with its template
   and inbox text (skynotify-frontend; waits for the SkyNotify move).
3. `PANEL_URL`, the panel's origin: `https://admin.yildizskylab.com` in
   production (sandbox: the sandbox panel's origin). Optional: unset, the
   mail has no door link. Anything but a bare `https://` origin (`http://`
   only for localhost) turns the mail off, and the startup line says why.
4. `DOOR_STAFF_MAIL=on`; the startup line reads
   `door staff mail: on (event.door-staff-added, after 2m0s; door link on the panel)`.

Rollback: `DOOR_STAFF_MAIL=off`. Waiting rows stay and are not sent while
off. The release before reads and writes none of it, so the image can go
back too; rolling the migration back drops the queue.
