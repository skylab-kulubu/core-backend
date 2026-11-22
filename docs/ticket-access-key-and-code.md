# A Ticket's access key, code and QR version

ADR 0082 (e-skylab); bilet-qr-ve-pdf ticket 01. Every Ticket, a member's and
a guest's, has three fields besides who holds it. They are the ground the
ticket page, the Ticket QR and the desk's code search stand on.

| Field | What | Shape | Who sees it |
|---|---|---|---|
| `access_key` | opens the ticket page without a sign-in: who holds the link holds the Ticket | 16 bytes of `crypto/rand` in base32 (RFC 4648, no padding): 26 characters, `A`–`Z` and `2`–`7`. Unique. | the holder only: their mail and their own list, as a link. **No answer carries the key itself**: not `GET /v1/tickets`, `/v1/tickets/{id}`, `/v1/tickets/user/{u}/event/{e}`, an Event's applicants, nor the holder's `GET /v1/tickets/me`. |
| `code` | what the desk reads out and searches by | 8 characters of Crockford's base32 (`0`–`9` and the letters without `I`, `L`, `O`, `U`; 40 random bits), stored without and answered with its hyphen: `K7M2-QX9F`. Unique. | the holder and the panel: the Ticket JSON's `code`. |
| `qr_version` | the Ticket QR's version | a whole number from 1 | nobody; it goes up when the panel renews the link, and a Ticket QR of an older version is refused online |

## Writing

Core draws the access key and the code itself, from `crypto/rand`
(`internal/ticket/code.go`), on every path that writes a Ticket: Member
apply, Apply-for-other, Guest apply (the public route, Forms' hop and the
panel's "Katılımcı ekle", a draft's included) and a Forms answer. They all
go through the store's `Create`. A code another Ticket holds (the unique
index) is drawn again, up to eight times; a second draw is already rare at
40 bits. The owner's and the guest e-mail's own unique index stays a
conflict (`409`), as before.

The columns have defaults that draw the same shapes in the database
(`ticket_random_text`, from `gen_random_uuid()`, a strong random source,
no pgcrypto). They filled the Tickets already there when the migration
ran, each its own, and cover a Ticket written by the release before during
the deploy, which knows none of the three columns.

## Reading a code

`ticket.NormalizeCode` reads a code as a person types or says it: spaces and
hyphens dropped, small letters made capital, and Crockford's look-alikes
mapped (`O` → `0`, `I` and `L` → `1`). `k7m2 qx9f`, `K7M2-QX9F` and
`K7M2QX9F` are one code. Anything that is not 8 characters of the alphabet
then is not a code. The code is not a credential: on its own it opens no
door and writes no check-in.

## Erasure

A Ticket is not deleted when its holder is erased: it stays, anonymous, with
its check-ins (account erasure detaches the member, `owner_id` NULL; the
guest data step and the guest identity retention rule empty a guest's name
and address). Account erasure and its guest data step also empty the access
key (NULL) and raise `qr_version`, so the erased person's ticket page and
Ticket QR stop working; the code stays, a random value no longer tied to
anyone. The migration emptied the key of every Ticket erased before it. The
retention sweep's guest identity rule (two years after the guest's last
Event) leaves the key; the ticket page is long closed by then (seven days
after the Event), and its answer treats a Ticket without a holder as not
found.

Nothing else holds the key or the code: deleting a Ticket (an Event's
cascade) takes them with it.

## Rollback

The down migration drops the three columns, their indexes and the
function; the release before reads none of them. Ticket pages and Ticket
QRs sent out stop working.
