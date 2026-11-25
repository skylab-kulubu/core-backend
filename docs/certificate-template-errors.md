# Certificate template errors

Saving, previewing or publishing a certificate template (`POST /v1/certificate-templates`, `PUT /v1/certificate-templates/{id}`, `POST /v1/certificate-templates/{id}/preview`, `POST /v1/certificate-templates/{id}/publish`) the Event preview (`POST /v1/events/{eventId}/certificates/preview`) and the asset routes (`GET /v1/certificate-assets/{id}/info`, `GET /v1/certificate-assets/{id}/preview`, `GET /v1/certificate-assets/{id}/text`, `POST /v1/certificate-assets/{id}/text-conversion`, below) answer a refused layout, asset or rendering with `400` and a `code` (RFC 9457 extension member), so the panel can say what to change, and a renderer that is down with `503` and `renderer_unavailable` (Gotenberg) or `pdf_tools_unavailable` (pdf-tools). A `400` without a `code` is still a malformed request (an unreadable body or id).

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
| `background_size_unknown` | An image background with a placement whose natural size core cannot read (PNG, JPEG, GIF, WebP, or an SVG with a `width` and `height` in px, pt, pc, mm, cm or in, or a `viewBox`), or a side under 0.001 px. |
| `asset_empty` | A background or image Media has no bytes. |
| `asset_too_large` | An image over 20 MB, or a template's assets over 50 MB together. |
| `asset_type_unsupported` | A background that is neither an image nor a PDF, or an image element that is not an image. |
| `asset_storage_unavailable` | Core runs without the storage the assets are copied to. |
| `background_pdf_unreadable` | A PDF background that is not a PDF the renderer can read. |
| `background_pdf_too_large` | A PDF background over 20 MB. |
| `background_pdf_pages` | A PDF background that is not exactly one page. |
| `background_pdf_encrypted` | A PDF background that is encrypted or password protected (pdf-tools reads it). |
| `background_text_selection_empty` | A text conversion with nothing chosen, or a chosen piece that cannot be converted, is not one of the PDF's (an id from an older list), or lies outside the page as placed. |
| `background_text_strip_failed` | pdf-tools could not remove the chosen text from the PDF (its `strip_incomplete`): the PDF was not changed. |
| `background_pdf_no_text` | A text conversion of a PDF that has no text at all: its text was turned into outlines (Canva's and Illustrator's outline export) or it is a picture. The text list says which (`textMissing`). |
| `renderer_unavailable` | `503`: core runs without its PDF renderer (Gotenberg), cannot reach it, or it failed on its side (5xx). A later try may pass; an issuance job tries again, at most five times. |
| `pdf_tools_unavailable` | `503`: core runs without pdf-tools (`PDF_TOOLS_URL` unset), cannot reach it, it failed on its side (5xx), or it answered something that is not what it should (no PDF, no PNG). A later try may pass. A busy pdf-tools (503) gets one more try a moment later, then this. Only a placed PDF background and the asset routes of a PDF need pdf-tools; issuance never calls it. |
| `render_failed` | The renderer refused the page (4xx) or answered no PDF. |
| `template_event_scoped` | An Event template bound to a team, the club or another Event (`PUT /v1/certificate-template-bindings`); it serves its own Event only ([certificate-event-templates.md](certificate-event-templates.md)). |
| `template_event_exists` | `409`: the Event has an Event template already (`POST /v1/events/{eventId}/certificate-template`). The answer's `templateId` is the current one, which the panel opens instead. |

## Fonts

An element's `fontFamily` is one of the renderer's fonts (Arial, Helvetica, Carlito, Noto Sans, Noto Sans Display, DejaVu Sans, DejaVu Sans Condensed, Liberation Sans, Georgia, Times New Roman, Caladea, Noto Serif, Noto Serif Display, DejaVu Serif, DejaVu Serif Condensed, Liberation Serif, Courier New, DejaVu Sans Mono, Liberation Mono) or one of core's catalog: Montserrat, Poppins, Open Sans, Roboto, Inter, Raleway, Playfair Display, Merriweather, Great Vibes, Dancing Script. Any other name prints in Liberation Sans. Names match in any case.

A catalog font used by the layout's text is embedded in the page it prints (`@font-face`, WOFF2 data URIs, Google Fonts' latin and latin-ext subsets, which hold every Turkish letter); a font the layout does not use is not. Each has 400 and 700 and an italic 400; Great Vibes and Dancing Script have 400 alone. The files and each family's `OFL.txt` are in `internal/certificate/fonts/`.

A page with an embedded font fits its text (shrink-to-fit, the Owner team's short code) once the fonts are in (`document.fonts.ready`) and then sets `window.__certFitDone`; core asks Gotenberg to print it once `window.__certFitDone === true` (`waitForExpression`). Gotenberg checks every 100 ms until the request times out, which answers `503 renderer_unavailable` (tried again); the flag is set even when fitting fails. A page without embedded fonts is posted as before, with no wait.

## PDF backgrounds

A one-page PDF background is kept as a vector: Gotenberg renders the layers as HTML and lays the PDF under them as a watermark (`watermarkSource=pdf`). Gotenberg reads `watermarkOptions` as a map of strings, so every value is quoted (`{"scale":"1","opacity":"1","rotation":"0"}`); a number in it is a `400` from Gotenberg, which publish answered until 2026-11-25 as a bare `400`. `rotation` 0 keeps pdfcpu from drawing the watermark on the diagonal.

pdfcpu's `scale` is relative to the page (`scale 1 rel`), measured on pdfcpu v0.15.0, the one Gotenberg 8.37 builds (`pkg/pdfcpu/model/watermark.go`, `CalcBoundingBox` and `LowerLeftCorner`): a PDF wider than tall, or square, is scaled to the page's width; a taller one to the page's height; it is centred on both axes (anchor `c`). Its size is the page's visible box (CropBox, else MediaBox). So a PDF whose shape is not the page's leaves margins on two sides, or reaches past the page and is cut off (a landscape PDF less wide than a landscape page, for one).

A layout without a `backgroundPlacement` is still printed this way, without pdf-tools.

### Placed PDF backgrounds

A layout with a `backgroundPlacement` and a PDF background is placed by pdf-tools (`PDF_TOOLS_URL`, [pdf-tools/README.md](../pdf-tools/README.md)): `/v1/inspect` reads the PDF's visible box, core computes the box (below, the PDF's pt × 96/72 as its natural size) and `/v1/place` draws the PDF's page into it on a new page of the layout's size in pt (px × 72/96), cut to the page. Gotenberg lays that placed PDF under the page as above; it has the page's size, so `scale 1 rel` fits it exactly, and the image and PDF backgrounds use the same box.

- **Publishing** stores the placed PDF beside the version's asset copy, `certificate-template-assets/<version>/<media>.placed.pdf`, encrypted in the private bucket when the asset is private, and names it in the version's asset manifest (`placed`: key, encryption, SHA-256). It counts toward the 50 MB of a template's assets. A publish that fails deletes its private copies, the placed one too. Without pdf-tools the publish is `503 pdf_tools_unavailable` and stores nothing; a layout without a placement publishes without it.
- **Issuing** a certificate, the Event preview and every other print of a published version read the version's placed copy and never call pdf-tools; a copy that does not match its SHA-256 is not printed. A version published before placed copies (2026-11-25) prints its PDF as it was published, by Gotenberg's scale above, its placement not applied: a published version does not change how it looks.
- **The draft preview** places the PDF on the fly; core keeps the placed PDFs in memory (at most 64 and 200 MiB, by the PDF's SHA-256, the page and the box).

`core_certificate_pdf_tools_errors_total{op}` (`/v1/metrics`) counts the calls that failed on pdf-tools' side, by operation (`inspect`, `place`, `render`, `strip-text`); a PDF it refuses, and a request its caller cancelled, is not counted.

## Background placement

`layout.backgroundPlacement` places the background on the page; a layout without it prints as before.

```json
"backgroundPlacement": { "fit": "contain", "x": 0.5, "y": 0.5, "scale": 1 }
```

`x` and `y` are CSS `object-position` fractions (0 left or top, 0.5 centre, 1 right or bottom); `scale` multiplies the scale the `fit` finds; `fill` ignores it, but it is still sent (1) and checked. For a `W×H` page and a background of natural size `w×h` (an image's pixels; a PDF's visible box in pt × 96/72):

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

## Certificate assets

The template editor reads a background's size and draws it from core. Both routes take a certificate asset (`certificate_asset`) or a Media uploaded without a purpose (`legacy`); another purpose's Media is `404`. They answer the Media's uploader, and whoever may edit a template (its draft or a published version) that links the Media; anyone else gets `404` too, as a Media link that is not the caller's does not say the Media exists.

`GET /v1/certificate-assets/{id}/info` answers the asset's size in layout px:

```json
{ "contentType": "application/pdf", "widthPx": 793.701, "heightPx": 1122.52, "pages": 1, "textCount": 12 }
```

A PDF's is its first page's visible box (CropBox, turned by `/Rotate`) in pt × 96/72, with its page count and the texts pdf-tools found (`/v1/inspect`); an image's is its natural size (`pages` 1, `textCount` 0). An encrypted PDF is `400 background_pdf_encrypted`; without pdf-tools a PDF is `503 pdf_tools_unavailable`.

`GET /v1/certificate-assets/{id}/preview?max=2000` answers a PDF's page as a PNG whose long side is `max` px (16–4000, default 2000; otherwise `400`), drawn by pdf-tools (`/v1/render`; core keeps the drawn pages in memory, at most 32 and 64 MiB, by the PDF's SHA-256 and `max`), and an image as itself, with its own type. Every answer has `X-Content-Type-Options: nosniff`; an SVG also `Content-Security-Policy: sandbox`.

Both answer `Cache-Control: private, max-age=300`: an asset does not change, and only the editor's own browser keeps it.

## Background text conversion

The text of a PDF background can become layers the editor edits (certificate editor v2, ticket 08). Both routes need pdf-tools and take the PDF assets the asset routes above take, for the same people; an image is `400 asset_type_unsupported`.

`GET /v1/certificate-assets/{id}/text` lists the PDF's text, one piece per run (one style along a line, pdf-tools' `runs`), in reading order, without positions. `Cache-Control: private, max-age=300`.

```json
{
  "spans": [
    {"id": "12", "text": "Katılım Belgesi", "sourceFont": "Montserrat-Bold", "font": "Montserrat", "fontWeight": 700,
     "fontSubstituted": false, "fontSizePt": 40, "convertible": true},
    {"id": "15", "text": "Ad Soyad", "sourceFont": "BrandonGrotesque-Regular", "font": "Noto Sans", "fontWeight": 400,
     "fontSubstituted": true, "fontSizePt": 32, "convertible": true},
    {"id": "20", "text": "SKY LAB", "sourceFont": "Helvetica", "font": "Helvetica", "fontWeight": 400,
     "fontSubstituted": false, "fontSizePt": 9, "convertible": false, "reason": "stroke"}
  ],
  "truncated": false
}
```

- `id` names the piece in a conversion (its first span's id).
- `font`, `fontWeight` and `fontStyle` (`"italic"`, or left out) are what a conversion sets it in; `fontSubstituted` says `font` stands in for the PDF's family. `fontSizePt` is the PDF's size in pt; the element's size in px follows the placement.
- `reason`, when `convertible` is false: pdf-tools' (`unreadable`, `stroke`, `invisible`, `clip`, `vertical`, `skewed`, `shared`, `unmatched`; [pdf-tools/README.md](../pdf-tools/README.md)), `limit` past the first 100 convertible pieces (a layout's most elements), `truncated` when the PDF has more text than pdf-tools reads (5000 spans; nothing of it can be removed then).
- A PDF that lists nothing says why in `textMissing`: `outlines` (no text, but at least 20 shapes: its text was turned into outlines, as Canva's and Illustrator's outline export does, and cannot be converted), `image` (the page is a picture), `unknown`. The list is never empty without it.

`POST /v1/certificate-assets/{id}/text-conversion` converts the chosen pieces:

```json
{
  "spanIds": ["12", "15"],
  "kinds": {"15": "recipientName"},
  "page": {"width": 1123, "height": 794},
  "placement": {"fit": "contain", "x": 0.5, "y": 0.5, "scale": 1},
  "existingElements": 4
}
```

- `kinds` turns a chosen piece into `recipientName`, `eventName`, `eventDates` or `issueDate`: the same box and style, the certificate's value instead of its text.
- `page` is the layout's page in px (300–4000, else `layout_size_invalid`); `placement` the background's (`layout_background_placement_invalid` when it is not one); without it, as a layout without a placement prints a PDF (above).
- `existingElements` is how many elements the draft keeps; with the new ones they stay within 100 (`layout_element_count`).
- A malformed body (a kind core does not know, a kind for a piece not chosen, over 500 ids) is a `400` without a code.

Core reads the PDF (decrypting a private one), asks pdf-tools for its text, turns the chosen pieces into elements, has pdf-tools remove exactly their text-showing operators (`/v1/strip-text`, which reads its result back and refuses with `strip_incomplete` when a chosen text is still there or another one moved) and stores the result as a new certificate asset: private, encrypted, the caller's, pending, so it is purged in 24 hours unless a template links it. It answers (`Cache-Control: no-store`):

```json
{
  "backgroundMediaId": "…the new PDF…",
  "backgroundOriginalMediaId": "…the PDF converted…",
  "elements": [{"id": "bgtext-…", "kind": "staticText", "text": "Katılım Belgesi", "x": 286.44, "y": 218.1, "width": 535.28, "height": 64,
                "fontFamily": "Montserrat", "fontSize": 53.33, "minFontSize": 45.33, "fontWeight": 700, "lineHeight": 1.2,
                "color": "#1a2659", "align": "center", "fit": "shrink", "origin": "backgroundText", "sourceFont": "Montserrat-Bold"}],
  "warnings": [{"spanId": "15", "elementId": "bgtext-…", "kind": "fontSubstituted", "from": "BrandonGrotesque-Regular", "to": "Noto Sans"}]
}
```

Nothing is saved: the editor sets the draft's background to `backgroundMediaId` and `backgroundOriginalMediaId` to the PDF converted (a draft that has an original already keeps its own), adds the elements, and the draft is saved as any other. `backgroundOriginalMediaId` is never printed and a published version copies no asset of it, but the template links it (the Media attachment keeps it), so the editor can go back: background = original, the original's field and every `origin: "backgroundText"` element removed. It needs a background beside it, else the layout is a `400` without a code.

### The rules

- **Pieces and paragraphs.** Each run is an element with its own box: a line of one style is one element, a line where the style changes (a bold word, another colour) one element per style. Lines alone on their baseline, of one style and in one of pdf-tools' blocks, evenly spaced (their gaps within 10% of each other) and sharing an edge within 1 pt are one element, a paragraph: its alignment that edge, its `lineHeight` their spacing over the size, its lines kept (`\n`). A line not chosen splits a paragraph. A piece turned into a field never joins one.
- **Alignment.** A one-line element is centred when its middle is within 2% of the page's middle (along the text), left aligned otherwise; a paragraph whose lines are of one width likewise.
- **Box.** The text's extent along its baseline; across, a line box of `1.2 ×` the size centred on the middle of the PDF font's ascent and descent (a paragraph: one line box per line). It grows 15% from the edge its alignment keeps (both sides when centred) and is cut to the page. A one-line element has `fit: "shrink"` and `minFontSize` 0.85 × its size: a wider stand-in font shrinks it a little rather than breaking the line. A paragraph wraps instead. A piece off the page as placed is left out.
- **Placement and turn.** pt on the PDF's visible page become px by the placement's box (`fill` scales width and height apart: the size follows the height). A turned text keeps its angle as the element's `rotation`, about the box's centre. Sheared, mirrored and vertical text is not convertible (pdf-tools).
- **Style.** `color` is pdf-tools' sRGB (CMYK and other colour spaces converted), `opacity` its fill alpha (left out at 1). The text is NFC (a decomposed ş becomes one letter), ligatures spelled out. A size over 300 px is 300 (warning `fontSizeBounded`).
- **Fonts** (`internal/certificate/fontmap.go`). The subset prefix (`ABCDEF+`) goes; the weight comes from the name (Thin 100 … Black 900) or the font's description, italic from the name (Italic, Oblique) or its angle. The family, in order: (1) a font the template's team or the club uploaded (ticket 18; none yet), (2) core's catalog or the renderer's font of that family or a known name of it (`ArialMT` → Arial, `TimesNewRomanPSMT` → Times New Roman, `Helvetica-Bold` → Helvetica 700, `Montserrat-Bold` → Montserrat 700, Calibri → Carlito, Cambria → Caladea), (3) by class: a script font (Script, Vibes, Brush, Allura, Pinyon, Dancing, Signature, or flagged so) Great Vibes, a monospace one DejaVu Sans Mono, a serif one Noto Serif, any other Noto Sans. Great Vibes and Dancing Script are set at 400, upright. The element keeps the PDF's name in `sourceFont` and a stand-in is warned of (`fontSubstituted`). The PDF's embedded font file is never used: a subset holds only the letters the PDF prints.

### Element fields

- `origin`: `"backgroundText"` for a converted element, else left out.
- `sourceFont`: the PDF's font name, at most 120 characters.
- `fontStyle`: `"italic"` or left out; the page prints `font-style:italic`. Another value of any of the three is `layout_element_style_invalid`. A layout without them prints and checksums as before.

### Limits

pdf-tools reads one page of at most 20 MiB, at most 5000 spans, 15 s per request; an encrypted PDF is `background_pdf_encrypted`, more than one page `background_pdf_pages`. A conversion takes at most 500 ids and makes at most 100 elements. Without pdf-tools both routes are `503 pdf_tools_unavailable`; `core_certificate_pdf_tools_errors_total{op="strip-text"}` counts strip-text's failures on its side.
