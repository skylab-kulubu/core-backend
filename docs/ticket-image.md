# Ticket image: the PDF ticket's band

The PDF ticket ([ticket-page.md](ticket-page.md)) opens with a band across
the top of its first page, 182 × 24 mm. An Event's editors choose what it
shows:

| `source` | The band shows |
|---|---|
| `auto` (default, no row) | the middle of the Event's cover, as before |
| `cover` | a region of the cover |
| `upload` | a region of an image of its own (a logo, a banner) |

Without a picture (no cover, an SVG cover, an image that cannot be read)
the band is the cover's first colour, else the theme's.

## The region

- `crop` is `{x, y, w, h}`: fractions of the image's width (`x`, `w`) and
  height (`y`, `h`), each in 0–1, `x + w ≤ 1`, `y + h ≤ 1`, sides at least
  0.01. The image itself is never changed.
- The panel's cropper keeps the band's shape (182 : 24). Core narrows a
  region of another shape about its centre to the band's.
- A `cover` region keeps `coverMediaId`, the cover it was drawn on. When
  the Event's cover is another, the band shows the new cover's middle and
  the answer says `coverChanged: true`.
- An `upload` image whose Media is gone (`media_id` set null) shows the
  cover's middle.

## Drawing

- The band is cut from the image's **original** (JPEG, PNG, WebP or GIF's
  first frame, at most 10 MiB) when it is the image core recorded: its
  decoded size is the Media's width and height, at most 2560 px a side. An
  older original (stored before re-encoding: EXIF orientation not applied,
  larger, or of no recorded size) and one the band cannot be cut from give
  the `page` size instead, which core made upright.
- It is **2150 px wide (300 dpi at 182 mm)**, or the region's own width
  when that is less (never enlarged), as a JPEG of at most **64 KB**: quality
  80, 70, 60, 50, then narrower, down to 600 px. The PDF stays under the
  mail attachment's 512 KB (`TestDocumentSize`).
- Bands are kept 30 minutes, 64 of them, by image and region.

## API

`GET /v1/events/{id}/ticket-image` and `PUT /v1/events/{id}/ticket-image`,
for whoever may edit the Event (its Co-organizers included). The ticket
page JSON does not change: the site's card shows the cover.

```json
PUT {"source": "upload", "mediaId": "…", "crop": {"x": 0, "y": 0.4, "w": 1, "h": 0.13}}
PUT {"source": "cover", "coverMediaId": "…", "crop": {"x": 0.1, "y": 0.2, "w": 0.8, "h": 0.105}}
PUT {"source": "auto"}
```

`coverMediaId` left out takes the Event's saved cover; another one (a cover
not saved yet) is `ticket_image_invalid`. The answer:

```json
{
  "source": "upload",
  "mediaId": "…",
  "mediaUrl": "https://cdn.yildizskylab.com/images/…",
  "crop": {"x": 0, "y": 0.4, "w": 1, "h": 0.13},
  "coverChanged": false,
  "updatedAt": "2026-11-25T21:00:00Z",
  "band": {"widthMm": 182, "heightMm": 24}
}
```

| Status | `code` | When |
|---|---|---|
| 400 | `ticket_image_invalid` | unknown source, region out of the image, `cover` without a cover or with another cover than the Event's, `upload` without an image |
| 403 | | not an editor of the Event |
| 403 | `media_team_mismatch` | the image is used on an Event of another Owner team |
| 404 | `event_not_found` | no such Event (a core without this route answers 404 without a code) |
| 422 | `media_purpose_mismatch`, `media_not_linkable` | the image's purpose does not fit, or it cannot be linked |

## Media

The uploaded image is an Event image (`event_cover` or `event_gallery`, as
a video's poster). The Event owns its Media attachment in the role
`event_ticket_image` (no legacy Media), written by the statement that writes
`event_ticket_images.media_id` (migration 20261127190000). The Team media
library rule applies: another team's Event's image is refused.

A copy of an Event does not take the choice yet (the copy's band is its
cover's middle).
