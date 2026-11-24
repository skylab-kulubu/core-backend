# Attendee import and the external registration link

Some Events take their sign-up on another product's form (Microsoft Forms,
Google Forms, a spreadsheet) instead of a Skyforms form. Two things serve
them (Yusuf, 2026-11-24):

- the Event's **external registration link**, where the site's "Kayıt ol"
  (and later SkyEvents') sends people;
- the **attendee import**, where an operator turns the list that form
  collected into Tickets, so that the door, the attendance and the
  certificate work for those people as for anyone else.

The same idea as Eventbrite's "Import attendees" and Luma's "Import guests
from CSV": a CSV or a pasted list, a preview, then the Tickets.

## The external registration link

`externalRegistrationUrl` on the Event: read on create, and on `PUT`/`PATCH
/v1/events/{id}` (left out keeps it, `""` or `null` clears it). It is an
absolute `https://` address with a host, no user or password in it, no blank
or control character, at most 2048 characters; the scheme is kept in lower
case and the blanks around it are trimmed. Anything else is `400`
`external_registration_url_invalid` and nothing is written. The column's
check (migration `20261125201700`) refuses what the code would.

Anyone who sees the Event sees the link, in every answer that carries the
Event (lists, the detail, `GET /v1/events/{id}` signed out); an Event
without one leaves the field out. A copy of the Event does not take it: a
new form for each Event, as the Skyforms form (event-copy.md).

The link and a Skyforms form may both be set; the site sends "Kayıt ol" to
the link when there is one.

## The attendee import

    POST /v1/events/{eventId}/attendee-imports/preview
    POST /v1/events/{eventId}/attendee-imports

Body: `{"text": "...", "sendMail": true, "pastCapacity": false}`. `text` is
the list: the panel reads a CSV file the operator picks (in the browser)
and sends its text, or the rows the operator pasted. `sendMail` is on when
left out; `pastCapacity` off.

**Who:** an operator of the Event, as Guest apply and "Katılımcı ekle"
([guest-apply.md](guest-apply.md)): who may update the Event or assign its
Tickets. Anybody else, a service account included, gets `403`; a draft the
caller may not see `404`; a cancelled Event `409 event_cancelled`. A draft
takes an import (speakers, the team), as "Katılımcı ekle" does.

**The list.** UTF-8, a byte order mark dropped. The separator is read from
the first line: a tab (pasted from a spreadsheet), a semicolon (Excel in
Turkish) or a comma. With a header, the columns are found by name, Turkish
or English, case and Turkish letters ignored: `Ad`/`First name`,
`Soyad`/`Last name`, `Ad Soyad`/`Name` (split at its last blank),
`E-posta`/`Email`, `Oturum`/`Session`; other columns are ignored.
Microsoft Forms' own respondent columns (`Email`, `Name`: the signed-in
respondent or "anonymous") give way to the form's questions. Without a
header, the column holding an address is the e-mail and the others, in
order, the first name, the last name and the Session. Blank lines are
skipped. Addresses are kept in lower case.

**Limits:** at most 2 MiB of text (`413 import_too_large`) and 1000 rows,
the header left out (`413 import_too_many_rows`). An empty list is `400
import_empty`, one without an e-mail column `400 import_no_email_column`, one
the CSV reader cannot read (not UTF-8) `400 import_unreadable`.

**Each row** gets one outcome:

| Outcome | When |
|---|---|
| `invalid` | `problem`: `invalid_email`, `missing_first_name`, `missing_last_name`, `name_too_long` (over 100 characters); at the import also `account_unavailable` (the account went away since the preview) |
| `duplicate` | the e-mail is on an earlier row; the first one wins |
| `existing` | the person already holds a Ticket on the Event, as a guest or through their account; `ticketStatus` says `active`, `waitlisted` or `cancelled`. It is left as it is: a cancelled Ticket is not reopened (the operator restores it by hand) |
| `active` | a Ticket written active; `overCapacity` when past the capacity |
| `waitlisted` | the Event is full and its waiting list on |
| `full` | the Event is full and its waiting list off: no Ticket |

**Accounts:** a row whose e-mail exactly one active account holds gets the
account's Ticket (a member's, `account: true`), as "Katılımcı ekle" writes it;
any other row a guest Ticket with its name and e-mail.

**Capacity** ([event-capacity.md](event-capacity.md)): the rows take the
places left in their order, then the waiting list (while it is on), else
they are `full`. With `pastCapacity` every new row is active, past the
capacity if it must, as "Katılımcı ekle". The preview counts as the import
will; the import decides each row under the Event's lock, so a place taken
in between turns a row `waitlisted` or `full`.

**Sessions:** the `Oturum` column may name one or more of the Event's
Sessions (by title or id, `;` between them). The answer says which it
found (`sessions`) and which it did not (`unknownSessions`). A Ticket is
the Event's, not a Session's: the choice is shown, not kept.

**The mail:** each new Ticket's mail goes as for any new Ticket:
"Biletin hazır" while `TICKET_CREATED_NOTIFICATION` is on, "Bekleme
listesindesin" while `TICKET_STATUS_MAIL` is
([ticket-created-notification.md](ticket-created-notification.md)). With
`sendMail: false` nothing is queued; the panel's "Yeniden gönder" sends it
later. `mail` in the answer says `queued`, `off`, `notifications_off` or
`on_publish` (a draft: its Tickets are told only if its publication asks).

**Twice:** the import is idempotent by who the rows are. The same list again
finds every row `existing` and writes nothing; two imports at the same
moment write one Ticket per person (the Tickets' unique indexes), the
second answering `existing`.

**Answer** (both routes, `200`, `Cache-Control: no-store`): `imported`
(false for the preview), `columns` (which column was read as what, and the
separator), `summary` (counts by outcome, `members`, `guests`,
`overCapacity`, `unknownSessions`), `capacity` (`capacity`, `active`,
`free` (-1 for no bound), `waitlistEnabled`), `mail` and `rows`.

## Personal data

The list is read once per request and dropped with the answer: no file is
stored, no row is logged, and the preview writes nothing. What stays is
the Tickets the import wrote (as any guest Ticket, under the retention
rules of [retention-sweep.md](retention-sweep.md)), one log line of counts
(`attendees_imported`: the request id, the Event and the counts) and one
Team audit row `ticket.import` ([team-audit.md](team-audit.md)): who, the
Event's team, the Event and the counts, never a name or an address. A row
that cannot be opened stops the import before it writes (`503
audit_unavailable`).

A database error midway stops the import: the rows written so far stay,
the audit row says `failed` with the counts so far, and the same list
again writes the rest.
