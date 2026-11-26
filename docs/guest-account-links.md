# Guest account links

A guest Ticket is written for an e-mail address: Guest apply, an operator's
"Katılımcı ekle" for a guest, a Forms answer without a signed-in person, an
Unticketed respondent's Ticket, an attendee import's guest row. Until this,
such a Ticket stayed a guest's even when the address was a member's, and the
person never saw it under their own Tickets (`GET /v1/tickets/me`) nor its
certificate under theirs. Now core **links** it: the Ticket's owner becomes
the account that holds the address verified (ADR in `skylab-kulubu/e-skylab`,
"Misafir biletleri ve sertifikaları doğrulanmış e-postanın hesabına bağlanır").

Code: `internal/guestlink`, the verified-address directory
(`internal/commpref/verified_addresses.go`), the ticket service's
`GuestOwners`. Migration `20261127234500`.

## What a link is

- `tickets.owner_id` becomes the account. The Ticket **stays `GUEST`**, with
  the name, e-mail and phone the guest gave, the same access key, code and QR
  version. Mails, the ticket page, the certificate's name and the door read
  the Ticket by its type, so all of them work as before.
- `tickets.owner_linked_at` and `owner_link_source` say that core linked it
  and how: `write` (written linked), `sync` (the user sync's pass), `bulk`
  (the one-time command). A Ticket core did not link has neither.
- A certificate without an owner gets one the same way
  (`certificates.owner_linked_at`, `owner_link_source` `sync` or `bulk`).
- Nobody's account is made. An address no account holds stays a guest's.

What the person sees: the Ticket in their own list (`GET /v1/tickets/me`,
with its link and QR), the certificate in theirs. They may cancel it as
their own (`POST /v1/tickets/{id}/cancel`). Registering on the Event as a
member answers "already registered" (`409`): no second Ticket.

## Whose address is it

The verified-address directory of the recipient question
([news-audience.md](news-audience.md#matching-verified-addresses-adr-0044)),
the same reads and the same rule, compared trimmed and lower case:

- the Primary e-mail while `emailVerified` (the person cannot change it
  without the e-mail code; User Profile has it `userViewOnly`);
- the School e-mail of an account linked to the YTÜ identity provider (the
  attribute is `edit=[admin]`, written from the YTÜ login);
- the Personal e-mail with a proof stamp (`personalEmailVerifiedAt`, which
  sky-account writes when the code is confirmed; `edit=[admin]`).

An address is linked only when exactly **one** account holds it so, that
account is enabled in Keycloak and not a service account, and core has an
active row for it without an erasure request (the database's
`active_account_reference` rule). An unverified address is never linked; it
is linked once verified, by the next pass. Two accounts holding one address
link neither.

Why only verified: a link hands the Ticket's page, QR and certificate to the
account. Somebody who adds another person's address to their own account
cannot verify it, so cannot take that person's Tickets.

The other way round, anybody who knows a member's verified address can fill
a public form with it: the Ticket then shows in the member's list. The mail
went to that address before linking too; the member may cancel it. Nobody
gets to see or use another person's Ticket.

## When

| Way | What |
|---|---|
| A guest Ticket is written | `writeGuest` (Guest apply, Katılımcı ekle, a Forms answer, an Unticketed respondent's Ticket) and an import's guest row write it linked when the address is one account's and that account holds no Ticket on the Event. The directory is the one in memory (at most 5 minutes old, 15 while Keycloak does not answer); a lookup waits at most 2 seconds. Any failure writes the Ticket unlinked, as before; a later pass links it. A database refusal of the link (the account took a Ticket meanwhile, or stopped being active) writes it unlinked. |
| An account is opened or an address verified | Keycloak's `REGISTER`, `UPDATE_EMAIL`, `UPDATE_PROFILE` (sky-account's Personal e-mail confirmation) and admin `USER` events wake the user sync ([user-sync.md](user-sync.md)); its pass ends by reading the directory anew and linking (`sync`). A verification without such an event waits for the 5-minute pass. |
| Once | `core-backend guest-account-link -apply` links what there is (`bulk`) and turns linking on. |

**The switch.** `guest_account_link_settings.enabled_at`: NULL links nothing
anywhere, not even a new Ticket. The migration leaves it off. `-apply` turns it
on after its links; `-off` and `-undo` turn it off. The ticket service reads it
at most every 30 seconds per replica. A database switch rather than an
environment variable: it is turned on with the links, without a release.

## Two Tickets of one person on one Event

The account holds a Ticket on the Event already (its own, or a guest Ticket
of another of its addresses linked earlier), and a guest Ticket of one of
its verified addresses is there too: **nothing is merged**. The guest Ticket
stays unlinked and keeps working: its QR, page, mails, check-ins and Venue
entries are its own. It is counted `duplicate`. Its certificate is still
linked to the account by its address, so the person sees it. Of two
unlinked guest Tickets of one person on one Event, the active one (else the
waitlisted, else the cancelled; the older among equals) is linked.

Ticket systems keep registrations apart (Luma, Eventbrite: each is an
admission, shown in the account by its e-mail); a merge is a CRM's contact
operation. Merging would refuse at the door the QR the guest was mailed, so it
is not done.

## Certificates

A certificate without an owner that still has its recipient's address is
linked to its Ticket's owner (one linked in the same run included), else to
the account its address is, by the rule above. A revoked one too: it is the
person's history. A certificate issued from a linked Ticket later takes the
Ticket's owner, as every certificate does. The verification page and the
certificate mail read the serial and the recipient's name and address,
which do not change.

## What else knows a linked Ticket

- **Account erasure** clears the guest name, e-mail, phone and link mark of
  every Ticket the account owns, whichever address linked it, and the linked
  certificates' owner and address ([account-lifecycle.md](account-lifecycle.md)).
- **Retention**: `guest_phone` v2 takes a linked Ticket's phone 90 days after
  its Event, as an unlinked guest's; its name and e-mail are the account's
  and go with it ([retention-sweep.md](retention-sweep.md)).
- **Unticketed respondents**: an answer of the address counts as ticketed
  ([form-respondents.md](form-respondents.md)).
- **The door** finds it by the guest's e-mail, name and code as before, and
  by the person (`personId`, SkyPass, `CheckInUser`) as well.
- **Counts**: the dashboard counts a linked Ticket as a guest's (its type).

## The command

Runs inside the core container, reads the server's environment, prints
counts only (`name: n` lines), never a person or an address:

```
core-backend guest-account-link           # dry run: what -apply would do now
core-backend guest-account-link -apply    # link, then turn linking on
core-backend guest-account-link -off      # turn linking off; the links stay
core-backend guest-account-link -undo     # turn off and take back every link
```

Counts, for Tickets and for certificates: `link` (and by kind: `primary`,
`school`, `personal`; certificates also `ticket`), `ambiguous`, `duplicate`
(Tickets), `account_disabled`, `account_inactive`, `no_account`,
`changed_meanwhile`; then what core linked so far by source. Each row is
written on its own, guarded by what it was read as: run twice, or stopped
half way, a row is linked once. The hub's `ops/wizards/misafir-hesap-baglama-wizard.sh`
runs it on the production server.

Log line of a pass that linked something:
`{"event":"guest_account_link","source":"sync","tickets_linked":1,…}`.
A failure: `{"event":"guest_account_link_failed","level":"warn","stage":"switch|directory|database",…}`.

## Rolling back

- `-undo` (or the migration's down): the switch goes off and every link core
  made is taken back in one transaction: the linked Tickets and certificates,
  and the certificates issued from a linked Ticket after the link. A link
  the person's own registration or an operator made is no link of core's and
  stays.
- Then the image. The release before does not clear a linked Ticket's guest
  name and e-mail on account erasure: do not run it with links in place.
