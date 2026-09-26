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
| `attach` | Who attaches the purpose's Media: `core` (a core record links it) or `service` (another product, through the [service attach API](#service-attach-api)). |
| `service` | Only with `attach: service`: the product that attaches the purpose's Media, `forms` or `cms`. It is also the purpose's owning product: only that product may link the purpose's Media at all. |
| `image` | Raster handling: `reencode`, `max_dimension`, `variants` (name → px), `rasterize_svg`. |
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
Answer file purposes) must all be in the file. `scan` and `image` are declared now
and not yet acted on: scanning and re-encoding with variants (media redesign
ticket 04) read them as they ship. Until re-encoding ships, a raster image
under any purpose gets the same metadata stripping as before (EXIF, XMP and
comments removed), not a re-encode. An unknown field or
value, a missing `legacy` entry, or a ceiling violation stops core at startup.

The initial entries:

| Purpose | Upload | Types | Max | Visibility | Transport | Attached by |
|---|---|---|---|---|---|---|
| `profile_picture` | authenticated | JPEG, PNG, WebP, GIF | 5 MiB | public | single-step | core |
| `event_cover`, `event_gallery` | event_editor | JPEG, PNG, WebP, GIF | 10 MiB | public | single-step | core |
| `certificate_asset` | certificate_template_editor | PNG, JPEG, PDF | 20 MiB | private | single-step | core |
| `cms_image` | authenticated | JPEG, PNG, WebP, GIF | 10 MiB | public | single-step | cms (no service client yet) |
| `cms_file` | authenticated | PDF | 20 MiB | public | single-step | cms (no service client yet) |
| `answer_file` | authenticated | PDF, JPEG, PNG, DOCX | 20 MiB | private, scanned | single-step | forms |
| `club_file` | event_editor | PDF | 1 GiB | public, scanned | direct | not settled (ticket 11) |
| `answer_file_large` | service_only | ZIP, PDF | 1 GiB | private, scanned | direct | forms |
| `video` | event_editor | MP4 | 2 GiB | public | direct | not settled (ticket 13) |
| `legacy` | authenticated | legacy rules | legacy rules | public | single-step | core, or a product for its uploader or once it holds it (transition rule) |

SVG joins `cms_image`, rasterized to PNG, once core rasterizes SVG.

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
  (`image.reencode`). The declaration is enforced now; the re-encoding
  itself arrives with ticket 04, and until then these images are only
  stripped of metadata;
- SVG is never stored as SVG: a purpose naming it rasterizes it to PNG
  (`image.rasterize_svg`);
- the maximum size stays under 20 MiB for single-step uploads and 2 GiB for
  Direct upload;
- the declared image size stays within 2560 px (`image.max_dimension`,
  variants), applied when re-encoding ships;
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
| 413 | `media_too_large` | `purpose`, `maxBytes` | Above the purpose's maximum. |
| 415 | `media_type_not_allowed` | `purpose`, `allowedTypes` | The content is not one of the purpose's types. |

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
`media_purpose_fits_role`, a copy of the role table that a test keeps equal
to `rolePurposes`). The link rules read the Media before the write and
outside its transaction, so the legacy backfill could give a legacy Media a
purpose in between; the trigger reads the Media under the lock the foreign
key takes anyway, which waits for the backfill and never for the status
trigger. Such a link is refused like a Media that cannot be linked (a
service attach answers `media_not_linkable`; retrying it gets the ordinary
purpose check), never written mismatched.

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
  file or an Event cover.
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
  are; nothing is re-encoded or moved. The backfill sets the Media's detach
  expiry hold in the same statement (below).
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
- Nothing else reads the hold. An archived held Media follows the archive
  window, and account erasure purges a held profile picture at once like any
  other.

`core-backend media-legacy-release-hold` ends the hold, after stage 5 (ticket
18). **It is not run yet.** Without `-apply` it only counts the held Media and
those of them no record uses. With `-apply` it walks the held Media by id, 25
at a time, clears each one's hold, and gives the ones no record uses by then
their 30 days **from the release**, not from when they were detached. A Media
that fails is named on standard error and stays held; a second run releases
only what is left, and a run over released Media changes nothing. It prints
counts only, to standard error, and exits non-zero if a Media failed or the
run was interrupted (with the counts so far).

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
the core links without a Media attachment, and the Media whose detach expiry
is still held. Columns:

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

- `MEDIA_SERVICE_CLIENTS` — the products' service clients for the
  [service attach API](#service-attach-api), `product:client` pairs
  separated by commas (products `forms`, `cms`), or `none`; default
  `forms:forms`. Startup fails on anything else. A product with no client
  cannot attach, and its purposes cannot be uploaded.

The detached window is fixed at 30 days by the database;
`MEDIA_BLOB_RECOVERY_DAYS` does not change it.
