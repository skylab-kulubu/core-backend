# Media lifecycle and blob retention

`DELETE /v1/media/{id}` archives media metadata. It is idempotent, hides the
record from ordinary media reads immediately, and does not delete the R2 object.
Authorized management reads can use `GET /v1/media?lifecycle=inactive` or
`lifecycle=all`. `POST /v1/media/{id}/restore` restores the record while its
blob is still recoverable.

A person removing their own profile picture
(`DELETE /v1/users/me/profile-picture`, see
[`account-self-service.md`](account-self-service.md)) archives that upload the
same way, recorded with the person as the deleting actor, and then unlinks it
from their User shadow. No media-management authority is involved: only the
uploader's own record can be released this way, and the blob then follows the
recovery window and reference check below.

The background purge worker runs one bounded batch at startup and on its
configured interval. A record is eligible only after the recovery window. The
worker refuses to purge media that anything still uses: any Media attachment
(see [Media attachment](#media-attachment)), and, as a safety net, core's own
links checked directly (an Event cover or gallery, a User profile, a
Certificate template draft, or a published Certificate template version).
Both must say unused. The legacy report counts the core links that have no
Media attachment (see [Legacy Media](#legacy-media)); once production shows
zero, removing the direct check is media redesign ticket 18. Database triggers also reject new
durable references and new Media attachments to inactive or purged media.
The same worker then runs the expiry cleanup described under
[Media attachment](#media-attachment).

Blob deletion is a crash-safe two-phase transition:

1. `blob_purge_started_at` is committed after the locked reference check.
2. Restore is rejected with a conflict while this durable claim exists.
3. R2 deletion is retried idempotently; a process failure after object deletion
   leaves the claim intact.
4. `blob_purged_at` is committed only after object deletion succeeds.

After `blob_purged_at` is set, the metadata remains available to authorized
lifecycle views for audit, but restore returns `410 Gone`. There is no public
force-purge endpoint.

New uploads use a separate subject-bound durable staging intent. Its insert and
metadata publication both acquire the same subject lock and active/deletion-
marker guard as every other current-identity link. Core inserts the object key
and uploader subject in PostgreSQL before writing R2, then inserts media
metadata and removes that intent in one row-locked transaction. If the request
crashes, the object write is rejected, or immediate compensation cannot reach
R2, the intent survives and a bounded sweeper retries idempotent deletion. The
sweeper takes the same row lock as metadata publication and rechecks
`media.file_url`, so it cannot delete a published object. An ambiguous metadata
commit is reconciled by media ID and never triggers eager deletion while the
publication outcome is unknown. Account erasure also waits for active intents
and checkpoints subject-specific blob cleanup before deleting the identity or
completing its request.

## Serving policy

Objects are public at `<base>/<key>`, the base being `CDN_BASE` (or
`R2_PUBLIC_URL`; `https://cdn.yildizskylab.com` when neither is set, see
[Addresses](#addresses)), so the metadata a Media is stored with decides what a
browser does with it. Every write to the
bucket takes that metadata from one policy (`media.ServingMetadata`,
`internal/media/serving.go`):

- The raster formats Upload accepts (JPEG, PNG, WebP, GIF; one table shared
  with the sanitizer) and PDF are served inline with their type. The SkyForms
  admin preview frames PDFs.
- SVG keeps `image/svg+xml`, so `<img>` still renders it, but carries
  `Content-Disposition: attachment`: opening its URL downloads it instead of
  running any script a sanitizer missed (see [SVG](#svg)).
- Every other file is `application/octet-stream` with
  `Content-Disposition: attachment; filename*=…` (the Media's name, RFC 2231
  encoded), whatever type its client declared.

The policy decides only the object's metadata. The media record keeps the
file's own type, detected or declared. The copies a published certificate
template keeps of its layout Media under `certificate-template-assets/` go
through the same policy (without a name: a bare `attachment`), and their
manifest keeps the asset's own type for rendering.

Objects stored before the policy are rewritten in place by two background
backfills that start with core: one over media records not yet flagged
`serving_policy_applied`, one over certificate template versions not yet
flagged `asset_serving_policy_applied`. Each pass walks the pending rows by id,
25 at a time. Where the policy serves an object differently from the type it
was stored with, the object gets an S3 `CopyObject` onto the same key with
`MetadataDirective=REPLACE` (supported by R2's S3 API); then the row is
flagged, and nothing else on it changes. Raster images and PDFs are only
flagged. Purged blobs are skipped and a missing object counts as done. A row
that fails is logged with its id and skipped, so it never holds up the rows
after it; passes repeat a minute apart until one ends with nothing failed.
Every step is idempotent, so the backfills are safe to interrupt and re-run.
Upload and template publishing flag what they write themselves.

Known gap: until Skyforms sends the `answer_file` purpose and private Media
ships, Answer files are still public legacy media; tracked by the media
redesign. `GET /v1/media/{id}` answers a caller without a token with no
`uploadedBy` and no `name`, but a signed-in caller still sees both, and the
object itself stays reachable at its CDN address.

`X-Content-Type-Options: nosniff` cannot be stored as R2 object metadata; it
needs a Cloudflare Transform Rule on `cdn.yildizskylab.com`.

## Media purpose

Every Media has a Media purpose (ADR-0052), stored on the record as
`purpose` and returned in the Media JSON to signed-in callers. Media stored
before purposes existed are `legacy`.

### The catalogue

The purposes are defined in `config/media-purposes.json`, embedded in the
binary so a running core always uses the version that passed review with its
code. CODEOWNERS covers the file and the ceilings. Each entry has:

| Field | Meaning |
|---|---|
| `description` | What the purpose is for (for reviewers). |
| `upload` | Who may upload: see the upload rules below. |
| `types` | Content types accepted, detected from the file's content, never from its name or declared type. SVG is stored sanitized, as a download (see [SVG](#svg)). |
| `max_mib` | Maximum size in MiB. |
| `visibility` | `public` (served from the CDN) or `private`. |
| `encrypted` | Encrypted before storage; true exactly for private purposes. |
| `scan` | Needs a malware scan before it can be opened. |
| `pending_ttl` | How long a Media with no Media attachment is kept (Go duration). `none` only for the legacy rules. |
| `transport` | `single_step` (through `POST /v1/media`) or `direct` (Direct upload). |
| `attach` | Who attaches the purpose's Media: `core` (a core record links it) or `service` (another product, through the [service attach API](#service-attach-api)). |
| `service` | Only with `attach: service`: the product that attaches the purpose's Media, `forms` or `cms`. It is also the purpose's owning product: only that product may link the purpose's Media at all. |
| `image` | Image handling: `reencode`, `max_dimension`, `sizes` (size name → px on the longer side). |
| `legacy_rules` | Only on `legacy`: the rules below instead of `types` and `max_mib`. |

`pending_ttl` sets a new Media's expiry (see
[Media attachment](#media-attachment)). `attach` and `service` decide whether a
purpose can be uploaded at all: a Media nothing can attach would only wait for
its expiry, so a `service` purpose is refused with `purpose_not_available`
unless its product has a service client configured
(`MEDIA_SERVICE_CLIENTS`, see [Who may call](#service-attach-api)). Today:

- `cms_image` and `cms_file` (the CMS) stay refused: the CMS has no service
  account yet, and opening them is Yusuf's decision once it has one;
- `answer_file` (Skyforms, configured by default) stays refused with
  `private_media_disabled` until private Media ships, and
  `answer_file_large` is a Direct upload purpose;
- `club_file` and `video` name no product: where club files and videos are
  attached is for the Direct upload and video tickets (11 and 13) to settle,
  and both are Direct upload purposes anyway.

Every purpose a product's role accepts must name that product, and the
purposes core refers to in code (the core purposes, the CMS purposes and the
Answer file purposes) must all be in the file. `image` is acted on (see
[Images and sizes](#images-and-sizes)); `scan` is declared now and read when
scanning ships. `image.sizes` may name only the sizes clients can ask for,
`card` and `page`. An unknown field or value, a missing `legacy` entry, or a
ceiling violation stops core at startup.

The initial entries:

| Purpose | Upload | Types | Max | Visibility | Transport | Attached by |
|---|---|---|---|---|---|---|
| `profile_picture` | authenticated | JPEG, PNG, WebP, GIF | 5 MiB | public | single-step | core |
| `event_cover`, `event_gallery` | event_editor | JPEG, PNG, WebP, GIF, SVG | 10 MiB | public | single-step | core |
| `certificate_asset` | certificate_template_editor | PNG, JPEG, PDF | 20 MiB | private | single-step | core |
| `cms_image` | authenticated | JPEG, PNG, WebP, GIF, SVG | 10 MiB | public | single-step | cms (no service client yet) |
| `cms_file` | authenticated | PDF | 20 MiB | public | single-step | cms (no service client yet) |
| `answer_file` | authenticated | PDF, JPEG, PNG, DOCX | 20 MiB | private, scanned | single-step | forms |
| `club_file` | event_editor | PDF | 1 GiB | public, scanned | direct | not settled (ticket 11) |
| `answer_file_large` | service_only | ZIP, PDF | 1 GiB | private, scanned | direct | forms |
| `video` | event_editor | MP4 | 2 GiB | public | direct | not settled (ticket 13) |
| `legacy` | authenticated | legacy rules | legacy rules | public | single-step | core, or a product for its uploader or once it holds it (transition rule) |

Every public raster purpose re-encodes (2560 px) and gets the `card` (400 px)
and `page` (1200 px) sizes. `legacy` does neither.

### Upload rules

`upload` names one rule of the authz vocabulary (`authz.MediaUploader`). Every
rule needs a signed-in caller.

- `authenticated`: any signed-in person.
- `event_editor`: someone who may create an Event for at least one Owner
  team, by the same decision as creating the Event (`_default` fallback
  included): today a privileged person (ADMIN, YK, DK), a leader or
  coordinator of a team, or a member of a team whose Event permissions let
  members create Events (GECEKODU). Which Event the Media ends up on is
  checked when it is linked.
- `certificate_template_editor`: someone who may create a certificate
  template for at least one Owner team, by the same decision as creating the
  template: a privileged person, a leader or coordinator of a team, or a team
  member with `certificate:template:manage`.

An upload names no Owner team yet, so the candidate teams are the names in
the person's group paths (leader subgroups aside).
- `service_only`: no person. Only the owning product's service identity may
  start such an upload; until that path exists nobody can.

CMS editor roles live on the CMS client, which core does not see, so the CMS
purposes are `authenticated` for now, as Media uploaded without a purpose
are.

### Hard ceilings

`internal/media/ceilings.go` holds limits no catalogue entry can loosen.
Core refuses to start with a catalogue that breaks one:

- a public purpose accepts only raster images (JPEG, PNG, WebP, GIF), SVG,
  PDF and MP4;
- a public purpose that accepts raster images declares re-encoding
  (`image.reencode`), and core re-encodes every such image (see
  [Images and sizes](#images-and-sizes));
- only `cms_image`, `event_cover` and `event_gallery` may list SVG (never a
  profile picture or a private purpose). In code, an SVG is stored only for
  a purpose that lists it, only sanitized, under a key ending in `.svg`, and
  is always served as a download (`Content-Disposition: attachment`);
- the maximum size stays under 20 MiB for single-step uploads and 2 GiB for
  Direct upload;
- the declared image size stays within 2560 px (`image.max_dimension`,
  `image.sizes`), and re-encoding scales a larger image down to it;
- a private purpose is encrypted.

### Uploading

`POST /v1/media` takes an optional multipart field `purpose`. With a purpose,
core checks, in order: the purpose exists, the caller may upload it, it is not
private (refused until private Media storage ships), it is single-step,
something can attach it, the size, and the type detected from the content. `POST /v1/users/me/profile-picture` always uploads
as `profile_picture`: raster images up to 5 MiB, no PDF, no SVG.

Media uploaded without a purpose are `legacy` and keep the rules they have
always had: a raster image or SVG up to 10 MiB, a PDF under a `.pdf`
name up to 20 MiB, any other named file up to 20 MiB (served as a download
by the serving policy), refused with a plain `400` without a code. Once
Skyforms and CMS send purposes, `legacy` falls to the strict rule (raster
only, 5 MiB, 24 hours unless attached) by a catalogue change.

Refusals by the purpose are `application/problem+json` with a stable `code`
and extension members. This is not the full list of upload refusals: a person
over their upload budget gets `429` `media_rate_limited`, described under
[Upload limits](#upload-limits).

| Status | `code` | Extra members | When |
|---|---|---|---|
| 400 | `purpose_unknown` | `purpose` | The purpose is not in the catalogue. |
| 403 | `purpose_forbidden` | `purpose` | The caller's upload rule does not allow it. |
| 422 | `private_media_disabled` | `purpose` | A private purpose. Private Media storage (encryption, the private bucket) is not built yet, so nothing is stored; retrying does not help. |
| 400 | `purpose_requires_direct_upload` | `purpose` | A `direct` purpose sent to `POST /v1/media`. |
| 422 | `purpose_not_available` | `purpose` | A `service` purpose whose product has no service client configured (`cms_image` and `cms_file` today), or that names no product (`club_file` and `video`, which reach `purpose_requires_direct_upload` first). Nothing is stored; the file would only wait for its expiry. |
| 413 | `media_too_large` | `purpose`, `maxBytes` | Above the purpose's maximum; an SVG above 1 MiB (`maxBytes` is then 1 MiB). |
| 413 | `media_image_too_large` | `purpose`, `maxPixels` | Decoding the image would take more than core allows, judged from its header before anything is decoded (see [Decode cost](#decode-cost)). `maxPixels` is the most pixels an image of its kind may have: 50 000 000, fewer for costly pixels (16-bit PNG, progressive JPEG, an animation's many frames). An animated GIF or WebP larger than 2560 px on a side is refused this way too. |
| 415 | `media_type_not_allowed` | `purpose`, `allowedTypes` | The content is not one of the purpose's types, or starts like one but does not decode or check as it: a broken image, a WebP whose frame is not its canvas, an animated WebP whose structure does not check, a GIF with more than 300 frames or a frame outside its screen, a JPEG with more than 64 scans, an SVG core does not sanitize. |
| 503 | `media_busy` | `retryAfterSeconds`, `Retry-After` header | The upload waited 10 seconds for a [decoding slot](#decode-budget) while other images were decoded. Nothing is stored and the upload is not charged to the upload budget; retry it. |

A body above the server's limit (20 MiB plus room for the form) is still
refused by the HTTP server with a bare `413` before any purpose is read.

### Upload limits

Each signed-in person has one budget for single-step uploads, shared by
`POST /v1/media` and `POST /v1/users/me/profile-picture` (ADR-0052, media
redesign ticket 05):

- at most 100 uploads per rolling 10 minutes;
- at most 2048 MiB (2 GiB) of request body per rolling 24 hours.

These are higher than the spec's example numbers (30 and 500 MB) on purpose.
Superadmin's gallery input uploads every selected image in one loop, so a
lower count would refuse an organizer's gallery partway through, on every
retry, and leave the uploads before it unattached; a normal event photo day
would use up 500 MB. The budget is there to bound a stolen or misused
account, and 100 uploads per 10 minutes and 2 GiB a day still do that. Both
stay configurable (see [Configuration](#configuration)).

An upload is charged before the route reads its form, purpose or file. Its
size is the request body core accepted: the declared `Content-Length`, which is
exactly what the server read, or, for a chunked body, the bytes that arrived.
(The server receives the whole body, up to its limit, before any route runs;
the charge comes before the form is parsed and before anything is stored.)

- A refusal by the route (`400`, `403`, `413`, `415`, `422`) stays charged:
  its bytes were received, and free refusals would let anyone send junk
  without end.
- An upload that ends in a server error (`5xx`, a crash included) is given
  back, count and bytes: the failure is core's.
- A request the limit refuses is not charged.
- A request without a token is not counted and is still refused with `401`.

Over either limit, core answers `429` problem+json with `code`
`media_rate_limited`, a `Retry-After` header in seconds, and these members:

| Member | Meaning |
|---|---|
| `limit` | The limit hit: `uploads` (the count) or `volume` (the bytes). When both refuse, the one with the longer wait. |
| `maxUploads` | Uploads allowed per window. |
| `uploadWindowSeconds` | That rolling window, `600` by default. |
| `maxDailyBytes` | Bytes allowed per rolling 24 hours. |
| `retryAfterSeconds` | The `Retry-After` value, for callers that cannot read the header across origins. |

`Retry-After` is when the same upload would fit again.

The budget lives in core's memory (`media.UploadLimiter`): a restart clears
it. It holds a person only while they have an upload inside a window, so its
size follows the people who uploaded in the last day. If core runs more than
one replica, each keeps its own budget, which loosens the limit (never
tightens it) until the budget moves to a shared store. The account-access
Redis is deliberately not that store: it is a security projection whose ACL
allows only the gate's keys and commands. It is not fiber's `limiter`
middleware either, which counts requests only: this one also counts bytes and
gives back server failures.

Direct upload is not counted here. The product that owns a Direct upload
grant limits it; the Direct upload routes, when they land, stay off this
limiter. A test (`TestEveryRouteThatStoresAFileIsChargedToTheUploadBudget`)
fails when any route stores a file sent through core without being charged.

## Images and sizes

Media redesign ticket 04 (ADR-0052, decisions Q13 and Q24).

### Re-encoding

A raster upload for a purpose whose `image.reencode` is set (every public
image purpose) is decoded and encoded again, so nothing that came with the
pixels reaches the CDN: EXIF and other metadata, comments, bytes after the
image, polyglot payloads.

1. **Decode cost first.** Core reads the header, and the markers a decoder
   would follow, and refuses the image before any pixel is decoded when
   decoding it would take more than core allows (see
   [Decode cost](#decode-cost)): `media_image_too_large`. A JPEG with more
   than 64 scans, and a WebP whose frame is not its canvas, are
   `media_type_not_allowed`.
2. **Decode, fit, upright.** The image is decoded (a decoder that panics
   refuses the file instead of stopping core), scaled down to the purpose's
   `max_dimension` on its longer side (2560 px), and only then turned upright
   by its EXIF Orientation, so a 48 MP photo is never turned at full size
   and the stored pixels need no tag. Content that starts like an accepted
   type but does not decode is `media_type_not_allowed`.
3. **Encode.** JPEG stays JPEG (quality 85). PNG stays PNG. Go has no WebP
   encoder, so a still WebP becomes a JPEG when it is opaque and a PNG when
   it has transparency. The Media's `type` is the stored type. Its cover
   colours are picked from the same decoded image.
4. **Colours.** The image's ICC colour profile (JPEG APP2 `ICC_PROFILE`,
   PNG `iCCP`, WebP `ICCP`) is **rebuilt**, never copied, and the rebuilt
   profile embedded in the re-encoded image and every size (JPEG as APP2
   segments of at most 65 519 profile bytes, PNG as `iCCP`). No colour
   conversion is done, so a Display P3 photo keeps its colours:
   - only a monitor or scanner (`mntr`, `scnr`) `RGB ` profile with PCS
     `XYZ ` or `Lab `, and with its matrix and curves (`wtpt`, `rXYZ`,
     `gXYZ`, `bXYZ`, `rTRC`, `gTRC`, `bTRC`);
   - it keeps `desc`, `cprt`, `wtpt`, `chad`, the primaries and the
     curves, each checked against its type (`XYZ `, `sf32`, `curv`, `para`,
     `mluc`/`desc`/`text`) with every length in bounds (an `mluc` header,
     record table and strings; a v2 `desc` is rebuilt around its ASCII
     text, so a reader following its counts stays inside it) and text at
     most 4 KiB; the colour-defining tags stay byte for byte;
   - under a new header (size recomputed, profile ID and maker fields
     zeroed) and a new tag table.

   A profile of lookup tables only, a CMYK or grey profile, one above 1 MiB,
   one without the `acsp` signature or its own size, or one with any tag
   outside its bounds is dropped, and the image is then shown as sRGB. A
   fault while rebuilding drops the profile the same way (logged once per
   process) instead of refusing the upload.

**Animations** look as uploaded (decision D2):

- One ceiling for every animation: at most 300 frames, and at most
  256 Mi pixels across frames × canvas (a GIF's logical screen); more is
  `media_image_too_large` (more frames: `media_type_not_allowed`).
- A **GIF** is decoded with every frame and encoded again, frame by frame:
  its frames, delays, disposal and loop count stay; comments and other
  extensions go. Before decoding, its blocks are walked once: a frame
  outside the logical screen is refused, and frames × screen counts against
  the decode cost. Its sizes are its first frame,
  still, as PNG. An animated GIF larger than 2560 px on a side is refused; a
  one-frame GIF that large is scaled like a still image, to PNG.
- An **animated WebP** cannot be re-encoded (no Go encoder), so it is the
  one image kept as uploaded, after its structure is checked chunk by chunk
  **and every frame decodes**: the RIFF size must be the file's (no trailing
  or missing bytes); only `VP8X` (first, with the animation flag), `ICCP`,
  `ANIM`, `ANMF`, `EXIF` and `XMP ` chunks; each frame within the canvas,
  its VP8/VP8L bitstream the size its `ANMF` declares; a canvas at most
  2560 px on a side. Then each frame (its bitstream, and its `ALPH` chunk
  where present) is decoded on its own, one after another within the
  decoding slot; a frame that does not decode, or decodes to another size
  than its `ANMF` declares, refuses the file. `EXIF` and `XMP` chunks go
  (with their VP8X flags), the `ICCP` profile is rebuilt or dropped (with
  its flag), and the RIFF size is rewritten. It gets no size objects: every
  size address is the image itself.

**Scaling** uses no kernel scaler on the source: Catmull-Rom keeps a float64
buffer of source height × target width (half a gigabyte for a 48 MP photo).
A bilinear pass that reads the source in place brings the image to 2 or 4
times the target (at most 128 MiB, never above the source), and exact 2×2
averages halve it the rest of the way.

### Decode cost

Before decoding, core estimates from the header what the decoder will
allocate and refuses the image when it has more than 50 000 000 pixels or the
estimate is above 256 MiB:

| Format | Estimate |
|---|---|
| JPEG | Each component's sample plane over whole MCUs (read from the frame header, SOF); for a **progressive** JPEG also the coefficients it keeps between scans, a 256-byte block per 8×8 samples (at 4:4:4 that is 15 bytes a pixel: a progressive JPEG gets about 17 MP); a CMYK JPEG also 4 bytes a pixel for its conversion. More than 64 scans is refused. |
| PNG | The pixels at their depth (up to 8 bytes a pixel at 16 bits a channel), twice for an interlaced PNG. |
| GIF | A byte a pixel for every frame, counted as frames × logical screen, plus a 4-byte canvas of the screen for the still first frame. More than 300 frames, or a frame outside the screen, is refused. |
| WebP | The frame its VP8 or VP8L bitstream declares, read from the bitstream: an extended WebP's VP8X canvas is what `image.DecodeConfig` reports, but the decoder allocates the frame, so a frame that is not the canvas is refused. 2 bytes a pixel for VP8, 8 for VP8L, plus an alpha plane over the canvas (5 bytes a pixel when compressed). |

### Decode budget

Everything that decodes an image shares one budget per core process
(`media.DecodeBudget`, made at startup and handed to each): uploads for a
purpose, SVG sanitizing, cover colours, the cover colour backfill and the
size backfill. At most **2** images are decoded at once, and at most **1** SVG
is sanitized (it also takes one of the 2).

- An upload for a purpose waits at most 10 seconds for a slot, then answers
  `503 media_busy` with `Retry-After: 5`; nothing is stored, and the 5xx is
  given back to the person's upload budget.
- Cover colours of an image uploaded without a purpose take a slot only when
  one is free; otherwise the image is stored without them and the cover
  colour backfill picks them later. Such an upload never waits and never
  fails for the budget.
- The backfills wait like an upload; a wait that runs out leaves the image
  for the next pass.
- A slot is given back however the decode ends, a panic included. A cover
  colour pick that panics picks none, and the backfill goes on.

**Worst case on the production host** (15 GiB, no swap), per slot: the decoded
image at most 256 MiB by the estimate; the upload body and its copies up to
about 60 MiB (20 MiB, held by the HTTP server, the form and the service);
the scaling buffers at most 128 MiB plus the 26 MiB result and its upright
copy (26 MiB); the encoded image and its sizes under 40 MiB. About 540 MiB
live per slot, 1.1 GiB for both. Sanitizing an SVG (at most 1 MiB, 10 000
elements) takes a few tens of MiB at most. Go's collector lets the heap grow to
about twice what is live before collecting (`GOGC=100`), so allow ~2.5 GiB
for image work at its peak.

### Sizes

Clients ask for an image by Media and size name. The names are fixed in code,
because clients build on them: `card` (400 px, e.g. an Event card) and `page`
(1200 px, a picture across a page). Each purpose's `image.sizes` sets which
it gets and how large, on the longer side.

A size smaller than the image is stored as its own object next to it, at
`<key>/card.jpg` or `<key>/card.png` (the extension is its type, so a CDN
that caches by extension caches it), upright, with the serving policy's
metadata. A size the image already fits in is not stored: its address is the
original.

Three things are called sizes, apart:

- `image.sizes` in the catalogue: size name → pixels;
- `size_objects` on the Media record (`Media.SizeObjects`): the sizes stored
  as objects, `{"card": {"width": 400, "height": 300, "type": "image/jpeg"}}`;
  `NULL` until core has made them, `{}` when the image needs none or could
  not be read;
- `sizes` in the Media JSON: every size's address, below.

The Media also records its `width` and `height` as shown (upright).

### Media uploaded without a purpose

`legacy` Media keep their own bytes: a raster image is stripped of metadata,
not re-encoded, and gets **no sizes**, neither at upload nor from the
backfill. Sizes would cost every such upload a decode and two more writes,
and would publish card and page copies of images nobody asked to be copied
(Skyforms Answer files among them). A legacy Media gets sizes once the legacy
backfill (ticket 08) gives it a purpose that has them.

Two changes to the strip, both for JPEG:

- The EXIF Orientation is kept, alone in a minimal EXIF segment, so a
  portrait phone photo whose rotation lives only in EXIF no longer shows
  sideways.
- The file ends with the primary image's EOI. A phone JPEG can carry an MPF
  index (APP2) of secondary images stored after the primary one, each with
  its own EXIF and GPS, and a motion photo's video appended after them; the
  MPF segment and everything after the primary image are dropped. Before,
  everything after the first scan was kept.

### SVG

An SVG is stored as SVG (decision D1), sanitized, for the purposes that list
it: `cms_image`, `event_cover` and `event_gallery` — not profile pictures, not
Answer files (a code ceiling keeps it there). A file is an SVG when, within
its first 64 KiB and past its prolog (a byte order mark, the XML
declaration, comments, processing instructions, whitespace, one DOCTYPE),
its root element is `svg` in the SVG namespace (or in none). Core reads it
as XML and writes it again from an allowlist; the original bytes are never
stored:

- **Kept:** shapes and paths (`path`, `rect`, `circle`, `ellipse`, `line`,
  `polyline`, `polygon`), text (`text`, `tspan`, `textPath`), gradients
  (`linearGradient`, `radialGradient`, `stop`), `pattern`, `clipPath`,
  `mask`, structure (`svg`, `g`, `defs`, `symbol`, `use`), `title`, `desc`
  and `style`, with their geometry and presentation attributes. `href` and
  `xlink:href` only as a same-document `#id`.
- **CSS** (a `style` attribute or element, and presentation attributes)
  keeps only the functions `rgb()`, `rgba()`, `hsl()`, `hsla()`, `calc()`,
  `var()` and `url(#id)`, plus the transform functions in transforms. A
  declaration with any other function (`image-set()`, `-webkit-image-set()`,
  `cross-fade()`, `element()`, `src()`, …) or with a string that looks like
  an address is dropped. A `<style>` keeps its rules and drops every
  at-rule (`@import`, `@font-face`, `@media`) whole; it is sanitized whole,
  so a comment cannot split a name; CSS with escapes is removed. A
  presentation attribute holding a comment (`/*`) or an escape is dropped.
- **Bitmaps:** an `<image>` stays only with a `data:image/png|jpeg|gif|webp;base64,`
  URI. The bitmap is decoded and re-encoded like an uploaded raster image
  (within the SVG's decoding slot), and embedded again as PNG or JPEG. A
  bitmap too large to decode, or bitmaps that together cost more than the
  decode budget, refuse the SVG (`media_image_too_large`); the 1 MiB limit
  applies to the result too. Any other `<image>` is removed.
- **Removed, with everything inside:** `script`, `foreignObject`, `iframe`,
  `a`, markers, the animation elements (`animate`, `set`, …, which can set
  `href`), filters (`feImage` loads images), and anything outside the SVG
  namespace (editor metadata, HTML). Every `on*` attribute, every attribute
  outside the allowlist, and every value naming `javascript:`, `data:` or an
  external address go too.
- **DOCTYPE:** one DOCTYPE before the root without an internal subset (the
  line Illustrator writes, `<!DOCTYPE svg PUBLIC "-//W3C//DTD SVG 1.1//EN" …>`)
  is dropped: `encoding/xml` fetches and expands nothing, and it is never
  written out.
- **Refused** (`media_type_not_allowed`): an internal subset, an entity, a
  second DOCTYPE or any other directive; anything but whitespace, comments
  and processing instructions after the root (a second root, text); anything
  that does not parse as XML in UTF-8; a root that is not `<svg>`; more than
  10 000 elements or nesting deeper than 64; nothing left to draw once
  sanitized (no shape, path, text, `use` or bitmap), rather than a blank
  file. A fault in the parser refuses the file too. Above 1 MiB:
  `media_too_large` (`maxBytes` 1 MiB).

The sanitized SVG is stored under a key ending in `.svg`, as `image/svg+xml`
with `Content-Disposition: attachment`: `<img>` renders it, opening its
address downloads it. It gets no size objects and every size address is the
SVG itself; it has no cover colours. One SVG is sanitized at a time, within a
shared decoding slot.

**An SVG uploaded without a purpose** goes through the same sanitizer and
gets a `.svg` key too. Media uploaded without a purpose keep accepting what
they accepted, so an SVG the sanitizer refuses is stored anyway, as an opaque
download (`application/octet-stream`, `attachment`, under `files/`) that never
renders. So is one above 1 MiB (up to 10 MiB), which the sanitizer does not read.

**Yusuf, on Cloudflare:** add a Response Header Transform Rule on `cdn.` for
paths ending in `.svg` that sets
`Content-Security-Policy: sandbox; default-src 'none'; style-src 'unsafe-inline'; img-src data:`,
so an SVG opened directly still runs nothing, whatever a sanitizer missed.

### Addresses

Every address core answers with is built from a base: `CDN_BASE`, else
`R2_PUBLIC_URL`, else `https://cdn.yildizskylab.com`. It is never a bare key.
Nothing stored holds a base, so moving the CDN (or to a separate user-content
domain) is a configuration change:

- a Media keeps its object key;
- a User's profile picture is read from the Media the profile links (its key);
  `users.profile_picture_url` holds the key for new pictures, and an absolute
  address stored there before is no longer read while the Media exists. The
  API's `profilePictureUrl` is unchanged while the base is the same.

Addresses built without a service's own base (Event resources in tickets and
competitors, team rosters) use the base core sets once at startup
(`media.UsePublicBase`), a process-wide setting: those call sites have no
media dependency to carry it.

`MEDIA_IMAGE_ADDRESS_MODE` picks where sizes point:

- `stored` (default): the stored size object, or the original for a size not
  stored;
- `cloudflare`: a Cloudflare image transformation of the original,
  `<base>/cdn-cgi/image/width=N,height=N,fit=scale-down/<key>` (never
  enlarged). Transformations must be enabled on the base's zone; 5 000 unique
  transformations a month are free, then $0.50 per 1 000 (Q24). Core keeps
  storing the sizes in both modes, so switching back is a configuration change
  too.

The Media JSON (upload response, `GET /v1/media/{id}` for everyone, the media
list) carries, for a raster image of a purpose with sizes:

```json
{
  "url": "https://cdn.yildizskylab.com/images/<id>",
  "width": 1600,
  "height": 1200,
  "sizes": {
    "card": { "url": "https://cdn.yildizskylab.com/images/<id>/card.jpg", "width": 400, "height": 300 },
    "page": { "url": "https://cdn.yildizskylab.com/images/<id>/page.jpg", "width": 1200, "height": 900 }
  }
}
```

`sizes` lists every size of the Media's purpose; `width`/`height` are left
out when core does not know them. Event and User responses keep their fields
(`coverImageUrl`, `images[].url`, `profilePictureUrl`); a client reads a size
from the Media by its id.

### Stored images before sizes

A background backfill, started with core, walks the current image Media of
the purposes that have sizes whose sizes are not made (`size_objects IS
NULL`, partial index `media_size_objects_pending_idx`) by id, 25 at a time,
and makes them from the stored original, which it never rewrites (images for a
purpose stored before re-encoding keep their stripped bytes). For each image
it:

1. reads the object: gone, not raster, or a key that cannot have sizes →
   recorded with none; a read error → left for the next pass;
2. waits for a decoding slot, then records the image as done without sizes
   **before** decoding it, so a decoder that panics, or a process that dies
   out of memory, does not decode it again after a restart;
3. decodes it (scaled to the largest size before it is turned upright) and
   stores its sizes, then records them. A write that fails puts the image
   back to not made, for the next pass; sizes written for an image whose
   purge began meanwhile are deleted again.

A panic anywhere in an image's step is recovered: the image stays done
without sizes, its slot is given back, and the pass goes on.

### Deleting sizes

Every path that deletes a Media's object deletes its sizes with it (the object
first, then the sizes; a purge cut short is retried whole): the archive
and expiry purges, account erasure's immediate purge (through the same store
call), the upload staging sweepers, and a refused upload. One rule decides
which keys may have sizes and where they are (`images/…` keys, at
`<key>/{card,page}.{jpg,png}`); the upload, the backfill and the purges all
use it, and the purges delete every key it names, not only the recorded ones,
so a size an interrupted upload or backfill wrote without recording is found
too.

## Media attachment

A Media attachment links a Media to the record that uses it, in core or in
another product. Each row names the Media, the owner (`owner_service`,
`owner_type`, `owner_id`) and the Media's role there, and is unique per link
(table `media_attachments`, migration `20260926120000`). `owner_id` is the
owning product's own id for its record, as text (migration `20260926121000`):
core's records and Skyforms responses have UUIDs, a CMS page is
`<clientId>:<slug>`.

### Status and expiry

A Media carries `status` and `expiresAt`, returned in the Media JSON to
signed-in callers. Archive (`deletedAt`) and purge (`blobPurgedAt`) stay
recorded apart.

| Status | Meaning | `expiresAt` |
|---|---|---|
| `pending` | No Media attachment yet. Every new Media starts here. | Upload time plus the purpose's `pending_ttl` (24 hours for every purpose today). |
| `attached` | At least one Media attachment. Never purged by expiry. | None. |
| `detached` | Its last Media attachment was removed. | 30 days after that, except while its detach expiry is held (below). Attaching it again within the window makes it attached. |

**A legacy Media gets no expiry by itself**: not when it is uploaded
(`legacy` has `pending_ttl` `none`), and not when its last Media attachment is
removed. Skyforms, CMS and superadmin upload without a purpose today, and a
legacy Media may still be used outside core by its address (CMS content stores
addresses) after core stops linking it. The only thing that gives one an
expiry is the orphan switch Yusuf runs after reviewing the legacy report (see
[Legacy Media](#legacy-media)). **Nor does a Media whose detach expiry is
held**: one the legacy backfill gave a purpose, until the hold is released
after stage 5 (see [Detach expiry hold](#detach-expiry-hold)).

The database keeps the status in step with the Media attachments, once per
statement that adds or removes them: a Media with a Media attachment is
attached, a Media whose last one went is detached. It first locks the Media
rows the statement touched, in id order and with a lock that does not wait
for foreign key references. Two statements therefore never lock the same
Media the other way round, two links of one Media written at once do not wait
for each other, and two transactions removing the last two Media attachments
of one Media cannot both see the other one still there. (Locks taken by
separate statements of one transaction, such as an Event delete's gallery
cascade and then its cover, follow the order of those statements; a deadlock
there is detected by PostgreSQL and the transaction can be retried.)

Restoring an archived Media (`POST /v1/media/{id}/restore`) starts its expiry
again: a Media no Media attachment keeps gets its purpose's `pending_ttl` from
the restore (a legacy one, or one whose detach expiry is held, none), so a
window that ran out while it was archived does not purge it on the next pass.

### Core's own links

Core's links write and remove their Media attachments in the statement that
writes the link, whoever writes it. Statement triggers on the linking tables
do this, so every writer is covered, including account erasure's
anonymization and maintenance SQL:

| Link | `owner_type` | `role` |
|---|---|---|
| Event cover (`events.cover_image_id`) | `event` | `event_cover` |
| Event gallery (`event_images`) | `event` | `event_gallery` |
| User profile picture (`users.profile_picture_id`) | `user` | `profile_picture` |
| Certificate template draft (`draft_layout` background and image elements) | `certificate_template` | `certificate_asset` |
| Published certificate template version (`layout` and `asset_manifest`) | `certificate_template_version` | `certificate_asset` |

`owner_service` is `core` for all of them. Only a changed link is written: a
record that still links a Media archived after it was linked can be saved as
long as the link itself does not change. Replacing a profile picture
therefore detaches the previous one, which is purged 30 days later unless
something attaches it again; removing the picture archives it as before.

### Link rules

Before an Event links a cover or a gallery photo, or a certificate template
draft links an asset, core checks the Media (`media.Linker`) and refuses the
link with `application/problem+json`, a stable `code`, and the members
`mediaId` and `role`:

| Status | `code` | Extra members | When |
|---|---|---|---|
| 422 | `media_purpose_mismatch` | `purpose` | The Media's purpose does not fit the role. An Event cover or gallery photo needs `event_cover` or `event_gallery` (the organizer's picker offers every photo of the team's Events for both); a certificate asset needs `certificate_asset`. A profile picture or a CMS page's PDF cannot be a cover. |
| 422 | `media_not_linkable` | | There is no such Media, or it is archived, its blob is purged or being purged, or its expiry has passed. A pending or attached Media can be linked, and a Media removed from a record can be linked again until its window ends. |
| 403 | `media_team_mismatch` | | Team media library: the Media is on an Event (archived ones included) of another Owner team. An Event may reuse a photo of another Event of its own Owner team. |

Only new links are checked: an Event saved with the cover it already has, or
a template draft keeping an asset, is not refused for it. One exception:
moving an Event to another Owner team checks the Team media library again for
its current cover and gallery, and refuses the move with
`media_team_mismatch` while another Event of the old team uses one of them;
the organizer removes that photo from the Event first. The profile picture
has no separate check: the only way to link one is
`POST /v1/users/me/profile-picture`, which uploads it as `profile_picture`.

**Transition rule for legacy Media.** A `legacy` Media fits every role, as
any Media could be linked anywhere before Media purpose. superadmin still
uploads Event covers, gallery photos and certificate assets without a purpose
until it sends one (ticket 09), and Skyforms and CMS until stage 5. The other
rules (linkable, Team media library) apply to legacy Media too. The rule ends
when purpose-less uploads fall to the strict rule (ticket 15).

The database's triggers are the backstop for the state rule: a new link or a
new Media attachment to an archived or purging Media is rejected whoever
writes it. They also check the purpose again (migration `20260926161000`,
`media_purpose_fits_role` over `media_role_purposes`, a copy of the role
table that a test keeps equal to `rolePurposes`, both ways). The link rules
read the Media before the write and outside its transaction, so the legacy
backfill could give a legacy Media a purpose in between; the trigger reads
the Media under the lock the foreign key takes anyway, which waits for the
backfill and never for the status trigger. Such a link is never written
mismatched: the trigger refuses it with its own code (SQLSTATE `23514`,
constraint `media_attachment_purpose_fits`), and the Event, certificate
template and service attach paths answer it as `422 media_not_linkable` for
that Media and role. Retrying gets the ordinary purpose check. (A profile
picture is linked only right after its upload as `profile_picture`, which
the backfill never touches, so that link cannot race.)

### Service attach API

Another product links a Media to its own records through core (media
redesign ticket 03). The product stores the Media id, attaches it when it
saves the record, and detaches it when the record lets it go; core keeps the
Media while any Media attachment does.

**Endpoints**

`POST /v1/media/{id}/attachments` with a JSON body. A CMS page:

```json
{ "owner": { "service": "cms", "type": "page", "id": "skylab-site:hakkimizda" }, "role": "image", "onBehalfOf": "5f0c…" }
```

A Skyforms answer:

```json
{ "owner": { "service": "forms", "type": "response", "id": "8b6e2d0a-…" }, "role": "answer", "onBehalfOf": "a41d…" }
```

- `owner.service`: the calling product (`forms` or `cms`).
- `owner.type`: the product's record type, lowercase snake_case, at most 64
  characters (`response`, `draft`, `page`, `block`, …). Core does not
  interpret it.
- `owner.id`: the record's id in the product, at most 200 letters, digits and
  `- _ . : /`: a Skyforms response or draft id, a CMS block or collection
  item Guid, or a CMS page as `<clientId>:<slug>`. Core does not interpret it
  either; core's own links use their records' UUIDs.
- `role`: one of the product's roles below.
- `onBehalfOf`: the id of the person the product acts for: the respondent
  whose answer it is, the editor saving the page. Required.

It answers `201 Created` with the new Media attachment, or `200 OK` with the
one already there when the same link (Media, owner and role) exists, even if
the Media was archived since. A retry is therefore safe:

```json
{ "id": "5b1e…", "mediaId": "0f2a…", "owner": { "service": "cms", "type": "page", "id": "skylab-site:hakkimizda" }, "role": "image", "createdAt": "2026-09-26T09:00:00Z" }
```

A retry that races a detach of the same link answers the state after both:
the link again, never a missing one.

`DELETE /v1/media/{id}/attachments/{attachmentId}` removes one of the
product's Media attachments and answers `204 No Content`, also when it is not
there (any more), so a retry is safe too. When it was the Media's last Media
attachment, the Media becomes `detached` and is purged 30 days later unless
something attaches it again; a `legacy` Media gets no expiry (see
[Status and expiry](#status-and-expiry)). The database's status trigger does
this for another product's Media attachments exactly as for core's own. The
Media attachment row itself is a link row and is deleted.

**Who may call**

Only a product's own service account: a client-credentials token of the
product's configured Keycloak client that carries `aud` `core` and the role
`media:attach` on the `core` client (`resource_access.core.roles`, ADR-0019).

- Core tells a service account from a person by the `client_id` claim, which
  Keycloak writes only into a client-credentials token (the `service_account`
  client scope), naming the same client as `azp`. A person's token is
  refused, whatever roles it carries and whichever client it was issued to.
- The token's client (`azp`) names the product through
  `MEDIA_SERVICE_CLIENTS`: `product:client` pairs separated by commas, read
  and checked at startup (`authz.ServiceClientsFromEnv`). Unset, it is
  `forms:forms`: forms-backend requests its service token as the `forms`
  client. The CMS has no service account yet; once it has one, adding
  `cms:<its client>` lets it attach and opens `cms_image` and `cms_file` for
  upload. `none` configures no product. A service account of any other
  client is refused; the Skyforms login client `skyforms` has no service
  account and speaks for nobody.
- A product manages only its own Media attachments: `owner.service` must be
  the calling product, and it can remove only Media attachments whose
  `owner_service` is its own. Core's own links (`owner_service` `core`) are
  never written or removed through this API.

Keycloak must therefore hold the client role `media:attach` on the `core`
client, assigned only to the configured products' service accounts, and
each of those clients' service tokens must carry `aud` `core` and the role.
Core does not set this up; it is a human step in Keycloak, done per realm
(sandbox, then production). Until it is done every call is refused with
`media_attach_forbidden`, which changes nothing for anyone today.

**Roles**

| Product | Role | Purposes it accepts |
|---|---|---|
| `forms` | `answer` | `answer_file`, `answer_file_large` |
| `cms` | `image` | `cms_image` |
| `cms` | `file` | `cms_file` |

A `legacy` Media fits every role (the transition rule above): Skyforms and
the CMS upload without a purpose until stage 5. Core's own roles and these
are one table (`rolePurposes`, `internal/media/attachment.go`), and core's
links and the service attach API share one link check.

**Which Media a product may link**

- A Media of one of the product's own purposes (the catalogue's `service`).
  An Answer file (`answer_file`, `answer_file_large`) belongs to the person
  who uploaded it: Skyforms links it only with that person as `onBehalfOf`.
- A `legacy` Media, uploaded before purposes, may be anyone's and used by
  anything: a product links one only with its uploader as `onBehalfOf`, or
  once the product already holds a Media attachment to it (a CMS editor
  reusing an image the CMS already uses). No product can pin another
  product's or core's legacy Media, such as a still-public legacy Answer
  file or an Event cover. A Media whose detach expiry is held (the legacy
  backfill gave it a purpose, see [Detach expiry hold](#detach-expiry-hold))
  counts as legacy here until the hold is released; linking it where its
  purpose does not fit gives it back `legacy` (below).
- Nothing else: not another product's Media, private or not, and not core's.

A Media the product may not link is refused exactly like a Media that does
not exist (`media_not_linkable`, without its purpose), so the refusal tells
the product nothing about it.

**Checks, in order**

1. The caller is a configured product's service account with `media:attach`
   (`media_attach_forbidden`). Nothing in the request is read before this: a
   person always gets `403`.
2. The request is well formed: the path ids, `owner`, `onBehalfOf` (a plain
   `400`).
3. `owner.service` is the caller (`media_attach_wrong_service`).
4. The role is one of the product's (`media_role_unknown`).
5. The same link already exists: answered with `200`, nothing else checked.
6. The Media exists and is linkable: not archived, no purge started, its
   expiry not passed; and the product may link it (above). Otherwise
   `media_not_linkable`.
7. The Media's purpose fits the role (`media_purpose_mismatch`). Only a Media
   the product may link reaches this check, so the purpose it names is the
   product's own.

The database's triggers stay the backstop: a Media archived or claimed by a
purge between these checks and the write is refused with
`media_not_linkable` too.

**Refusals** are `application/problem+json` with a stable `code`:

| Status | `code` | Extra members | When |
|---|---|---|---|
| 401 | | | No token, or an invalid one. |
| 403 | `media_attach_forbidden` | | Not a configured product's service account with `media:attach` on the core client: a person, a service account without the role, or of a client not in `MEDIA_SERVICE_CLIENTS`. |
| 400 | | | A malformed request: not JSON, a path id or `onBehalfOf` that is not a UUID, `owner.type` not lowercase snake_case, `owner.id` empty, too long or with other characters. |
| 403 | `media_attach_wrong_service` | | `owner.service` is another product, or the Media attachment to remove belongs to another product or to core. |
| 400 | `media_role_unknown` | `role` | Not a role of the calling product. |
| 422 | `media_not_linkable` | `mediaId`, `role` | No such Media, or archived, being purged, expired, or not the product's to link. |
| 422 | `media_purpose_mismatch` | `mediaId`, `role`, `purpose` | One of the product's own Media whose purpose does not fit the role (a CMS file as an image). |

### Expiry cleanup

The purge worker, after its archived batch, makes one pass over the Media
whose `expiresAt` has passed: pending Media past their purpose's pending TTL
and detached purposed Media past their 30 days. It walks them by id, 25 at a
time, and purges each blob with the same locked check and two-phase claim as
an archived Media: a Media that a Media attachment or a core link still uses
is kept. The Media is archived as its blob goes, so ordinary reads hide it
and restore answers `410 Gone`. A Media whose blob cannot be deleted is
logged with its id and retried on the next pass; it never holds up the
others. Attached Media, legacy Media (they have no expiry, unless the orphan
switch gave them one), Media whose detach expiry is held and archived Media
are never purged by expiry; archived Media keep the archive window above.

### Media stored before Media attachments

The migration gives every Media core already links its Media attachment and
the attached status, including a Media archived after it was linked. A Media
nothing in core links stays `pending` with no expiry: this change purges
nothing that existed before it. The unused `attached` column of the first
media migration is dropped; `status` replaces it. What happens to these
Media next is under [Legacy Media](#legacy-media).

## Legacy Media

Every Media stored before Media purpose is `legacy` (media redesign ticket
08, decision Q19: a purpose comes from where the Media is used; a Media used
nowhere is kept 30 days, reported to Yusuf, then deleted). Core does four
things with them. Only the first runs by itself; the two commands that end
retention are for after stage 5 (ticket 18).

### Purpose backfill

A background backfill starts with core, beside the serving policy backfill,
and gives each legacy Media that core attaches the purpose of its use. It
walks the legacy Media with at least one of core's own Media attachments by
id, 25 at a time. For each one it locks the Media row (a new Media attachment
checks its Media under a lock that waits for this one), reads all of its
Media attachments, and picks:

| Media attachments | Purpose |
|---|---|
| Event cover | `event_cover` |
| Event gallery | `event_gallery` |
| Event cover and gallery (one Event or several) | `event_cover` |
| User profile picture | `profile_picture` |
| Certificate template draft or version asset | stays `legacy` (private purpose, below) |
| Uses no one purpose fits: an Event photo that is also a profile picture or a certificate asset, or a core use plus another product's Media attachment | stays `legacy` (mixed use, K1) |

The rule behind the table: the backfill tries core's roles in the order Event
cover, Event gallery, profile picture, certificate asset, and takes the
role's own purpose (the first `rolePurposes` lists for it) for the first role
the Media plays whose purpose fits **every** Media attachment it has, by the
same check a new link gets (`rolePurposes`, `internal/media/attachment.go`:
an Event role takes `event_cover` or `event_gallery`, the profile picture
`profile_picture`, a certificate asset `certificate_asset`, a product's role
only its own purposes). Both Event purposes fit both Event roles, so an Event
photo always gets one, and the cover wins over the gallery. Every Media
attachment still fits its Media's purpose afterwards, including one written
while the backfill ran: the database checks the purpose again (see [Link
rules](#link-rules)).

- **Only the purpose changes, and the hold is set.** Status, expiry,
  `updatedAt`, the blob, its address and its serving metadata stay as they
  are; nothing is re-encoded or moved. Until the hold is released, the
  backfill sets the Media's detach expiry hold in the same statement
  (below); after the release it gives purposes without it.
- **Private purposes are never given.** `certificate_asset` is private: the
  purpose says the file is encrypted in the private bucket, and these blobs
  are public. Certificate template assets stay `legacy` until private Media
  storage (ticket 06) moves them and gives them the purpose itself.
- **Mixed uses stay legacy (decision K1).** The backfill does not pick
  between an Event and a person's profile picture, or between core and a
  product's record. Those Media keep the legacy rules: no expiry of their
  own, fit every role, and no image variants.
- Skyforms answers and CMS content are not backfilled here: core cannot see
  them (stage 5). A legacy Media only another product attaches is left
  alone.

Every step is idempotent: a Media that got a purpose is no longer listed, and
a pass cut short is run again. A Media that fails is skipped, so it never
holds up the ones after it; passes repeat a minute apart until one ends with
nothing failed. The log names a failing Media once an hour, not on every
pass, and prints a pass only when it assigned something or its number of
failures changed: `media legacy purpose backfill: assigned N, kept legacy P
(private purpose, until private Media storage) and M (mixed uses), skipped S,
failed F`. Skipped are Media that were no longer legacy, or no longer used by
core, when the pass reached them.

What changes for a Media that got a purpose: a new link checks its purpose (a
former legacy profile picture cannot become an Event cover), and, once its
hold is released, removing it from its last record starts the 30 days of any
purposed Media.

### Detach expiry hold

A Media the backfill gave a purpose may still be shown by CMS content through
its address, which core cannot see until stage 5 gives CMS images their
Media attachments. So its detach expiry is held (decision K2, column
`media.detach_expiry_held`, migration `20260926161000`):

- Removed from its last record, it is detached with **no** expiry, as a
  legacy Media is. Restoring it after an archive sets none either.
- Linking it again works as for any Media; the hold stays.
- **A product may link it as a legacy Media** (for its uploader, or once the
  product holds it), because the uses the hold protects are the products'.
  When its purpose does not fit the product's role (an Event cover becoming
  a CMS page's `image`, the stage 5 case), the service attach gives it back
  `legacy` and attaches it in one transaction: it locks the Media row for
  update first, so two attaches of one held Media queue instead of
  deadlocking, sets `legacy`, keeps the hold, and inserts the Media
  attachment. Core logs one line with the Media, its old purpose and the
  role. Its uses are now mixed, so the backfill keeps it legacy (K1). A
  Media uploaded with a purpose is never given back: a product is refused as
  before.
- Nothing else reads the hold. An archived held Media follows the archive
  window, and account erasure purges a held profile picture at once like any
  other.

`core-backend media-legacy-release-hold` ends the hold, after stage 5 (ticket
18). **It is not run yet.** Without `-apply` it only counts the held Media and
those of them whose 30 days the release starts. With `-apply` it first
records the release (table `media_legacy_hold`, the first release's time is
kept): the backfill runs on every start and purpose-less uploads keep
arriving until stage 6, and from then on it gives purposes without the hold,
so nothing is held for ever. The backfill reads the release under a share
lock that the release's update waits for, so a hold written before the
release is there for it to clear. Then it walks the held Media by id, 25 at
a time, clears each one's hold, and gives the ones no record uses by then
their 30 days **from the release**, not from when they were detached. A held
Media a product's attach gave back `legacy` gets no window: only the orphan
switch gives a legacy Media one. A Media that fails is named on standard
error and stays held; a second run releases only what is left, and a run
over released Media changes nothing. It prints counts only, to standard
error, and exits non-zero if a Media failed or the run was interrupted (with
the counts so far).

On the server running core, inside the core container:

```sh
docker exec <core container> ./core-backend media-legacy-release-hold          # counts
docker exec <core container> ./core-backend media-legacy-release-hold -apply   # release
```

### Orphan report

A legacy orphan is a current legacy Media that nothing in core uses: no Media
attachment (from core or any product) and no core link read directly (the
safety net above), not archived, no purge started. Archived legacy Media are
not listed: the archive window already decides them. **An orphan is not
unused**: Skyforms answers and CMS content use Media by their address, which
core cannot see, so most Answer files and CMS images are orphans here.

`core-backend media-legacy-report` reads core's database (and Keycloak,
read-only, for the uploaders' groups) and changes nothing. Standard output
carries only the report: a header row and one tab-separated row per orphan,
so it can be redirected to a file and opened as a spreadsheet. The summary
goes to standard error: the orphans and their size, the legacy Media core
still attaches (what the purpose backfill has not reached or keeps legacy),
the core links without a Media attachment, the Media whose detach expiry is
still held, and whether (and when) the hold was released. If an interrupt or
the ten-minute limit cuts off the uploaders' group lookups, the summary says
how many and the command exits non-zero: the report is incomplete. Columns:

| Column | Value |
|---|---|
| `id` | The Media id. The expiry switch reads this column. |
| `created_at` | Upload time, UTC. |
| `type`, `size` | The recorded type and size in bytes. |
| `name` | The file name (tabs and line breaks become spaces). |
| `uploader_groups_now` | The uploader's Keycloak group paths today (not when they uploaded), comma separated; `-` for none or a deleted account, `?` when Keycloak could not be read. The uploader is not named. |
| `status`, `expires_at` | `pending` (never attached) or `detached`; the expiry, `-` until the switch runs. |
| `key` | The object key: the Media's address is the CDN base followed by it. Match it against Skyforms answers and CMS content. |

Whether CMS content uses an address is not checked: the CMS database is not
core's. The file names in the report can be personal data (CVs): keep the
file off shared places and delete the server copy once it is downloaded.

On the server running core, inside the core container, which has the
environment:

```sh
docker exec <core container> ./core-backend media-legacy-report > legacy-media-report.tsv
```

### Orphan expiry switch

`core-backend media-legacy-expire` starts the 30-day window (Q19) of the
orphans Yusuf reviewed. **It is not run yet.** It must wait until Skyforms
answers and CMS content have their Media attachments (stage 5, ticket 18):
until then their files are orphans here, and the switch would delete them 30
days later.

It reads Media ids from standard input: the first column of each row, with
the header, `#` lines and empty lines skipped, so the reviewed report, with
the rows to keep deleted, goes back in as it is. Without `-apply` it only
counts. Each Media is checked again as it is written: one that is no longer
a legacy orphan (attached, given a purpose, archived, linked by a core
record) is listed and left alone, and a Media that already has a window
keeps it. An orphan that gets its window keeps `legacy`, gets `expiresAt` 30
days from the run, and the [expiry cleanup](#expiry-cleanup) purges it when
the window ends, unless something attaches it first (which clears the
expiry). It writes to standard error only, and an interrupted run prints what
it applied before it exits non-zero.

```sh
docker exec -i <core container> ./core-backend media-legacy-expire < reviewed.tsv          # dry run
docker exec -i <core container> ./core-backend media-legacy-expire -apply < reviewed.tsv   # start the windows
```

A window already started ends early only by an attachment: a core record or
a product linking the Media clears its expiry. There is no command that
clears it otherwise yet; review the report before `-apply`.

## Configuration

- `MEDIA_BLOB_RECOVERY_DAYS` — recovery window in whole days; default `30`.
- `MEDIA_BLOB_PURGE_INTERVAL` — Go duration between bounded runs; default `1h`.
  The expiry cleanup runs on the same interval.
- `MEDIA_BLOB_PURGE_BATCH_SIZE` — maximum archived records per run; default
  `25`. The expiry cleanup walks every expired Media on each run.
- `MEDIA_UPLOAD_STAGING_GRACE` — delay before abandoned uploads are eligible;
  minimum `2m`, default `24h`.
- `MEDIA_UPLOAD_STAGING_SWEEP_INTERVAL` — retry sweep interval; default `15m`.
- `MEDIA_UPLOAD_STAGING_BATCH_SIZE` — maximum staging intents per run; default
  `25`.
- `MEDIA_UPLOAD_RATE_MAX` — single-step uploads per person per window; default
  `100`.
- `MEDIA_UPLOAD_RATE_WINDOW` — Go duration of that rolling window; default
  `10m`.
- `MEDIA_UPLOAD_DAILY_MAX_MIB` — MiB of upload body per person per rolling
  24 hours; default `2048`.
- `CDN_BASE` (or `R2_PUBLIC_URL`) — the public base of every Media address;
  default `https://cdn.yildizskylab.com`.
- The decode budget (2 images at once, 1 SVG, a 10-second wait) is fixed in
  code (`media.DecodeBudgetConfig`).
- `MEDIA_IMAGE_ADDRESS_MODE` — where image sizes point: `stored` (default) or
  `cloudflare`. Any other value stops core at startup.

- `MEDIA_SERVICE_CLIENTS` — the products' service clients for the
  [service attach API](#service-attach-api), `product:client` pairs
  separated by commas (products `forms`, `cms`), or `none`; default
  `forms:forms`. Startup fails on anything else. A product with no client
  cannot attach, and its purposes cannot be uploaded.

The detached window is fixed at 30 days by the database;
`MEDIA_BLOB_RECOVERY_DAYS` does not change it.
