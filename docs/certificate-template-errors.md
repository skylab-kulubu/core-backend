# Certificate template errors

Saving, previewing or publishing a certificate template (`POST /v1/certificate-templates`, `PUT /v1/certificate-templates/{id}`, `POST /v1/certificate-templates/{id}/preview`, `POST /v1/certificate-templates/{id}/publish`) and the Event preview (`POST /v1/events/{eventId}/certificates/preview`) answer a refused layout, asset or rendering with `400` and a `code` (RFC 9457 extension member), so the panel can say what to change. A `400` without a `code` is still a malformed request (an unreadable body or id).

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
| `asset_empty` | A background or image Media has no bytes. |
| `asset_too_large` | An image over 20 MB, or a template's assets over 50 MB together. |
| `asset_type_unsupported` | A background that is neither an image nor a PDF, or an image element that is not an image. |
| `asset_storage_unavailable` | Core runs without the storage the assets are copied to. |
| `background_pdf_unreadable` | A PDF background that is not a PDF the renderer can read. |
| `background_pdf_too_large` | A PDF background over 20 MB. |
| `background_pdf_pages` | A PDF background that is not exactly one page. |
| `renderer_unavailable` | Core runs without its PDF renderer (Gotenberg). |
| `render_failed` | The renderer refused or failed the page. |

## PDF backgrounds

A one-page PDF background is kept as a vector: Gotenberg renders the layers as HTML and lays the PDF under them as a watermark (`watermarkSource=pdf`). Gotenberg reads `watermarkOptions` as a map of strings, so every value is quoted (`{"scale":"1","opacity":"1","rotation":"0"}`); a number in it is a `400` from Gotenberg, which publish answered until 2026-11-25 as a bare `400`. `rotation` 0 keeps pdfcpu from drawing the watermark on the diagonal.
