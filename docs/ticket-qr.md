# The Ticket QR

ADR 0082 (e-skylab), ADR 0022 addendum; bilet-qr-ve-pdf ticket 02. Every
Ticket, a member's and a guest's, has a Ticket QR: a signed, static code the
door scans with the same camera and on the same route as the SkyPass QR. The
mail, the PDF, the ticket page and sky-app show the same code. Staff scan
it; the holder never scans it. This page is the contract the scanner
(sky-app, Mobile Lab) builds on.

## The code

```text
header  {"alg":"ES256","kid":"tk-e1","typ":"JWT"}
payload {"sub":"<ticket id>","evt":"<event id>","qv":<QR version>,"exp":<unix seconds>}
```

- **No personal data:** two ids and two numbers. About 300 characters, QR
  version 12–13 at error correction M: readable on a phone and at 3 cm on
  paper.
- **Static.** It is signed deterministically (RFC 6979): the same Ticket,
  QR version and Event dates always give the same string, wherever it is
  shown. It changes when the panel renews the link (`qv` + 1, ticket 05) or
  the Event's dates change (`exp`). Core never stores it; it is signed when
  shown.
- **`exp`:** a day after the Event ends (its end date, else a day after its
  start: ADR 0066). An Event with no date never ends; its codes expire a
  year after the Ticket was written.
- **Key.** ES256 (P-256), derived from the SkyPass signing key with
  HKDF-SHA256 (RFC 5869, info `skylab ticket-qr es256 v1`, 40 bytes reduced
  to a scalar): no new secret, and the SkyPass key itself never signs a
  Ticket QR, nor the Ticket QR key a SkyPass QR. It is `internal/ticketqr`.

## Keys: `GET /v1/skypass/jwks` (also `GET /v1/door/jwks`)

One JWKS, no sign-in, both names the same: SkyPass's key (`sp-e1`) and the
Ticket QR's (`tk-e1`). A scanner that verifies on the device picks the key
by the token's `kid`, and tries every key with that `kid` (during a key
rotation's window there are two `tk-e1` keys, below).

## At the door: `POST /v1/sessions/{sessionId}/check-in/scan`

The body is `{"token": "<what the QR says>"}`, as before.
`POST /v1/sessions/{sessionId}/check-in/skypass` is the same route under its
old name and stays: the sky-app in the stores calls it.

Core reads the token's header: `kid` `tk-e1` is a Ticket QR; anything else
goes where it went before (a SkyPass QR, a Google Wallet code `SPW1:…`, or
`{"uid"}` for a Student card).

The order, as for SkyPass:

1. No such Session: `404`. The scanner may not take check-ins at this
   Event's door (door staff, the owning team's leaders, Privileged, team
   door scan): `403`. A draft or cancelled Event: `403 session_closed`.
   All before the code is read, so a refusal says nothing about the holder.
2. The signature, with the Ticket QR key: any failure is `401
   ticket_qr_invalid` (another key, the SkyPass key, a changed body).
3. `evt` must be the Session's Event, and the Ticket must be there with its
   holder (an erased person's Ticket is not): otherwise `404`.
4. The expiry, **by the Event's dates now**, not by the code's `exp`: an
   Event moved later keeps the codes already sent working; an Event moved
   earlier ends them earlier. Past it: `401 ticket_qr_expired`.
5. `qv` must be the Ticket's QR version: otherwise `410
   ticket_qr_superseded`.
6. The check-in: Ticket × Session, the same row a SkyPass scan, the
   Session QR or the desk writes. `201` with the check-in (`id`, `ticketId`,
   `sessionId`, `eventDayId`, `createdAt`, as SkyPass's); already in: `409`.

| Status | `code` | Meaning | What today's sky-app shows |
|---|---|---|---|
| 201 | | checked in | "Giriş alındı" and the name (from `/check-ins`) |
| 409 | | this Ticket is already in for this Session | "Zaten giriş yapmış" |
| 404 | | another Event's Ticket, or none | "Bu etkinliğe kaydı yok" |
| 401 | `ticket_qr_invalid`, `ticket_qr_expired` | not core's code, or past its Event | "Kodun süresi dolmuş" |
| 410 | `ticket_qr_superseded` | the link was renewed; `detail`: "Bu bilet yenilenmiş; kişiden son postadaki bileti açmasını iste." | the generic error (`detail`) |
| 403 | `session_closed` | draft or cancelled Event | as today |
| 403 | | this scanner may not take check-ins here | as today |

**Today's sky-app needs no change.** Its scanner sends a code only when
`DoorService.isSkyPassToken` says yes: three parts, `alg` ES256 and a
non-empty `sub`. A Ticket QR is one (a Go test mirrors the rule).

**Asked of the scanner, not urgent** (bilet-qr-ve-pdf ticket 11):

- (a) "Tanınmayan kod" instead of "SkyPass kodu değil".
- (b) For `410 ticket_qr_superseded`: show `detail`.
- (c) On-device verification and Queued check-in (ADR 0022): the key from
  the JWKS by `kid`; for `tk-e1`, `evt` must be the selected Event and
  `exp` in the future. Offline, `exp` is all the device has: a code sent
  before the Event moved later may be refused offline; online core takes
  it.
- (d) The member's own Ticket QR in the app, from `/v1/tickets/me`
  (`qrToken`, ticket 03).

## The desk: `POST /v1/sessions/{sessionId}/check-in/resolve`

`{"query": …}` tries the Ticket's **code** first, read as a person types or
says it (`k7m2 qx9f`, `K7M2-QX9F` and `K7M2QX9F` are one code; `O` is `0`,
`I` and `L` are `1`), then the name and the e-mail as before. A code no
Ticket of the Event has falls through to them. `GET
/v1/events/{id}/door-attendees?q=` finds the Ticket of a code alone. A code
is not a credential: it never writes a check-in by itself, only staff at the
desk do.

## Forwarded codes

A static code can be photographed and forwarded. What bounds it:

- One check-in per Ticket and Session: the first scan wins, the second is
  `409`.
- The scanner shows the name of who checked in; staff compare.
- The panel renews the link (ticket 05): a new `qv`, the old code `410`
  online.
- Offline (Queued check-in), a renewed code can still pass until the
  scanner syncs: the accepted cost of ADR 0022's fail-open door.

## Key rotation

The Ticket QR key follows the SkyPass key: rotating `SKYPASS_EC_PRIVATE_KEY`
(the platform hub's `skypass-anahtar-dondurme-wizard.sh`) changes it, and
every Ticket QR already mailed or printed stops verifying. The ticket page,
the mail's QR image (fetched when the mail is opened) and the app sign with
the new key at once; a PDF saved before does not.

To keep the codes sent working through a **planned** rotation, copy the
`tk-e1` entry of the JWKS before rotating and set, with the new key:

| Variable | Value |
|---|---|
| `TICKET_QR_PREVIOUS_JWK` | that entry, one JSON object (a public key, not a secret) |
| `TICKET_QR_PREVIOUS_UNTIL` | when it stops (RFC 3339), at most 120 days ahead: the last Event the codes were sent for, plus a day |

Until then core takes a code signed with either key and the JWKS publishes
both under `tk-e1`; after it, only the new one. Both unset (the default):
only the current key. One without the other, a value that does not parse or
a window over 120 days: core does not start. **A rotation for a leaked key
sets neither**: the old key could sign anything. Holders then open their
ticket page (or the panel resends the ticket, ticket 05) for a new code.

## Metrics

`skylab_door_scans_total{credential="ticket_qr",outcome=…}` on `/v1/metrics`:
`checked_in`, `already`, `not_found`, `invalid`, `expired`, `superseded`,
`session_closed`, `forbidden`, `failed`. No ids, no names.

## Industry practice

Matches: a static code per ticket, first scan wins (Eventbrite); a signed
code verified offline with published keys (EU Digital COVID Certificate,
COSE_Sign1 ES256). Deviation: no rotating code (Ticketmaster SafeTix,
Google Wallet `rotatingBarcode`): the ticket has to go on a PDF and on
paper, the club's Events are free, and the door is fail-open (ADR 0022).
