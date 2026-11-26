# Event theme

An Event picks the look of what its participants get: its PDF ticket, its
participants' mails and its ticket page (the club site). The themes are
code, not data: core's catalogue (`internal/ticket/theme`), the same key
words as the forms service's brand. Kurumsal (SKY LAB) is the default; the
catalogue has one more today, `artlab`. Each surface draws a theme in its own
code by the key: core the PDF, SkyNotify the mails (one template, the theme
chosen when it is sent), the site the ticket page. Hub plan:
`notes/etkinlik-temasi-plan-2026-11-26.md`; contract for the panel, the site
and SkyNotify: `.scratch/etkinlik-temasi/SOZLESME.md`.

## The Event's fields

| Field | Read | Write |
|---|---|---|
| `theme` | the theme's key, `null` for Kurumsal (and for a stored key the catalogue no longer has) | a key of the catalogue; `null`, `""` or `"kurumsal"` is Kurumsal (stored `NULL`); any other `400 event_theme_unknown`; left out keeps it |
| `ticketAccentColor` | `#RRGGBB` in capitals, `null` for none | `#rrggbb` in any case; `null` or `""` clears it; any other `400 event_ticket_accent_color_invalid`; left out keeps it |

On `POST /v1/events`, `PUT` and `PATCH /v1/events/{id}`; whoever may edit the
Event. Every answer of the Event carries both, the public one too. A copy
keeps both ([event-copy.md](event-copy.md)). Migration 20261127200000 adds
`events.theme VARCHAR(32)` and `events.ticket_accent_color VARCHAR(7)`, both
`NULL`, their checks (the key's shape, `^[a-z0-9-]{1,32}$`, and `#RRGGBB`)
`NOT VALID`; its down keeps them.

## The catalogue: `GET /v1/event-themes`

Public, the same for everyone (`Cache-Control: public, max-age=300`): the
themes in the panel's order, the default first.

```json
[
  {"key": "kurumsal", "name": "SKY LAB (Kurumsal)", "default": true,
   "preview": {"paper": "#FFFFFF", "ink": "#000000", "accent": "#7C3AED", "secondary": "#7C3AED"}},
  {"key": "artlab", "name": "ARTLAB", "default": false,
   "preview": {"paper": "#0A1222", "ink": "#EAF2FF", "accent": "#FF8A1F", "secondary": "#3AA0FF"}}
]
```

## The ticket page

`GET /v1/public/tickets/{key}` ([ticket-page.md](ticket-page.md)) answers
`theme.key`, the Event's theme (`kurumsal` for none), and `theme.accent`:
the Event's `ticketAccentColor`, else the theme's own colour (a theme other
than Kurumsal: ARTLAB's `#FF8A1F`), else the cover's first colour, else
`#8B5CF6`. It also answers `ticketAccentColor` itself (`null` for none).

## The PDF ticket

Drawn with the Event's theme on the fixed layout: the QR is black on white,
44 mm, in the same cell in every theme. **Kurumsal's PDF is the same, byte
for byte, as before themes** (its HTML and page head, `TestKurumsalTicketUnchanged`).

A theme's own drawing is in `internal/ticket/theme` alone (`theme.Look`): its
typefaces (embedded static WOFF2, Latin and Latin Extended), its CSS after
the layout's, its band without a picture, its page head, and whether it
prints the Event's poster.

**ARTLAB** (the board's variant B, 2026-11-26): the page dark to its edges
(`#0A1222` as the page box's background, under the page head), the poster's
orange `#FF8A1F` and electric blue `#3AA0FF`, headings in Exo 2 800 and 900
(SIL Open Font License 1.1, `fonts/OFL-Exo2.txt`; the poster's own face lacks
Turkish capitals, Exo 2 has them: `TestExo2DrawsTurkish`), text in Manrope.
The QR stays 44 mm on white, in a rounded white box with 2 mm more white
around it. The page head shows ARTLAB's icon and "ARTLAB · SKY LAB". The map
is darkened by its opacity (a CSS filter would be rasterized into a larger
file). At the top:

- the Event's own ticket image (an "Ayrı görsel", [ticket-image.md](ticket-image.md))
  **whole, as a 3 : 1 poster** across the page (cut about its centre, 2150
  pixels wide, a JPEG of at most 144 KB);
- a cover region: the band it was cut as (182 × 24 mm);
- no choice, or an image that cannot be read: the theme's band (dark, an
  orange-to-blue line along its foot), never the cover's middle.

`ticketAccentColor` colours the PDF only as Kurumsal's band without a
picture. What Gotenberg receives for a long ARTLAB ticket with the largest
poster and a map stays under 640 KB (`TestArtlabDocumentSize`); the PDF
embeds the images as they are and the fonts' used glyphs only (a full
ARTLAB sample measured 255 KB with Chromium 153, under the mail
attachment's 512 KB).

## Mails

Every participant's mail carries two more variables, always (empty for
none; SkyNotify renders with `missingkey=error`):

| Variable | Value |
|---|---|
| `Theme` | the theme's key, `""` for Kurumsal |
| `EventBannerUrl` | the Event's poster: its own ticket image's page size (`media.SizePage`, 1200 pixels on the longer side, the original's format) or its original when it is smaller, an https JPEG or PNG; `""` without an "Ayrı görsel", for a cover region, an image that cannot be served, of another format, or an address that is not https |

The mails: `event.ticket-created`, `event.waitlist-joined`,
`event.waitlist-promoted`, `event.ticket-cancelled`, `event.reminder`,
`event.updated`, `event.changed`, `core.event-cancelled`,
`core.event-reinstated`, `event.attendance-welcome`,
`event.attendance-recorded`, `core.feedback-request`,
`core.feedback-reminder` and `core.certificate`. Not the team's own (door
staff, Co-organizers, the certificate's automatic finalization). A template
that does not read them ignores them; SkyNotify's template reads them
guarded: `{{if and .Theme (eq .Theme "artlab")}}`.

## Deviations

Themes are code (Microsoft Teams' webinar themes and Dynamics' brand
profiles are data): one theme beside the default, and a typeface, a PDF
layout and a page head cannot be data safely. Each surface defines its
themes in its own code (services do not read one another, ADR 0075); the key
words are shared. The QR is black on white, at its place and size, in every
theme (the door reads it the same way).
