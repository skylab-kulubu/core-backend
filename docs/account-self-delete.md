# Account Center self-delete contract

This interface is the only least-privilege path from Account Center into the
Core account-erasure lifecycle. It does not expose the privileged
`DELETE /v1/users/{id}` route, accept a subject in a body/header, use a service
account, run JIT synchronization, or return profile data.

## Authentication and intake

`POST /v1/account-deletion-requests/self` requires all of the following:

- `Authorization: Bearer <account access token>`
- `X-Account-Reauth-Token: <fresh ID token>`
- `Idempotency-Key: <43-character unpadded base64url value>`
- an empty request body

The idempotency key therefore represents exactly 32 caller-generated random
bytes. Core verifies both JWTs independently with the realm JWKS and RS256.
The access token must have the canonical issuer, a canonical UUID subject,
`typ=JWT`, `azp=account-center`, exactly `aud=account`, exactly
`scope=openid`, and a future integer expiry. The ID token must have the same
issuer and subject, `typ=JWT`, exactly `aud=account-center`, a non-empty `sid`,
a future integer expiry, and an integer `auth_time` no more than five minutes
old (with five seconds of future clock skew). The verified matching subject is
the only deletion target.

This route is intentionally registered before Core's normal resource bearer,
shared-access-gate and JIT middleware. That narrow exception lets a lost HTTP
response retry the same irreversible command after the permanent marker has
made the old token unusable everywhere else. The route cannot read product
data, select a different subject, reactivate an account, or initiate a second
request with a different idempotency key.

Core hashes the key with a domain separator and verified subject. The raw key
is never stored. The first call creates or reuses one durable lifecycle request
and its fenced outbox record. Core then writes and verifies the permanent
shared-access marker and records `platform_blocked_at`. A successful response
is impossible until a second database read proves that durable postcondition.
A failed projection returns `503` with no receipt; retrying the same key reuses
the already durable request.

Successful intake returns `202` while work is active and `200` for a terminal
state, with exactly:

```json
{
  "receipt": "adr_<43 base64url characters>",
  "status": "pending",
  "partial": false,
  "platformBlocked": true,
  "requestedAt": "2026-09-20T08:00:00Z",
  "updatedAt": "2026-09-20T08:00:00Z",
  "completedAt": null,
  "receiptExpiresAt": "2026-12-19T08:00:00Z"
}
```

No request UUID, subject, session ID, attempt counter or internal error code is
returned.

## Receipt status and retry

After Account Center revokes its local subject sessions, it uses the receipt as
a capability:

- `GET /v1/account-deletion-requests/status`
- `POST /v1/account-deletion-requests/status/retry`
- `Authorization: DeletionReceipt <receipt>`

Both responses omit `receipt` and otherwise have exactly the seven remaining
fields in the intake response. Allowed coarse states are `blocking`, `pending`,
`processing`, `completed` and `manual_intervention`. `blocking` with
`platformBlocked=false` exists only for recovery diagnostics; normal intake
never discloses a receipt before confirmation. `partial=true` means at least
one durable erasure checkpoint completed while the request is not complete.
It does not identify the step or expose its error.

Retry is idempotent. It only moves `manual_intervention` back to `pending`,
clears the stable internal error and retry budget, and preserves the permanent
marker and `deletion_pending`/`anonymized` account state. Pending, processing
and completed requests are returned unchanged.

The receipt is a domain-separated HMAC-SHA256 capability derived from the
verified subject and scoped idempotency digest. PostgreSQL stores only separate
domain-separated lookup and proof hashes. The proof comparison is constant
time after lookup. A receipt has one fixed 90-day expiry and a nullable
revocation timestamp; invalid, expired and revoked receipts all return the same
`404 account_deletion_receipt_not_found` response. Raw keys, raw receipts and
JWTs must never appear in logs or metrics.

Planned receipt-key rotation must wait until the 90-day retry window for the
old key has elapsed. Existing receipts remain verifiable from their persisted
hashes after a rotation, but Core cannot reproduce the old deterministic
receipt for an intake retry and returns the generic idempotency conflict. An
emergency operator can revoke individual or all outstanding receipts by
setting `receipt_revoked_at`; that action never changes deletion state.

Ingress, reverse proxies, request tracing and APM instrumentation must redact
both `Authorization` and `X-Account-Reauth-Token` before this route is exposed.
The custom re-authentication header carries a raw ID token and must never be
captured, indexed, sampled or emitted in access/application logs. Release
validation must include a synthetic request through the production ingress and
an explicit inspection of proxy, APM and application telemetry proving that
neither header value was retained.

## Errors and release gate

All responses use `Cache-Control: no-store`. Stable error codes are:

- `400 invalid_idempotency_key` for a missing/malformed idempotency key;
- `400 invalid_request` for a non-empty body;
- `401 invalid_end_user_token` for either missing/invalid/mismatched JWT;
- `409 idempotency_conflict` for a different key on the same durable request;
- `404 account_deletion_receipt_not_found` for every unusable receipt;
- `503 account_deletion_unavailable` while disabled or when projection fails.

`ACCOUNT_ERASURE_WORKER_ENABLED` remains the shared, destructive default-off
gate for intake, retry and worker consumption. When it is true,
`ACCOUNT_ACCESS_GATE_MODE=enforce`, the Keycloak worker configuration and an
unpadded base64url `ACCOUNT_DELETION_RECEIPT_KEY` containing exactly 32 random
bytes are mandatory. Status reads remain available while the feature is off.

Production enablement is still blocked on the documented real LDAP
production-clone disable/logout/delete/reimport rehearsal, cross-service old
JWT exercise, the ingress/APM redaction validation above and identity-service
owner approval. This implementation does not enable the flag or deploy
production.
