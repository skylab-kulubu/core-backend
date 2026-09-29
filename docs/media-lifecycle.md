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
links checked directly (an Event cover, gallery photo, file or video, a User
profile, a Certificate template draft, or a published Certificate template
version).
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
bucket takes that metadata from one policy (`internal/media/serving.go`:
`media.ServingMetadataFor` for a Media, by its purpose; `media.ServingMetadata`
for an object written without one, such as a certificate's PDF):

- The raster formats Upload accepts (JPEG, PNG, WebP, GIF; one table shared
  with the sanitizer) and PDF are served inline with their type. The SkyForms
  admin preview frames PDFs.
- SVG keeps `image/svg+xml`, so `<img>` still renders it, but carries
  `Content-Disposition: attachment`: opening its URL downloads it instead of
  running any script a sanitizer missed (see [SVG](#svg)).
- A video (the `video` purpose's MP4, ticket 22) keeps `video/mp4` and is
  served inline, with no `Content-Disposition`, at a key ending in `.mp4`
  (`videos/<uuid>.mp4`, and its faststart copy `videos/<uuid>.fs.mp4`, see
  [Video faststart](#video-faststart)), so a `<video>` element and the
  browser's player take it. The CDN answers Range requests for it, and `nosniff` comes from a
  Cloudflare rule (below). An MP4 of any other purpose, or of none, is a
  download like every other file; only `video` may name MP4 publicly (see
  [Hard ceilings](#hard-ceilings)).
- Every other file is `application/octet-stream` with
  `Content-Disposition: attachment; filename*=…` (the Media's name, RFC 2231
  encoded), whatever type its client declared. A club file's ZIP is one of
  them: download-only, never inline (decision D6, see
  [Hard ceilings](#hard-ceilings)).

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

Known gap: until Skyforms sends the `answer_file` purpose (stage 5), Answer
files are still public legacy media; tracked by the media redesign.
`GET /v1/media/{id}` answers a caller without a token with no `uploadedBy`
and no `name`, but a signed-in caller still sees both, and the object itself
stays reachable at its CDN address. An Answer file uploaded with its purpose
is private (see [Private Media](#private-media)).

`X-Content-Type-Options: nosniff` cannot be stored as R2 object metadata; it
needs a Cloudflare Transform Rule on `cdn.yildizskylab.com`. Production has
it, and the CDN answers Range requests from R2 (checked 2026-09-28: a
`Range: bytes=0-99` request for an image on `cdn.` answered `206` with
`Accept-Ranges: bytes`, `Content-Range` and `X-Content-Type-Options:
nosniff`). A video plays only with both; check the sandbox CDN the same way
before its videos open (see [Before Event files open](#before-event-files-open)).

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
- `answer_file` (Skyforms, configured by default) and `certificate_asset`
  are private: refused with `private_media_disabled` while
  `MEDIA_PRIVATE_ENABLED` is off, stored encrypted in the private bucket
  when it is on ([Private Media](#private-media)). `answer_file` also needs
  a malware scan: refused (`purpose_not_available`) while no scanner is
  configured (`MEDIA_CLAMAV_ADDR`), and stored `scanning` until clamd finds
  it clean once one is ([Malware scan](#malware-scan)). `answer_file_large`
  is a Direct upload purpose no person may start (`service_only`), and a
  private one Direct upload does not take yet (ticket 21, see
  [Direct upload](#direct-upload));
- `club_file` and `video` are Direct upload purposes core attaches: an
  Event's files and videos (decision C1, ticket 22, see
  [Event files and videos](#event-files-and-videos)). Being attachable does
  not open them: a Direct upload purpose opens only where its side names it
  in `MEDIA_DIRECT_UPLOAD_PURPOSES` (none by default; see
  [Checks](#checks)), since this file is the same on sandbox and production.
  `club_file` also needs a malware scan (`MEDIA_CLAMAV_ADDR`), and a file
  that is a ZIP is checked against clamd's limits before it is scanned
  ([The ZIP check](#the-zip-check), ticket 23). `video` needs no scan (at
  2 GiB it is larger than what clamd scans). See
  [Before Event files open](#before-event-files-open).

Every purpose a product's role accepts must be attached by that product (a
core role's by core, `attach: core`), and the purposes core refers to in code
(the core purposes, club files and videos, the CMS purposes and the Answer
file purposes) must all be in the file. `image` is acted on (see
[Images and sizes](#images-and-sizes)). So is `scan`: while no malware
scanner is configured (`MEDIA_CLAMAV_ADDR`), a purpose with `scan: true`
cannot be uploaded (`purpose_not_available`), since its Media are opened only
once clean; with one, its Media wait `scanning` until clamd finds them clean
(see [Malware scan](#malware-scan)). `image.sizes` may name only the sizes clients can ask for, `card` and
`page`. An unknown field or value, a missing `legacy` entry, or a ceiling
violation stops core at startup.

The initial entries:

| Purpose | Upload | Types | Max | Visibility | Transport | Attached by |
|---|---|---|---|---|---|---|
| `profile_picture` | authenticated | JPEG, PNG, WebP, GIF | 5 MiB | public | single-step | core |
| `event_cover`, `event_gallery` | event_editor | JPEG, PNG, WebP, GIF, SVG | 10 MiB | public | single-step | core |
| `certificate_asset` | certificate_template_editor | PNG, JPEG, PDF | 20 MiB | private | single-step | core |
| `cms_image` | authenticated | JPEG, PNG, WebP, GIF, SVG | 10 MiB | public | single-step | cms (no service client yet) |
| `cms_file` | authenticated | PDF | 20 MiB | public | single-step | cms (no service client yet) |
| `answer_file` | authenticated | PDF, JPEG, PNG, DOCX | 20 MiB | private, scanned | single-step | forms |
| `club_file` | event_editor | PDF, ZIP (download only) | 1 GiB | public, scanned | direct | core (an Event's files) |
| `answer_file_large` | service_only | ZIP, PDF | 1 GiB | private, scanned | direct | forms |
| `video` | event_editor | MP4 | 2 GiB | public | direct | core (an Event's videos) |
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
  PDF and MP4, and ZIP only for `club_file` (decision D6): a ZIP is always
  served as a download (`Content-Disposition: attachment`), never inline. A
  private purpose may name ZIP (`answer_file_large`): it never reaches the
  CDN;
- only `video` accepts MP4 publicly: its MP4 is the one file served inline
  besides images and PDF, to play (`video/mp4`, see
  [Serving policy](#serving-policy));
- `video` needs no malware scan: its served key follows its type
  (`videos/<uuid>.mp4`), while a scanned file is served once clean at
  `files/<Media id>`, which every purge of a held Media finds by the id
  alone, so a scanned video would lose its extension;
- a Direct upload purpose accepts only PDF, ZIP and MP4: core never receives
  a Direct upload's bytes, it reads their start, and these three prove their
  type there. An image would reach storage without the re-encoding every
  stored image gets, and a DOCX is told from another ZIP only by reading all
  of it;
- a public purpose that accepts raster images declares re-encoding
  (`image.reencode`), and core re-encodes every such image (see
  [Images and sizes](#images-and-sizes));
- only `cms_image`, `event_cover` and `event_gallery` may list SVG, and only
  while public: never a profile picture, and never a private purpose, whatever
  its name. In code, an SVG is stored only for
  a purpose that lists it, only sanitized, under a key ending in `.svg`, and
  is always served as a download (`Content-Disposition: attachment`);
- the maximum size stays under 20 MiB for single-step uploads and 2 GiB for
  Direct upload;
- the declared image size stays within 2560 px (`image.max_dimension`,
  `image.sizes`), and re-encoding scales a larger image down to it;
- a private purpose is encrypted, and one that accepts raster images declares
  re-encoding (`image.reencode`), so the image is re-encoded before it is
  encrypted;
- a public purpose that needs a malware scan is a Direct upload purpose:
  core holds a Direct upload's file under `pending/scan/` until the scan
  finds it clean, while a single-step upload is written straight to its
  served key (see [Malware scan](#malware-scan));
- a purpose that needs a malware scan allows at most 1 GiB
  (`media.MaxScanBytes`), what clamd takes in one stream: its
  `StreamMaxLength`, which the ClamAV wizard sets. Raising it means raising
  that first.

### Uploading

`POST /v1/media` takes an optional multipart field `purpose`.

The file's name is kept as the browser sent it, whichever way the file is
uploaded (with a purpose, without one, as a profile picture, or by
[Direct upload](#direct-upload)): emoji (ZWJ sequences and tag flags
included), every script's joiners (the Persian ZWNJ), soft hyphens and
decomposed letters stay; a byte order mark is dropped. Refused with `400`
`media_name_invalid` is only what makes a name read as another or break
where it is shown: invalid UTF-8, a C0 or C1 control character (a line
break, a tab, NUL), and the bidirectional formatting controls (U+202A–U+202E,
U+2066–U+2069, U+200E, U+200F, U+061C), such as the right-to-left override
that shows `a\u202Egnp.exe` as `aexe.png`. With a purpose,
core checks, in order: the purpose exists, the caller may upload it, private
Media is on if the purpose is private, it is single-step, something can attach
it, a malware scanner is configured if the purpose needs a scan, the size, and the type detected from the content (a raster format, PDF by
its header, or DOCX: a ZIP package whose `[Content_Types].xml` declares a
macro-free Word document part `word/document.xml`). `POST /v1/users/me/profile-picture` always uploads
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
| 400 | `media_name_invalid` | | The file name carries a control character or a bidirectional formatting control (above). Also for uploads without a purpose. |
| 403 | `purpose_forbidden` | `purpose` | The caller's upload rule does not allow it. |
| 422 | `private_media_disabled` | `purpose` | A private purpose while `MEDIA_PRIVATE_ENABLED` is off. Nothing is stored, and never publicly instead; retrying does not help. |
| 503 | `private_media_unavailable` | | A private purpose while OpenBao cannot be reached. Nothing is stored; retry later (`Retry-After`). Public purposes are not affected. |
| 400 | `purpose_requires_direct_upload` | `purpose` | A `direct` purpose sent to `POST /v1/media` (see [Direct upload](#direct-upload)). |
| 422 | `purpose_not_available` | `purpose` | A `service` purpose whose product has no service client configured (`cms_image` and `cms_file` today), or that names no product (none today): nothing is stored, the file would only wait for its expiry. Also a purpose that needs a malware scan while no scanner is configured (`MEDIA_CLAMAV_ADDR` unset). At `POST /v1/uploads`, also a Direct upload purpose this side does not switch on (`MEDIA_DIRECT_UPLOAD_PURPOSES`). |
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

Direct upload is not counted here (decision Q23): the product that grants a
Direct upload limits it, and for the Direct uploads core starts itself core
is that product, with a budget of their own (see
[Direct upload budget](#direct-upload-budget)). A test
(`TestEveryRouteThatStoresAFileIsChargedToTheUploadBudget`) fails when any
route stores a file sent through core without being charged.

## Direct upload

Media redesign ticket 11 (ADR-0052). A purpose whose `transport` is `direct`
(`club_file`, `answer_file_large`, `video`) is not sent through core: the
browser sends the file straight to R2 as an S3 multipart upload, part by
part, to addresses core presigns, then asks core to complete it. Core never
holds the file; it checks what R2 holds, copies it to its final key and
creates the Media.

Core attaches `club_file` and `video` (an Event's files and videos, see
[Event files and videos](#event-files-and-videos)), but a Direct upload
purpose opens only where its side switches it on
(`MEDIA_DIRECT_UPLOAD_PURPOSES`, see [Checks](#checks)): none by default.
Switched on, `video` can be started wherever core has R2; `club_file` also
needs the malware scanner (`MEDIA_CLAMAV_ADDR`), which checks a file that
is a ZIP before it scans it ([The ZIP check](#the-zip-check));
`answer_file_large` is private and `service_only` (see
[The catalogue](#the-catalogue)). A browser can send the parts only once
the bucket's CORS allows its origin (the R2 wizard below). The tests switch
both on and run the flow with `club_file`'s malware scan off
(`directCatalogue` in `internal/httpx/media_direct_upload_test.go`), and on
in `internal/httpx/media_scan_test.go`.

### Endpoints

All three need a signed-in person. `POST /v1/uploads` starts one:

```json
{"purpose": "club_file", "name": "veri seti.zip", "size": 734003200, "limits": {"types": ["application/zip"], "maxBytes": 800000000}}
```

- `name`: the file's name, 1 to 255 bytes, under the rule every upload's
  name follows (see [Uploading](#uploading)): `a\u202Epiz.exe` cannot pass
  for `aexe.zip` (`400` `media_name_invalid`). It is kept with the Media and
  names the download; it is never part of a key.
- `size`: the file's exact size in bytes.
- `limits` (optional): what the owning product allows for this one upload
  (Skyforms: "only PDF, 5 MB"). It may only narrow the purpose: `types` the
  purpose accepts, a `maxBytes` no larger than the purpose's. Core keeps the
  narrowed limits with the upload and checks the file against them.

It answers `201 Created` with `Cache-Control: no-store`:

```json
{
  "id": "0f2a…",
  "purpose": "club_file",
  "name": "veri seti.zip",
  "size": 734003200,
  "partSize": 16777216,
  "partCount": 44,
  "expiresAt": "2026-09-28T21:00:00Z",
  "uploaded": [],
  "parts": [
    {"partNumber": 1, "size": 16777216, "url": "https://<account>.r2.cloudflarestorage.com/<bucket>/pending/0f2a…?partNumber=1&uploadId=…&X-Amz-Signature=…"}
  ],
  "partUrlsExpireAt": "2026-09-28T10:00:00Z"
}
```

Every part is 16 MiB but the last (R2 wants the parts equal; a 2 GiB file is
128 parts). The browser PUTs each part's bytes to its `url` and keeps the
`ETag` R2 answers (the bucket's CORS exposes it, see
[R2 lifecycle and CORS](#r2-lifecycle-and-cors)). An address is signed for
its part's length (`Content-Length` is a signed header), so no PUT can carry
more, and it works for an hour. The addresses are handed only to the
uploader and are never logged; the answer that carries them is never cached.

`POST /v1/uploads/{id}/parts` answers the same shape with `200`: `uploaded`
lists the parts R2 holds whole, each with its `partNumber`, `size` and
`etag`, and `parts` has new addresses for the others. An interrupted upload
continues from there, and an upload that outlives its addresses asks here
for new ones. An upload whose purpose this side has switched off since its
start gets `422` `purpose_not_available` here and is ended (see
[Checks](#checks)).

`POST /v1/uploads/{id}/complete` completes it, with every part in order:

```json
{"parts": [{"partNumber": 1, "etag": "\"a54f…\""}, {"partNumber": 2, "etag": "\"9c1e…\""}]}
```

It answers `201 Created` with the Media, as `POST /v1/media` does. The Media
takes the upload's id, so a completion retried after its answer was lost
answers the same Media.

An upload is its uploader's: anyone else gets `404` for it, as for one that
does not exist. So does an upload past `expiresAt` (12 hours after its
start, `media.DirectUploadTTL`) and one that ended.

### Checks

The start checks the purpose as a single-step upload does, in the same order
(see [Uploading](#uploading)): the purpose exists, the caller's upload rule,
private Media on, the transport is `direct` (`purpose_requires_single_step`
otherwise), **this side switches the purpose on**, a private purpose (none
yet, ticket 21), something can attach it, the malware scan gate. Then the
request (name and size), the narrowed limits, the size against the
(narrowed) maximum, and the person's
[Direct upload budget](#direct-upload-budget). Only then does core register
the upload and open the multipart upload at `pending/<id>`, stored as an
opaque download (`application/octet-stream`, `Content-Disposition:
attachment`) whatever the browser sends.

The switch is `MEDIA_DIRECT_UPLOAD_PURPOSES`: the Direct upload purposes
this side opens, separated by commas (`video`, or `club_file,video`), none
when unset or empty. A purpose not in it is refused with `422`
`purpose_not_available` whatever else holds: attachable, scanner
configured, R2 there. The catalogue is the same file on sandbox and
production, so what a side opens is decided here, per environment. Core
refuses to start with a name that is not a Direct upload purpose of the
catalogue, or with one that can never open (a private purpose, such as
`answer_file_large`, until ticket 21), and logs the purposes switched on.
Single-step purposes never read it: ClamAV going live for Answer files
opens no club file.

Taking a purpose out of the list is core's change, not the uploader's
doing. An upload of it already started is refused with `422`
`purpose_not_available` when it asks for part addresses
(`POST /v1/uploads/{id}/parts`, so no more parts are sent for an upload
that cannot complete) and at its completion (step 2 below). Either way the
upload ends: its pending object and the multipart upload open at it are
deleted, and its charge is given back, the open place and the volume both
(see [Direct upload budget](#direct-upload-budget)).

A completion holds no database lock and no connection while storage works.
It **claims** the upload in one short transaction: the upload's record gets
the claim and a lease (`claim_until`, 20 minutes,
`media.DirectUploadClaimLease`), the pending object is staged until the
lease ends, and the key the file will be copied to until an hour after it
(`media.DirectUploadLateCopyMargin`, below). Then, with no
transaction open, it does the storage work, each call under its own timeout
(30 seconds for a listing, a `HEAD`, the ranged `GET` or a delete, 3 minutes
for R2 to join the parts, 12 minutes for the copy of up to 2 GiB, under R2's
5 GiB `CopyObject` limit), all of it stopping 2 minutes before the lease
ends. It **finishes** in another short transaction that checks the claim is
still its own and its lease live, creates the Media, removes the copy's
staging row and ends the upload. Only one pooled connection is ever held at
a time.

A second completion of an upload under a live claim, such as a retry that
races the first, does not wait: it answers `409` `upload_completing` with
`Retry-After` (5 seconds), and so does `POST /v1/uploads/{id}/parts`. Once
the first is done, a completion answers the Media it created: the file is
checked and copied once.

If core stops in the middle of a completion (a crash, a deploy), nothing
lets go of its claim: the upload answers `409` `upload_completing` for up
to the 20 minutes of the lease. After that, a completion claims it again
and carries on (R2 still holds the parts, or the joined file). But the
sweeper ends an upload whose claim went stale at its next pass (within 15
minutes of the lease's end), possibly before the client's retry: the client
then gets `404` and must start the upload again. In order:

1. The caller's upload, not expired.
2. The purpose's rules as they are now: a deploy may have changed the
   catalogue since the start. The file must meet both the purpose's current
   types and maximum and the limits the upload started with (their
   intersection); a purpose that is no longer public is refused.
3. The parts: every part from 1 to `partCount`, in order, each with the ETag
   R2 holds and whole. Parts that are not R2's (a wrong ETag, one missing)
   are `upload_parts_mismatch`: nothing is refused about the file, and the
   upload stays open. A part of another size is the file's refusal
   (`upload_size_mismatch`).
4. R2 joins the parts (`CompleteMultipartUpload`) into the pending object.
5. The size, by a `HEAD`, is exactly the declared size.
6. The first 512 bytes, by a ranged `GET`, name one of the allowed types:
   PDF by its `%PDF-` header, ZIP by its first local file header (or an
   empty archive's end), MP4 by its `ftyp` box. Where an MP4's `moov` box
   sits is not checked here: a video whose `moov` comes after its media data
   is rewritten after its completion (see [Video faststart](#video-faststart)).
7. The object is copied to `files/<uuid>` with the `Content-Type` and
   `Content-Disposition` of the [serving policy](#serving-policy), written by
   core: a PDF inline, a ZIP as a download under its name, a video's MP4 to
   play, inline as `video/mp4`, at `videos/<uuid>.mp4` instead. The final
   key is staged like every object core writes.
   A file whose purpose needs a malware scan is copied instead to
   `pending/scan/<uuid>`, an opaque download without a name at a key only
   core knows, and reaches `files/` only once clean (see
   [Public files before their scan](#public-files-before-their-scan)).
8. The Media is created `pending` (`scanning` when its purpose needs a
   scan), with the purpose's `pending_ttl`, and the upload ends in the same
   transaction; then the pending object is deleted.

Once the parts are joined (step 4), every way out but a created Media ends
the upload. A short transaction first checks the claim is still this
completion's and ends the upload; only then does storage delete the copy
and the pending object, and their staging rows go once deleted. So a
completion whose lease ran out never deletes an upload another completion
has claimed since: it finds the claim lost, deletes only its own copy, and
answers `503` `upload_claim_lost`. A copy whose outcome core does not know
(it failed or timed out: R2 may still finish it after core gave up) keeps
its staging row until an hour after the lease, and so does a copy whose
delete failed: the sweeper and account erasure delete whatever lands there. That is a refused file (step 2,
5 or 6) and core's own failure after the join (a `HEAD`, the ranged `GET`,
the copy, or storing the Media failing). The exceptions: a Media whose
storing may have succeeded although the database answered an error (its
objects stay for the staging sweeper, and the pending one is a download
meanwhile), and a completion so slow its lease ran out (it deletes only its
own copy; the upload is the sweeper's). Before the join, core's failure (R2
or the database answering an error) and `upload_parts_mismatch` let go of
the claim: the upload is open again for another completion.

### Refusals

The purpose's refusals of [Uploading](#uploading) (`purpose_unknown`,
`purpose_forbidden`, `private_media_disabled`, `purpose_not_available`,
`media_too_large` with the maximum that applies, `media_type_not_allowed`
with the types that apply), the budget's `429` (below), and these:

| Status | `code` | Extra members | When |
|---|---|---|---|
| 400 | `purpose_requires_single_step` | `purpose` | A `single_step` purpose sent to `POST /v1/uploads`. |
| 400 | `media_limits_too_wide` | `purpose`, `allowedTypes`, `maxBytes` | `limits` names a type the purpose does not accept or a larger maximum. |
| 422 | `purpose_not_available` | `purpose` | Also a purpose `MEDIA_DIRECT_UPLOAD_PURPOSES` does not switch on: at the start, and at `/parts` and the completion when it was taken out since (the upload then ends and is given back). And a private purpose, until ticket 21. |
| 400 | `upload_parts_mismatch` | | The completion's parts are not the parts R2 holds. The upload stays open: `POST /v1/uploads/{id}/parts` lists them. |
| 409 | `upload_completing` | `retryAfterSeconds`, `Retry-After` header | Another request is completing the upload (a completion, or `POST /v1/uploads/{id}/parts`). Retry: once it is done, a completion answers its Media. |
| 503 | `upload_claim_lost` | `retryAfterSeconds`, `Retry-After` header | This completion outlived its lease (core's failure: the volume is given back). Retry: the upload may still be completed, or answer the Media another completion created; `404` means it is gone and must be started again. |
| 403 | | | The uploader's account is being erased (a refusal: the charge stays, and the erasure takes the upload). |
| 422 | `upload_size_mismatch` | `declaredSize`, `size` | The stored file is not the declared size. The upload is ended. |
| 429 | `media_rate_limited` | `limit`, `maxOpenUploads`, `maxDailyBytes`, `retryAfterSeconds`, `Retry-After` header | The [Direct upload budget](#direct-upload-budget): `limit` is `open` (too many uploads open; `Retry-After` is when the first expires) or `volume`. |
| 404 | | | Someone else's upload, an expired or ended one, or none. |
| 503 | `direct_upload_unavailable` | | This core has no R2 (a local run without `R2_*`). |
| 400 | `media_name_invalid` | | A blank name, one over 255 bytes, or one the name rule refuses. |
| 400 | | | A body that is not JSON, a size of 0 or less. |

### Direct upload budget

Direct upload is not charged to the single-step
[upload limits](#upload-limits) (decision Q23): the product that grants a
Direct upload limits it, and core is that product for the uploads it starts.
Each signed-in person has:

- at most 3 Direct uploads open at once (`MEDIA_DIRECT_UPLOAD_MAX_OPEN`):
  started, and neither completed, refused nor expired. They are counted in
  the database, one start at a time per person, so the limit holds across
  restarts and replicas. Completing, a refusal or the upload's expiry frees
  its place;
- at most 10 GiB declared per rolling 24 hours
  (`MEDIA_DIRECT_UPLOAD_DAILY_MAX_MIB`, default `10240`), in core's memory
  like the single-step budget (a restart forgets it).

A start is charged its declared size once it passes every rule; a start the
budget refuses is not charged. The charge stays, as a single-step refusal's
does, when the file is refused at completion (`upload_size_mismatch`,
`media_type_not_allowed`, `media_too_large`, a purpose rule) and when the
upload is never completed: its bytes may have been sent, and free refusals
would let anyone send 2 GiB after 2 GiB without end. Only core's own doing
is given back: a start that fails on core's side, a completion core fails
after joining the parts (the upload is then ended, and the file must be
sent again), a completion that outlived its lease
(`upload_claim_lost`), and an upload whose purpose core switched off since
its start (`MEDIA_DIRECT_UPLOAD_PURPOSES`, refused at `/parts` or at its
completion, see [Checks](#checks)). `upload_parts_mismatch` changes nothing: the upload is still
open.

### Storage and cleanup

The pending object is registered in `media_upload_staging` before R2 opens
anything at its key, like every object write core causes: its staging row
(key, uploader, `cleanup_after` = the upload's expiry) is what the staging
sweeper and account erasure delete by. The upload's details (purpose, file
name, narrowed limits, declared size, part size, R2's multipart upload id,
expiry) are in `media_direct_uploads` (migration `20260928100000`), which
goes with that staging row (`ON DELETE CASCADE`). The records survive a
restart.

Deleting a pending key (`pending/…`, or `private/pending/…` in the private
bucket) also aborts every multipart upload still open at exactly that key
(`media.R2.Delete`). So every path that deletes by key alone leaves no parts
behind:

- the staging sweeper (`MEDIA_UPLOAD_STAGING_SWEEP_INTERVAL`, 15 minutes by
  default, `MEDIA_UPLOAD_STAGING_BATCH_SIZE` at a time) ends every upload
  that expired, and finishes a refusal or completion whose own cleanup
  failed; it is batched, idempotent, and walks past a failing row, which it
  retries an hour later. An upload under a live claim is not due before
  the lease ends. A claim whose lease ran out is stale, left by a
  completion that died: the sweeper then deletes the pending object and the
  upload, and an hour later the copy's key, with anything a late copy put
  there;
- a completion deletes its pending object once the Media is stored, and
  every way out after the parts are joined deletes it too (above);
- account erasure's `erase_staged_uploads` treats an open Direct upload as a
  live upload: it defers (its attempt is given back) until the upload
  expires, or until a completion's lease ends (at most 12 hours; the
  person's access is already blocked, so it cannot complete), then aborts
  it and deletes its record, file name included. It never waits on a
  completion: no lock is held while storage works.

The public bucket serves every key at the CDN, pending ones too. A pending
object exists only between the join and the end of its completion, and is
an opaque download (`attachment`), never rendered. A file waiting for its
malware scan is held under `pending/scan/`, at a random key only core knows,
also as an opaque download. An optional Cloudflare rule closes even that:
see below.

The R2 lifecycle rule below is the backstop for anything all of these miss.

### R2 lifecycle and CORS

A human step, done by `ops/wizards/media-direct-upload-r2-wizard.sh` in
sky_lab_genel (sandbox first, then production):

- a lifecycle rule on the pending prefix of the public and the private
  bucket of each side (`pending/` in the public bucket, `private/pending/` in
  the private one) that deletes objects and aborts incomplete multipart
  uploads after 2 days;
- a CORS rule on the public bucket only that lets browsers `PUT` from the
  origins core's API allows (the Traefik `cors.yml` on the server; it is not
  in this repo), with `AllowedHeaders` `content-type` and `ExposeHeaders`
  `ETag`, so the browser can read each part's ETag. The private bucket's
  CORS stays empty until private Direct upload (ticket 21);
- optionally, a Cloudflare WAF custom rule on the CDN host that blocks
  (`403`) every path starting with `/pending/`.

No wizard holds a Cloudflare API token, so the wizard shows where to click in
the Cloudflare dashboard and what to paste. It checks the CORS rule with a
preflight from the server (every origin may `PUT`, a foreign one may not)
and the CDN rule with two requests (`/pending/…` answers `403`, `/files/…`
does not); the lifecycle rule is confirmed in the dashboard. It stops before
anything is changed if one bucket name comes up for both sides or both
roles.

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

Addresses built without a service's own base (Event resources in tickets,
competitors and the door, team rosters) use the base core sets once at
startup (`media.UsePublicBase`), a process-wide setting: those call sites
have no media dependency to carry it. The address mode below is set the same
way (`media.UseImageAddressMode`) and reaches them only through
`media.ConfiguredAddresses()`. Everything with a media dependency gets its
base and mode from startup explicitly: the Media service
(`media.ServiceOptions.ImageAddressMode`) and the Event service
(`event.ServiceOptions.PublicBase`, `ImageAddressMode`). An `Addresses` that
names no mode serves the stored sizes; it never follows the process-wide mode
on its own.

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
out when core does not know them.

### Sizes in Event and User responses

Media redesign ticket 17. A record that shows an image answers its `card`
and `page` addresses next to its full-size address, so a list (Event cards,
a member roster) loads the small image:

| Response | Full-size address (unchanged) | Sizes |
|---|---|---|
| Event, list and detail: every Event answer under `/v1/events` (list, active list, detail, create, update, restore, gallery add and remove, files and videos add, remove and order) and `GET /v1/seasons/{id}/events` | `coverImageUrl` | `coverImageSizes` |
| Event gallery image, in the same answers | `images[].url` (and `imageUrls`) | `images[].sizes` |
| Event summary (`event.Resource`): `GET /v1/door/events`; the `event` of every ticket answer (`/v1/tickets/me`, `/v1/tickets`, `/v1/tickets/{id}`, `/v1/tickets/user/{userId}/event/{eventId}`, `/v1/events/{eventId}/tickets`, the application answers under `/v1/events/{eventId}/applications/…`); the `event` of every competitor answer (`/v1/competitors…`, `/v1/events/{eventId}/competitors…`; leaderboards have none) | `coverImageUrl` | `coverImageSizes` |
| The caller's profile (`GET`/`PUT`/`PATCH /v1/users/me`, `POST /v1/users/me/profile-picture`) | `profilePictureUrl` | `profilePictureSizes` |
| Public team roster (`GET /v1/teams/{team}/members`) | `members[].profilePictureUrl` | `members[].profilePictureSizes` |

```json
{
  "coverImageId": "0b7e4c1a-…",
  "coverImageUrl": "https://cdn.yildizskylab.com/images/<id>",
  "coverImageSizes": {
    "card": { "url": "https://cdn.yildizskylab.com/images/<id>/card.jpg", "width": 400, "height": 300 },
    "page": { "url": "https://cdn.yildizskylab.com/images/<id>/page.jpg", "width": 1200, "height": 900 }
  },
  "images": [
    {
      "id": "5d1c9a70-…",
      "url": "https://cdn.yildizskylab.com/images/<id2>",
      "sizes": {
        "card": { "url": "https://cdn.yildizskylab.com/images/<id2>/card.jpg", "width": 400, "height": 300 },
        "page": { "url": "https://cdn.yildizskylab.com/images/<id2>", "width": 1000, "height": 750 }
      }
    }
  ]
}
```

- The sizes are built by the same builder, base and mode as the Media
  JSON's `sizes` (`media.Addresses.LinkedSizes`): a stored size object, a
  Cloudflare transformation in that mode, the SVG itself for an SVG, the
  original for a size the image already fits in (the `page` above).
- They always name both `card` and `page` when the record links a Media
  with a public address. A size with no object of its own is the original,
  as in the Media JSON: an image already smaller than the size, an animated
  WebP, one whose sizes the backfill has not made yet. A Media whose purpose
  has no sizes (`legacy`), which the Media JSON answers without `sizes`,
  answers the original at both. So a client never builds an address and
  needs no fallback while the field is there.
- They are left out when the record links no Media (an Event without a
  cover; a profile whose `profilePictureUrl` is an address stored before
  Media, with no Media behind it), and for a Media with no public address:
  private, or its object purged. No record can link a private Media.
- A profile picture whose object is purged while the profile still links it
  keeps answering `profilePictureUrl`, as before sizes existed, but has no
  `profilePictureSizes`. An Event cannot show one: its cover and gallery
  read only Media that are not archived, and only an archived Media is
  purged.
- Existing fields keep their names and values; `imageUrls` stays a list of
  full-size addresses.
- The record reads the Media it links in its own query
  (`media.LinkedImageSQL`, scanned into a `media.LinkedImage`), so sizes cost
  no query per Media. The Event list takes three queries whatever the number
  of Events: the Events with their covers, every listed Event's gallery, and
  every listed Event's door staff. Gallery images are in upload order, and
  two uploaded at the same instant in id order.

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
| `scanning` | Its purpose needs a malware scan that has not ended yet. It may be attached, but it is not opened or served ([Malware scan](#malware-scan)). Clean, it becomes `pending`, or `attached` when a Media attachment links it. A week after its upload it is rejected (`scan_timeout`). | As `pending` while nothing keeps it: upload time plus the purpose's `pending_ttl`. None while a Media attachment links it; 30 days after its last one is removed. |
| `rejected` | The malware scan rejected it (`scanResult`: `infected`, `too_large_to_scan`, `archive_invalid`, `archive_nested`, `lost`, `integrity` or `scan_timeout`); its object is deleted. | None. |

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
attached, a Media whose last one went is detached. A `scanning` or `rejected`
Media keeps its status (migration `20260928140000`); only its expiry follows
its Media attachments. It first locks the Media
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
A `scanning` one a Media attachment links gets none, as an attached one.

### Core's own links

Core's links write and remove their Media attachments in the statement that
writes the link, whoever writes it. Statement triggers on the linking tables
do this, so every writer is covered, including account erasure's
anonymization and maintenance SQL:

| Link | `owner_type` | `role` |
|---|---|---|
| Event cover (`events.cover_image_id`) | `event` | `event_cover` |
| Event gallery (`event_images`) | `event` | `event_gallery` |
| Event files (`event_files`, migration `20260928160000`) | `event` | `event_file` |
| Event videos (`event_videos`, migration `20260928160000`) | `event` | `event_video` |
| User profile picture (`users.profile_picture_id`) | `user` | `profile_picture` |
| Certificate template draft (`draft_layout` background and image elements) | `certificate_template` | `certificate_asset` |
| Published certificate template version (`layout` and `asset_manifest`) | `certificate_template_version` | `certificate_asset` |

`owner_service` is `core` for all of them. Only a changed link is written: a
record that still links a Media archived after it was linked can be saved as
long as the link itself does not change. Replacing a profile picture
therefore detaches the previous one, which is purged 30 days later unless
something attaches it again; removing the picture archives it as before.

### Link rules

Before an Event links a cover, a gallery photo, a file or a video, or a
certificate template draft links an asset, core checks the Media
(`media.Linker`) and refuses the link with `application/problem+json`, a
stable `code`, and the members `mediaId` and `role`:

| Status | `code` | Extra members | When |
|---|---|---|---|
| 422 | `media_purpose_mismatch` | `purpose` | The Media's purpose does not fit the role. An Event cover or gallery photo needs `event_cover` or `event_gallery` (the organizer's picker offers every photo of the team's Events for both); an Event's file needs `club_file` and its video `video`; a certificate asset needs `certificate_asset`. A profile picture or a CMS page's PDF cannot be a cover. |
| 422 | `media_not_linkable` | | There is no such Media, or it is archived, its blob is purged or being purged, or its expiry has passed. A pending or attached Media can be linked, and so can a scanning one (an Answer file while its malware scan runs); a rejected one cannot (its object is being or was deleted). A Media removed from a record can be linked again until its window ends. |
| 403 | `media_team_mismatch` | | Team media library: the Media is on an Event (archived ones included) of another Owner team, as its cover, a gallery photo, a file or a video. An Event may reuse a photo, file or video of another Event of its own Owner team. |

Only new links are checked: an Event saved with the cover it already has, or
a template draft keeping an asset, is not refused for it. One exception:
moving an Event to another Owner team checks the Team media library again for
its current cover, gallery, files and videos, and refuses the move with
`media_team_mismatch` while another Event of the old team uses one of them;
the organizer removes that photo, file or video from the Event first. The profile picture
has no separate check: the only way to link one is
`POST /v1/users/me/profile-picture`, which uploads it as `profile_picture`.

**Transition rule for legacy Media.** A `legacy` Media fits every role but
an Event's files and videos, as any Media could be linked anywhere before
Media purpose. superadmin still uploads Event covers, gallery photos and
certificate assets without a purpose until it sends one (ticket 09), and
Skyforms and CMS until stage 5. The other rules (linkable, Team media
library) apply to legacy Media too. The rule ends when purpose-less uploads
fall to the strict rule (ticket 15). An Event's files and videos
(`event_file`, `event_video`) came after Media purpose, so no Media was ever
linked there without one, and their purposes are sent by Direct upload (a
club file also scanned), which a legacy upload never was: a legacy Media is
refused there with `media_purpose_mismatch` (`rolesWithoutLegacy`,
`internal/media/attachment.go`).

The database's triggers are the backstop for the state rule: a new link or a
new Media attachment to an archived or purging Media is rejected whoever
writes it. They also check the purpose again (migration `20260926161000`,
`media_purpose_fits_role` over `media_role_purposes`, a copy of the role
table that a test keeps equal to `rolePurposes`, both ways; migration
`20260928160000` adds the Event files' and videos' roles and
`media_roles_without_legacy`, the copy of `rolesWithoutLegacy`, which the
same test keeps equal). The link rules
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
(a `scanning` Media nothing keeps counts as pending: one whose scan never
ended in time is purged too) and detached purposed Media past their 30 days. It walks them by id, 25 at a
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
  are public. Certificate template assets stay `legacy` for good: nothing
  moves them into private storage (decision G1; see
  [Certificate assets](#certificate-assets)).
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
(their purpose would be private) and M (mixed uses), skipped S, failed F`. Skipped are Media that were no longer legacy, or no longer used by
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
  deadlocking, sets `legacy` (with a new `updatedAt`), keeps the hold, and
  inserts the Media attachment. A Detach of the same link can still
  deadlock with it (the Detach removed the link and waits for the Media
  row; the insert waits for the removal); when PostgreSQL fails the attach,
  it is tried once more, after the Detach. Core logs one line with the
  Media, its old purpose and the role. Its uses are now mixed, so the backfill keeps it legacy (K1). A
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
release is there for it to count and clear: it counts only after recording.
The table has exactly one row, made by the migration (and checked by its
fingerprint); without it the backfill, the release and the report fail
with an error naming it instead of guessing. Then it walks the held Media by id, 25 at
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

## Private Media

Private purposes (`answer_file`, `certificate_asset`) are stored encrypted in a
second, non-public R2 bucket and never get a public address (ADR-0052, media
redesign ticket 06, decisions Q16, Q18, Q25, Q28, Q29, G1, G2). Everything
here sits behind `MEDIA_PRIVATE_ENABLED`, which stays `false` in production
until the wizard (`ops/wizards/media-private-storage-wizard.sh` in
sky_lab_genel) has run and Yusuf turns it on.

### Encryption

Each object gets its own random 256-bit data key and is encrypted with it
before it leaves core (`internal/envelope`), so R2 and Cloudflare only ever
hold ciphertext. The format (`aes-256-gcm-chunked-v1`) is AES-256-GCM over
64 KiB segments in the STREAM construction, so a file of any size is written
and read in constant memory:

```text
header   "SKYM" | 0x01 | segment size (uint32 BE, 65536) | nonce prefix (7 random bytes)
segment  AES-256-GCM(up to 64 KiB of plaintext) | 16-byte tag   (repeated)
nonce    nonce prefix | segment index (uint32 BE) | 0x01 on the last segment, else 0x00
AAD      the 16-byte header, then the object key (e.g. private/files/<uuid>)
```

A changed byte anywhere, a segment moved, dropped or added, a file cut short,
or an object copied under another key fails a segment's check; no plaintext
of a segment that fails is released. (Binding the object key came in before
anything was stored, so the format kept `v1`.) The object key starts with
`private/` (`media.PrivateObjectKey`), which is how the work that deletes by
key alone (the blob purge, the expiry cleanup, the staged upload sweeper,
account erasure's `erase_profile_media` and `erase_staged_uploads`) reaches
the private bucket (`media.Buckets`); a private object is never read, written
or given serving metadata as a public one. Deleting an object that is not
there succeeds in either bucket, so those steps can be repeated.

The data key is wrapped by the OpenBao Transit key `MEDIA_TRANSIT_KEY`
(`media`) on `MEDIA_TRANSIT_MOUNT` (`transit/<side>`): core makes the data key
itself and calls `encrypt`, since the policy grants no `datakey`. The Media
record keeps the wrapped key (`wrapped_data_key`, Transit's `vault:v<N>:…`
ciphertext), its key version (`key_version`, read from that prefix) and the
format (`encryption_algorithm`); migration `20260926170000` adds them with
`visibility` and a check that a private Media has all three (a key version of
at least 1) and a public one none. The key rotates every 90 days in OpenBao;
older versions still unwrap what they wrapped, so nothing is re-encrypted.
Rewrap (`transit/<side>/rewrap/media`) is allowed by the policy and not used
yet.

Core signs in to OpenBao with AppRole (`auth/approle/login`,
`MEDIA_OPENBAO_ROLE_ID` and `MEDIA_OPENBAO_SECRET_ID`) on the first private
upload or read, not at startup: an OpenBao that is down never stops core
(`internal/transit`). It keeps the token and renews it in the background once
half its lease (1 hour) has passed; a token with lease left is used until a
new one arrives, and stops being used 30 seconds before its lease ends (a
quarter of a shorter lease). It logs in again when the token nears its end or
its 24-hour maximum, and once when OpenBao refuses it; the token a login
replaces is revoked (revoke-self) as far as OpenBao lets it. Callers that need
a new token share one login, each waiting only as long as its own request
allows. A failed login or renewal is answered from memory for 5 seconds, and
so is a refusal of a token that was just issued: that is core's policy not
covering the request (or encrypt having to create a missing key), a
configuration to fix (`503`), not something another login mends. Every
request to OpenBao has a 5-second timeout, and a redirect from OpenBao is
never followed (the token would go wherever it points).

### Storing a private Media

A private purpose goes through the same checks as any other, including
re-encoding a raster image (`image.reencode`). What is stored is the result,
encrypted, in the private bucket instead of the public one: the Media is
`"visibility": "private"` with `"url": ""`. A private image keeps its width
and height but gets no sizes and no cover colours, so nothing of it reaches
the public bucket; its sizes are recorded as none, and the size backfill
never picks it up.

A purpose whose entry has `scan: true` is refused with `422`
`purpose_not_available` while no malware scanner is configured
(`MEDIA_CLAMAV_ADDR`): a Media that needs a scan is not opened before it is
clean, so nothing of it could be opened. Today that is `answer_file`. With a
scanner, an Answer file is stored encrypted as above and waits `scanning`:
the scan worker decrypts it as it streams it to clamd, and no read link is
issued before it is clean ([Malware scan](#malware-scan)). `certificate_asset`
needs no scan. `answer_file_large`, sent by [Direct upload](#direct-upload),
is refused whatever the flag says until ticket 21 encrypts a large file after
its completion.

An OpenBao that cannot be reached, is sealed, refuses core's identity, or has
no such mount or key fails only private uploads and reads, with `503`
`private_media_unavailable`; public Media are not affected. The log names
OpenBao's answer (the mount and key when one is missing), never a token.

### Metadata

`GET /v1/media/{id}` answers a private Media only to its owning product's
service account (Skyforms for an Answer file) and to privileged admins, never
with an address. Anyone else, anonymous or signed in, the uploader included,
gets `404`. The admin list (`GET /v1/media`) shows private Media without an
address.

### Read links

A private Media is opened through a five-minute link core issues:

```http
POST /v1/media/{id}/links
Authorization: Bearer <token>

{"onBehalfOf": "<user id>"}
```

`201` with `Cache-Control: no-store`:

```json
{"url": "https://api.yildizskylab.com/v1/media/{id}/content?token=…", "expiresAt": "2026-09-26T12:05:00Z"}
```

Two callers may ask:

- **The owning product**, for one of its purposes: its service account with
  `media:attach` on the core client (the role that already manages its Media
  attachments), with `onBehalfOf` naming the person it decided may open the
  file (Skyforms for an Answer file: the reviewer).
- **A privileged admin** (ADMIN, YK, DK), for a core purpose
  (`certificate_asset`): the admin is the person the link is for, and the body
  names no one (`{}`); any `onBehalfOf`, a malformed one included, is `400`.
  This is how superadmin's certificate template editor shows a private
  background.

Anything else is `404`: another product's Media, a public one, a core Media
for a product, a product's Media for an admin, one that does not exist. The
person a link is for must be an active account in core (`422`
`media_link_subject_inactive` otherwise), like every other current-identity
link ([`account-lifecycle.md`](account-lifecycle.md)). For a product's link
to a person core has no row for (a reviewer who never signed in to core),
core first ensures the row from Keycloak and, as the person's first sign-in
would, writes their Sky number to Keycloak; it makes no row for a person
Keycloak does not know or has disabled, or core has erased or is erasing
(`422`), nor while Keycloak cannot be asked (`503` or `500`, see Refusals).
Nothing is ensured for an admin, who acts for themselves: an admin with no
core row gets `422`.

The token is `base64url(claims) "." base64url(signature)`: the claims are a
format byte, a disposition byte (always download), the link's id, the Media
id and the expiry in Unix seconds; the signature is HMAC-SHA256 under
`MEDIA_LINK_SIGNING_KEY` over a fixed domain string followed by the claims,
so the key signs nothing but read links. The key has the format of
`ACCOUNT_DELETION_RECEIPT_KEY`: 32 random bytes, unpadded base64url; the
wizard makes it inside OpenBao. Changing it ends every link out there, which
is harmless five minutes later. Links point at `PUBLIC_API_ORIGIN`.

`GET /v1/media/{id}/content?token=…` needs no sign-in: the token is the
permission. It checks the signature, the Media id and the expiry, unwraps the
data key, decrypts as it streams, and answers with:

- `Content-Type`: the type detected at upload;
- `Content-Disposition: attachment; filename*=UTF-8''<the name, percent-encoded>`;
- `X-Content-Type-Options: nosniff`, `Cache-Control: private, no-store`,
  `Referrer-Policy: no-referrer`, `Content-Security-Policy: default-src 'none'; sandbox`.

The first segment is checked before the answer starts, so a file that is
wrong from the start is a `500`. A later segment that fails its check ends
the download short (the status is already sent), and the log names the Media
and the segment. The route is limited to 120 requests a minute per client
address, like the public certificate routes. A link may be opened more than
once until it expires. A problem answer never repeats the token; the edge
proxy's own request log may still hold it for its five minutes.

### Access log

Every link core issues is written before it is handed out
(`media_read_links`: id, Media, product, the person it is for, issued at,
expires at), and every successful open before any byte is sent
(`media_read_link_opens`: link, time, and the client address as the trusted
proxies report it, see [`client-ip-trust.md`](client-ip-trust.md)). A failed
open (a bad or expired token, OpenBao down, a file that fails its check) is
not logged as an open.

The access log is kept **one year** (decision G2, `media.ReadLinkRetention`).
An hourly cleanup deletes, a batch at a time, the links issued more than a
year ago with their opens, and any open older than a year. It runs in the
background from startup on (its first run never holds up core), with the flag
off too, logs how many links (and their opens) it deleted, and says nothing
when there is nothing to delete. A person's
account erasure leaves their rows in place until their year is up: they are
the access audit record, and the erasure steps do not touch them
([`data-lifecycle.md`](data-lifecycle.md)).

### Certificate assets

Certificate rendering reads a private asset through decryption, in the draft
preview and at publish, and decrypts only `certificate_asset` Media: another
product's private Media named in a layout is never read. A published version's
copy of a private asset is kept encrypted in the private bucket under a data
key of its own (`private/certificate-template-assets/<version>/<asset>`; the
version id is new on every publish); its manifest entry carries the
encryption, and the asset serving backfill skips it. A publish that fails
deletes the private copies it wrote, however far it got; one whose version
may have been stored although the store answered an error (a lost commit
answer) keeps them, since that version's certificates need them: they are
deleted only when the version is provably not there. One rare race remains:
an INSERT still running on the database when the lookup misses it; a request
context is cancelled only at shutdown, so the window is narrow, and what it
could leave is a version whose private copy is gone.

Issued certificate PDFs (`certificates/<serial>.pdf`) are written to the
public bucket by design, and contain the rendered assets, private ones
included: a certificate is meant to be shared and verified by anyone who has
its link.

Certificate assets uploaded before private Media are `legacy` and public, and
stay so: they render as before, and their version copies stay in the public
bucket. Nothing moves them (decision G1: the certificate feature has not been
used in production, so there is nothing to move).

### Refusals

| Status | `code` | When |
|---|---|---|
| 403 | `media_link_forbidden` | A link asked for by anyone but a configured product's service account with `media:attach` or a privileged admin. |
| 400 | | A product's request without `onBehalfOf`, an admin's request with one, or ids that are not UUIDs. |
| 404 | | A link to a Media that is not a current private Media the caller may open; a content request for a Media that is not a current private one. |
| 422 | `media_link_subject_inactive` | The person the link is for is erased or being erased, or has no core row and is unknown to or disabled in Keycloak (product links only). Admins are never ensured: an admin with no core row gets it too. |
| 503 | `media_link_subject_unavailable` | The person the link is for has no core row and core cannot make one right now: Keycloak cannot be reached or answers 5xx or 429, or Sky number assignment stayed contended. Retry later (`Retry-After`). |
| 500 | `media_link_subject_lookup_failed` | The person the link is for has no core row and Keycloak refuses core's lookup (a permission core lacks, a wrong client secret) or answers something that is not a user: a misconfiguration, logged with Keycloak's status. A retry does not help. |
| 403 | `media_link_invalid` | A token core did not sign for this Media. |
| 403 | `media_link_expired` | A token past its five minutes. |
| 422 | `private_media_disabled` | Private Media is off. |
| 422 | `purpose_not_available` | An upload of a purpose that needs a malware scan while no scanner is configured. |
| 409 | `media_scanning` | A read link or content for a Media waiting for its malware scan (`Retry-After`). See [Opening](#opening). |
| 410 | `media_rejected` | A read link or content for a Media the malware scan rejected; `scanResult` says why. |
| 503 | `private_media_unavailable` | OpenBao cannot be reached, is sealed, refuses core's identity, or has no such mount or key. Retry later (`Retry-After`). |
| 500 | `private_media_integrity` | The stored object or its wrapped key is not what core wrote. Nothing of it is served; the log names the request. |

## Malware scan

Media redesign ticket 12 (ADR-0052, decision Q17). A purpose whose catalogue
entry has `scan: true` (`answer_file`, `club_file` and `answer_file_large`
today) is scanned by ClamAV before any of its Media is opened. ClamAV runs in
its own container, reachable only on the internal network, and core talks to
its daemon, clamd, over TCP: `MEDIA_CLAMAV_ADDR` (`host:port`, see
[Configuration](#configuration)).

### The scan gate

Without `MEDIA_CLAMAV_ADDR` nothing changes from before the scanner existed: a
purpose that needs a scan is refused (`422` `purpose_not_available`), since
its Media could never be opened. With it, the gate is lifted and such a
purpose is uploaded under its other rules. For each purpose that needs a scan,
that means:

- `answer_file` can be uploaded once private Media is on too
  (`MEDIA_PRIVATE_ENABLED`) and Skyforms has its service client;
- `club_file` stays refused unless `MEDIA_DIRECT_UPLOAD_PURPOSES` names it
  (core attaches it as an Event's file, ticket 22); a file that is a ZIP is
  checked before it is scanned ([The ZIP check](#the-zip-check), ticket
  23). The Cloudflare `/pending/*` rule below must be in place too;
- `answer_file_large` stays refused, since private Direct upload is ticket 21.

A value that is not `host:port` stops core at startup. Core never needs clamd
to start: a clamd that is down only keeps Media waiting. At startup core logs
`media scan: on (clamd at <addr>)`, or
`media scan: off (MEDIA_CLAMAV_ADDR is not set); purposes that need a scan are refused`.

Two [hard ceilings](#hard-ceilings) keep the catalogue in step with the scan:

- a public purpose that needs a scan is a Direct upload purpose (see below);
- a purpose that needs a scan allows at most 1 GiB (`media.MaxScanBytes`),
  which is what clamd takes in one stream (the wizard's `StreamMaxLength`).

### Statuses

A Media of such a purpose is created `scanning`, by a single-step upload and
by a Direct upload's completion alike, with its purpose's pending expiry. See
[Status and expiry](#status-and-expiry).

- **Clean**: the Media takes the status its Media attachments give it. It
  becomes `attached` (with no expiry) if a Media attachment already links it,
  and `pending` (keeping its expiry) otherwise. `scanResult` is `clean`.
- **Infected**: the Media is `rejected` and `scanResult` is `infected`. Its
  objects are deleted from the bucket that holds them, and the rejection is
  recorded (below).
- **Too large to scan**: the Media is `rejected` and `scanResult` is
  `too_large_to_scan`. This happens when clamd refuses the stream as longer
  than its `StreamMaxLength` (`INSTREAM size limit exceeded`), reports a
  `Heuristics.Limits.Exceeded.*` signature (`AlertExceedsMax`, see
  [ClamAV](#clamav)), or [the ZIP check](#the-zip-check) finds a ZIP (any
  file whose content is one, a DOCX too) holds more than clamd scans whole.
  Either way the file could not be scanned whole.
- **Archive invalid**: [the ZIP check](#the-zip-check) finds a ZIP
  malformed, or holding what clamd cannot read (an encrypted member, say).
  The Media is `rejected` and `scanResult` is `archive_invalid`; clamd never
  reads it.
- **Archive nested**: [the ZIP check](#the-zip-check) finds a ZIP holding an
  archive core cannot check: a format core cannot open (7-Zip, RAR, …), one
  nested too deep, or an inner ZIP too large to read again from memory. The
  Media is `rejected` and `scanResult` is `archive_nested`; clamd never
  reads it. The uploader can unpack the inner archives and upload again.
- **Lost**: the file to scan is gone, so it can never be scanned. The R2
  lifecycle rule deletes a held file after two days, so this happens when
  clamd was down that long. The Media is `rejected` and `scanResult` is
  `lost`.
- **Integrity**: a private object fails its integrity check while it is read
  for the scan. The Media is `rejected`, `scanResult` is `integrity`, and the
  worker logs it by the Media's id.
- **Scan timeout**: the Media is still `scanning` a week after its upload
  (`media.ScanDeadline`), attached or not, and whether clamd answers or not.
  The Media is `rejected` and `scanResult` is `scan_timeout`.
- **clamd unreachable**: the Media stays `scanning` and is tried again.
- **clamd answered an error, or the file could not be read** (storage, OpenBao
  down): the Media stays `scanning` and is tried again later.

Every rejection deletes the Media's objects and is recorded (below). The
uploader must upload the file again.

A `scanning` Media can be attached, so a Skyforms draft or submission holds
an Answer file while it is scanned. It is not opened or served until it is
clean (see [Opening](#opening)). The database's status trigger keeps a
`scanning` or `rejected` Media's status whatever its Media attachments do.
A Media attachment written to a `scanning` Media clears its expiry, as for an
attached Media. When its last Media attachment is removed, it expires 30 days
later, as a detached Media does.

The owning product and privileged admins see the status and `scanResult` in
the Media JSON, and so does the uploader of a public Media. An Answer file's
uploader does not read its metadata; Skyforms tells them, and they can upload
a clean copy.

### The scan worker

The scan worker (`media.ScanWorker`) starts with core when `MEDIA_CLAMAV_ADDR`
is set, on its own context. It makes a pass at once, whenever an upload
stores a Media waiting for its scan, and every 30 seconds. A pass first
rejects the Media past the scan deadline, then walks the due Media by id: the
Media waiting for their scan whose retry time has come, and the rejected ones
whose objects are still to delete.

Each Media is **claimed** before any clamd or storage work. A short
transaction picks the next due Media (`FOR UPDATE SKIP LOCKED`), records the
claim (`scan_claim_id`) and its lease (`scan_claimed_until`), and commits.
Rolling deploys start the new core before stopping the old one, so two
workers overlap on every deploy; a Media another worker has claimed is left
alone. The lease is the claim's work (the ZIP check at its longest, which
reads every file once more; the scan; the copy of a clean held file; the
storage calls around them) plus two minutes, and the work stops before the
lease ends. Every step that moves the Media on checks the claim
is still its own. A worker whose lease ran out (another worker has claimed
the Media since) moves nothing on and deletes nothing. The archive and
expiry purges wait for a live scan claim, so a copy the scan makes can never
land after them. Account erasure does not wait.

For each claimed Media the worker streams the file's plaintext to clamd with
`INSTREAM`, in 64 KiB chunks, once [the ZIP check](#the-zip-check) has
passed it:

- a public file is read straight from R2;
- a private file is decrypted as it streams, through the same private storage
  read path as a read link.

Nothing is written to disk on core's side, and a file is never held whole in
memory. Each file's scan is bounded by a timeout of two minutes plus a second
per MiB (about 19 minutes for 1 GiB). A verdict counts only when clamd sends
exactly one NUL-terminated answer after the end of the stream (the
zero-length chunk). clamd answers before the end only to refuse the stream
(a size limit, an `ERROR`). Any other answer there, an answer cut short, or
one core does not know is a protocol failure (`clamd.ErrProtocol`): never
clean, and tried again later. After the answer, core reads on for 200
milliseconds: clamd closes the connection once it has answered, and any
second answer, even in a later write, is a protocol failure too.

One case cannot be told apart at the protocol level. An early `OK` that
arrives between the last chunk and the end of the stream reads like the real
answer, and for a stream of one chunk (64 KiB or less) there is no later
chunk to catch it. A real clamd never answers before the end of the stream,
so this only matters for something that is not clamd.

- A Media whose scan fails waits 30 seconds before it is tried again. Each
  failure in a row doubles the wait, up to an hour (`scan_attempts`,
  `scan_retry_at` on the Media), and the pass walks past it to the others.
- A clamd that cannot be reached ends the pass without counting a failure
  against any Media (its claim is let go). The worker then waits 10 seconds
  before the next pass, doubling the wait up to 5 minutes while clamd stays
  down.
- It logs what a pass changed (clean, rejected, failed) and each Media that
  failed (by id; never a file name). It says once that clamd is down and once
  that it answers again. A pass with nothing to do logs nothing.

Every step is idempotent, and a pass cut short is simply made again. A
rejection is recorded before any object is deleted. The Media is `rejected`,
with the claim that its object is being deleted (`blob_purge_started_at`,
which refuses new Media attachments and restores as a purge's claim does).
The objects are deleted after that, and the purge is recorded
(`blob_purged_at`). A rejection cut short (a crash, a storage failure) is
finished by a later pass. The Media record stays, not archived, so its
owning product reads why it was rejected.

An archived Media waiting for its scan is not scanned: restored, it is
scanned then.

### The ZIP check

Media redesign ticket 23. Checked against a real clamd 1.5.4 (the fixtures
are in `internal/zipcheck/clamav_integration_test.go`; they load a test
signature that matches a marker anywhere, so that a marker clamd misses is
one it did not read):

- clamd reads an archive member that inflates past its `MaxFileSize` only up
  to `MaxFileSize`, without a report ("trimming output size to
  maxfilesize" in its debug log): the rest goes unscanned. It goes by what
  the member really inflates to, not by the sizes its headers declare. A
  stored member that large is reported.
- It does not read a ZIP member with ZIP64 sizes, nor one compressed with a
  method it does not know.
- It unpacks archives it finds inside other files: an Office document is a
  ZIP, and a ZIP appended to a program, an image or a PDF is unpacked from
  past the file's first byte, with the same limits and the same silent
  truncation.
- It unpacks a lone local header wherever it finds one, with no directory
  listing it: hidden in a ZIP's end record comment or a member's extra
  field, in a ZIP and in a ZIP appended to a PDF. It goes by the signature
  alone: a header of version 25.5, with no name or one of 2000 bytes, or
  with a reserved flag, is unpacked too.
- A gzip or tar member past `MaxFileSize` is reported, not truncated
  silently.

So **every scanned file** is read once before clamd streams it, and not
streamed to clamd until core has checked that clamd would scan all of it
(`internal/zipcheck`). What the file is comes from its first bytes, whatever
its type says:

- **a file whose content is a ZIP** (a ZIP, a DOCX, an XLSX, a JAR) is
  checked whole (below);
- **any other file** (a PDF, an image) is streamed once and searched for
  archives past its first byte, as a member is (see "Any other file" under
  what a member holds): a ZIP appended to a PDF is checked as a nested
  ZIP, any other archive there is refused as `archive_nested`.

The check runs in the scan worker, under the Media's scan claim, holding no
database connection. A public held file is read by ranged GETs. A private
file (an Answer file) is decrypted as it streams, into nothing but memory,
never to disk; a ZIP is kept in memory to be checked, at most
`MEDIA_ZIP_CHECK_BUFFER_MIB` (64 MiB), which an Answer file (20 MiB) always
fits. The extra read of every file is the price of it, and the claim's
lease allows for it.

A ZIP is checked so:

1. Its end and central directory are read by ranged GETs (the file's last
   65,577 bytes: the end record's longest comment and a ZIP64 locator, then
   the directory and what follows it, in reads of at most 1 MiB; a
   directory over 8 MiB with what follows it is refused as
   `too_large_to_scan`), or from memory.
2. The file is streamed once, in order, and every member is inflated
   without being kept, to check what each really holds.
3. What each member holds is read as clamd would unpack it (below).

A file that fails is rejected before any byte of it reaches clamd, as any
rejection (its object deleted, the rejection recorded without a
signature):

- as `too_large_to_scan` when a member inflates to more than `MaxFileSize`,
  all members of every level together to more than `MaxScanSize`, or the
  members of every level together are more than `MaxFiles`;
- as `archive_invalid` when it is malformed or holds what clamd cannot
  read:
  - no end record at its end;
  - an end record signature after its own end record (in its comment), or
    outside the members' data (in a local header, a data descriptor, the
    directory): a reader could take it for the end, and read another
    archive;
  - a central directory that is not where the end record says, or that
    holds another number of entries;
  - a ZIP64 end record or locator that disagrees with the end record, or an
    end record that leaves its counts to ZIP64;
  - members on another disk;
  - an entry that points outside the members, members that overlap (the
    overlap bomb), or bytes before, between or after members that no member
    accounts for;
  - an encrypted member;
  - a compression method other than stored (0) or deflate (8);
  - ZIP64 sizes (only needed past 4 GiB);
  - a name that is empty, longer than 1024 bytes, or holds a NUL;
  - a local header or data descriptor that disagrees with the directory;
  - a local header signature outside the members' data (in a local
    header's name or extra field, a data descriptor, the directory, the end
    record's comment): the ZIP's own local headers are the only ones, and
    clamd would unpack a hidden one no directory lists;
  - a member that inflates to other bytes than it declares (size or
    checksum), or has bytes after its deflate stream;
  - a gzip, bzip2 or tar inside that is corrupt or cut short;
- as `archive_nested` when it holds an archive core cannot check (the
  nesting rule below).

An end record signature inside a member's data is that member's content,
checked as such: a stored inner ZIP's own, or an Office file's deflated
into stored blocks (deflate keeps incompressible data as it is). So a ZIP
ending with a PPTX of images, a small ZIP with a stored inner ZIP, and what
Info-ZIP writes when it stores an inner ZIP it cannot shrink all pass.
clamd 1.5.4 reads the same, last end record in each of these (checked:
it finds a test file in the first member, which only that directory
lists).

The worker logs why (a `media.ScanRejection`), naming members by their
place (`member 3`, `member 2 of member 5`, `entry 1 of what member 4
unpacks to`), never by name. A check that could not finish (storage down,
a timeout) says nothing about the file: the Media waits scanning and is
tried again, as after any failure. The check's time grows with what the
file inflates to: two minutes, two seconds per MiB of the file, and a
second per MiB its members inflate to (`zipcheck.Timeout`; at most the
`MaxScanSize` worth), and every scan's lease allows for its longest.

**What a member holds.** Each member is told by its content, never by its
name alone: a `raspi.img` of plain bytes is a plain file, and a
`not-really.zip` of text too.

- **A ZIP** (Office packages, JARs and APKs are ZIPs) is checked the same
  way, recursively, down to `min(MaxRecursion - 2, 3)` archives deep (the
  upload is level 0): a PPTX with an embedded workbook passes. A stored
  inner ZIP is read again from its own range of the file; a deflated one,
  or one inside a gzip or tar, is kept in memory to be read again, at most
  `MEDIA_ZIP_CHECK_BUFFER_MIB` (64 MiB). A larger one is refused as
  `archive_nested`.
- **A gzip or bzip2** (`data.csv.gz`) is unpacked; what it unpacks to is one
  more member, measured against `MaxFileSize` and `MaxScanSize`, and read
  in turn. A gzip of more than one member is refused as `archive_nested`
  (some readers unpack only the first).
- **A tar** (`.tar`, `.tar.gz`, the oldest kind too) is read header by
  header: each entry is a member of its own, counted toward `MaxFiles`,
  held to `MaxFileSize` and `MaxScanSize`, and read in turn.
- **An archive of a format core cannot open**, told by its full signature
  (7-Zip, RAR, xz, cab, cpio, ARJ, LHA, ISO 9660, XAR, EGG, ALZip), is
  refused as `archive_nested`: core cannot see what clamd would leave
  unread inside it. A file that only starts like one (a text beginning `BZh`, an ARJ mark
  whose header does not check out) is plain, and so are formats clamd does
  not open (a static library, a Debian or RPM package).
- **Any other file** is searched, as it streams, for archives embedded past
  its first byte (a self-extracting program, a ZIP appended to an image):
  a ZIP local header as clamd takes one (the signature, a method the format
  defines, and a name, extra field and data that fit in the rest of the
  file, whatever its version, flags or name length), or the full signature
  of RAR, 7-Zip, cab or ARJ (an ARJ header whose CRC checks out). The
  first local header starts a ZIP that must end the file, checked as a
  nested one, its offsets counted from the ZIP or from the file's first
  byte (as `zip -A` leaves a self-extractor); from its own range when the
  member is stored, from memory otherwise. Every later local header must
  then be that ZIP's own or inside its members' data, which its check
  holds it to. A lone local header (no ZIP ending the file there), a ZIP
  that does not end the file, or one larger than the buffer, is refused as
  `archive_nested`; so is any other embedded archive. Chance data passes
  for a local header about once in 100,000 GiB.

Depth is counted as clamd counts it: a member of an archive n deep is inside
n + 1 archives, which clamd scans only while n + 1 is below `MaxRecursion`
(checked), and core reads no deeper than 3. An archive deeper than
`min(MaxRecursion - 2, 3)` is refused as `archive_nested`; with
`MaxRecursion` below 3, any archive inside the ZIP is.

A private ZIP larger than the buffer is not checked yet: it would have to be
read by ranged GETs, which a private object does not offer until private
Direct upload (ticket 21). It is never scanned unchecked: it waits
scanning, logged at each try (`media.ErrPrivateZIPUnchecked`), until its
scan deadline rejects it.

Not covered: core does not look inside a PDF's compressed streams.

### Public files before their scan

A public file must not reach the CDN before it is clean, and every key of the
public bucket is served at `cdn.`. So a public purpose that needs a scan is a
Direct upload purpose (ceiling), and a Direct upload's file is held apart
until its scan ends:

1. The completion copies the joined file to `pending/scan/<random uuid>`
   instead of `files/<uuid>`. It is stored as an opaque download
   (`application/octet-stream`, `Content-Disposition: attachment`, no name),
   and nobody but core knows the key. The pending key the browser wrote to
   (`pending/<upload id>`, which the uploader knows from the part addresses)
   is deleted as usual. The Media is `scanning`, its `url` is empty and it
   has no sizes.
2. Once clean, the worker copies the file to `files/<Media id>` with the
   [serving policy](#serving-policy)'s metadata, points the Media there,
   gives it its status and then deletes the held copy. The served key is the
   Media's id, so a copy retried after a crash lands on the same key, and
   only clean bytes are ever written there. The copy's download name is read
   before it, so the worker reads the Media again afterwards and rewrites the
   metadata if an account erasure cleared the name meanwhile.
3. A rejected file is deleted where it is held and never reaches `files/`.

Because the served key follows from the Media's id, every purge of a held
Media deletes both keys: the held one and `files/<Media id>`. This covers the
archive, expiry and erasure purges and a rejection. A clean copy that landed
before the Media was purged, or that a worker made before it crashed, is
therefore never left behind. A missing object counts as deleted.

The hold sits under `pending/`, so two things cover it. The Cloudflare rule
that answers `403` for `/pending/*` on `cdn.` refuses it even to someone who
guessed the key. **This rule is required before any public purpose that
needs a scan opens (`club_file`, an Event's files, ticket 22).** Both the ClamAV wizard and the
Direct upload R2 wizard check it (a request to a `/pending/` path answers
`403`) and report its absence as a failure. The R2 lifecycle rule deletes
anything left there after two days (see
[R2 lifecycle and CORS](#r2-lifecycle-and-cors)): a held copy the worker
could not delete, or a file whose scan waited that long (its Media is then
rejected as `lost`). A private file needs no hold, since the private bucket
is never served: an `answer_file` stays at its `private/files/…` key
throughout.

Records that link a Media (an Event's cover, gallery, files and videos, a
User's profile picture) build no address for one that is `scanning` or
`rejected` (`media.ServedKeySQL`, and the status in
`media.LinkedImageSQL`), so a held key never becomes an address. The one
reviewed purpose that can be linked while it is scanned is `club_file`, as
an Event's file: see [Event files and videos](#event-files-and-videos).

### Opening

A read link (`POST /v1/media/{id}/links`) and the content it opens
(`GET /v1/media/{id}/content`) are refused while the Media is not clean. The
checks that the caller may open the Media come first, so to anyone else it
still does not exist (`404`):

| Status | `code` | Extra members | When |
|---|---|---|---|
| 409 | `media_scanning` | `retryAfterSeconds`, `Retry-After` header (30) | The Media is waiting for its malware scan. Show it as pending, not as missing, and retry later. |
| 410 | `media_rejected` | `scanResult` | The scan rejected the Media (`infected`, `too_large_to_scan`, `archive_invalid`, `archive_nested`, `lost`, `integrity` or `scan_timeout`) and its object is deleted. The uploader must upload the file again (`archive_nested`: with its inner archives unpacked). |

A public Media gets its address (`url`, `sizes`) only once clean.

### Rejections

Every rejection is kept in `media_scan_rejections` (migration
`20260928140000`, `archive_invalid` and `archive_nested` since
`20260928180000`) as the scan's event record. It holds the Media id, the
result (`infected`, `too_large_to_scan`, `archive_invalid`,
`archive_nested`, `lost`, `integrity` or `scan_timeout`), the name clamd
gave what it found (`Eicar-Test-Signature`,
`Heuristics.Limits.Exceeded.MaxFiles`, …; empty when clamd named nothing) and
the time. It
holds no file name and no person: the Media id leads to them while the Media
keeps them, and account erasure clears both there. The row stays with the
Media's record, which is never deleted.

### ClamAV

A human step, done by `ops/wizards/media-clamav-wizard.sh` in sky_lab_genel
(on the server, sandbox first, then production). It creates a Dokploy
application for each side, in core's project and environment:

- the official image `clamav/clamav:1.5.4` (pinned; it ships a signature
  database, and freshclam keeps it current);
- the internal network only (`dokploy-network`), no published port and no
  domain. Core reaches it at `<its appName>:3310`;
- a named volume on `/var/lib/clamav` for the signature database;
- clamd and freshclam configuration through the image's `CLAMD_CONF_*` and
  `FRESHCLAM_*` environment:
  - `ConcurrentDatabaseReload no`: one copy of the database in memory; scans
    wait about a minute during a reload;
  - `StreamMaxLength`, `MaxFileSize` and `MaxScanSize` of `1024M`, the
    largest purpose that needs a scan;
  - `MaxFiles` of 10000 and `MaxRecursion` of 17 (clamd's own defaults,
    written out because core's ZIP check must use the same);
  - `MaxScanTime` of 10 minutes;
  - `AlertExceedsMax yes`: a file clamd cannot scan whole is reported as
    `Heuristics.Limits.Exceeded.<limit>` instead of passing as far as it got.
    The limits are `MaxFileSize`, `MaxScanSize`, `MaxFiles` (10,000 files per
    archive) and `MaxRecursion` (17 nested levels), and core rejects such a
    file as `too_large_to_scan`. It is the only `AlertExceeds*` setting clamd
    1.5.4 has;
  - freshclam checks 6 times a day.

  Being under `1024M` does not mean a file is scanned whole: an archive also
  meets the limits above. Checked against a real clamd 1.5.4:

  - `AlertExceedsMax` reports `MaxFiles`, `MaxFileSize` and `MaxRecursion`
    exceeded;
  - a compressed archive member that inflates past `MaxFileSize` while the
    archive itself stays under it is read only up to `MaxFileSize`, without
    a report, whatever sizes its headers declare (a stored one is
    reported);
  - a ZIP member with ZIP64 sizes, or one compressed with a method clamd
    does not know, is not read at all, and the same holds inside an archive
    clamd finds in another file (an Office document, a ZIP appended to a
    program or a PDF) and for a lone local header it finds anywhere.

  So core checks every file whose content is a ZIP before clamd scans it,
  and refuses one clamd would not scan whole: see
  [The ZIP check](#the-zip-check). The wizard writes `MaxFiles 10000` and
  `MaxRecursion 17` (clamd's own defaults) explicitly, so that all four
  limits are set in one place. Core's limits (`MEDIA_CLAMAV_MAX_*`, see
  [Configuration](#configuration)) must be these; their defaults are, and
  the wizard checks core's startup line against them. The
  `AlertEncrypted*` settings stay off: they would report every
  password-protected PDF (a common kind of official document) as malware.
  A ZIP with an encrypted member is refused by the ZIP check instead.
- memory: a 3 GiB limit and a 1.5 GiB reservation. clamd holds about 1 GiB
  with the full database loaded (measured, 1.5.4). freshclam's database test
  briefly loads a second copy, and a redeploy runs the old and the new clamd
  side by side for a few minutes. The production server has 15 GiB of RAM,
  about 9.8 GiB available and no swap (decision S3: ClamAV if the RAM holds
  it, and it does). The limit keeps a runaway clamd from taking core down with
  it: the kernel kills ClamAV, and Swarm restarts it.

The wizard waits for clamd to answer and for freshclam's first update. It
then runs the self-test (below) from inside core's container and prints the
line for core's environment, `MEDIA_CLAMAV_ADDR=<appName>:3310`; it does not
set it. Once Yusuf has added it and core has been redeployed, the wizard runs
the self-test again with core's own environment and checks core's startup
line.

### Self-test

`core-backend media-scan-selftest [-addr host:port]` sends the EICAR test
file to clamd at `MEDIA_CLAMAV_ADDR` (or `-addr`) and prints clamd's version
and the name it gave the file. It exits:

- 0 only when clamd reports it `FOUND`;
- 1 when clamd answers it clean (no database that knows it), cannot be
  reached, or answers an error;
- 2 without a usable address.

Run inside core's container, it also proves core reaches clamd over the
internal network:

```sh
docker exec <core container> ./core-backend media-scan-selftest
```

Core's source and binary never carry the EICAR file whole; it is put together
at run time.

## Event files and videos

Media redesign ticket 22 (ADR-0052, decision C1). An Event offers two lists
of Media, each in the order its organizers give: its **files**
(`club_file`: PDF or ZIP up to 1 GiB, scanned) and its **videos** (`video`:
MP4 up to 2 GiB, served to play). Both are sent by
[Direct upload](#direct-upload), once their side switches them on
(`MEDIA_DIRECT_UPLOAD_PURPOSES`). The
Event links them in two link tables, `event_files` and `event_videos`
(migration `20260928160000`), whose triggers write and remove their Media
attachments (roles `event_file` and `event_video`) as the gallery's do (see
[Core's own links](#cores-own-links)). The admin UI is core-frontend's
(media redesign ticket 20).

### Endpoints

Whoever may edit the Event may change its lists, by the same decision as
`PUT /v1/events/{id}` (its Owner team's leaders and coordinators, a
privileged person, members where the team's Event permissions let them).
Each route takes a JSON array of Media ids, as the gallery's do, and answers
the Event's detail (`200`, below):

| Route | What it does |
|---|---|
| `POST /v1/events/{id}/files` | Appends the club files after those listed, in the order given. One already listed stays where it is. |
| `DELETE /v1/events/{id}/files` | Removes them: all of them, or none (`404`) when one is not listed. |
| `PUT /v1/events/{id}/files/order` | Orders the list: every listed file once, in the new order. |
| `POST /v1/events/{id}/videos`, `DELETE /v1/events/{id}/videos`, `PUT /v1/events/{id}/videos/order` | The same for videos. |

```sh
curl -X POST https://api.yildizskylab.com/v1/events/$EVENT/files \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '["0f2a…","7c41…"]'
curl -X PUT https://api.yildizskylab.com/v1/events/$EVENT/files/order \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '["7c41…","0f2a…"]'
```

Refusals (problem+json):

| Status | `code` | When |
|---|---|---|
| 401 | | No token. |
| 403 | | The caller may not edit the Event. |
| 404 | | No such Event, or it is archived; a removal names a Media the list does not hold. |
| 400 | | The body is not a JSON array of UUIDs; an order names an item twice. |
| 409 | | An order does not name exactly the list's items: the list changed meanwhile. Read the Event again. |
| 422 | `media_purpose_mismatch` | A Media of another purpose: a video among the files, a club file among the videos, a photo, or a legacy Media (see [Link rules](#link-rules)). |
| 422 | `media_not_linkable` | No such Media, or archived, rejected by its scan, being purged, or expired. |
| 403 | `media_team_mismatch` | Another Owner team's Event lists it (Team media library). |

A refused addition adds nothing: one Media that may not be linked refuses the
whole request.

### In Event responses

An Event's detail carries both lists: `GET /v1/events/{id}` and the answer of
every change to one Event (create, update, restore, the gallery's, files'
and videos' routes, a season assignment).

```json
{
  "files": [
    { "id": "0f2a…", "name": "veri seti.zip", "type": "application/zip", "size": 734003200, "status": "attached", "url": "https://cdn.yildizskylab.com/files/9243…" },
    { "id": "7c41…", "name": "sunum.pdf", "type": "application/pdf", "size": 1048576, "status": "scanning" },
    { "id": "b810…", "name": "araç.zip", "type": "application/zip", "size": 52000, "status": "rejected", "scanResult": "infected" }
  ],
  "videos": [
    { "id": "5e9d…", "name": "açılış.mp4", "type": "video/mp4", "size": 1610612736, "status": "attached", "url": "https://cdn.yildizskylab.com/videos/1c07….mp4" }
  ],
  "fileCount": 1,
  "videoCount": 1
}
```

- `id` is the Media's id; `name` the name the file was uploaded under
  (empty once its uploader's account was erased); `type` the type detected
  from its first bytes; `size` in bytes. Only a ZIP downloads under `name`
  (`Content-Disposition`); a PDF opens in the browser and a video plays, and
  saving either takes the name of its address (its key).
- `status` is the Media's: `attached`, `scanning` while its malware scan runs,
  `rejected` once the scan rejected it (`pending` only for a moment no
  answer shows).
- `url` is there only while the item can be served, built like the cover's
  address (`CDN_BASE`, see [Addresses](#addresses)). "Can be served" is one
  rule, `media.ServableSQL` in the query and `media.Media.Servable` in the
  Media JSON (a test keeps them equal): public, its object not being or
  already purged, neither waiting for its malware scan nor rejected. So never
  while it is scanned or once rejected (whose key is core's alone), never
  for a private Media. A video's address is `videos/<uuid>.mp4`, served
  inline as `video/mp4` to play (see [Serving policy](#serving-policy)),
  until its faststart rewrite moves it to `videos/<uuid>.fs.mp4` (see
  [Video faststart](#video-faststart)). A video has no poster image yet.
- `scanResult` is only on a rejected item, and only the Event's editors see
  one.
- Whoever may not edit the Event (and anyone without a sign-in) sees only the
  items with an address. `fileCount` and `videoCount` count those, by the
  same rule, for everyone.
- An item whose Media was archived is left out, as in the gallery.

Lists (`GET /v1/events`, `/v1/events/active`, the lifecycle views,
`GET /v1/seasons/{id}/events`) and the Event summary (`event.Resource`: the
door's Events, a ticket's or a competitor's `event`) carry `fileCount` and
`videoCount` alone, never `files` or `videos`. The counts are subqueries of
the query that reads the Events, so a list still takes its three queries
(a test lists Events with files and videos and counts them); a detail reads
both lists in one more query.

### Rules

- `event_file` takes only `club_file`, `event_video` only `video`, and a
  legacy Media neither (see [Link rules](#link-rules)); the database checks
  it again.
- A club file may be added while its scan runs. Its editors see it
  `scanning`; once clean the scan worker copies it to `files/<Media id>`,
  it becomes `attached` and everyone sees it. A rejected one stays listed,
  for the editors only, until they remove it.
- The Team media library holds as for photos, and moving an Event to another
  Owner team checks its files and videos again.
- Removing an item removes its Media attachment; a Media with no other one
  is detached and purged 30 days later unless something attaches it again.
  An archived Event keeps its lists, as it keeps its gallery.
- The Event's row is locked while one of its lists changes, so two editors'
  additions take their places one after the other and an order is checked
  against the list it rewrites. Ties in `order_index` are read in
  `added_at`, then id order.
- Account erasure keeps an Event's files and videos (club purposes, see
  [Account erasure](#account-erasure)).

### Before Event files open

Nothing opens by deploying this: a Direct upload purpose opens only where
`MEDIA_DIRECT_UPLOAD_PURPOSES` names it, and it names none by default.
Attaching and listing work whatever the switch says.

- **`video`**: switch it on (`MEDIA_DIRECT_UPLOAD_PURPOSES=video`) once
  items 1 and 2 below hold on that side. Anyone who may create an Event
  (`event_editor`) can then upload one. It needs no scan (`scan: false`,
  unchanged; at 2 GiB it is above what clamd scans, `media.MaxScanBytes`).
- **`club_file`**: switch it on (`club_file,video`) once items 1 to 4 hold
  on that side, on a core that has [the ZIP check](#the-zip-check)
  (ticket 23: core refuses a ZIP clamd cannot scan whole). ClamAV going live
  for Answer files (single-step) does not open it.

On each side, sandbox first:

1. **R2**: `ops/wizards/media-direct-upload-r2-wizard.sh` has run: the
   lifecycle rule on `pending/` and the bucket's CORS for the admin panel's
   origin (core-frontend). Without the CORS rule a browser cannot send the
   parts.
2. **CDN for video**: the CDN answers Range requests and adds
   `X-Content-Type-Options: nosniff` (the Cloudflare rule of
   [Serving policy](#serving-policy)). Production does (checked 2026-09-28).
   Check a side with any object on its CDN:
   `curl -sI -H 'Range: bytes=0-99' <an object's address>` answers `206`
   with `Accept-Ranges: bytes`, `Content-Range` and
   `x-content-type-options: nosniff`.
3. **Cloudflare `/pending/*`**: the WAF rule that answers `403` for
   `/pending/*` on `cdn.` is in place (both wizards check it). Required
   before `club_file`: a file waiting for its scan is held under
   `pending/scan/`.
4. **ClamAV**: `ops/wizards/media-clamav-wizard.sh` has run, core's
   environment has `MEDIA_CLAMAV_ADDR`, and the self-test in core's
   container finds EICAR. Without it `club_file` is refused
   (`purpose_not_available`) even when switched on. Core's startup line
   `media scan limits (clamd.conf): ...` shows the limits the ZIP check
   holds a ZIP within: they must be the wizard's clamd settings (the
   defaults are), and the wizard checks that they are.

## Video faststart

Media redesign ticket 13 (ADR-0052, decision Q26). An MP4 whose `moov` box
(the index of its samples) comes after its `mdat` (the samples) plays only
once the browser has it whole, or after a Range request to its end: cameras
and phones write it so. Core rewrites every `video` Media into a faststart
copy, `moov` first, without re-encoding: the samples stay byte for byte.

Q26 said ffmpeg (`-movflags +faststart`) in a separate process with time
and memory limits. Core does it without ffmpeg instead, with qt-faststart's
method written in Go (`internal/faststart`): no binary in the image, no
subprocess, and the video is never downloaded.

### The rewrite

1. The video's top-level boxes are read by bounded ranged reads (32- and
   64-bit sizes, and a last box whose size 0 runs to the end of the file).
   A video whose `moov` already comes before its first `mdat`, or that has
   none, needs nothing: `not_needed`.
2. The `moov` is read whole (up to 64 MiB; a two-hour video's is a few MiB)
   and walked down `moov/trak/mdia/minf/stbl` to every track's chunk offset
   table (`stco`, or `co64` for 64-bit offsets). Every other box is kept as
   it is.
3. The copy is: the video's bytes up to its first `mdat`, the rewritten
   `moov`, the bytes from that `mdat` up to the old `moov`, and whatever
   followed the old `moov`. Every chunk offset moves with the bytes it
   points into: by the new `moov`'s size for the media data, by its growth
   for what followed it. An `stco` whose offsets would pass 4 GiB once moved
   is upgraded to a `co64` (every `stco` at once, as qt-faststart does), and
   the shift is worked out again with the larger `moov`.
4. The copy is written to `videos/<uuid>.fs.mp4`, beside the original,
   never over it: a half-written rewrite is never served. A copy of up to
   16 MiB is one `PutObject`. A larger one is a multipart upload of 16 MiB
   parts (R2 wants every part but the last the same size, at least 5 MiB):
   core writes the parts that hold the rewritten `moov` or straddle a
   boundary (at most a few, each read by ranged `GET`s), and R2 copies every
   other part from the original's bytes itself (`UploadPartCopy` with a
   byte range). Its metadata comes from the [serving policy](#serving-policy):
   `video/mp4`, inline.
5. The copy is checked before anything points at it: its size by a `HEAD`,
   its top-level boxes (`moov` first, where planned), its `moov` byte for
   byte, and the first and last chunk of each track (up to 16) against the
   original's.
6. A short transaction points the Media at the copy, with the copy's size
   (an `stco` upgraded to a `co64` grows it). The original stays an hour
   (`media.FaststartOriginalGrace`), for a player that loaded its address,
   then goes: the Media is `done`.

A video the rewrite cannot move safely is `failed` and served as it is (it
still plays once downloaded), logged by its id with why (box types and
offsets, never a file name): a malformed box, a file cut short, a `moov`
over 64 MiB before or after the rewrite, more than 256 top-level or
100 000 `moov` boxes, a compressed `moov` (`cmov`), a fragmented file
(`mvex` in the `moov`, or a top-level `moof`, `mfra`, `sidx` or `ssix`
with the `moov` after the media data), sample auxiliary offsets (`saio`),
a chunk offset outside the media data, or no chunk offsets at all. Other
absolute offsets (an `iloc` in a `meta` box) are not moved, as
qt-faststart does not move them either.

### The worker

The faststart worker (`media.FaststartWorker`) starts with core wherever it
has R2 (`R2_*`), on its own context; without R2, videos are served as they
are. It makes a pass at once, whenever a Direct upload stores a video, and
every minute. A pass claims at most 25 videos (another pass follows at once
when it claimed 25), each by id:

- A video is **claimed** in a short transaction (`FOR UPDATE SKIP LOCKED`,
  `video_faststart_claim_id` and `video_faststart_claimed_until`) before
  any storage work, and no database connection is held while storage
  works. The lease is the claim's work (five minutes and a second per MiB:
  about 39 minutes for 2 GiB) plus two minutes, and the work stops before
  the lease ends. A video another worker (the other core of a rolling
  deploy) has claimed is left alone. Every step that moves the Media on
  checks the claim is still its own.
- A step storage fails is tried again a minute later, each failure in a
  row doubling the wait up to six hours (`video_faststart_attempts`,
  `video_faststart_retry_at`). After twelve failed rewrites (about a day)
  the video is `failed`, served as it is. A moved video whose original
  cannot be deleted is tried again, never given up.
- A copy cut short is deleted, and so is any multipart upload still open
  at its key: deleting a `videos/<uuid>.fs.mp4` key aborts them, as
  deleting a pending key does. A rewrite starts by deleting whatever an
  earlier attempt left there.
- It logs what a pass changed and each video that failed or was refused,
  by id; a pass with nothing to do logs nothing.

The state is `video_faststart` (migration `20260929120000`): empty while
the video waits (a moved video waits for its original's hour), then
`done`, `not_needed` or `failed`. Videos stored before this are rewritten
by the same worker after the deploy.

### Purges

Each of a video's two keys names the other, so every purge of the Media
(archive, expiry, a scan rejection, account erasure) deletes both,
whichever the Media points at: the copy a rewrite wrote, and the original
still in its hour. The archive and expiry purges wait while a rewrite's
claim is live, as they wait for a scan's, so no copy lands after them. A
copy whose claim's lease ran out (a stalled worker) never survives a purge
that did not wait: the worker checks the Media before pointing it at the
copy, and deletes the copy when the Media's purge has begun or it is gone.
A worker that lost its claim to another deletes nothing the Media points
at: both write the same bytes to the same key. Account erasure keeps a
video (club content) at whichever key it is.

## Account erasure

A person's account erasure ([`account-lifecycle.md`](account-lifecycle.md))
tells their uploads apart by Media purpose (media redesign ticket 07). The
rule is one function, `personalOnErasureSQL` in
`internal/media/account_erasure.go`, read under the purge's locks when
`erase_profile_media` comes to each upload:

- **Personal** (`answer_file`, `answer_file_large`): purged at once, whatever
  still uses them and whatever their malware scan state (a `scanning` one
  too; a `rejected` one's object is gone already).
- **Profile picture** (`profile_picture`): the person's own and purged, unless
  it is someone's current profile picture: a picture belongs to the person it
  shows, not to its uploader, so one the erased person uploaded for someone
  else is kept like club content.
- **Legacy** (decision E1, Yusuf, 2026-09-27): the person's own and purged
  while only personal uses hold it (nothing, or a Skyforms answer); any other
  use (another product's Media attachment, one of core's own links, the
  safety net, someone's profile) makes it club content.
- **Club** (every other purpose: `event_cover`, `event_gallery`, `cms_image`,
  `cms_file`, `club_file`, `video`, `certificate_asset`): the Media and its
  file stay, without the uploader and the file name. A club file still
  waiting for its scan stays held, unserved, as the nameless download it
  already is, and reaches `files/` once clean without the name. An Event's
  file or video stays on the Event, attached and at its address, with an
  empty `name`: a ZIP downloads under its key from then on; a PDF or a video
  named nobody in its metadata and is served as before.

**Known consequence of E1.** Until stage 5, when the CMS attaches the Media
its pages use (ticket 18), a legacy image a CMS page uses only by its address,
with no Media attachment, looks unused: it is purged when its uploader's
account is erased, and the page loses the image. Yusuf decided this knowingly
(E1).

No new saga step does this; the two existing ones do:

1. `anonymize_core`, in its one transaction and in this order, records every
   upload of the person that still has its object in `account_deletion_media`
   (by id only: request id and Media id), clears the file name of every upload
   of theirs, and then clears their uploader. The current profile picture
   stays with `profile_media_id` and today's rule, unless it is shared:
   anything but the person's own profile uses it (an Event, a gallery, another
   profile, a certificate template, or a Media attachment of another product,
   such as a CMS page). A shared picture is not the profile erasure's
   (`profile_media_id` stays empty): it is recorded with the other uploads,
   and the rule keeps it as club content, so it also stops being served under
   the person's file name. No Media is both `profile_media_id` and a record.
   A rerun finds no uploads left with their uploader, records nothing new and
   keeps what the first run recorded.
2. `erase_profile_media` hands the eraser the profile picture (unless its
   object is purged already), then every recorded upload, and the rule above
   decides each one:
   - The person's own is purged through the same two-phase purge as the
     archive and expiry purges, with the same table locks, the durable claim
     that blocks restore and new Media attachments, and the image sizes, but
     without their reference check. Its Media attachments stay, pointing at a
     purged Media, which reads refuse as not found (a Skyforms read link,
     `404`). The object is deleted from the bucket that holds it
     (`media.Buckets`), and an object already gone counts as deleted. Its
     record goes in the transaction that records the purge.
   - Club content keeps its file. A public object stored to download under
     the person's file name (an SVG, a ZIP, a legacy document) gets new
     metadata from the serving policy for its purpose without a name:
     `Content-Disposition: attachment`, so it downloads under its key. The
     key never held the name (`images/<uuid>`, `images/<uuid>.svg`,
     `files/<uuid>`, `videos/<uuid>.mp4`, `videos/<uuid>.fs.mp4`). Raster images, PDFs and videos
     are served inline and named nothing, so they keep their metadata: a
     video still plays. Then its record goes.

   A rerun gets only what is left; a Media purged another way meanwhile only
   loses its record. Club content the archive or expiry purge has already
   claimed keeps that claim untouched (it guards an object that may be half
   deleted): the erasure only rewrites its metadata, lets its record go, and
   the other purge finishes. The serving-policy backfill, which may have read
   an upload's file name before the erasure, reads the Media again after each
   metadata write and writes once more if the name changed, so it never
   brings an erased name back. Every upload is tried even when another fails. A pass
   that erased some but not all (the step's time ran out for a person with
   many files) gives its attempt back and comes again in 30 seconds; a pass
   that erased none spends its attempt, so a lasting failure ends in manual
   intervention. The step's error goes to the worker's log: it counts the
   failures and names no Media, object, file or person, only errors core wrote
   itself and a database error's SQLSTATE. `erase_staged_uploads` reports its
   errors the same way.

`cdn.` is not edge-cached (`cf-cache-status: DYNAMIC` for images, sizes and
files, checked 2026-09-27), so the new metadata is what the CDN serves at
once. If edge caching is ever enabled, the erasure's metadata rewrite must
also purge the URL from Cloudflare's cache.

What happens to the records when the request completes is in
[`data-lifecycle.md`](data-lifecycle.md).

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
- `MEDIA_DIRECT_UPLOAD_MAX_OPEN` — [Direct uploads](#direct-upload-budget) a
  person may have open at once; default `3`.
- `MEDIA_DIRECT_UPLOAD_DAILY_MAX_MIB` — MiB a person may declare in Direct
  uploads per rolling 24 hours; default `10240` (10 GiB).
- `MEDIA_DIRECT_UPLOAD_PURPOSES` — the Direct upload purposes this side
  opens, separated by commas (`video`, `club_file,video`); unset or empty
  opens none. Any other Direct upload purpose is refused
  (`purpose_not_available`), and an upload started before its purpose was
  taken out ends and is given back. A name that is not a Direct upload
  purpose of the catalogue, or a private one (`answer_file_large`, until
  ticket 21), stops core at startup. See [Checks](#checks) and
  [Before Event files open](#before-event-files-open).
- The rest of Direct upload is fixed in code: an upload lives 12 hours, a
  part address an hour, every part but the last is 16 MiB, and a
  completion's claim is leased for 20 minutes (`media.DirectUploadTTL`,
  `media.DirectUploadPartURLTTL`, `media.DirectUploadClaimLease`). It uses core's
  R2 (`R2_*`); without it, it answers `503` `direct_upload_unavailable`. The
  staging sweeper's settings above end expired uploads.
- [Video faststart](#video-faststart) has no settings: it runs wherever
  core has R2. A pass every minute (and after every video uploaded), 25
  videos a pass, 16 MiB parts, a 64 MiB `moov` at most, an original kept an
  hour after its Media moves, a failed step retried after a minute doubling
  to six hours, twelve failed rewrites before a video is `failed`, and a
  claim leased for five minutes and a second per MiB plus two minutes are
  fixed in code.
- `CDN_BASE` (or `R2_PUBLIC_URL`) — the public base of every Media address;
  default `https://cdn.yildizskylab.com`.
- The decode budget (2 images at once, 1 SVG, a 10-second wait) is fixed in
  code (`media.DecodeBudgetConfig`).
- `MEDIA_IMAGE_ADDRESS_MODE` — where image sizes point, in the Media JSON and
  in Event and User responses alike: `stored` (default) or `cloudflare`. Any
  other value stops core at startup.

- `MEDIA_CLAMAV_ADDR` — clamd's `host:port` on the internal network (the
  ClamAV Dokploy application's appName and `3310`, printed by the ClamAV
  wizard). Unset, core has no scanner and a purpose that needs a scan is
  refused; anything but `host:port` stops core at startup. See
  [Malware scan](#malware-scan). The rest is fixed in code: a pass every
  30 seconds (and after every scanned upload), a failed Media retried after
  30 seconds doubling to an hour, a pass after clamd was unreachable after
  10 seconds doubling to 5 minutes, a file's scan bounded by two minutes
  plus a second per MiB, a ZIP's check by two minutes, two seconds per MiB
  of the file and a second per MiB it inflates to (`zipcheck.Timeout`), a
  scan's claim leased for its work (the check at its longest, which reads
  every file once more) plus two minutes, and a Media
  still scanning a week after its upload rejected (`media.ScanDeadline`).
- `MEDIA_CLAMAV_MAX_FILE_MIB`, `MEDIA_CLAMAV_MAX_SCAN_MIB`,
  `MEDIA_CLAMAV_MAX_FILES`, `MEDIA_CLAMAV_MAX_RECURSION` — clamd's
  `MaxFileSize` and `MaxScanSize` (whole MiB, 1 to 4095), `MaxFiles` (1 to
  65535) and `MaxRecursion` (2 to 255), which
  [the ZIP check](#the-zip-check) holds a ZIP within. **They must match
  clamd's own configuration**: a ZIP within core's limits but past clamd's
  would be scanned in part without a report. Defaults `1024`, `1024`,
  `10000` and `17`: the ClamAV wizard's `MaxFileSize`, `MaxScanSize`,
  `MaxFiles` and `MaxRecursion`. Change one only with the same change to the
  wizard's `CLAMD_CONF_*` settings; the wizard checks core's startup line
  against them. They are read only with `MEDIA_CLAMAV_ADDR`; a value that is
  not a whole number in its range stops core at startup, and so do limits
  the worker is given only in part. Core logs them at startup:
  `media scan limits (clamd.conf): MaxFileSize 1024 MiB, MaxScanSize 1024 MiB, MaxFiles 10000, MaxRecursion 17; ZIP check buffer 64 MiB`.
- `MEDIA_ZIP_CHECK_BUFFER_MIB` — core's own limit (whole MiB, 1 to 1024;
  default `64`) on what [the ZIP check](#the-zip-check) keeps in memory: an
  inner ZIP it reads again (deflated, or inside a gzip or tar, or embedded
  past a member's first byte), and a private file it decrypts to check.
  Larger inner archives are refused as `archive_nested`; a larger private
  ZIP waits. The check holds at most one such buffer per nesting level
  (three) at once.
- `MEDIA_SERVICE_CLIENTS` — the products' service clients for the
  [service attach API](#service-attach-api), `product:client` pairs
  separated by commas (products `forms`, `cms`), or `none`; default
  `forms:forms`. Startup fails on anything else. A product with no client
  cannot attach, and its purposes cannot be uploaded.

The detached window is fixed at 30 days by the database;
`MEDIA_BLOB_RECOVERY_DAYS` does not change it.

[Private Media](#private-media) reads these names (the wizard prints their
values). With `MEDIA_PRIVATE_ENABLED` unset or `false`, none of the others is
read or required (and `PUBLIC_API_ORIGIN` keeps its default for certificate
and QR links). With `true`, every one is required and checked at startup, and
core does not start on a missing or invalid one; the error names the
variable, never its value.

| Variable | Value in core's Dokploy env | |
|---|---|---|
| `MEDIA_PRIVATE_ENABLED` | `false` | `true` or `false`; anything else stops core. |
| `MEDIA_OPENBAO_ADDR` | `http://<OpenBao Swarm service>:8200` | Internal network only; an http(s) address with no path. |
| `MEDIA_TRANSIT_MOUNT` | `transit/<side>` | |
| `MEDIA_TRANSIT_KEY` | `media` | |
| `MEDIA_OPENBAO_ROLE_ID` | `${{vault.bao-<side>.<core appName>/MEDIA_OPENBAO_ROLE_ID:value}}` | Not secret, kept in KV. |
| `MEDIA_OPENBAO_SECRET_ID` | `${{vault.bao-<side>.<core appName>/MEDIA_OPENBAO_SECRET_ID:value}}` | Secret. |
| `R2_PRIVATE_BUCKET` | `skylab-private-<side>` | Never the public bucket (`R2_BUCKET`). |
| `R2_PRIVATE_ACCESS_KEY` | `${{vault.bao-<side>.<core appName>/R2_PRIVATE_ACCESS_KEY:value}}` | Scoped to the private bucket; never the public bucket's `R2_ACCESS_KEY`. |
| `R2_PRIVATE_SECRET_KEY` | `${{vault.bao-<side>.<core appName>/R2_PRIVATE_SECRET_KEY:value}}` | |
| `MEDIA_LINK_SIGNING_KEY` | `${{vault.bao-<side>.<core appName>/MEDIA_LINK_SIGNING_KEY:value}}` | 32 random bytes, unpadded base64url; the wizard makes it in OpenBao. Signs read links. |
| `PUBLIC_API_ORIGIN` | sandbox `https://sandbox-api.yildizskylab.com`, production `https://api.yildizskylab.com` | Where read links point. Required with the flag on, so a sandbox without it cannot hand out links to production; the sandbox must set it. |

The private bucket uses core's `R2_ENDPOINT`.
