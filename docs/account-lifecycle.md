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

Before any erasure or cross-service outbox work can advance, the shared account-access marker must be durably projected to the dedicated Redis and `platform_blocked_at` must be recorded. New outbox rows remain at `available_at = infinity` until that confirmation. The worker claims only confirmed requests and re-asserts the permanent marker before its first side effect. See `docs/account-access-gate.md` for the exact contract and recovery order.

## Service erasure framework

ADR-0051 has this worker send SkyMail, CMS and Forms one Erasure command each; the contract is [`account-erasure-command.md`](account-erasure-command.md). The framework below exists, but its steps are not yet part of the saga above: the next change places them after logout and before core anonymization, and makes identity deletion wait for all three.

**Registry.** A fixed list in code (`internal/erasure`). A new service that stores personal data is not finished until it has an entry here.

| Step | Service | Internal URL setting | Token scope |
|---|---|---|---|
| `erase_skymail` | SkyMail | `ACCOUNT_ERASURE_SKYMAIL_URL` | `account-erase-skymail` |
| `erase_cms` | CMS | `ACCOUNT_ERASURE_CMS_URL` | `account-erase-cms` |
| `erase_forms` | Forms | `ACCOUNT_ERASURE_FORMS_URL` | `account-erase-forms` |

**Configuration.** Nothing is read while `ACCOUNT_ERASURE_WORKER_ENABLED` is off. While it is on, startup requires the three URLs (absolute `http`/`https` base URLs without credentials, query or fragment), `ACCOUNT_ERASURE_CLIENT_ID` (`core-erasure`) and `ACCOUNT_ERASURE_CLIENT_SECRET` (an OpenBao reference), and refuses to start with the missing or malformed variable's name only, never its value. `ACCOUNT_ERASURE_ALERT_AFTER` defaults to `480h` (day 20) and `PERIODIC_DESTRUCTION_INTERVAL` to `2160h` (90 days).

**Step group.** Every pass attempts each service step that has no checkpoint yet; one failing service does not stop the others, but the request advances only when all three are checkpointed. The addresses are read once at the start of a pass and live only in memory for it. A confirmation is checkpointed under the lease fence together with the service's `counts`; a lost lease stops the pass before any further call. Retry codes:

- `erase_<service>_failed`: deferred (`202`, `429`, `5xx`, timeout, connection or token-endpoint failure; the attempt is refunded until the deferral horizon) or ordinary (`401`, an unexpected status or an invalid `200` body; spends an attempt);
- `erase_<service>_rejected_<http>`: `400`, `403`, `404` or `409`; the request goes to `manual_intervention` at once, even while other services in the same pass were deferred;
- `erase_<service>_checkpoint_failed` and `erasure_addresses_failed`: ordinary.

After an operator fixes a rejection, the existing retry path returns the request to `pending`, and checkpointed services are not called again.

**Tokens.** One `client_credentials` cache per service scope, held until 30 seconds before the token's expiry and never longer; a token that lives 30 seconds or less is not cached. Keycloak client secrets rotate nightly (ADR-0050), so core keeps no copy of `ACCOUNT_ERASURE_CLIENT_SECRET`: startup only checks that it is set, and every token request reads it from the configuration again. A `401` from a service drops that token and spends one ordinary attempt; the retry asks for a new token with the secret the configuration holds then. A failure of the token endpoint, including a `401` for a secret that has just rotated, is deferred.

**Watchdog.** While the worker is on, core counts every five minutes and publishes four unlabelled gauges on `/v1/metrics`, starting with the first successful count:

- `skylab_account_erasure_open_requests`: every request that is not `completed`, `manual_intervention` included;
- `skylab_account_erasure_overdue_requests`: open and at least `ACCOUNT_ERASURE_ALERT_AFTER` past `created_at`;
- `skylab_account_erasure_manual_intervention_requests`;
- `skylab_account_erasure_oldest_open_age_seconds`.

A request that becomes overdue or goes to manual intervention gets one log line, `account_erasure_attention request_id=… reason=overdue|manual_intervention step=… code=…`, with no subject. It is written once per request and reason while that state lasts, and once more after a restart. Alarm delivery belongs to ops, outside this repository (`ops/wizards/`): an hourly timer on the server reads these gauges over the internal network and, while `overdue` or `manual_intervention` is above zero, mails `/ADMIN` once a day through the rotator's direct SMTP path (ADR-0050), so the alarm arrives even when SkyMail is the broken service.

**Completion proof.** A completed request (`created_at`, `completed_at`) and its step rows (`completed_at` per step; `counts` on service steps) are the KVKK destruction record. They hold no e-mail and no name. They are kept at least three years (KVKK deletion regulation art. 7(3)): no code path deletes these rows, `TestNoCodePathDeletesErasureProof` guards that, and no foreign key cascades into them from anything but their own request. Since they carry no personal data, they need not be deleted after three years either. The proof query, without the subject:

<!-- erasure-proof-query -->
```sql
SELECT request.id AS request_id,
       request.created_at AS requested_at,
       request.completed_at,
       step.step,
       step.completed_at AS step_completed_at,
       step.counts
FROM account_deletion_requests request
JOIN account_deletion_steps step ON step.request_id = request.id
WHERE request.status = 'completed'
ORDER BY request.completed_at, request.id, step.completed_at, step.step;
```

**Periodic destruction interval.** `PERIODIC_DESTRUCTION_INTERVAL` is the one configured periodic-destruction period (KVKK deletion regulation art. 11): at most six months if the data controller owes a retention and destruction policy, otherwise at most three; 90 days is valid either way. Whether the club or YTÜ is the controller is undecided, so the value is configuration, not code. The backup and log retention caps and every cleanup job this work adds take their period from it. Startup accepts at most 184 days.

## Keycloak and federated users

Disable reads the complete Keycloak user representation, changes only `enabled`, and writes the representation back. This preserves `federationLink` and federated attributes before logout and deletion. Adapter integration tests cover that HTTP contract and idempotent delete retry. Core accepts deletion requests and starts the erasure worker only when `ACCOUNT_ERASURE_WORKER_ENABLED=true`; while it is off the privileged DELETE route returns `503` before changing Core state. The flag is default-off and startup then requires non-empty `KEYCLOAK_URL`, `KEYCLOAK_REALM`, `KEYCLOAK_CLIENT_ID` and `KEYCLOAK_CLIENT_SECRET`. Merely configuring Keycloak for normal identity operations never enables destructive account erasure, and the in-memory development directory can never acknowledge identity-erasure steps.

Account Center uses the separate least-privilege self-service intake and
hash-only status capability documented in
[`account-self-delete.md`](account-self-delete.md). That route derives the
subject only from a matching Account Center access token and freshly
authenticated ID token, preserves the same durable request/outbox/marker
ordering, and never exposes the privileged target-by-ID command.

**Blocking release gate:** before account deletion is enabled in production, run disable, logout, delete, retry-after-404 and subsequent LDAP synchronization/import against a production-clone Keycloak realm connected to the real provider configuration. Record whether the external directory entry is retained, disabled or recreated and obtain the identity-service owner's approval. Repository tests cannot prove that external policy, so a release must not waive this rehearsal on the basis of the adapter tests alone.

## Rollback boundary

Migration `20260925120000` (service erasure steps and `counts`) is forward-only once a service erasure step row exists: that row is completion proof, and its down migration refuses rather than delete it.

Migration `20260920010000` is forward-only after the first deletion request or non-active account. Its down migration locks every subject-link table for the complete preflight/drop transaction, refuses to remove the durable anti-resurrection marker (including after a hard purge), and also fails if detached competitor or media history contains `NULL`; it never deletes or fabricates historical rows to force a rollback. After live deletion traffic, rollback means restoring a pre-migration database backup or shipping a forward repair while keeping the service stopped. Rehearse both migration and backup restore on a production clone before release.
