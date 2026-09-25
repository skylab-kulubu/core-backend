# Account Center self-delete contract

This interface is the only least-privilege path from Account Center into the
Core account-erasure lifecycle. It does not expose the privileged
`DELETE /v1/users/{id}` route, accept a subject in a body/header, use a service
account, run JIT synchronization, or return profile data.

## Authentication and intake

`POST /v1/account-deletion-requests/self` requires all of the following:

- `Authorization: Bearer <account access token>`
- one proof of recent authentication: `X-Sky-Sudo: <sky-account sudo token>`
  or `X-Account-Reauth-Token: <fresh ID token>` (see below)
- `Idempotency-Key: <43-character unpadded base64url value>`
- an empty request body

The idempotency key therefore represents exactly 32 caller-generated random
bytes. Core verifies the access token with the realm JWKS and RS256. It must
have the canonical issuer, a canonical UUID subject, `typ=JWT`,
`azp=account-center`, an `aud` **set containing** `account`, exactly
`scope=openid`, and a future integer expiry. The verified subject is the only
deletion target; each proof must name the same subject.

Keycloak serialises `aud` either as a bare string or as an array, and the
reconciled Account Center client resolves more than one audience, so the access
token's claim is read as a set: `"account"`, `["account"]` and
`["account","core"]` are all accepted, while `["core"]`, an empty array and a
missing claim are refused. The ID token audience stays exclusive - only its
JSON shape is relaxed, so `"account-center"` and `["account-center"]` are
accepted but `["account-center","core"]` is refused. A re-authentication proof
minted for a second client is a different token than the one this route asks
for.

### Accepted proof headers

Core accepts two proofs of recent authentication while Account Center moves
from the Keycloak re-authentication hop to Sudo mode, so that Core and Account
Center can ship independently. One of them is mandatory. When both headers
arrive, `X-Sky-Sudo` decides alone: a refused sudo token is not rescued by the
ID token, and a malformed `X-Sky-Sudo` is refused rather than skipped.

1. `X-Sky-Sudo` - the sky-account Sudo mode token Account Center already holds
   for `credentials/*` and `identity/username`, minted after the person
   re-proved themselves inside `my.` (password, TOTP, passkey, or a fresh
   Microsoft login). Freshness is the token's own five-minute lifetime.
2. `X-Account-Reauth-Token` - a fresh Keycloak ID token, verified with the
   realm JWKS and RS256: the same issuer and subject as the access token,
   `typ=JWT`, an `aud` naming exactly `account-center`, a non-empty `sid`, a
   future integer expiry, and an integer `auth_time` no more than five minutes
   old (with five seconds of future clock skew). Kept for the rollout and for
   rollback; it goes away once Account Center only sends the sudo proof.

#### Sudo proof

The sudo token is a Keycloak *internal* token: it is signed `HS512` with the
realm HMAC key, which never leaves Keycloak, so Core cannot verify it against
the JWKS. Core asks the realm instead, with RFC 7662 token introspection at
`{issuer}/protocol/openid-connect/token/introspect`, authenticated as Core's
own confidential client (`client_secret_post`). Keycloak 26 answers
`active:true` only for a token whose audience contains the calling client, so
this works once sky-account mints the sudo token with
`aud: ["sky-account", "core"]` (Keycloak ticket K3e). The audience check stays
on for every token Core introspects;
`allow.token.introspection.without.audience.check` is not needed and must not
be set on the `core` client.

The access token is verified first, locally, and must additionally carry a
non-empty `sid` (the Keycloak session id; sky-account requires the same claim
on every Account Center bearer). Only then does Core call the realm, so an
anonymous caller cannot make Core introspect anything. The introspection
answer must have:

- `active` exactly `true`;
- `typ` = `sky-sudo`;
- `iss` = the realm issuer Core derives from `KEYCLOAK_URL`/`KEYCLOAK_REALM`;
- `azp` = `account-center`;
- `aud` containing both `sky-account` and `core` (a bare string or an array);
- `sub` = the verified access token's subject;
- `sid` = the verified access token's `sid`, so a sudo token proves only the
  Account Center session that made it, as it does inside sky-account;
- an integer `exp` in the future.

The ID-token `auth_time` rule does not apply on this path: an in-product proof
never moves the Keycloak session's `auth_time`, and the token's own
five-minute life is the freshness bound.

A realm that answers with anything but `200` and a JSON object - unreachable,
a 5xx, a timeout after three seconds, or a `401` for Core's own client
credentials - is not a refusal. The intake answers
`503 account_deletion_unavailable` with `Retry-After`, and Account Center may
try again. Core logs that failure with the endpoint and the reason only; the
sudo token and the client secret travel in the request body and never appear
in errors or logs.

Configuration reuses what Core already has: the realm issuer and
`KEYCLOAK_CLIENT_ID`/`KEYCLOAK_CLIENT_SECRET`, the same client Core uses for
its SkyMail client-credentials token. `KEYCLOAK_CLIENT_ID` must be `core`,
the audience sky-account adds; Core warns at startup otherwise. Without the
client credentials the sudo proof is refused (`401`) and Core says so at
startup.

#### Replay of an accepted key

Keycloak answers `active:false` for a sudo token once the user session named
by its `sid` is gone, and Core's deletion saga closes the person's sessions.
If Core accepted the request but the answer was lost, Account Center's retry
with the same idempotency key and proof would then be refused although the
deletion is under way. So on the sudo path a replay of a key Core already
accepted is answered **before** the proof is introspected:

- The request must look exactly like the sudo intake: one well-formed
  `X-Sky-Sudo` header (its value is not checked on a replay), an empty body
  and `Authorization: Bearer`.
- The bearer is still verified locally with every rule above - realm JWKS and
  RS256, `typ=JWT`, the realm issuer, `azp=account-center`, an `aud` set
  containing `account`, exactly `scope=openid`, a canonical UUID subject, a
  non-empty `sid` and a future integer `exp`. An expired bearer is refused;
  the replay never asks the realm anything.
- The verified subject and the key must name an intake Core already
  **accepted**: the receipt Core derives from them finds a stored intake for
  that subject and key whose global block is confirmed (`platformBlocked`),
  whose receipt is neither revoked nor expired, while the feature is enabled.
  That is the only state in which Core has answered the key with success, and
  the saga cannot close the session before it.
- The answer is the one a retry through the proof would give: the intake
  response with the receipt, the request's current coarse state, `202` while
  work is active and `200` for `completed`/`manual_intervention`. The replay
  only reads; it creates, projects and changes nothing.

Everything else falls through to the proof unchanged: a key Core never
accepted, the same key under another subject (keys are scoped to the verified
subject, so it names a different intake), an intake whose block is not yet
confirmed (the proof is still good, and `Begin` finishes it) and every
request on the ID-token path, whose proof is verified locally and does not
depend on the session. A new key therefore always needs a live proof. A
replay is only as good as its bearer: Account Center seals the bearer with the
proof when the intent is prepared and cannot refresh it after the saga closed
the session, so a retry after that bearer's own `exp` gets `401`.

This route is intentionally registered before Core's normal resource bearer,
shared-access-gate and JIT middleware. That narrow exception lets a lost HTTP
response retry the same irreversible command after the permanent marker has
made the old token unusable everywhere else, and the replay above relies on it:
the gate blocks the subject everywhere else once the marker is written, and it
never runs for this route. The route cannot read product data, select a
different subject, reactivate an account, or initiate a second request with a
different idempotency key.

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

`completed` means all nine erasure steps are checkpointed: SkyMail, CMS and
Forms each confirmed the Erasure command, core is anonymized together with its
guest data, and the Keycloak identity is deleted last (ADR-0051, the saga in
[`account-lifecycle.md`](account-lifecycle.md#durable-erasure-flow)). While a
service has not confirmed, the request stays `pending` or `processing` (or
`manual_intervention` after a rejection) with `partial=true`; the person is
already blocked and logged out. The response shape does not change.

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
`Authorization`, `X-Sky-Sudo` and `X-Account-Reauth-Token` before this route is
exposed. The custom re-authentication headers carry a raw sudo token or ID
token and must never be captured, indexed, sampled or emitted in
access/application logs. Release validation must include a synthetic request
through the production ingress and an explicit inspection of proxy, APM and
application telemetry proving that no header value was retained.

## Errors and release gate

All responses use `Cache-Control: no-store`. Stable error codes are:

- `400 invalid_idempotency_key` for a missing/malformed idempotency key;
- `400 invalid_request` for a non-empty body;
- `401 invalid_end_user_token` for a missing/invalid/mismatched access token
  or proof, including a sudo token the realm reports inactive or whose claims
  do not match - except on a replay of an already accepted key (see above);
- `409 idempotency_conflict` for a different key on the same durable request;
- `404 account_deletion_receipt_not_found` for every unusable receipt;
- `503 account_deletion_unavailable` while disabled, when projection fails, or
  when the realm cannot be asked about a sudo proof.

`ACCOUNT_ERASURE_WORKER_ENABLED` remains the shared, destructive default-off
gate for intake, retry and worker consumption. When it is true,
`ACCOUNT_ACCESS_GATE_MODE=enforce`, the Keycloak worker configuration and an
unpadded base64url `ACCOUNT_DELETION_RECEIPT_KEY` containing exactly 32 random
bytes are mandatory. Status reads remain available while the feature is off.

Account Center's `ACCOUNT_ERASURE_MODE` stays `off`, and Core's
`ACCOUNT_ERASURE_WORKER_ENABLED` stays `false`, until Account Center sends the
sudo proof for deletion and Keycloak K3e (sudo tokens naming `core` in their
audience) is live in the same environment. Before that, a sudo proof
introspects as inactive and is refused, and deletion depends on the Keycloak
re-authentication hop. The replay of an accepted key must also be live in
that environment, so that a lost answer is still recoverable after the saga
has closed the session.

Production enablement is still blocked on the release gate in
[`account-lifecycle.md`](account-lifecycle.md#keycloak-and-federated-users).
The local full harness proves disable, logout, delete and the retry after
`404` against a real Keycloak, and that a deleted person who signs in again
through `OBS` gets a new `sub` (account-erasure ticket 10). The old-JWT matrix
then runs in production with a throwaway test account, and the
identity-service owner approves (ticket 11). No production clone is built.
Enablement is also blocked on the ingress/APM redaction validation above and
the erase endpoints of all three services, Forms' included (ADR-0051): the
worker does not start without every service URL. This implementation does not
enable the flag or deploy production.
