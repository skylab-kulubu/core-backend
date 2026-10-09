# Health, readiness and shutdown

How core tells Docker Swarm whether a task should get traffic, and how it
stops. Code: `internal/health`, `cmd/core-backend/serve.go`,
`cmd/core-backend/healthcheck.go`.

Dokploy updates a service start-first (the new task starts, then the old one
stops) and rolls back a failed update. Without a health check Swarm stops the
old task as soon as the new one is running, before it listens, and gives it
traffic at once; with one, the old task keeps serving until the new one is
healthy. A health check therefore helps with one replica as much as with
several.

## Endpoints

| Route | Answers | Asks |
|---|---|---|
| `GET /v1/health` | always `204` | nothing (liveness: the process answers) |
| `GET /v1/ready` | `204`, or `503` with `Cache-Control: no-store` and `Retry-After: 1` | in order: the task is not shutting down; the database answers a ping; the account access gate's contract sentinel (`ACCOUNT_ACCESS_GATE_MODE=enforce` only, docs/account-access-gate.md) |

`/v1/ready` keeps answering `204` when all is well, which is what the release
wizards and their markers read.

- **Database.** A ping on readiness's own connection (one per task, never
  the main pool's: a pool whose connections are all busy is a slow moment,
  not an outage, and must not get the task restarted under load), within
  2 s. The answer is reused for a second, so however often the public route is
  asked the database sees at most one ping a second. The log says once when
  the database stops answering and once when it is back.
- **Migrations** are done by construction: core starts listening only after
  `migrate.Apply` has returned, so a task that answers at all has its schema.
- **Shutting down.** From the stop signal on, `/v1/ready` answers `503`.
- **Not asked:** Keycloak, SkyMail, Gotenberg, ClamAV, the frame service, R2,
  OpenBao, Cloudflare. Their outage fails only the requests that need them, the
  same on every task; failing readiness for them would take every task out (or
  get them restarted) and turn a partial outage into a whole one.

Swarm has one health check, not Kubernetes' separate liveness and readiness:
a task that fails it `retries` times in a row is replaced. Its settings below
let a database restart pass (a minute) without a restart of core.

## The health check

`core-backend healthcheck` asks this container's core for `/v1/ready`
(`http://127.0.0.1:$PORT`, 3 s) and exits 0 on `2xx`, 1 otherwise, saying why
on stderr (kept by Docker: `docker inspect`). The image needs no curl or wget.

The image carries it (`Dockerfile`):

```
HEALTHCHECK --interval=10s --timeout=5s --start-period=120s --start-interval=2s --retries=6 \
  CMD ["/app/core-backend", "healthcheck"]
```

A Swarm service uses the image's health check unless its own replaces it, so
Dokploy's Health Check field may stay empty for core. Set there (Advanced →
Swarm Settings; durations in nanoseconds), it is:

```json
{
  "Test": ["CMD", "/app/core-backend", "healthcheck"],
  "Interval": 10000000000,
  "Timeout": 5000000000,
  "StartPeriod": 120000000000,
  "StartInterval": 2000000000,
  "Retries": 6
}
```

| Setting | Value | Why |
|---|---|---|
| Interval | 10 s | a failing task is noticed within seconds |
| Timeout | 5 s | over the command's 3 s, which is over the ping's 2 s |
| StartPeriod | 120 s | startup: migrations, the Keycloak role checks (up to 2 × 15 s), the access gate's reconciliation (up to 30 s). A release with a long migration needs more |
| StartInterval | 2 s | during the start period: the new task is healthy within 2 s of listening, so a deploy moves over quickly (Docker Engine 25+; older engines ignore it and use Interval) |
| Retries | 6 | a task is replaced after about a minute of failures; a database restart passes |
| **Stop Grace Period** | **30 s** (`30000000000`) | must be over core's 25 s shutdown; Swarm's default is 10 s |

The Dokploy settings are written by the hub's Swarm settings wizard
(sky_lab_genel `.scratch/horizontal-scale`, ticket 19).

## Shutdown

On SIGTERM (Docker's stop; SIGINT too, a second one kills at once) core:

1. answers `/v1/ready` with `503`;
2. stops taking connections, closes idle keep-alive connections, answers each
   request in flight with `Connection: close`, and waits up to **20 s** for
   them; a request still open then is cut off (a single-step upload on a slow
   link can be);
3. stops the background workers and waits for them, and for the contact
   consent confirmation mails going, until **25 s** after the signal;
4. closes the access gate's Redis client and the database pools, and exits.

Swarm takes a task out of its load balancer (and waits about two seconds)
before it sends SIGTERM, so step 2 turns away no new connection. Whatever has
not stopped at 25 s is named in the log and left behind, so the process exits
before Docker's SIGKILL at 30 s.

Workers keep working while requests drain and are stopped after. Shutdown
waits for those that claim work, so that each settles or releases its claim
on the open pool: the media scan, faststart and frame workers (they settle
their claims even when stopped), the CDN purge (its own pool), the blob purge,
the upload staging cleanup, the retention sweep, the certificate issuance
worker and the account erasure worker. A certificate job or an erasure request
cut short stays claimed until its lease runs out (2 and 5 minutes) and is then
taken up again, as after a crash. The other loops (backfills, the hourly
retention deletes, the access projection, the erasure watchdog) are idempotent
and only stopped.

A contact consent's confirmation mail goes after the request that recorded it
was answered, while its row already says it was sent; shutdown waits for it.
A mail cut off all the same (SkyMail slower than the deadline) leaves the grant
pending, and the person's next request mails it again after
`ConfirmationResend` (docs/contact-consents.md).

A stop signal during startup is acted on once startup is through (the
migrations are not cut off halfway); during the access gate's reconciliation
core exits at once.

## Migrations: expand, then contract

Start-first means the old task keeps serving while the new one migrates and
starts, with the new schema. A migration must keep the previous release
working:

- **expand** (this release): add tables, nullable columns, columns with a
  default, indexes (`CONCURRENTLY` on large tables), new constraints
  `NOT VALID` first;
- **contract** (a later release, once no running code reads it): drop or
  rename columns and tables, make columns `NOT NULL`, validate constraints.

A rename is an expand (add the new column, write both) and a later contract
(drop the old one). A release that cannot follow this is deployed stop-first,
once, with the downtime announced.

## Connections

Each task holds its main pool (pgxpool's default, max(4, CPUs)), the CDN
purge's 2 and readiness's 1. During a start-first update two tasks hold them.
