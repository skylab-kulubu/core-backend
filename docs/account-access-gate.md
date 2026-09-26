# Account access gate

Core implements the v1 shared denylist contract for the exact issuer `https://e.yildizskylab.com/realms/e-skylab`. Keys contain only `SHA-256(issuer + NUL + subject)`. Markers always have value `1` and no TTL. Core is the sole runtime writer; readers receive read-only ACLs.

`ACCOUNT_ACCESS_GATE_MODE` defaults to `off`. `enforce` has no observe or fail-open behavior and requires every Redis address, username, password, DB, TLS/server-name, timeout and durability setting explicitly. The dedicated Redis must use authenticated TLS, `noeviction`, AOF with `appendfsync always`, and the ACL boundaries documented in `deploy/account-access-redis`.

## Request ordering

Verified Bearer identity is evaluated after signature/issuer/audience validation and before JIT. A missing subject remains anonymous. An allowed subject continues; marker `1` returns a generic `401`; Redis, contract and malformed-value failures return `503`, `Cache-Control: no-store` and `Retry-After: 1` before JIT or product code. `/v1/go/:alias` and its channel form `/v1/go/:alias/:channel` use this same chain and a PostgreSQL tri-state attribution check: a durable deletion marker returns `401`, a database failure returns `503`, and neither path records a hit or increments the click count. A genuinely anonymous hop keeps the existing silent `301`. `/v1/health` is process-only. `/v1/ready` checks the exact contract sentinel.

The only non-product exception is the Account Center self-delete
intake/status contract in `docs/account-self-delete.md`. It is isolated before
the general gate and JIT middleware so a response-loss retry can recover the
same irreversible request after marker installation. It accepts only the
pinned two-token Account Center end-user context, a hash-only status receipt,
or - for a replay of an idempotency key already accepted for the same subject -
the locally verified Account Center bearer alone, which can only read that
request's stored outcome; it cannot read product data, choose a subject or
create/update a profile.

`/v1/metrics` exposes fixed, unlabeled Prometheus counters for aggregate gate decisions, Redis unavailability, contract mismatch, malformed markers, reconciliation drift, positive TTL detection and readiness failures. The request gate also writes a JSON decision event containing only the request correlation ID and `allowed`, `blocked` or `unavailable`; subjects, digests and Redis keys are never logged or used as metric labels.

## Deletion projection

The PostgreSQL deletion request commits first with `platform_blocked_at = NULL` and its outbox row fenced at `available_at = infinity`. The HTTP command then synchronously writes and verifies the permanent marker, verifies the exact contract, records `platform_blocked_at`, and releases the outbox row. A failed projection returns `503`; repeating the command reuses and retries the same durable request.

The erasure worker can claim only confirmed requests and re-asserts the marker immediately before its first side effect. The default-off `ACCOUNT_ERASURE_WORKER_ENABLED` flag additionally requires gate `enforce` at startup. The periodic projector retries unconfirmed requests. The paged reconciler re-asserts every durable request, including completed/anonymized ones. On an empty Redis, it writes every marker first and the contract sentinel last, keeping all readers fail-closed throughout recovery.

## Cutover

1. Provision the dedicated Redis and least-privilege credentials.
2. Keep deletion intake/worker off.
3. Start Core in `enforce`; its startup reconciliation must finish and readiness must pass.
4. Compare durable request count with marker count and sample marker values/TTL using the recovery operator, without logging keys or subjects.
5. Enable every active service reader and pass the cross-service old-token exercise.
6. Only then may deletion intake and the worker be enabled.

The migration refuses rollback while any durable deletion request exists. After the first confirmed marker, do not roll back to an ungated service and never flush the namespace. During disaster recovery, the operator removes only the contract key, Core restores all durable markers, validation runs, and Core installs the exact contract last.
