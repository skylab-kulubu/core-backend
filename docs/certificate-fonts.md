# Certificate fonts

A team, or the club, uploads its own fonts for its certificate templates
(certificate editor v2, ticket 18). A font is one face: a family, a weight and
a style. pdf-tools checks the file and writes it as WOFF2; core keeps that file
and embeds it in every page that sets text in it. Errors are RFC 9457 problems
with a `code` ([certificate-template-errors.md](certificate-template-errors.md)).

## Model

`certificate_fonts` (migration `20261127183000`):

| Column | |
|---|---|
| `id` | |
| `owner_team` | The team whose templates use it; `''` for the club's, which every template may use. Upper case. |
| `family` | The font's family (name ID 16, else 1), 1–80 characters. |
| `weight` | OS/2 `usWeightClass`, 1–1000. |
| `style` | `normal` or `italic`. |
| `media_id` | The checked WOFF2: a `certificate_font` Media, private and encrypted. |
| `missing_turkish` | The letters of `ğĞıİşŞçÇöÖüÜ`, `A–Z`, `a–z`, `0–9` the font lacks (`''`: all there). A font that lacks some is not refused; the panel warns, and a missing letter prints in the element's fallback. |
| `uploaded_by` | Who added it (set to null when their account is erased). |
| `created_at`, `archived_at` | |

One current face per team: `(owner_team, lower(family), weight, style)` is
unique among the fonts not archived (`409 font_exists`). An archived font keeps
its row and its file.

The row links its file in the Media attachment role `certificate_font`. A text
element names the file in `fontMediaId`, and the layout links it too, as a
certificate asset (`certificate_layout_media_ids` counts `fontMediaId`), so a
published version copies it like an image: a version prints what it was
published with, whatever happens to the font later.

## Who

- **Add and archive** a team's font: who may create its templates (a Leader,
  `certificate:template:manage`); the club's: who may create a club-wide one
  (Privileged: YK, ADMIN). The upload rule is `certificate_font_editor` (`can`
  key `media.uploadCertificateFont`): the template rule alone. An Event's
  editor who may not create a template, who makes the Event template and
  uploads its background (`certificate_template_editor`), does not upload
  fonts.
- **Use** a team's fonts (list them, read their files, set text in them): who
  may read or edit its templates, or edit its Events (an Event template is
  theirs). The club's: anyone who may make a template or an Event template for
  some team.

## Adding a font

1. The panel uploads the file as a `certificate_font` Media (`POST
   /v1/media`, multipart field `purpose=certificate_font`): TrueType, OpenType (CFF) or WOFF2,
   told by the first bytes, at most 10 MiB, private, pending for 24 hours.
2. `POST /v1/certificate-fonts {"mediaId": "…", "ownerTeam": "ARTLAB"}`
   (`ownerTeam` `""` for the club's). Core reads the upload (only the caller's
   own `certificate_font` upload: anything else is `404`), sends it to
   pdf-tools (`/v1/font/sanitize`, [pdf-tools/README.md](../pdf-tools/README.md)):
   OTS (the checker browsers run on web fonts), then fontTools; pdf-tools
   answers the facts and the checked font as WOFF2. Core checks that answer
   (a WOFF2 of at most 10 MiB), stores the WOFF2 as a new `certificate_font`
   Media of the caller's and writes the row, which attaches it. The upload is
   never linked: it expires in 24 hours.
3. `201` with the font.

pdf-tools' refusals: `font_unreadable` (not a font, refused by OTS, no family
name, over 10 s), `font_too_large`, `font_unsupported` (a variable font or a
collection), `font_embedding_restricted` (OS/2 `fsType` Restricted License,
or bitmap embedding only). Without pdf-tools (`PDF_TOOLS_URL` unset), or with
a pdf-tools older than `/v1/font/sanitize` (its `404`), adding a font is `503
pdf_tools_unavailable`; fonts already added keep working, issuance never calls
pdf-tools. pdf-tools checks one font at a time (`503 busy` after 10 s, tried
once more).

## Using a font

An element of a text kind (`staticText`, `recipientName`, …) sets
`fontMediaId` to the font's `mediaId` and `fontFamily` to its family (what the
editor shows); its `fontWeight` and `fontStyle` are the face's. Saving a draft
checks every font it links that the draft did not have: a current font of the
template's team or the club's (an Event template: its Event's team), else `400
layout_font_out_of_scope`. A font the draft had already stays, archived or not.
A new Event template keeps the fonts of the layout it copies
(`POST /v1/events/{eventId}/certificate-template`): one archived since, or
another team's, stays as in the source.

A font's file, or a `certificate_font` upload, is no background and no image:
naming one as `backgroundMediaId`, `backgroundOriginalMediaId` or an image's
`mediaId` is `400 asset_type_unsupported` when the draft is saved.

The page embeds one `@font-face{font-family:'cf-<mediaId>';font-style:<the
element's>;font-weight:1 1000;src:url(data:font/woff2;base64,…)}` a file (and a
style), whatever weights its elements ask: the face claims every weight, so
the file itself draws each and the browser makes no bolder or slanted copy
of it. The element is set in `'cf-<mediaId>',` followed by `fontFamily`'s
stack. A font no element uses is not on the page. Gotenberg's image does not
change.

A background text conversion matches the PDF's fonts against the template's
team's and the club's fonts first
([certificate-template-errors.md](certificate-template-errors.md), "Background
text conversion").

## Routes

| Route | |
|---|---|
| `GET /v1/certificate-fonts?team=ARTLAB` | The current fonts of the team and the club the caller may use, by team, family, weight, style (`Cache-Control: no-store`). Without `team`, the club's. A team the caller may not use is `403`. |
| `GET /v1/certificate-fonts?template={id}` | The editor's list for a template the caller may edit (an Event template's editor and a Co-organizer included; else `403`/`404`): the current fonts of its team (an Event template: its Event's) and the club's, then the fonts its draft uses that are none of those, `"draftOnly": true` (archived, or another team's in a copy): shown and kept, not offered for new text. `template` wins over `team`. |
| `POST /v1/certificate-fonts` | `{mediaId, ownerTeam}` → `201` with the font (above). |
| `GET /v1/certificate-fonts/{id}/file` | The checked WOFF2: `Content-Type: font/woff2`, `Cache-Control: private, max-age=86400`, `X-Content-Type-Options: nosniff`. An archived font's file too (its drafts still use it). Who may use the font's team's fonts reads it, and so does who may read a current template whose draft or a published version uses it (a copy whose team changed, a Co-organizer); anyone else `404`. |
| `DELETE /v1/certificate-fonts/{id}` | Archives it (`204`; again is `204`). It leaves the list and no new element may take it. A font the caller may use but not archive is `403`, one they may not see `404`. |

A font:

```json
{
  "id": "…",
  "ownerTeam": "ARTLAB",
  "family": "Brandon Grotesque",
  "weight": 700,
  "style": "normal",
  "mediaId": "…the checked WOFF2, the elements' fontMediaId…",
  "missingTurkish": "ğı",
  "uploadedBy": "…",
  "createdAt": "2026-11-27T18:30:00Z"
}
```

The editor loads a file with `fetch` and `new FontFace('cf-<mediaId>', buffer)`:
no font URL is in its CSS, so its `font-src` does not change.
