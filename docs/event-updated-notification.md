# An Event's new time or place (`event.updated`)

iletişim-servisi 06; ADR 0070 (core says what happened and to whom, SkyNotify
says by which channel and with which text). When a published Event gets a new
start, end or location, everyone holding a Ticket of it is told: by the in-app
inbox and by e-mail, as SkyNotify's Notification type `event.updated` says. It
is off until `EVENT_UPDATED_NOTIFICATION=on`.

## When

| Change | Told |
|---|---|
| `PATCH /v1/events/{id}` changes `startDate`, `endDate` or `location` of a published Event that is not over | every Ticket holder, once, 15 minutes after the first change; unless the body says `"notifyTicketHolders": false` ([the editor's choice](#the-editors-choice)) |
| Name, description, capacity, links or anything else | nobody |
| A draft's, a cancelled Event's, an archived or an ended Event's change | nobody |
| Changes within 15 minutes of the first | gathered: one Notification with the Event as it is then |
| A change undone within the 15 minutes (time and place back to what they were) | nobody |

The worker reads the Event again when it sends: an Event cancelled, archived,
unpublished or over by then tells nobody (the cancellation has its own mail,
[event-cancel.md](event-cancel.md#mails)).

## The editor's choice

The panel asks before it saves a new start, end or location of a published
Event that is not over: "Bilet sahiplerine bildir (N kişi)", ticked by
default, as in the cancel dialog. The update body carries the answer:

```http
PATCH /v1/events/{id}

{"startDate": "2026-12-04T18:00:00+03:00", "notifyTicketHolders": false}
```

| `notifyTicketHolders` | The change |
|---|---|
| `true` | told, as above |
| `false` | not told |
| left out (an older client, sky-app) | told, as before the question |
| `null` | refused, `400` |

The panel sends it with every save. It decides about this save's change of
time or place only; anything else in the same body is saved either way.

Within the 15 minutes the waiting notice gathers only what was said yes to.
The row keeps which of start, end and location the saves that said yes
changed (`told_fields`), and the mail's `ChangeSummary` names only those:

| Saves within the window | Told |
|---|---|
| start (yes), then location (no) | "Etkinliğin saati değişti." |
| start (yes), location (no), location again (yes) | "Etkinliğin saati ve yeri değişti." |
| location (no) alone | nothing; no notice is queued |
| start (yes), start again (no) | "Etkinliğin saati değişti.", with the newest start |

A no does not take back a notice already waiting, and the mail always shows
the Event as it is when sent (`NewStartsAt`, `NewEndsAt`, `NewVenue`): it
never shows an old time or place, even one changed by a save that said no.
A row the release before this one queued has no `told_fields`; it tells all
three, as it did.

Each answer is recorded in `event_change_notice_choices`, in the update's
transaction: the Event, who saved (`decided_by`, a `users` row; a principal
without one, such as a service account, is recorded as nobody), which fields
changed, the answer (`NULL` when the body did not say) and whether a notice
was queued. Only changes of a published Event that is not over are
recorded: of its time or place always, of its other details only while
`EVENT_CHANGED_NOTIFICATION` is on ([Other details](#other-details)). One
save is one row with all its changed fields. `queued` is true when any of
them joined a notice: its time or place told (not a no, while
`EVENT_UPDATED_NOTIFICATION` is on) or its details told (a yes, while
`EVENT_CHANGED_NOTIFICATION` is on); false for a no, and for an answer
whose switch is off. Ids and field names only: an
account deleted outright empties `decided_by`; an Event deleted takes its
rows. A row is kept three years after `decided_at`, as the Team audit
record, then deleted by the retention sweep's `event_change_notices_age`
rule ([retention-sweep.md](retention-sweep.md)). There is no route that
reads it yet:

```sql
SELECT decided_at, decided_by, fields, notify, queued
FROM event_change_notice_choices WHERE event_id = '…' ORDER BY decided_at;
```

## Other details

While `EVENT_CHANGED_NOTIFICATION=on`, the same question covers a published
Event's other details (etkinlik-bildirim-secenekleri b): its name
(`name`), description (`description`), attendee note (`attendeeNote`),
capacity (`capacity`) and links (`links`: the external registration link,
LinkedIn, the application form and the extra forms, by label and address).
The panel's box is then unticked by default.

| The save | Its details |
|---|---|
| `"notifyTicketHolders": true` | told |
| `false`, or the field left out (an older client) | not told |

They join the same waiting row and window. The row keeps, for each detail
field first told, the SHA-256 of its value before that change
(`before_details`; never the text), and the worker leaves out a field whose
value is back to it. The row then sends one Notification:

- a told start, end or location changed: `event.updated`, as above (the
  details are not named; the Ticket page shows the Event as it is);
- only told details changed: `event.changed` (Idempotency-Key
  `core:event.changed:<rowId>:<sub>:v1`), the same way to members and guests,
  with `EventId`, `EventName` (the name now), `ChangedFields` ("adı,
  açıklaması ve kontenjanı"), `ChangeSummary` ("Etkinliğin adı, açıklaması
  ve kontenjanı değişti."), `StartsAt`, `Venue`, and `TicketUrl` and
  `TicketCode` while `TICKET_PAGE_URL` is set. The new text is not in the
  mail.

Off, a change of details queues nothing and is not recorded; a waiting row
that told details alone tells nobody. The type needs SkyNotify's template
`event.changed` and its inbox text before it is turned on. The worker runs
while either switch is on; the startup line reads
`event updated notification: time and place on, other details off (event.updated, event.changed; changes gathered for 15m0s)`.

This is a transactional mail about the holder's own Ticket, not an
announcement: the type is required (no preferences, no consent, no unsubscribe
link) and rides SkyNotify's `system` lane, so it leaves from the transactional
mailbox.

## How

```text
Event update ──(same transaction)──▶ event_update_notifications ──(15 min)──▶ worker ──▶ SkyNotify
```

- The Event's update reads the row under its lock and, when the start, end
  or location changed on a published Event that was not over, records the
  editor's answer and, while `EVENT_UPDATED_NOTIFICATION=on` (and SkyNotify is
  configured) and the answer is not no, writes a row in the same
  transaction: what the Event said before, the fields to tell, due 15
  minutes later. Off, nothing is queued, so turning it on later tells none
  of the changes made meanwhile.
- One row per Event gathers the changes until the worker first takes it (a
  later yes adds its fields to `told_fields`); a change after that queues a
  row of its own, which waits until the first is done.
- The row holds the Event's id, its old times and place, never a person. The
  worker reads the Ticket holders when it sends, so a person erased or
  disabled meanwhile gets nothing.
- A registered holder is a Notification to their subject:

  ```http
  POST /v1/notifications
  Idempotency-Key: core:event.updated:<rowId>:<sub>:v1

  {"type": "event.updated",
   "recipients": [{"sub": "<ownerId>"}],
   "variables": {"EventId": "…", "EventName": "…", "ChangeSummary": "Etkinliğin saati değişti.",
                 "NewStartsAt": "20.11.2026 18:30", "NewEndsAt": "20.11.2026 21:00", "NewVenue": "…"},
   "source": {"kind": "event", "ref": "<eventId>"},
   "expires_at": "<the Event's end>"}
  ```

  The type is required, so no `allowed_channels`; SkyNotify reads the address
  from Keycloak. `expires_at` takes the inbox item away when the Event ends
  (its end, else a day after its start).
- A guest has no subject SkyNotify could take, so the guest gets the type's
  e-mail template alone, by address: `POST /v1/mail_tasks/single` with
  `template_key: event.updated`, the same variables and the guest's name. No
  inbox item. A guest Ticket under a registered holder's primary e-mail is
  theirs: one Notification, not two.
- The variables are the Event as it is when sent. `ChangeSummary` says what
  changed (`Etkinliğin saati değişti.`, `Etkinliğin yeri değişti.`,
  `Etkinliğin saati ve yeri değişti.`); times are Europe/Istanbul, empty
  without one. Every variable is always sent: SkyNotify renders the inbox text
  with `missingkey=error`. `EventId` fills the inbox item's link
  (`/events/{{.EventId}}`). The template's `EventUrl` is not sent: the site
  has no Event page, and the template leaves the button out when it is
  empty.
- **The holder's Ticket** (ADR 0082; bilet-qr-ve-pdf ticket 04, addendum),
  while `TICKET_PAGE_URL` is set ([ticket-page.md](ticket-page.md)):
  `TicketUrl`, the Ticket's page, whose QR is signed with the Event's new
  dates, and `TicketCode`. One holder, one Ticket: a registered holder's
  own before a guest Ticket under their address. Unset, the variables of
  before. A sensitive variable in the template (the link is a bearer key);
  optional there, since core sends it only with the page on. A mail
  attachment (bilet-qr-ve-pdf ticket 09) is not sent again with the change.

## Retries

- Each Ticket a Notification reached is recorded
  (`event_update_notification_sends`, ids only), so a retry tells only who
  was not reached. A member's key is the same on every try: a retry after a
  lost answer is SkyNotify's `Idempotent-Replayed`.
- A guest's mail has no key (`/v1/mail_tasks/single` reads none). When it
  went out and no answer came, SkyNotify may have taken it, so it counts as
  reached and is not tried again.
- `400` and `422` are permanent: that holder is counted as refused and the
  others go on. One `422` is not: `notification.type_unknown`, the type not
  seeded yet, passes once the Template seed has run. It, and anything else
  (SkyNotify down, `401`, `403` before Keycloak gives core
  `skynotify:notifications:send`, `409` before the type's e-mail or inbox text
  is there, `404` before a guest's template is seeded), keeps the row, tried
  again 30 seconds later, doubling to 30 minutes, for a day from the first
  change; then it is given up unsent.
- Several cores share the queue: a claim skips the row another holds, and
  renews it every 20 sends. A change that finds the row still gathering
  writes it too (a no-op), so the row stays locked until that change
  commits: a claim in between skips it rather than read the Event before the
  change.

Each pass that did something logs one line of counts, never a person:

```text
event updated notification: 1 changes done (0 dropped, 0 given up), 0 tried again later; 41 sent, 2 skipped, 0 refused by SkyNotify, 0 guest mails unanswered and not tried again
```

## Erasure

Nothing to do: the queue holds ids, times and the old place, no person. The
record of the editors' answers holds who saved: an account erased keeps its
anonymized `users` row and so its records; one deleted outright empties
`decided_by` (`ON DELETE SET NULL`). A
Ticket deleted takes its record of sends with it; an Event deleted, its rows.

## Before turning it on

1. Keycloak: `core`'s service account holds `skynotify:notifications:send`
   (already, for `event.ticket-created`).
2. SkyNotify: the type `event.updated` is seeded with its inbox text
   (skynotify-frontend Template seed: the template `event.updated` and
   `PUT /v1/notification_types/event.updated`).
3. `EVENT_UPDATED_NOTIFICATION=on`; the startup line reads
   `event updated notification: time and place on, other details off (event.updated, event.changed; changes gathered for 15m0s)`.

Rollback: `EVENT_UPDATED_NOTIFICATION=off` (or unset); the panel's question
then decides nothing (nothing is queued either way). The rows already
queued stay; turned on again within a day they are sent (if still news),
later they are given up. The release before the notification reads and
writes none of it, so the image can go back too. An image from before the
editor's choice reads neither `told_fields` nor `notifyTicketHolders`: it
tells every change of time or place, as before.
