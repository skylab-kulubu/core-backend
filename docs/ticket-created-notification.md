# The new Ticket's Notification (`event.ticket-created`)

iletişim-servisi 06; ADR 0070 (core says what happened and to whom, SkyMail
says by which channel and with which text). A person whose Ticket was just
written is told that it is there: by the in-app inbox and by e-mail, as
SkyMail's Notification type `event.ticket-created` says. Until this release
no ticket mail went at all; it is off until `TICKET_CREATED_NOTIFICATION=on`.

## When

| Write | Told |
|---|---|
| Member apply (`POST /v1/events/{id}/applications/me`) | the person, by subject |
| Apply-for-other from the panel | the person added, by subject |
| Guest apply (the panel's "Katılımcı ekle", Forms, the public route) | the guest, by address |
| Guest apply that finds the Ticket already there, a Ticket updated | nobody: no new Ticket |

Only a Ticket of a **published** Event that is **not over** is told of, when
the worker sends it: a draft's Tickets (speakers, the team and guests,
added from the panel, or applications from a form linked to the draft), a cancelled or
archived Event's and a past Event's (over at its end date, else a day after
its start, as everywhere in core: an Event without an end date is over too)
are dropped. A draft's Tickets are told of later only if the organizer asks
when publishing it (below). Walk-in
(CONTEXT.md) writes no Ticket in core today; when it does, it must not queue
one ("no ticket mail").

## Publishing a draft

The worker drops a draft's row at its first pass, so a Ticket written while
the Event was a draft would never be told of. The publish dialog asks (Yusuf,
2026-11-22, K3): `POST /v1/events/{id}/publish` with
`{"notifyTicketHolders": true}` queues a `ticket_notifications` row for each
of the Event's Tickets in the transaction that publishes it (a row still
queued stays as it is; [event-drafts.md](event-drafts.md#publish-and-unpublish)).
The panel shows the box checked, with how many people and who; speakers and
the team get the mail too, which says what is true: they have a Ticket.

- Only while `TICKET_CREATED_NOTIFICATION=on`: off, the publication queues
  nothing, whatever the body says. The panel shows the box either way (it
  does not read the switch; production has it on), so with the switch off a
  checked box sends nothing.
- Only when the call publishes the draft. An Event already published, or a
  call without the field (sky-app, an older panel), queues nothing.
- The worker then sends these rows as any other: a Ticket whose Event ended
  meanwhile, or a person erased since, gets nothing.
- The publication logs one line of the count, naming nobody:
  `event <id>: published, 3 ticket notifications queued`.

## How

```text
Ticket insert ──(same statement)──▶ ticket_notifications ──▶ worker ──▶ SkyMail
```

- While `TICKET_CREATED_NOTIFICATION=on` (and SkyMail is configured), the
  Ticket's `INSERT` writes a `ticket_notifications` row in the same statement:
  neither exists without the other. Off, nothing is queued, so turning it on
  later tells none of the Tickets written meanwhile.
- The row holds the Ticket's id alone. The worker reads the Ticket, its
  Event and its holder when it sends, so a person erased or disabled
  meanwhile gets nothing.
- A registered holder is a Notification to their subject:

  ```http
  POST /v1/notifications
  Idempotency-Key: core:event.ticket-created:<ticketId>:v1

  {"type": "event.ticket-created",
   "recipients": [{"sub": "<ownerId>"}],
   "variables": {"EventId": "…", "EventName": "…", "EventDate": "10.11.2026 18:30", "Venue": "…", "OwnerTeam": "WEBLAB",
                 "TicketUrl": "https://yildizskylab.com/bilet/<key>", "TicketQrUrl": "https://api.yildizskylab.com/v1/public/tickets/<key>/qr.png", "TicketCode": "K7M2-QX9F"},
   "source": {"kind": "ticket", "ref": "<ticketId>"},
   "expires_at": "<the Event's end>"}
  ```

  The type is required (a ticket), so no `allowed_channels`; SkyMail reads the
  address from Keycloak. `expires_at` takes the inbox item away when the Event
  ends; an Event without an end leaves it at SkyMail's 90 days.
- A guest has no subject SkyMail could take (`recipients[].sub` is required),
  so the guest gets the type's e-mail template alone, by address:
  `POST /v1/mail_tasks/single` with `template_key: event.ticket-created`, the
  same variables and the guest's name. No inbox item.
- `OwnerTeam` is the template's "Düzenleyen" row: the organizer as the club
  shows it (Yusuf, 2026-11-23), "SKY LAB: Bilgisayar Bilimleri Kulübü" for
  an Event of YK, DK or ADMIN or of no Owner team, else the Owner team's
  code (`internal/organizer`). The variable keeps its name, so SkyMail's
  template needs no change.
- `EventDate` is the start in Europe/Istanbul, empty without one: every
  variable is always sent, since SkyMail renders the inbox text with
  `missingkey=error`. `EventId` fills the inbox
  item's link (`/events/{{.EventId}}` in SkyMail's type text).
- **The Ticket's own three** (ADR 0082; bilet-qr-ve-pdf ticket 04), while
  `TICKET_PAGE_URL` is set ([ticket-page.md](ticket-page.md)), to a member
  and a guest alike:
  - `TicketUrl`: the Ticket's page, the "Biletini aç" button. The link is a
    bearer key: a sensitive variable in the template.
  - `TicketQrUrl`: the Ticket QR as a remote PNG on `PUBLIC_API_ORIGIN`
    (`/v1/public/tickets/<key>/qr.png`), signed when the mail is opened.
    Empty when `PUBLIC_API_ORIGIN` is unset. No base64 or CID image: Gmail
    shows a remote image through its proxy, classic Outlook shows none and
    the button and the code do the work.
  - `TicketCode`: the code as text, `XXXX-XXXX`.

  Unset, the five of before go, as before. `EventUrl` is never sent: the
  site has no Event page. The Idempotency-Key stays `:v1`: the three are the
  same Notification's content, not another Notification. The template shows
  each row only when its variable is set, so it can go out before or after
  core; once core sends them, `TicketUrl` and `TicketCode` become the
  contract's required variables (CONTEXT, Required variable).

## Sending it again

The panel sends a Ticket's mail again ("Bileti yeniden gönder", and with
"Bağlantıyı yenile"; [ticket-resend.md](ticket-resend.md)): the queue's row
then carries the resend's number `n` and goes under
`core:event.ticket-created:<ticketId>:r<n>`, its own key. The first mail's
key stays `…:v1`.

## Retries

- Same key on every try: a retry after a lost answer is SkyMail's
  `Idempotent-Replayed`, never a second Notification.
- A guest's mail has no key (`/v1/mail_tasks/single` reads none). When it
  went out and no answer came (a timeout, a dropped connection), SkyMail may
  have taken it, so it is **not** tried again: the row is done with, counted
  as "unanswered". A send that never connected, or that SkyMail answered with
  an error, is tried again as above.
- `400` and `422` are permanent (the same body would be refused again): the
  row is done with, counted as refused. Anything else (SkyMail down, `401`,
  `403` before Keycloak gives core `skymail:notifications:send`, `409` or
  `422 notification.type_unknown` before the type is seeded) is tried again, 30 seconds first, doubling to 30
  minutes, for a day. A row a day old is given up unsent, whether it failed
  until then or waited while the worker was off.
- Several cores share the queue: a claim skips the rows another holds, and
  a late write of a claim another core has taken again changes nothing.

Each pass that did something logs one line of counts, never a person:

```text
ticket notification: 3 sent, 1 dropped, 0 refused by SkyMail, 0 given up, 0 tried again later, 0 guest mails unanswered and not tried again
```

## Erasure

Account erasure deletes the unsent rows of the person's Tickets and of their
addresses' guest Tickets ([account-lifecycle.md](account-lifecycle.md)); a row
also goes with its Ticket (`ON DELETE CASCADE`).

## Before turning it on

1. Keycloak: `core`'s service account holds `skymail:notifications:send`
   (iletişim-servisi 04; e-skylab-keycloak).
2. SkyMail: the type `event.ticket-created` is seeded with its inbox text
   (`PUT /v1/notification_types/event.ticket-created`, the skymail-frontend
   Template seed) and the template is in use.
3. Tell the club: this is a new mail; Forms' own confirmation still goes for
   a form that writes a Ticket.
4. `TICKET_CREATED_NOTIFICATION=on`; the startup line reads
   `ticket notification: on (event.ticket-created), ticket page on` with
   `TICKET_PAGE_URL` set, `… no ticket page (TICKET_PAGE_URL unset)` without.

Rollback: `TICKET_CREATED_NOTIFICATION=off` (or unset). The rows already
queued stay; turned on again within a day they are sent, later they are given
up unsent. The previous release reads and writes none of it, so the image can
go back too.

## Audience channels

`POST /v1/audiences/resolve` answers each recipient's `allowedChannels`
([news-audience.md](news-audience.md)), what SkyMail passes on as
`allowed_channels`: `["email"]` while core keeps preferences for e-mail alone.
