# Account erasure command

This is the canonical contract of the **Erasure command**: core's instruction to one service to erase one person's data for one deletion request (ADR-0051). SkyMail, CMS and Forms implement it and link here. Core's side (saga order, where the addresses come from, registry, configuration, retries, watchdog, completion proof) is in [`account-lifecycle.md`](account-lifecycle.md#service-erasure-steps).

## 1. Endpoint

`PUT {SERVICE_INTERNAL_URL}/internal/v1/account-erasures/{request_id}`

- **Path:** the same in every service, at the service root, independent of its public API prefix.
- **Network:** the call comes over the internal Docker DNS (ADR-0016). There is one server, so the traffic is plain HTTP on the internal network.
- **Public ingress:** a request that arrives through Traefik gets `404`. The service chooses how to tell: the `X-Forwarded-*` headers Traefik adds, or not routing the path in Dokploy. The real security boundary is still the token. Core's calls carry none of `X-Forwarded-*`, `Forwarded` or `X-Real-Ip`.
- **Why PUT:** the caller chooses the identifier and the operation is idempotent. Repeating the same `PUT` also serves as the status query; there is no separate `GET`.

Headers: `Authorization: Bearer <token>`, `Content-Type: application/json`. There is no `Idempotency-Key` header; the key is `request_id`.

## 2. Body

```json
{
  "request_id": "0b6f2a3e-…",
  "subject_id": "3f1c9d70-…",
  "emails": ["ad.soyad@std.yildiz.edu.tr", "kisi@example.com"]
}
```

- `request_id`: must equal the value in the path, otherwise `400`.
- `subject_id`: the person's Keycloak `sub`, which is core's `users.id`. A canonical UUID.
- `emails`: 0–3 addresses, the union of the School e-mail, the Personal e-mail and the Primary e-mail. Core trims them, lower-cases them and drops repeats and blanks. At most 254 characters per address.
- An unknown field gets `400`. The body is at most 4 KB.
- **`subject_id` and the addresses live only in the body.** They are never in the URL, a header, the outbox, a log, an error message, a metric or a durable store. Core reads them again on every attempt.

## 3. Idempotency

- The service keeps a receipt table in its own database: `account_erasure_receipts(request_id UUID PK, completed_at TIMESTAMPTZ, counts JSONB)`. The table holds neither `subject_id` nor any address.
- If a receipt exists, the service does not repeat the work and returns the stored `200` body unchanged.
- If there is no receipt, the erasure and the receipt write happen in **one transaction**. The transaction takes an advisory lock on `request_id`, so two concurrent `PUT`s do the work once.
- Parts outside the transaction, such as Redis (drafts), are deleted before the transaction. That step is idempotent too: when nothing is left to delete, it does nothing.
- The erasure logic is idempotent even without the receipt: a second run finds nothing to change.
- Because the receipt holds no `subject_id`, the service cannot tell a different subject arriving under the same `request_id`. In core, `request_id` and `subject_id` are bound one to one (both are UNIQUE), which is enough.

## 4. Responses

Errors use RFC 7807 (ADR-0010) and carry a fixed `code`. No response writes back any value from the body.

| Case | HTTP | Body | What core does |
|---|---|---|---|
| Done (a repeat included) | `200` | `{"request_id","status":"completed","completed_at","counts":{…}}` | Writes the checkpoint and the `counts`, moves on |
| In progress | `202` + `Retry-After` | `{"request_id","status":"in_progress"}` | Repeats the same `PUT` later (deferred retry) |
| Transient failure | `429`, `500`, `502`, `503`, `504`, timeout, connection failure | problem | Deferred retry |
| Token refused | `401` | problem | Drops the cached token, ordinary retry (spends an attempt) |
| Permanent failure | `400 invalid_erasure_command`, `403 erasure_forbidden`, `404` (no endpoint), `409 subject_not_blocked` | problem | The request goes to `manual_intervention` at once. The error code is `erase_<service>_rejected_<http>`, for example `erase_cms_rejected_403`. A `403` also drops the cached token, so the retry after the fix uses a new one |

- **`counts`:** keys the service chooses, snake_case, non-negative integers, at most 32 keys. For example `{"recipients_deleted":1,"queue_rows_cleared":12,"actor_columns_replaced":3}`. No personal data.
- **Deferred retry:** core's existing `RetryAt` path (`internal/account/worker.go`). The attempt is refunded.
  - The wait comes from `Retry-After` and is clamped between 30 seconds and 15 minutes. Without the header it is 5 minutes.
  - The horizon is the existing `DeferredRetryHorizon` (48 hours by default, counted from the request's creation). After the horizon the ordinary budget applies: 8 attempts × 30 seconds, then `manual_intervention` and the alarm.
  - This window comfortably outlasts the nightly 03:30 secret rotation (ADR-0050).
- A **permanent failure** is a configuration or contract fault: a missing role, an endpoint not deployed, a missing marker. An operator fixes it, and the existing retry path then returns the request to `pending`.

## 5. Authentication

Core takes the token from a separate Keycloak client, `core-erasure`.

- **Client:** confidential, service account only (no other flow), `fullScopeAllowed=false`.
- **Token request:** `grant_type=client_credentials`, `scope=openid account-erase-<service>`. Core caches the token per service until `exp − 30 s`. A failure of the token endpoint counts as transient.
- **Token claims:** `azp=core-erasure`, the service's resource client in `aud`, its erase role in `resource_access`, and `sub` (the service account's), which the client's `basic` default scope adds. Core asks for no other scope.
- **Why a separate client:** a token for one service must not carry another service's erase role. Otherwise the service that receives the token could replay it to another service and have it erase arbitrary addresses.
  - Core's own client (`core`) has `fullScopeAllowed` on. Turning it off would affect core's current tokens (SkyMail sends, Admin REST).
  - The precedent is Keycloak's mail client (`keycloak-mailer`, K5).

| Service | Resource client (`aud` must contain it) | Role (`resource_access.<client>.roles`) | Current check | Added on the erase endpoint |
|---|---|---|---|---|
| SkyMail | `skymail` | `skymail:account:erase` | The token is checked with `userinfo`; no `aud`/`azp` check | Local JWKS verification (signature, `iss`, `exp`), then `azp`, `aud` and the role |
| CMS | `skycms` | `cms:account:erase` | JwtBearer, `Audience=skycms`. Roles are read only from `resource_access[azp]` (ADR-0014) | The role is read from `resource_access.skycms` (not from azp), plus an `azp` check |
| Forms | `forms` | `skyforms:account:erase` | JwtBearer, `aud` ∋ `forms`, `skyforms:*` roles | `azp` and the role |

Rules shared by all three services:

1. `azp` must be exactly `core-erasure`.
2. The subject must be blocked: the service reads the marker of `subject_id` in the account-access Redis (`SHA-256(issuer + NUL + subject)`, the existing reader ACL `get`/`mget`). No marker: `409 subject_not_blocked`. Redis unreachable: `503`. A compromised caller therefore cannot have the data of someone who never asked for deletion erased.
3. The existing check on the caller's own marker does not change. The `core-erasure` service account is not blocked.

## 6. Timeouts

- **Core:** a 15-second HTTP timeout per call. Core's step timeout in production is 20 seconds.
- **Service:** answers within 10 seconds. If the work takes longer, it commits what it has done and returns `202`. Example: a queue row that is being sent (`processing`).

## 7. Logs and personal data

- The body of this route is not logged. The request logger writes only the method, the path (which holds only `request_id`), the status and the duration.
- Error logs carry `request_id`, the step and a fixed code. `subject_id`, addresses and names are not written. The HTTP client does not write the body even in debug mode.
- Metrics are unlabelled.
- Both sides' tests capture the log output and check that the test addresses and the subject do not appear.

## 8. Common erasure rules on the service side (ADR-0042, ADR-0051)

- **Silinmiş kullanıcı:** the fixed sub `00000000-0000-4000-8000-000000000000`, display name `Silinmiş kullanıcı`. The same for everyone; there is no per-person pseudonym.
  - Every actor column in a service that names the person takes this value.
  - Nullable columns take it too, because NULL can mean something else. Example: in SkyMail `actor_sub IS NULL` means "SkyMail itself".
  - Name columns get `Silinmiş kullanıcı`. E-mail columns become NULL, or `''` when NOT NULL.
  - An interface that asks core to resolve a name for this value shows `Silinmiş kullanıcı` instead.
  - Core keeps its current behaviour (NULL) in its own columns.
- **Free text and rendered bodies:** a field that contains the person's address, or the full name the service holds for the person, is deleted **whole**; there is no partial masking.
  - The match is a case-insensitive substring search.
  - Single-word names are not used for the search. Also deleting the body of a namesake is an accepted over-deletion.
  - A text field that cannot be empty gets the fixed `[silindi]`.
- **Relationship rows** (list membership, collaborator, recipient row) and **transient data** (drafts, tokens, queue residue) are hard-deleted.
- The records themselves, their dates and their counts stay.
