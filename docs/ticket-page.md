# The ticket page, its QR and the PDF ticket

ADR 0082 (e-skylab); bilet-qr-ve-pdf ticket 03. A Ticket opens without a
sign-in by its access key ([ticket-access-key-and-code.md](ticket-access-key-and-code.md)):
the link is the right to read, because a guest has no account (ADR 0029).
The page is the club site's (`yildizskylab.com/bilet/{key}`, ticket 07, the
certificate's pattern of ADR 0040); core answers the least it needs on three
public routes.

## Routes

No sign-in. Every answer has `Cache-Control: no-store`,
`Referrer-Policy: no-referrer` and `X-Robots-Tag: noindex, nofollow`. A
client may ask 120 times a minute (the certificate routes' per-client
budget, `perClientLimit`). A key that is not 26 characters of `A`–`Z` and
`2`–`7` is `404` before the database is asked.

### `GET /v1/public/tickets/{key}`

```json
{
  "eventName": "Yapay Zeka Kampı",
  "eventStart": "2026-12-12T07:00:00Z",
  "eventEnd": "2026-12-12T15:00:00Z",
  "venue": "YTÜ Davutpaşa Kampüsü",
  "ownerTeam": "AILAB",
  "eventStatus": "published",
  "holderName": "Elif Yılmaz",
  "code": "K7M2-QX9F",
  "qrToken": "eyJhbGciOiJFUzI1NiIsImtpZCI6InRrLWUxIiwidHlwIjoiSldUIn0…",
  "qrUrl": "https://api.yildizskylab.com/v1/public/tickets/{key}/qr.png",
  "pdfUrl": "https://api.yildizskylab.com/v1/public/tickets/{key}/pdf",
  "state": "active",
  "theme": {"key": "kurumsal", "accent": "#1F4E79"}
}
```

- Exactly these fields: **no e-mail, phone, Sky number or ticket type**.
  `eventStart` and `eventEnd` are `null` when the Event has none.
- `holderName`: the guest's name, or the member's from core's row.
- `qrToken` is the Ticket QR ([ticket-qr.md](ticket-qr.md)), signed with
  the Event's dates now; the site draws it, or shows `qrUrl`.
- `qrUrl` and `pdfUrl` are on `PUBLIC_API_ORIGIN` (paths when it is unset).
- `theme`: the look the PDF has; the site draws the same (ticket 07).
  Ticket 16 adds the Event's choice, the team's logo and the guilloche.
- `state`:

| State | When | QR and PDF |
|---|---|---|
| `active` | published, not over | yes |
| `ended` | over (end date, else a day after the start, ADR 0066), less than `TICKET_PAGE_GRACE` ago | yes; the door refuses the QR a day after the end |
| `event_cancelled` | cancelled; undoing the cancellation opens it again | `qrToken`, `qrUrl`, `pdfUrl` empty; the QR and PDF routes `410` |

`404` (with nothing about anyone): an unknown or malformed key, a draft
Event's Ticket (a draft does not exist when signed out, ADR 0066), an
archived Event's, a deleted Ticket, and a Ticket whose holder was erased
(an erased member's Ticket is detached; an erased guest's has no address).
Account erasure also empties the key, so the old link meets nothing.

`410 ticket_page_closed`, with no personal data: the Event ended more than
`TICKET_PAGE_GRACE` (7 days) ago, on all three routes. An Event with no date
never ends, so its pages never close.

### `GET /v1/public/tickets/{key}/qr.png`

The Ticket QR as a PNG: error correction M, a four-module quiet zone,
black on white, 512 px. The mail shows it as a remote image (ticket 04): it
is signed when fetched, so it always has the Event's dates of the moment.

### `GET /v1/public/tickets/{key}/pdf`

The PDF ticket, `Content-Disposition: attachment;
filename="bilet-<the Event's name in ASCII>.pdf"` (never the person's
name). Five a minute per key besides the client's budget (`429
ticket_pdf_rate_limited`). Gotenberg not set or busy for 20 seconds: `503
ticket_pdf_unavailable`, `Retry-After: 30`. Core draws it on each request
and keeps nothing; a renamed holder gets the new name.

## The PDF

Drawn by core with the certificate's Gotenberg (Chromium) client, at most
two at once, as a **tagged PDF** (`generateTaggedPdf`): Turkish (`lang="tr"`),
titled "<Event> bileti", the QR an image with the alternative text "Bilet
QR kodu", the code real text.

- **Page:** 210 × 148 mm (A5 landscape), one page. It prints on A4 (a
  viewer scales it or prints it at the top) and fills a phone's screen; an
  A4 portrait page would leave half of it empty.
- **Shows:** the Event's name, date and time (Europe/Istanbul, in
  Turkish), place and organizing team; the holder's name; the QR (47 mm)
  and the code. **Never** an e-mail, a phone number, a Sky number or the
  ticket type. One line: "Kapıda QR'ı göster. Okunmazsa bilet kodunu söyle."
- **Look, Kurumsal** (`internal/ticket/theme`): an official document's
  ticket. A guilloche ground of two woven wave families and two rosettes,
  drawn from the code (each Ticket its own, always the same); the code as
  the serial number; microtext of the Event's name, the club and the code
  along the edges; the seal, the SKY LAB mark in a ring with the team's
  name, and a second seal with the organizing team's logo when its group
  has one (`logo_media_id`, a public image of at most 512 KB, read once
  every ten minutes); the stub (koçan) behind a perforation with a small
  QR, the code, the Event and its date. All of it is ornament: what
  proves a Ticket is the signature in its QR, and no text says otherwise.
- The QR keeps its place and size; a theme changes colours, typefaces and
  ornament only (ticket 16 adds themes).

## The holder's own list: `GET /v1/tickets/me`

Each Ticket also carries `ticketUrl` (its page, while `TICKET_PAGE_URL` is
set) and `qrToken` (on a published Event), besides `code`: not on a draft,
nor after the page closed. Only the holder's own list has them; `GET
/v1/tickets/{id}`, the panel's lists and an Event's applicants never do.

## Settings

| Variable | Default | |
|---|---|---|
| `TICKET_PAGE_URL` | unset | the page's address, `https`, `{key}` once, no query: `https://yildizskylab.com/bilet/{key}` (sandbox: the sandbox site). Unset, core makes no link and the mail goes as before; the routes answer either way. A value that does not parse stops core. |
| `TICKET_PAGE_GRACE` | `168h` | how long after its Event a page stays open; 0 to `2160h`. |

## Logs

A refusal (404, 410) is not logged. A failure is, naming the key by its
first four characters (`K7M2…`), the certificate serial's pattern; the
error handler never sees these routes' paths.

## Industry practice

Matches: a ticket page reached by a link in the mail with the QR, the code
and a PDF (Biletix); the code as text beside the QR (an image's text
alternative, WCAG 1.1.1); a tagged PDF with alternative text (PDF/UA); no
contact details on the ticket (a boarding pass's barcode carries the name
and the flight, not a phone).

Deviations:
1. **No sign-in.** Eventbrite and Biletix show a holder's tickets in their
   account; a guest has none, so the link is the right, bounded by 128
   random bits, the client's budget, `no-referrer` and the closing a week
   after the Event.
2. **A5 landscape**, not the A4 print-at-home page most sites send, which
   fills its lower half with terms and advertising; we have neither.
3. **The PDF is not kept**, unlike a certificate: it is drawn when asked.
