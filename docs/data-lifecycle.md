# Core data lifecycle API

Durable business records are never physically removed by the public API. Existing `DELETE` routes remain compatible, return `204 No Content`, and perform an idempotent domain transition. Repeating the same request is safe.

Normal and public reads return only current records. Management list routes accept `lifecycle=current|inactive|all`; `inactive` and `all` require the same authority as the corresponding archive action. An invalid value returns `400 Bad Request`.

| Record | Operator action | Compatibility route | Restore route |
| --- | --- | --- | --- |
| Event | Arşivle | `DELETE /v1/events/{id}` | `POST /v1/events/{id}/restore` |
| Event day | Arşivle | `DELETE /v1/event-days/{id}` | `POST /v1/event-days/{id}/restore` |
| Session draft | Arşivle | `DELETE /v1/sessions/{id}` | `POST /v1/sessions/{id}/restore` |
| Season | Arşivle | `DELETE /v1/seasons/{id}` | `POST /v1/seasons/{id}/restore` |
| Competitor | Yarışmadan çek | `DELETE /v1/competitors/{id}` | `POST /v1/competitors/{id}/reinstate` |
| Media | Arşivle | `DELETE /v1/media/{id}` | `POST /v1/media/{id}/restore` |
| Short URL | Devre dışı bırak | `DELETE /v1/urls/{id}` | `POST /v1/urls/{id}/restore` |

Lifecycle filters are supported by these management lists:

- `GET /v1/events?lifecycle=...`
- `GET /v1/events/{eventId}/days?lifecycle=...`
- `GET /v1/event-days/{id}/sessions?lifecycle=...`
- `GET /v1/seasons?lifecycle=...`
- `GET /v1/competitors?lifecycle=...`
- `GET /v1/media?lifecycle=...`
- `GET /v1/urls?lifecycle=...`
- `GET /v1/urls/all?lifecycle=...`

Restore revalidates authorization and aggregate invariants. An EventDay cannot be restored under an archived Event. A Session cannot be restored under an archived Event or EventDay and must still have a valid schedule. A Competitor cannot be reinstated into an archived or inactive Event. These conflicts return `409 Conflict`.

Archiving an Event hides the Event and its schedule from ordinary reads without changing child rows. Tickets, check-ins, certificates, EventDays, Sessions and event-media relationships remain intact. Disabling a short URL makes its redirect and QR unavailable while preserving hit history; the same alias remains reserved and history becomes available again after restore.

Archived Media metadata is hidden immediately, while its R2 object remains recoverable for 30 days. A bounded background sweep may purge the object after that window only when no Media attachment and no retained Event cover/gallery, user profile or certificate-template draft/version uses it. The same sweep also purges, without an earlier archive, the object of a Media no Media attachment keeps once its expiry passes: a purposed upload never attached within its purpose's window (24 hours), or a purposed Media 30 days after its last Media attachment was removed. Such a Media is archived as its object goes. Legacy Media (uploaded without a purpose, or stored before purposes) get no expiry by themselves, and neither do the Media the legacy backfill gave a purpose while their hold lasts (decision K2). After stage 5 (media redesign ticket 18) two commands change that: one starts the 30 days of the legacy orphans Yusuf reviewed, the other releases the hold and starts the 30 days of the held Media no record uses by then. See [`media-lifecycle.md`](media-lifecycle.md#media-attachment) and its [Legacy Media](media-lifecycle.md#legacy-media) section. Purge uses a durable in-progress state, blocks restore once removal starts and safely retries storage failures. Restore after completed purge returns `410 Gone`; there is no public force-purge route. A person removing their own profile picture (`DELETE /v1/users/me/profile-picture`) archives their own upload under the same rules and unlinks it; that route has no restore, the person uploads again.

A Media whose purpose needs a malware scan waits `scanning` until the scan ends and, while nothing keeps it, expires like a pending one: the same sweep purges it if its window passes first. A Media the scan rejects (`rejected`, as `infected`, `too_large_to_scan`, `archive_invalid`, `archive_nested`, `lost`, `integrity`, or `scan_timeout` once it has waited a week) has its object deleted at once, without an archive or a recovery window; its record stays, not archived, so its owning product can tell the uploader why, and restore answers `410 Gone`. Every rejection is kept in `media_scan_rejections` (Media id, result, the signature clamd named, time; no file name, no person) with the Media's record, which is never deleted. See [`media-lifecycle.md`](media-lifecycle.md#malware-scan).

Production defaults are `MEDIA_BLOB_RECOVERY_DAYS=30`, `MEDIA_BLOB_PURGE_INTERVAL=1h` and `MEDIA_BLOB_PURGE_BATCH_SIZE=25`. An invalid or non-positive override prevents startup instead of silently disabling retention.

The access log of private Media (who got a read link for which private Media, for whom, and every open with the opener's address; see [`media-lifecycle.md`](media-lifecycle.md#access-log)) is kept one year (decision G2). An hourly cleanup physically deletes the links issued more than a year ago, with their opens, and any open older than a year, in batches; a failed run is retried on the next one. A person's account erasure does not touch these rows: they are the access audit record and stay until their year is up, and a link can no longer be issued for a person being erased. With the retention sweep in apply mode the record is kept three years and an open's address is emptied after one ([`retention-sweep.md`](retention-sweep.md)).

The periodic destruction run (`RETENTION_SWEEP_MODE`, ADR-0062, [`retention-sweep.md`](retention-sweep.md)) empties, once a day, the personal fields of what is past its retention period without deleting the record: a guest's phone 90 days after the Event, a guest's name and address (and their certificate's address) two years after their latest Event, a short-link click's address, user agent, account and full referer after a year (the click row stays, which in apply mode replaces the 90-day deletion of click rows), an open's address after a year. It deletes an Event's door staff 90 days after the Event, a relationship row. Tickets, check-ins and certificates (name, serial, PDF) stay; archiving an Event does not stop the clock.

Certificates are revoked or reissued through their existing commands and are never deleted. Event gallery membership, team/group membership and role assignment are relationship operations rather than durable aggregate deletion, so their existing removal routes remain physical relationship changes. Media attachment rows are link rows of the same kind: `DELETE /v1/media/{id}/attachments/{attachmentId}` (another product's service account removing its own Media attachment) hard-deletes the row, and core's own links remove theirs with the link. The Media itself is not deleted: it follows its own lifecycle (detached, then purged 30 days after its last Media attachment unless something attaches it again; see [`media-lifecycle.md`](media-lifecycle.md#service-attach-api)).

User identities follow the separate irreversible account lifecycle described in [account-lifecycle.md](account-lifecycle.md). The privileged user DELETE route queues that lifecycle and never physically deletes operational history.

Account erasure purges a person's personal-purpose Media at once, whatever their malware scan state, and keeps the club content they uploaded without their uploader and file name ([`media-lifecycle.md`](media-lifecycle.md#account-erasure)). Every upload of theirs but a current profile picture no one else uses (that one is `profile_media_id`'s) is listed, by id only, in `account_deletion_media` (request id, Media id; migration `20260927100000`) from `anonymize_core` until `erase_profile_media` has handled it. Each row goes with its own Media: in the transaction that records the purge of the person's own, or once club content is kept and its stored object no longer names the person's file. A rerun therefore gets only what is left. A row whose Media is never handled stays as the request's retry state while the request is pending or in manual intervention: nothing else leads back to that Media, since its uploader is already cleared. So a request cannot complete while any row is left: `erase_profile_media` is not checkpointed, and the database refuses the change to `completed` (`account erasure has recorded uploads left to erase`), so the worker retries and a lasting failure ends in manual intervention and the erasure alarm. The completion proof that is kept for three years therefore never carries the list, and no upload is left behind unseen. The down migration refuses while any row is left.
