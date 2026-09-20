# Core account lifecycle

User removal is an irreversible lifecycle command, not a synchronous row delete. The authenticated Core API keeps the existing privileged `DELETE /v1/users/{id}` compatibility route, but that route now creates one durable deletion request and changes the local account from `active` to `deletion_pending`. Repeating the command returns the same request and does not create another outbox event.

## States and access guard

- `active`: JIT identity synchronization and profile changes are allowed.
- `deletion_pending`: new and previously issued JWTs cannot repopulate or update the Core user.
- `anonymized`: only the non-PII tombstone and durable deletion marker remain.

The JIT upsert checks both `users.account_state` and `account_deletion_requests`. The latter remains after an internal hard purge, so a subject from an old valid JWT cannot recreate its user row.

Public short-link attribution requires a current active Core identity and performs the deletion-marker check inside the hit transaction. It shares a transaction-scoped subject lock with deletion-request creation, so an in-flight redirect either commits before deletion and is scrubbed by anonymization, or observes the marker and records an anonymous hit. Valid JWT subjects that have no active Core row are also recorded anonymously.

PostgreSQL applies that same lock-and-marker invariant to every durable current-identity link, even when SQL bypasses the Go services. Ticket owners, competitors, media uploaders, certificate owners, URL creators/hit attribution and event door staff can reference only an active subject with no deletion marker. Audit actors use the same rule for lifecycle archivers/deleters/withdrawers, certificate template publishers and binding/finalization/batch operations, and deletion-request requesters. A writer that commits first is observed and detached by anonymization; a writer behind the deletion marker is rejected atomically. `NULL` history remains valid and detached competitors cannot be reinstated.

## Durable erasure flow

`account_deletion_requests` stores queue state without an e-mail, name or other erased profile data. `account_deletion_steps` checkpoints these idempotent steps:

1. disable the Keycloak identity;
2. log out all Keycloak sessions;
3. anonymize Core PII and detach historical identity links;
4. erase an unreferenced profile-picture blob immediately;
5. wait for and erase every durable staged upload owned by the subject;
6. delete the Keycloak identity.

The worker leases one request at a time. Every claim receives a new random fence token; step checkpoints, retry transitions and request completion succeed only for the current token. An expired worker therefore cannot overwrite a newer claim. A crashed or uncertain external call is safe to retry: completed steps are skipped and Keycloak `404 Not Found` is treated as the desired result. Exhausting the retry budget changes the request to `manual_intervention`; it is no longer claimed automatically and retains only a stable error code. `account_deletion_outbox` contains one `account.deletion_requested` event per request and only opaque UUID references. Core intentionally produces but does not publish or acknowledge these rows; the Account Center cross-service orchestrator owns that consumer contract in its erasure-saga work.

Core anonymization clears the user profile, student-card UID, contact fields and profile-media link. It stores the opaque profile-media UUID only as temporary retry state on the deletion request, removes the original filename and uploader, and marks the media deleted when no event, gallery, other profile or certificate template still references it. The dedicated erasure step bypasses the normal media recovery window and asks the existing locked, reference-aware blob purger to delete those bytes immediately. A `false, nil` purge result is not success: restored, current or newly referenced media keeps the retry linkage and eventually requires manual intervention unless it becomes purgeable. Its successful step checkpoint and removal of the temporary UUID happen in one fenced SQL statement: a lost checkpoint retains the UUID for retry, while a completed request retains no subject-to-profile-asset association. Subject-bound upload intents are checked in a separate checkpoint before identity deletion; an active upload lease or failed R2 compensation keeps the request retryable and prevents false completion. These durable deferrals schedule their next claim no later than the configured staging grace plus a 24-hour recovery horizon and atomically refund the current claim's failure attempt under the lease-token fence. Legitimate waits therefore do not poison the later Keycloak/database failure budget. Shared event/certificate media keeps its blob and current lifecycle state but loses profile metadata PII. Ticket owners, media uploaders, URL actors and door-staff membership are detached; a subject's URL-hit IP, user agent and referer are cleared at the same time, and detached competitors are withdrawn from current leaderboards. Certificate owner and recipient e-mail are cleared, while recipient name, serial, PDF/artifact fields and verification history remain intact.

The physical purge primitive is intentionally absent from the HTTP and service interfaces. Its concrete PostgreSQL method only removes an already-anonymized row whose deletion request is completed. The durable request remains as the anti-resurrection marker.

## Keycloak and federated users

Disable reads the complete Keycloak user representation, changes only `enabled`, and writes the representation back. This preserves `federationLink` and federated attributes before logout and deletion. Adapter integration tests cover that HTTP contract and idempotent delete retry. Core accepts deletion requests and starts the erasure worker only when `ACCOUNT_ERASURE_WORKER_ENABLED=true`; while it is off the privileged DELETE route returns `503` before changing Core state. The flag is default-off and startup then requires non-empty `KEYCLOAK_URL`, `KEYCLOAK_REALM`, `KEYCLOAK_CLIENT_ID` and `KEYCLOAK_CLIENT_SECRET`. Merely configuring Keycloak for normal identity operations never enables destructive account erasure, and the in-memory development directory can never acknowledge identity-erasure steps.

**Blocking release gate:** before account deletion is enabled in production, run disable, logout, delete, retry-after-404 and subsequent LDAP synchronization/import against a production-clone Keycloak realm connected to the real provider configuration. Record whether the external directory entry is retained, disabled or recreated and obtain the identity-service owner's approval. Repository tests cannot prove that external policy, so a release must not waive this rehearsal on the basis of the adapter tests alone.

## Rollback boundary

Migration `20260920010000` is forward-only after the first deletion request or non-active account. Its down migration locks every subject-link table for the complete preflight/drop transaction, refuses to remove the durable anti-resurrection marker (including after a hard purge), and also fails if detached competitor or media history contains `NULL`; it never deletes or fabricates historical rows to force a rollback. After live deletion traffic, rollback means restoring a pre-migration database backup or shipping a forward repair while keeping the service stopped. Rehearse both migration and backup restore on a production clone before release.
