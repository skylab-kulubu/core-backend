# The ticket page, its QR and the PDF ticket

ADR 0082 (e-skylab); bilet-qr-ve-pdf ticket 03. A Ticket opens without a
sign-in by its access key ([ticket-access-key-and-code.md](ticket-access-key-and-code.md)):
the link is the right to read, because a guest has no account (ADR 0029).
The page is the club site's (`yildizskylab.com/bilet/{key}`, ticket 07, the
certificate's pattern of ADR 0040); core answers the least it needs on four
public routes.

## Routes

No sign-in. Every answer has `Cache-Control: no-store`,
`Referrer-Policy: no-referrer` and `X-Robots-Tag: noindex, nofollow`. A
client may ask for the JSON and the PDF 120 times a minute (the certificate
routes' per-client budget, `perClientLimit`); the QR image has a budget of
its own, 1200 a minute per client, because the mail's image is fetched by
Gmail's image proxy and Apple Mail's privacy preloading from a few addresses
for many people at once. A key that is not 26 characters of `A`–`Z` and
`2`–`7` is `404` before the database is asked.

### `GET /v1/public/tickets/{key}`

```json
{
  "eventName": "Yapay Zeka Kampı",
  "eventStart": "2026-12-12T07:00:00Z",
  "eventEnd": "2026-12-12T15:00:00Z",
  "coverImageUrl": "https://cdn.yildizskylab.com/images/….webp",
  "coverColors": ["#0F766E", "#111111"],
  "venue": "YTÜ Davutpaşa, EEF Konferans Salonu",
  "venueAddress": "Davutpaşa Cd. No:127, Esenler/İstanbul",
  "venueLat": 41.025376,
  "venueLng": 28.889648,
  "directionsUrl": "https://www.google.com/maps/dir/?api=1&destination=41.025376%2C28.889648",
  "mapsUrl": "https://www.google.com/maps/search/?api=1&query=41.025376%2C28.889648",
  "mapImageUrl": "https://cdn.yildizskylab.com/images/….jpg",
  "mapAttribution": "© OpenStreetMap contributors",
  "mapAttributionUrl": "https://www.openstreetmap.org/copyright",
  "ownerTeam": "AILAB",
  "organizerName": "AILAB",
  "eventStatus": "published",
  "holderName": "Elif Yılmaz",
  "code": "K7M2-QX9F",
  "qrToken": "eyJhbGciOiJFUzI1NiIsImtpZCI6InRrLWUxIiwidHlwIjoiSldUIn0…",
  "qrUrl": "https://api.yildizskylab.com/v1/public/tickets/{key}/qr.png",
  "pdfUrl": "https://api.yildizskylab.com/v1/public/tickets/{key}/pdf",
  "state": "active",
  "theme": {"key": "kurumsal", "accent": "#0F766E"}
}
```

- Exactly these fields: **no e-mail, phone, Sky number or ticket type**.
- `organizerName` is who organizes the Event as the page and the PDF show
  it (Yusuf, 2026-11-23): "SKY LAB: Bilgisayar Bilimleri Kulübü" for an
  Event of YK, DK or ADMIN or of no Owner team, else the Owner team's code
  and Turkish name as core's mails name a Team ("WEBLAB · Web Geliştirme";
  the code alone while the name is unknown or unreadable). The names are
  Keycloak's `display_name_tr`, read at most every 15 minutes.
  `ownerTeam` stays the Owner team itself (`internal/organizer`).
  `eventStart` and `eventEnd` are `null` when the Event has none.
- The place ([event-location.md](event-location.md)): `venue` is its name;
  `venueAddress`, `venueLat` and `venueLng` are left out while the Event has
  none. `directionsUrl` ("Yol tarifi") and `mapsUrl` ("Haritada aç") are
  Google Maps links to the point, else to the name and address: links, no
  key, nothing embedded. A place with neither a point nor an address
  ("Online") has neither link.
- `mapImageUrl` is the map of the point core drew ([event-map.md](event-map.md)),
  1280 × 480; `mapAttribution` the line the site prints under it, linked to
  `mapAttributionUrl`. Both are left out while there is no map.
- `holderName`: the guest's name, or the member's from core's row.
- `qrToken` is the Ticket QR ([ticket-qr.md](ticket-qr.md)), signed with
  the Event's dates now; the site draws it, or shows `qrUrl`.
- `qrUrl` and `pdfUrl` are on `PUBLIC_API_ORIGIN` (paths when it is unset).
- `coverImageUrl`: the Event's cover at its public media address, left out
  while it has none; `coverColors` its colours, the dominant first (`[]`
  without a cover). The page draws its front from them.
- `theme`: `key` is the look (Kurumsal); `accent` the page's accent:
  the cover's first colour, else the club's purple `#8B5CF6` (Yusuf's order;
  the Event's own ticket colour goes first once ticket 16 adds it). The
  PDF keeps its own purple rule (`#7C3AED`, the v5 design).
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
ticket_pdf_rate_limited`). Gotenberg not set, busy for 20 seconds,
unreachable, timed out or failing: `503 ticket_pdf_unavailable`,
`Retry-After: 30` (the cause is logged, the key cut). Core draws it on each request
and keeps nothing; a renamed holder gets the new name.

### `GET /v1/public/tickets/{key}/check-ins`

What the page asks for while it is open on the Event's day, so it can greet
its holder the moment the door lets them in (Yusuf, 2026-11-24):

```json
{"checkIns": [{"session": "Açılış", "at": "2026-12-12T07:05:00Z"}]}
```

- The Ticket's own check-ins alone, oldest first: the Session's title and
  when. No id, no name, nothing of another Ticket. `[]` while there is none.
- The page's rules: `404` for a malformed or unknown key, a Ticket without
  its holder (or a member whose account is not active), a draft's or an
  archived Event's; `410 ticket_page_closed` once the page has closed. A
  cancelled Event's Ticket answers what it has.
- **One query** (`ticket.PostgresStore.PageCheckIns`: the Ticket by its
  key, its Event's status and dates, the member's account state, the
  check-ins with their Sessions), not the page's several: the page asks
  every 4 seconds.
- **Budget:** 30 a minute per key (a hash of it, as the PDF's; `429
  ticket_check_ins_rate_limited`, `Retry-After: 60`), and 3000 a minute per
  client, its own: at the door many holders share one address (the campus
  network, a mobile operator's NAT).
- **Load.** The site asks only while the tab is shown, on the Event's day
  (from three hours before the start to the end), and only until a check-in
  appears. 500 holders with the page open at the same moment, every 4
  seconds, are 125 requests a second at most, each one indexed query
  (`tickets.access_key`, `ticket_checkins (ticket_id, session_id)`); in
  practice only the queue at the door has it open.

### The site's server

The club site's ticket page reads `GET /v1/public/tickets/{key}` from its
own server. The routes' budget (120 a minute per client, the PDF's 5 a
minute per key besides) follows the client address Core resolves
([client-ip-trust.md](client-ip-trust.md)), so:

- the site calls Core **on the internal network** (the core service's
  address on `dokploy-network`), not at its public address: Traefik
  replaces any `X-Forwarded-For` it is sent;
- it sends `X-Forwarded-For: <the visitor's address>`, the one it
  resolved itself (Cloudflare's `CF-Connecting-IP` where Cloudflare fronts
  the site, else the socket's address), never a header the visitor sent;
- Core believes that header only from a proxy `TRUSTED_PROXY_RANGES` names:
  the site's tasks are named there (`tasks.<the site's appName>`, besides
  `dokploy-traefik`); from any other container it is ignored and the
  socket's address counts, so the site's visitors would share one budget.

## The PDF

PDF v5 (Yusuf, 2026-11-22). Drawn by core with the certificate's Gotenberg
(Chromium) client, at most two at once, as a **tagged PDF**
(`generateTaggedPdf`): Turkish (`lang="tr"`), titled "Etkinlik Giriş Bileti
<code>", each QR an image with its alternative text ("Bilet QR kodu", "Yol
tarifi QR kodu"), the code real text.

- **Page:** A4 portrait, margins 25 / 14 / 14 mm; as many pages as it
  needs. Every page has a head (Gotenberg's `header.html`): the SKY LAB
  mark and "SKY LAB: Bilgisayar Bilimleri Kulübü" on the left, "Bilet kodu
  <code> · Sayfa n/N" on the right, over the club's purple rule. The head
  is a document of its own and sees no embedded font: it is set in Noto
  Sans (Liberation Sans where there is none).
- **Order, fixed:**
  1. the cover band (182 × 24 mm): the cover's page size cut from its
     middle to 1240 × 164 px, a JPEG of about 25 KB, kept 30 minutes per
     cover; without a cover, the cover's first colour, else the purple;
  2. "ETKİNLİK GİRİŞ BİLETİ" and the ticket: a grid of the Event (name),
     its date and hours (one day) or its start and end (longer), the place's
     name, the holder and the organizer (`organizerName`), beside the QR (44 mm) and the
     code. It never splits and is always on the first page;
  3. "Program": the current days and their Sessions in time order, a
     cancelled Session left out. **Days stand side by side, at most two to
     a row**: two days are two columns, a third day starts a new row under
     the first, and so on. One day takes the whole width (with one Session
     in larger type). A row is hours, title and speaker (on the right); a
     long title wraps, never cut. A row never splits across pages, and a
     day's heading stays with its rows;
  4. "Etkinlik hakkında": the description, as written (line breaks kept);
  5. "Etkinlik yeri", **always last** and never split (it moves to the next
     page when it does not fit): the name, the address, the map when there
     is one ([event-map.md](event-map.md); its JPEG embedded as it is
     stored, about 80 KB) with "© OpenStreetMap contributors ·
     openstreetmap.org/copyright" under it, and the directions QR ("Yol
     tarifi", `directionsUrl`, only with a point or an address);
  6. the footnote, two plain lines: "Bu bilet kişiye özeldir. Düzenlenme:
     <time>" and "Etkinlikte fotoğraf ve video çekilir, Kulübün
     kanallarında paylaşılabilir. Kişisel veriler KVKK Metni'ne göre
     işlenir: skyl.app/kvkk-metni" (the ticket mail's photo sentence, the
     site's "KVKK Metni" link). **The ticket page's link is never printed:**
     its key also matches the holder on the door's attendance page (decision
     12), so a printed or forwarded PDF must not carry it; the link stays in
     the mail.
- **Text is data:** no sentence tells the holder what to do (no "show the
  QR", no "scan"); the labels and the footnote only.
- **Never** an e-mail, a phone number, a Sky number or the ticket type.
- **Type:** Manrope (the club site's), embedded in the page: the static
  400, 500, 600, 700 and 800 weights, Latin and Latin Extended
  (`internal/brandfont`, from Fontsource 5.3.0, SIL Open Font
  License 1.1, `OFL.txt`). Static fonts stay text in the PDF; a variable
  font would become Type 3 glyphs. The renderer reaches nothing outside
  the page: fonts, images and QRs are inline.
- **Size:** under 512 KB, the mail attachment's limit (ADR 0082 decision
  6). What Gotenberg receives for a long three-day ticket with a cover and
  a map stays under it (`TestDocumentSize`); Chromium embeds the images as
  they are and only the glyphs used. Measured with Chromium 2026-11-22:
  see the PR; to be measured again on the sandbox's Gotenberg.
- **Look, Kurumsal** (`internal/ticket/theme`): black on white, the club's
  purple (`#7C3AED`) for the head's rule and a band without a cover. The QR
  keeps its place and size; a theme changes colours and type only (ticket
  16 adds themes).

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
first four characters (`K7M2…`), the certificate serial's pattern. The
error handler, which logs a route that does not exist, a wrong method and a
panic, cuts the key the same way in any path under `/v1/public/tickets/`,
in any case (`/V1/PUBLIC/TICKETS/…` reaches the same routes).
An access log in front of core (Traefik) and the site's own `/bilet/*`
logs are outside core: they must not keep these paths either.

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
2. **Programme, description and map on the ticket.** A print-at-home
   ticket (Biletix, TCDD's e-ticket, FlixBus's boarding pass) is A4 with
   the ticket at the top, as ours; they fill the rest with terms and
   advertising, we with the Event's programme, description and place.
3. **The PDF is not kept**, unlike a certificate: it is drawn when asked.
