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
worker refuses to purge media still referenced by an Event cover or gallery,
a User profile, a Certificate template draft, or a published Certificate
template version. Database triggers also reject new durable references to
inactive or purged media.

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

Objects are public at `https://cdn.yildizskylab.com/<key>`, so the metadata a
Media is stored with decides what a browser does with it. Every write to the
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
| `image` | Raster handling: `reencode`, `max_dimension`, `variants` (name → px), `rasterize_svg`. |
| `legacy_rules` | Only on `legacy`: the rules below instead of `types` and `max_mib`. |

`scan`, `pending_ttl` and `image` are declared now; the cleanup worker,
re-encoding, variants and scanning read them as they ship. An unknown field or
value, a missing `legacy` entry, or a ceiling violation stops core at startup.

The initial entries:

| Purpose | Upload | Types | Max | Visibility | Transport |
|---|---|---|---|---|---|
| `profile_picture` | authenticated | JPEG, PNG, WebP, GIF | 5 MiB | public | single-step |
| `event_cover`, `event_gallery` | event_editor | JPEG, PNG, WebP, GIF | 10 MiB | public | single-step |
| `certificate_asset` | certificate_template | PNG, JPEG, PDF | 20 MiB | private | single-step |
| `cms_image` | authenticated | JPEG, PNG, WebP, GIF | 10 MiB | public | single-step |
| `cms_file` | authenticated | PDF | 20 MiB | public | single-step |
| `answer_file` | authenticated | PDF, JPEG, PNG, DOCX | 20 MiB | private, scanned | single-step |
| `club_file` | event_editor | PDF | 1 GiB | public, scanned | direct |
| `answer_file_large` | service_only | ZIP, PDF | 1 GiB | private, scanned | direct |
| `video` | event_editor | MP4 | 2 GiB | public | direct |
| `legacy` | authenticated | legacy rules | legacy rules | public | single-step |

SVG joins `cms_image`, rasterized to PNG, once core rasterizes SVG.

### Upload rules

`upload` names one rule of the authz vocabulary (`authz.MediaUploader`). Every
rule needs a signed-in caller.

- `authenticated`: any signed-in person.
- `event_editor`: someone who may create Events for at least one Owner team:
  a privileged person (ADMIN, YK, DK), a leader or coordinator of any team, or
  a member of a team whose Event permissions let members create Events
  (GECEKODU). Which Event the Media ends up on is checked when it is linked.
- `certificate_template`: someone who may create certificate templates for at
  least one Owner team: a privileged person, a leader or coordinator of any
  team, or a team member with `certificate:template:manage`.
- `service_only`: no person. Only the owning product's service identity may
  start such an upload; until that path exists nobody can.

CMS editor roles live on the CMS client, which core does not see, so the CMS
purposes are `authenticated` for now, as purpose-less uploads are.

### Hard ceilings

`internal/media/ceilings.go` holds limits no catalogue entry can loosen.
Core refuses to start with a catalogue that breaks one:

- a public purpose accepts only raster images (JPEG, PNG, WebP, GIF), PDF and
  MP4;
- a public purpose re-encodes its raster images (`image.reencode`);
- SVG is never stored as SVG: a purpose naming it rasterizes it to PNG
  (`image.rasterize_svg`);
- the maximum size stays under 20 MiB for single-step uploads and 2 GiB for
  Direct upload;
- images stay within 2560 px (`image.max_dimension`, variants);
- a private purpose is encrypted.

### Uploading

`POST /v1/media` takes an optional multipart field `purpose`. With a purpose,
core checks, in order: the purpose exists, the caller may upload it, it is not
private while private Media is off, it is single-step, the size, and the type
detected from the content. `POST /v1/users/me/profile-picture` always uploads
as `profile_picture`: raster images up to 5 MiB, no PDF, no SVG.

Without a purpose the upload is `legacy` and keeps the rules purpose-less
uploads have had: a raster image or SVG up to 10 MiB, a PDF under a `.pdf`
name up to 20 MiB, any other named file up to 20 MiB (served as a download
by the serving policy), refused with a plain `400` without a code. Once
Skyforms and CMS send purposes, `legacy` falls to the strict rule (raster
only, 5 MiB, 24 hours unless attached) by a catalogue change.

Refusals are `application/problem+json` with a stable `code` and extension
members:

| Status | `code` | Extra members | When |
|---|---|---|---|
| 400 | `purpose-unknown` | `purpose` | The purpose is not in the catalogue. |
| 403 | `purpose-forbidden` | `purpose` | The caller's upload rule does not allow it. |
| 503 | `private-media-disabled` | `purpose` | A private purpose while `MEDIA_PRIVATE_ENABLED` is off. Nothing is stored. |
| 400 | `purpose-requires-direct-upload` | `purpose` | A `direct` purpose sent to `POST /v1/media`. |
| 413 | `media-too-large` | `purpose`, `maxBytes` | Above the purpose's maximum. |
| 415 | `media-type-not-allowed` | `purpose`, `allowedTypes` | The content is not one of the purpose's types. |

A body above the server's limit (20 MiB plus room for the form) is still
refused by the HTTP server with a bare `413` before any purpose is read.

## Configuration

- `MEDIA_BLOB_RECOVERY_DAYS` — recovery window in whole days; default `30`.
- `MEDIA_BLOB_PURGE_INTERVAL` — Go duration between bounded runs; default `1h`.
- `MEDIA_BLOB_PURGE_BATCH_SIZE` — maximum records per run; default `25`.
- `MEDIA_UPLOAD_STAGING_GRACE` — delay before abandoned uploads are eligible;
  minimum `2m`, default `24h`.
- `MEDIA_UPLOAD_STAGING_SWEEP_INTERVAL` — retry sweep interval; default `15m`.
- `MEDIA_UPLOAD_STAGING_BATCH_SIZE` — maximum staging intents per run; default
  `25`.
- `MEDIA_PRIVATE_ENABLED` — private Media purposes; default `false`, which
  refuses them with `private-media-disabled`. This build has no private Media
  storage, so `true` stops core at startup.
