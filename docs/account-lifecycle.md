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

`account_deletion_requests` stores queue state without an e-mail, name or other erased profile data. `account_deletion_steps` checkpoints these idempotent steps, in this order (ADR-0051):

1. `disable_identity`: disable the Keycloak identity;
2. `logout_sessions`: log out all Keycloak sessions;
3. `erase_skymail`: send SkyMail the [Erasure command](account-erasure-command.md);
4. `erase_cms`: send CMS the Erasure command;
5. `erase_forms`: send Forms the Erasure command (steps 3–5 are the service erasure steps below; every pass tries each unfinished one);
6. `anonymize_core`: anonymize Core PII, detach historical identity links and clear core's guest data of the person's addresses;
7. `erase_profile_media`: erase an unreferenced profile-picture blob immediately;
8. `erase_staged_uploads`: wait for and erase every durable staged upload owned by the subject;
9. `delete_identity`: delete the Keycloak identity.

Steps run in this order and a failure ends the pass, so a step runs only once every step before it is checkpointed: `anonymize_core` waits for all three services, and `delete_identity` for steps 1–8. While a service is unavailable the person is disabled and logged out and holds no session, core keeps their row and Keycloak keeps the disabled user; only the erasure is late. A request is `completed` only after all nine checkpoints.

The worker leases one request at a time. Every claim receives a new random fence token; step checkpoints, retry transitions and request completion succeed only for the current token. An expired worker therefore cannot overwrite a newer claim. A crashed or uncertain external call is safe to retry: completed steps are skipped, Keycloak `404 Not Found` is treated as the desired result, and a service answers a repeated command from its receipt. Exhausting the retry budget changes the request to `manual_intervention`; it is no longer claimed automatically and retains only a stable error code.

`account_deletion_outbox` contains one `account.deletion_requested` event per request and only opaque UUID references. It is a producer-only record and audit trail: nothing consumes it and it never carries an address. Erasure in the other services is run by this worker itself, through the service erasure steps (ADR-0051), not by a consumer of the outbox.

Core anonymization clears the user profile, student-card UID, contact fields and profile-media link. It stores the opaque profile-media UUID only as temporary retry state on the deletion request, removes the original filename and uploader, and marks the media deleted when no event, gallery, other profile or certificate template still references it. The dedicated erasure step bypasses the normal media recovery window and asks the existing locked, reference-aware blob purger to delete those bytes immediately. A `false, nil` purge result is not success: restored, current or newly referenced media keeps the retry linkage and eventually requires manual intervention unless it becomes purgeable. Its successful step checkpoint and removal of the temporary UUID happen in one fenced SQL statement: a lost checkpoint retains the UUID for retry, while a completed request retains no subject-to-profile-asset association. Subject-bound upload intents are checked in a separate checkpoint before identity deletion; an active upload lease or failed R2 compensation keeps the request retryable and prevents false completion. These durable deferrals schedule their next claim no later than the configured staging grace plus a 24-hour recovery horizon and atomically refund the current claim's failure attempt under the lease-token fence. Legitimate waits therefore do not poison the later Keycloak/database failure budget. Shared event/certificate media keeps its blob and current lifecycle state but loses profile metadata PII. Ticket owners, media uploaders, URL actors and door-staff membership are detached; a subject's URL-hit IP, user agent and referer are cleared at the same time, and detached competitors are withdrawn from current leaderboards. Certificate owner and recipient e-mail are cleared, while recipient name, serial, PDF/artifact fields and verification history remain intact.

**Guest data.** Core also holds guest data keyed by e-mail alone, which no subject link reaches. In the same transaction, before it clears the `users` row, anonymization reads the row's `email` and `school_email` under the row lock, adds the pass's addresses (see **Addresses** below), and clears:

| Table | Rows | Change |
|---|---|---|
| `tickets` | `owner_id IS NULL` and the guest e-mail is one of the addresses | `guest_first_name`, `guest_last_name`, `guest_email` and `guest_phone_number` become `''`; the ticket and its check-ins stay |
| `certificates` | `owner_id IS NULL` and the recipient e-mail is one of the addresses | `recipient_email` becomes `''`; recipient name, serial and PDF stay, as on owned certificates |

The match ignores case and surrounding spaces. Other guests' rows are untouched.

The physical purge primitive is intentionally absent from the HTTP and service interfaces. Its concrete PostgreSQL method only removes an already-anonymized row whose deletion request is completed. The durable request remains as the anti-resurrection marker.

Before any erasure step can advance, in core or in another service, the shared account-access marker must be durably projected to the dedicated Redis and `platform_blocked_at` must be recorded. New outbox rows remain at `available_at = infinity` until that confirmation. The worker claims only confirmed requests and re-asserts the permanent marker before its first side effect. See `docs/account-access-gate.md` for the exact contract and recovery order.

## Service erasure steps

ADR-0051 has this worker send SkyMail, CMS and Forms one Erasure command each, as steps 3–5 of the saga above; the contract is [`account-erasure-command.md`](account-erasure-command.md).

**Registry.** A fixed list in code (`internal/erasure`). A new service that stores personal data is not finished until it has an entry here.

| Step | Service | Internal URL setting | Token scope |
|---|---|---|---|
| `erase_skymail` | SkyMail | `ACCOUNT_ERASURE_SKYMAIL_URL` | `account-erase-skymail` |
| `erase_cms` | CMS | `ACCOUNT_ERASURE_CMS_URL` | `account-erase-cms` |
| `erase_forms` | Forms | `ACCOUNT_ERASURE_FORMS_URL` | `account-erase-forms` |

**Configuration.** Nothing is read while `ACCOUNT_ERASURE_WORKER_ENABLED` is off, and nothing of the service steps is built. While it is on, startup requires the three URLs (absolute `http`/`https` base URLs without credentials, query or fragment), `ACCOUNT_ERASURE_CLIENT_ID` (`core-erasure`) and `ACCOUNT_ERASURE_CLIENT_SECRET` (an OpenBao reference), and refuses to start with the missing or malformed variable's name only, never its value. `ACCOUNT_ERASURE_ALERT_AFTER` defaults to `480h` (day 20) and `PERIODIC_DESTRUCTION_INTERVAL` to `2160h` (90 days).

**Forms.** Forms' erase endpoint waits on Fatih's decisions (spec §3.3), and ADR-0051 does not enable erasure before it ships. There is no switch that leaves one service out: the Forms step is in the saga like the other two, and a request is never completed without Forms' confirmation.

- While Forms has no endpoint, `ACCOUNT_ERASURE_FORMS_URL` has nothing to point at, so the worker cannot be enabled: startup refuses with the variable's name.
- A URL that points at a Forms without the endpoint gets `404`: `erase_forms_rejected_404`, manual intervention, before core is anonymized.
- A worker built without a sender for a registry entry refuses the request with `erase_<service>_not_configured` (manual intervention) before it calls any service.

**Addresses.** Read the first time a pass needs them, by a service step or by `anonymize_core`, and handed to both; the next pass reads them again.

- **Sources:** Keycloak's user representation, read-only (`email`, `attributes.schoolEmail`, `attributes.personalEmail`; the user is disabled but present), and core's `users.email` and `users.school_email`.
- **Shape:** trimmed, lower-cased, blanks and repeats dropped, at most three.
- **Nowhere durable:** they live in memory for the pass and go only into the command body and `anonymize_core`'s transaction. They are not written to the outbox, a table, a log, an error message or a metric.
- **Keycloak unreachable:** `erasure_addresses_failed`. The pass calls no service and leaves core untouched; it never goes on with core's row alone.
- **Keycloak `404`:** the user is unexpectedly gone, and core's row is used.
- **More than three distinct addresses:** `erasure_addresses_failed` rather than a cut list, since an address left out would keep its data. The person's earlier addresses are stored nowhere and cannot be found.

**Step group.** Every pass attempts each service step that has no checkpoint yet; one failing service does not stop the others, but the request advances only when all three are checkpointed. A confirmation is checkpointed under the lease fence together with the service's `counts`; a lost lease stops the pass before any further call. Retry codes:

- `erase_<service>_failed`: deferred (`202`, `429`, `5xx`, timeout, connection or token-endpoint failure; the attempt is refunded until the deferral horizon) or ordinary (`401`, an unexpected status or an invalid `200` body; spends an attempt);
- `erase_<service>_rejected_<http>`: `400`, `403`, `404` or `409`; the request goes to `manual_intervention` at once, even while other services in the same pass were deferred;
- `erase_<service>_not_configured`: no sender for that service; `manual_intervention` at once;
- `erase_<service>_checkpoint_failed` and `erasure_addresses_failed`: ordinary.

After an operator fixes a rejection, the existing retry path returns the request to `pending`, and checkpointed services are not called again.

Every failed pass logs one line, `account erasure worker: account erasure <code> request_id=…: <reason>`. The code names the step; the line carries no subject, address or name.

**Tokens.** One `client_credentials` cache per service scope (`scope=openid account-erase-<service>`), so each token carries only its own service's audience and erase role. It is held until 30 seconds before the token's expiry and never longer; a token that lives 30 seconds or less is not cached. Keycloak client secrets rotate nightly (ADR-0050), so core keeps no copy of `ACCOUNT_ERASURE_CLIENT_SECRET`: startup only checks that it is set, and every token request reads it from the configuration again. A `401` from a service drops that token and spends one ordinary attempt; the retry asks for a new token with the secret the configuration holds then. A `403` drops the token too: the request goes to `manual_intervention`, and the retry after the operator's fix asks for a new token instead of reusing the refused one. A failure of the token endpoint, including a `401` for a secret that has just rotated, is deferred.

**Watchdog.** While the worker is on, core counts every five minutes and publishes four unlabelled gauges on `/v1/metrics`, starting with the first successful count:

- `skylab_account_erasure_open_requests`: every request that is not `completed`, `manual_intervention` included;
- `skylab_account_erasure_overdue_requests`: open and at least `ACCOUNT_ERASURE_ALERT_AFTER` past `created_at`;
- `skylab_account_erasure_manual_intervention_requests`;
- `skylab_account_erasure_oldest_open_age_seconds`.

A request that becomes overdue or goes to manual intervention gets one log line, `account_erasure_attention request_id=… reason=overdue|manual_intervention step=… code=…`, with no subject. It is written once per request and reason while that state lasts, and once more after a restart. Alarm delivery belongs to ops, outside this repository (`ops/wizards/erasure/erasure-alarm.sh`, installed as `skylab-erasure-alarm` with the hourly systemd timer in `ops/wizards/erasure/systemd/`): it reads these gauges from inside core's container over loopback, never through Traefik, and while `overdue` or `manual_intervention` is above zero it mails `/ADMIN` through the rotator's direct SMTP path (ADR-0050), so the alarm arrives even when SkyMail is the broken service. It mails when the state starts, again at once when either count rises, and once a day while the state lasts; it stays silent while the gauges are absent.

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

Disable reads the complete Keycloak user representation, changes only `enabled`, and writes the representation back. This preserves `federationLink` and federated attributes before logout and deletion. The address read of the service steps and `anonymize_core` is a plain `GET` of the same representation and changes nothing. Adapter integration tests cover that HTTP contract and idempotent delete retry. Core accepts deletion requests and starts the erasure worker only when `ACCOUNT_ERASURE_WORKER_ENABLED=true`; while it is off the privileged DELETE route returns `503` before changing Core state. The flag is default-off and startup then requires non-empty `KEYCLOAK_URL`, `KEYCLOAK_REALM`, `KEYCLOAK_CLIENT_ID` and `KEYCLOAK_CLIENT_SECRET`. Merely configuring Keycloak for normal identity operations never enables destructive account erasure, and the in-memory development directory can never acknowledge identity-erasure steps.

Account Center uses the separate least-privilege self-service intake and
hash-only status capability documented in
[`account-self-delete.md`](account-self-delete.md). That route derives the
subject only from a matching Account Center access token and freshly
authenticated ID token, preserves the same durable request/outbox/marker
ordering, and never exposes the privileged target-by-ID command.

**Blocking release gate.** The realm has no LDAP or other user-storage federation: 0 components and 0 `federation_link` (checked 2026-09-24). YTÜ sign-in goes through the `OBS` identity provider, which links an identity and imports no directory. A deleted person who signs in again with YTÜ Microsoft therefore gets a new, empty account with a new `sub`. Nothing links it to the old account, and the old marker does not block the new `sub` (Yusuf, 2026-09-24). No production clone is built. Before account deletion is enabled in production:

1. The local full harness proves disable, logout, delete and the retry after `404` against a real Keycloak, inside the nine-step saga with its three service checkpoints (ADR-0051; account-erasure ticket 10).
2. In the same harness, a deleted person signs in again through a fake `OBS` identity provider. The record shows a new `sub`, no block from the old marker and no link to the old data (ticket 10).
3. The old-JWT matrix then runs in production with a throwaway test account, and the identity-service owner approves in writing, the `OBS` behaviour above included (ticket 11).

Repository tests cannot prove this, so a release must not waive it on the basis of the adapter tests alone.

## Rollback boundary

Migration `20260925120000` (service erasure steps and `counts`) is forward-only once a service erasure step row exists: that row is completion proof, and its down migration refuses rather than delete it.

Migration `20260920010000` is forward-only after the first deletion request or non-active account. Its down migration locks every subject-link table for the complete preflight/drop transaction, refuses to remove the durable anti-resurrection marker (including after a hard purge), and also fails if detached competitor or media history contains `NULL`; it never deletes or fabricates historical rows to force a rollback. After live deletion traffic, rollback means restoring a pre-migration database backup or shipping a forward repair while keeping the service stopped. Rehearse both the migration and a backup restore in the local full harness before release (account-erasure ticket 10); no production clone is built.
