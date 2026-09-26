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
(see [Media attachment](#media-attachment)), and, as a safety net until the
legacy backfill (media redesign ticket 08) has proven every link has its
attachment, core's own links checked directly (an Event cover or gallery, a
User profile, a Certificate template draft, or a published Certificate
template version). Both must say unused. Database triggers also reject new
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
  running any script the regex sanitizer missed.
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
| `types` | Content types accepted, detected from the file's content, never from its name or declared type. |
| `max_mib` | Maximum size in MiB. |
| `visibility` | `public` (served from the CDN) or `private`. |
| `encrypted` | Encrypted before storage; true exactly for private purposes. |
| `scan` | Needs a malware scan before it can be opened. |
| `pending_ttl` | How long a Media with no Media attachment is kept (Go duration). `none` only for the legacy rules. |
| `transport` | `single_step` (through `POST /v1/media`) or `direct` (Direct upload). |
| `attach` | Who attaches the purpose's Media: `core` (a core record links it) or `service` (another product, through the service attach API). |
| `image` | Raster handling: `reencode`, `max_dimension`, `variants` (name → px), `rasterize_svg`. |
| `legacy_rules` | Only on `legacy`: the rules below instead of `types` and `max_mib`. |

`pending_ttl` sets a new Media's expiry (see
[Media attachment](#media-attachment)). `attach` decides whether a purpose can
be uploaded at all: a Media nothing can attach would only wait for its expiry,
so a `service` purpose is refused until the service attach API exists (media
redesign ticket 03). Today that is `cms_image`, `cms_file`, `answer_file`,
`answer_file_large`, `club_file` and `video`; where club files and videos are
attached is for the Direct upload tickets to settle. `image` is acted on (see
[Images and sizes](#images-and-sizes)); `scan` is declared now and read when
scanning ships. `image.variants` may name only the sizes clients can ask for,
`card` and `page`. An unknown field or value, a missing `legacy` entry, or a
ceiling violation stops core at startup.

The initial entries:

| Purpose | Upload | Types | Max | Visibility | Transport |
|---|---|---|---|---|---|
| `profile_picture` | authenticated | JPEG, PNG, WebP, GIF | 5 MiB | public | single-step |
| `event_cover`, `event_gallery` | event_editor | JPEG, PNG, WebP, GIF | 10 MiB | public | single-step |
| `certificate_asset` | certificate_template_editor | PNG, JPEG, PDF | 20 MiB | private | single-step |
| `cms_image` | authenticated | JPEG, PNG, WebP, GIF, SVG (rasterized to PNG) | 10 MiB | public | single-step |
| `cms_file` | authenticated | PDF | 20 MiB | public | single-step |
| `answer_file` | authenticated | PDF, JPEG, PNG, DOCX | 20 MiB | private, scanned | single-step |
| `club_file` | event_editor | PDF | 1 GiB | public, scanned | direct |
| `answer_file_large` | service_only | ZIP, PDF | 1 GiB | private, scanned | direct |
| `video` | event_editor | MP4 | 2 GiB | public | direct |
| `legacy` | authenticated | legacy rules | legacy rules | public | single-step |

Every public raster purpose re-encodes (2560 px) and gets the `card` (400 px)
and `page` (1200 px) sizes; `legacy` gets the sizes without the re-encode.

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

- a public purpose accepts only raster images (JPEG, PNG, WebP, GIF), PDF and
  MP4;
- a public purpose that accepts raster images declares re-encoding
  (`image.reencode`), and core re-encodes every such image (see
  [Images and sizes](#images-and-sizes));
- SVG is never stored as SVG: a purpose naming it rasterizes it to PNG
  (`image.rasterize_svg`);
- the maximum size stays under 20 MiB for single-step uploads and 2 GiB for
  Direct upload;
- the declared image size stays within 2560 px (`image.max_dimension`,
  variants), and re-encoding scales a larger image down to it;
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
| 422 | `purpose_not_available` | `purpose` | Another product attaches Media of this purpose (`attach: service`) and the service attach API it needs arrives with media redesign ticket 03. Nothing is stored; the file would only wait for its expiry. |
| 413 | `media_too_large` | `purpose`, `maxBytes`, and `maxPixels` for an image | Above the purpose's maximum; an image whose header claims more pixels than core decodes (`maxPixels`); an SVG above 1 MiB (`maxBytes` is then 1 MiB). |
| 415 | `media_type_not_allowed` | `purpose`, `allowedTypes` | The content is not one of the purpose's types, or starts like one but does not decode as it (a broken image, an animated WebP, a JPEG with more than 64 scans, an SVG core does not rasterize). |

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

1. **Header first.** Core reads only the header and refuses an image with more
   than 50 000 000 pixels, or whose decoding would take more than 256 MiB
   (deep 16-bit PNGs reach that first), as `media_too_large` with
   `maxPixels`: a decompression bomb costs nothing to refuse. A JPEG with more
   than 64 scans is refused as `media_type_not_allowed` before decoding (each
   scan makes a decoder walk the whole image again; encoders write one, or
   about ten when progressive).
2. **Decode, upright, fit.** The image is decoded, scaled down (Catmull-Rom)
   to the purpose's `max_dimension` on its longer side (2560 px), and turned
   upright by its EXIF Orientation, so the stored pixels need no tag. Content
   that starts like an accepted type but does not decode is
   `media_type_not_allowed`.
3. **Encode.** JPEG stays JPEG (quality 85). PNG stays PNG. Go has no WebP
   encoder, so a WebP becomes a JPEG when it is opaque and a PNG when it has
   transparency. A GIF becomes a PNG of its **first frame**: animations are
   not kept, so no frame count can multiply the work (an animated WebP does
   not decode at all and is refused). Colour profiles (ICC) are not carried
   over. The Media's `type` is the stored type.

At most two images are decoded at once in a core process (uploads and the
backfill below together), which bounds the memory decoding takes.

### Sizes

Clients ask for an image by Media and size name. The names are fixed in code,
because clients build on them: `card` (400 px, e.g. an Event card) and `page`
(1200 px, a picture across a page). Each purpose's `image.variants` sets
which it gets and how large, on the longer side.

A size smaller than the image is stored as its own object next to it, at
`<key>/card` and `<key>/page`, upright, in the image's stored type, with the
serving policy's metadata. A size the image already fits in is not stored:
its address is the original. The Media records its `width` and `height`
(as shown) and the sizes it stored (`variants` column: `{"card": {"width":
400, "height": 300}}`; `NULL` until core has made them, `{}` when it needs
none).

### Media uploaded without a purpose

`legacy` Media keep their own bytes, as before: a raster image is stripped of
EXIF, XMP and comments, not re-encoded. **One change:** a JPEG's EXIF
Orientation is kept, alone in a minimal EXIF segment, so a portrait phone
photo whose rotation lives only in EXIF no longer shows sideways. Their sizes
are made at upload time from the stripped image (upright); an image core
cannot decode, or with too many pixels or scans, is stored as before without
sizes. An SVG gets none.

### SVG

`cms_image` names SVG with `rasterize_svg`. Core never stores the SVG: it draws
it in pure Go (`github.com/fyne-io/oksvg` and `github.com/srwiley/rasterx`,
BSD-3-Clause) into a PNG whose longer side is 1200 px (the `page` size), makes
the `card` size, and stores those. Scripts, event handlers and links go with
the markup. The drawing is bounded:

- above 1 MiB: `media_too_large` (`maxBytes` 1 MiB);
- read as XML first, and `media_type_not_allowed` for a document type, more
  than 10 000 elements (counting what each `<use>` copies), nesting deeper than
  64, a `<use>` inside `<defs>` (which could refer to itself), no size
  (`viewBox` or `width`/`height`), or anything that does not parse;
- drawing stops after 5 seconds, checked on every line a shape is flattened
  into, so a single huge shape cannot run on; the upload is then
  `media_type_not_allowed`. Every shape costs a pass over the canvas, so an
  SVG with more than about a thousand shapes can hit the limit;
- a fault in the parser refuses the file instead of stopping core;
- dashes are drawn solid; text, filters and embedded images are not drawn
  (convert text to paths).

It runs in the request, in the core process, not in a separate one: the
limits above bound it instead. `cms_image` itself stays unavailable
(`purpose_not_available`) until the service attach API ships (ticket 03).

### Addresses

Every address core answers with is built from the configured base: `CDN_BASE`,
else `R2_PUBLIC_URL`, else `https://cdn.yildizskylab.com`. Nothing stored
holds a base, so moving the CDN (or to a separate user-content domain) is a
configuration change:

- a Media keeps its object key;
- a User's profile picture is read from the Media the profile links (its key);
  `users.profile_picture_url` holds the key for new pictures, and an absolute
  address stored there before is no longer read while the Media exists. The
  API's `profilePictureUrl` is unchanged while the base is the same.

`MEDIA_IMAGE_ADDRESS_MODE` picks where sizes point:

- `stored` (default): `<base>/<key>/<size>`, or the original for a size not
  stored;
- `cloudflare`: a Cloudflare image transformation of the original,
  `<base>/cdn-cgi/image/width=N,height=N,fit=scale-down/<key>` (never
  enlarged). Transformations must be enabled on the base's zone; 5 000 unique
  transformations a month are free, then $0.50 per 1 000 (Q24). Core keeps
  storing the sizes in both modes, so switching back is a configuration change
  too.

The Media JSON (upload response, `GET /v1/media/{id}` for everyone, the media
list) carries, for a raster image:

```json
{
  "url": "https://cdn.yildizskylab.com/images/<id>",
  "width": 1600,
  "height": 1200,
  "variants": {
    "card": { "url": "https://cdn.yildizskylab.com/images/<id>/card", "width": 400, "height": 300 },
    "page": { "url": "https://cdn.yildizskylab.com/images/<id>/page", "width": 1200, "height": 900 }
  }
}
```

`variants` lists every size of the Media's purpose; `width`/`height` are left
out when core does not know them yet. Event and User responses keep their
fields (`coverImageUrl`, `images[].url`, `profilePictureUrl`); a client reads
a size from the Media by its id.

### Stored images before sizes

A background backfill, started with core, walks the current image Media whose
sizes are not made (`variants IS NULL`, partial index
`media_variants_pending_idx`) by id, 25 at a time, and makes them from the
stored original, which it never rewrites: legacy originals keep their bytes,
purposed ones were re-encoded on upload. An image whose object is gone, that
does not decode, or is not a raster image is recorded with no sizes, so no pass
tries it again. A failure to read or write an object is logged with the Media's
id and retried on the next pass (a minute later) without holding up the rest.
Sizes written for an image whose purge began meanwhile are deleted again.

### Deleting sizes

Every path that deletes a Media's object deletes its sizes first: the archive
and expiry purges, account erasure's immediate purge (through the same store
call), the upload staging sweepers, and a refused upload. The size keys are
derived from the object key (`<key>/card`, `<key>/page`, for `images/` keys),
not read from the record, so a size an interrupted upload or backfill wrote
without recording is found too.

## Media attachment

A Media attachment links a Media to the record that uses it, in core or in
another product. Each row names the Media, the owner (`owner_service`,
`owner_type`, `owner_id`) and the Media's role there, and is unique per link
(table `media_attachments`, migration `20260926120000`).

### Status and expiry

A Media carries `status` and `expiresAt`, returned in the Media JSON to
signed-in callers. Archive (`deletedAt`) and purge (`blobPurgedAt`) stay
recorded apart.

| Status | Meaning | `expiresAt` |
|---|---|---|
| `pending` | No Media attachment yet. Every new Media starts here. | Upload time plus the purpose's `pending_ttl` (24 hours for every purpose today). |
| `attached` | At least one Media attachment. Never purged by expiry. | None. |
| `detached` | Its last Media attachment was removed. | 30 days after that. Attaching it again within the window makes it attached. |

**A legacy Media never gets an expiry**: not when it is uploaded (`legacy`
has `pending_ttl` `none`), and not when its last Media attachment is removed.
Skyforms, CMS and superadmin upload without a purpose today, and a legacy
Media may still be used outside core by its address (CMS content stores
addresses) after core stops linking it. The expiry cleanup therefore never
touches a legacy Media; the legacy backfill (ticket 08) reports unused ones to
Yusuf before anything removes them.

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
the restore (a legacy one none), so a window that ran out while it was
archived does not purge it on the next pass.

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
writes it.

### Expiry cleanup

The purge worker, after its archived batch, makes one pass over the Media
whose `expiresAt` has passed: pending Media past their purpose's pending TTL
and detached purposed Media past their 30 days. It walks them by id, 25 at a
time, and purges each blob with the same locked check and two-phase claim as
an archived Media: a Media that a Media attachment or a core link still uses
is kept. The Media is archived as its blob goes, so ordinary reads hide it
and restore answers `410 Gone`. A Media whose blob cannot be deleted is
logged with its id and retried on the next pass; it never holds up the
others. Attached Media, legacy Media (they have no expiry) and archived Media
are never purged by expiry; archived Media keep the archive window above.

### Media stored before Media attachments

The migration gives every Media core already links its Media attachment and
the attached status, including a Media archived after it was linked. A Media
nothing in core links stays `pending` with no expiry: this change purges
nothing that existed before it. All of them are legacy, so removing a core
link from one later sets no expiry either. The legacy backfill (ticket 08)
assigns purposes, reports the orphans to Yusuf, and only then gives them an
expiry. The unused `attached` column of the first media migration is
dropped; `status` replaces it.

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
- `MEDIA_IMAGE_ADDRESS_MODE` — where image sizes point: `stored` (default) or
  `cloudflare`. Any other value stops core at startup.

The detached window is fixed at 30 days by the database;
`MEDIA_BLOB_RECOVERY_DAYS` does not change it.
