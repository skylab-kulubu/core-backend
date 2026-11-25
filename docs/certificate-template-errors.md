# Certificate template errors

Saving, previewing or publishing a certificate template (`POST /v1/certificate-templates`, `PUT /v1/certificate-templates/{id}`, `POST /v1/certificate-templates/{id}/preview`, `POST /v1/certificate-templates/{id}/publish`) and the Event preview (`POST /v1/events/{eventId}/certificates/preview`) answer a refused layout, asset or rendering with `400` and a `code` (RFC 9457 extension member), so the panel can say what to change, and a renderer that is down with `503` and `renderer_unavailable`. A `400` without a `code` is still a malformed request (an unreadable body or id).

## Required layers

A layout needs the recipient's name (`recipientName`) and at least one of the verification QR (`verificationQr`) and the serial (`serial`). The Event's name (`eventName`) is optional since 2026-11-25 (ADR-0040 addendum): a background may print it already.

## Codes

| Code | Meaning |
|---|---|
| `template_name_invalid` | The name is empty or over 160 characters. |
| `template_source_invalid` | The source kind is not `upload`, `canva`, `figma` or `sky`, or its reference or edit address is too long or not an http(s) address. |
| `layout_size_invalid` | The page is not between 300 and 4000 px on each side. |
| `layout_orientation_invalid` | The orientation is not `landscape` or `portrait`. |
| `layout_color_invalid` | A page or element color is not a CSS color. |
| `layout_element_count` | No element, or more than 100. |
| `layout_element_id_invalid` | An element id is empty, over 80 characters or used twice. |
| `layout_element_kind_unknown` | An element of a kind core does not draw. |
| `layout_element_out_of_bounds` | An element starts before or ends past the page, or has no size. |
| `layout_element_style_invalid` | A font size, weight, line height, letter spacing, border, rotation, opacity, alignment or font name out of range. |
| `layout_text_too_long` | A text over 4000 characters. |
| `layout_image_missing` | An image element without its Media. |
| `layout_recipient_name_missing` | No recipient's name. |
| `layout_not_verifiable` | Neither the verification QR nor the serial. |
| `layout_background_placement_invalid` | A background placement with an unknown `fit`, an `x` or `y` outside 0–1, a `scale` outside 0.1–10, or no background. |
| `background_size_unknown` | An image background with a placement whose natural size core cannot read (PNG, JPEG, GIF, WebP, or an SVG with a size or a `viewBox`). |
| `asset_empty` | A background or image Media has no bytes. |
| `asset_too_large` | An image over 20 MB, or a template's assets over 50 MB together. |
| `asset_type_unsupported` | A background that is neither an image nor a PDF, or an image element that is not an image. |
| `asset_storage_unavailable` | Core runs without the storage the assets are copied to. |
| `background_pdf_unreadable` | A PDF background that is not a PDF the renderer can read. |
| `background_pdf_too_large` | A PDF background over 20 MB. |
| `background_pdf_pages` | A PDF background that is not exactly one page. |
| `renderer_unavailable` | `503`: core runs without its PDF renderer (Gotenberg), cannot reach it, or it failed on its side (5xx). A later try may pass; an issuance job tries again. |
| `render_failed` | The renderer refused the page (4xx) or answered no PDF. |

## PDF backgrounds

A one-page PDF background is kept as a vector: Gotenberg renders the layers as HTML and lays the PDF under them as a watermark (`watermarkSource=pdf`). Gotenberg reads `watermarkOptions` as a map of strings, so every value is quoted (`{"scale":"1","opacity":"1","rotation":"0"}`); a number in it is a `400` from Gotenberg, which publish answered until 2026-11-25 as a bare `400`. `rotation` 0 keeps pdfcpu from drawing the watermark on the diagonal.

pdfcpu's `scale` is relative to the page (`scale 1 rel`), measured on pdfcpu v0.15.0, the one Gotenberg 8.37 builds (`pkg/pdfcpu/model/watermark.go`, `CalcBoundingBox` and `LowerLeftCorner`): a PDF wider than tall, or square, is scaled to the page's width; a taller one to the page's height; it is centred on both axes (anchor `c`). Its size is the page's visible box (CropBox, else MediaBox). So a PDF whose shape is not the page's leaves margins on two sides, or reaches past the page and is cut off (a landscape PDF less wide than a landscape page, for one).

A layout with a `backgroundPlacement` and a PDF background is still printed this way until core places the PDF itself (pdf-tools).

## Background placement

`layout.backgroundPlacement` places the background on the page; a layout without it prints as before.

```json
"backgroundPlacement": { "fit": "contain", "x": 0.5, "y": 0.5, "scale": 1 }
```

`x` and `y` are CSS `object-position` fractions (0 left or top, 0.5 centre, 1 right or bottom); `scale` multiplies the scale the `fit` finds. For a `W×H` page and a background of natural size `w×h` (an image's pixels; a PDF's visible box in pt × 96/72):

| `fit` | Scale |
|---|---|
| `contain` | k = min(W/w, H/h) · scale |
| `cover` | k = max(W/w, H/h) · scale |
| `fitWidth` | k = W/w · scale |
| `fitHeight` | k = H/h · scale |
| `none` | k = scale |
| `fill` | the page's own size, W×H (`scale` ignored) |

The box is `w·k × h·k`, at `left = (W − w·k)·x`, `top = (H − h·k)·y`; the page cuts off what reaches past it. `internal/certificate/testdata/background-placement.json` is the table the code and the panel's editor are tested with.

An image background with a placement is drawn as an `<img>` in that box, under every element. A layout without a placement keeps the old print, which is the same as:

- an image: `{"fit": "cover", "x": 0.5, "y": 0.5, "scale": 1}`;
- a PDF: `{"fit": "fitWidth", "x": 0.5, "y": 0.5, "scale": 1}` when it is wider than tall or square, `{"fit": "fitHeight", …}` when it is taller (pdfcpu, above).
