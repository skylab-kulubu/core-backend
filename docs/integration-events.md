# Integration events

Core consumes Integration events (CONTEXT "Integration event", ADR-0071): facts
another service publishes through RabbitMQ ("a response was created", "a form
closed") without knowing who reads them. Code: `internal/events`, wired in
`cmd/core-backend/integration_events.go`.

This is the infrastructure only. The queues' handlers come with their own
changes: `core.keycloak` wakes the user sync, `core.forms` writes Tickets and
feedback (RabbitMQ tickets R09 and R13 in the hub). Until they are registered
core consumes nothing, even when on.

## Off by default

`EVENTS_ENABLED` is `false` unless set. Off, core never connects to RabbitMQ
and reads none of the `RABBITMQ_*` variables. The migration and the hourly
cleanup of `consumed_events` (below) run either way: the cleanup of an empty
table is one indexed `DELETE`, and records left from a time the consumer was
on still go on time.

On, core connects in the background. A broker that is down or refuses core
never stops startup and never fails `/v1/ready` ([health-and-shutdown.md](health-and-shutdown.md)):
an event waits in its queue (or the publisher's outbox) and the daily pull and
the user sync's pass catch up. The log and `/v1/metrics` say core is not
connected.

## Settings

| Variable | Default | Meaning |
| --- | --- | --- |
| `EVENTS_ENABLED` | `false` | `true` turns the consumer on. Any other value stops core. |
| `RABBITMQ_HOST` | none | The broker's host name on the environment's events network. Required when on. |
| `RABBITMQ_PORT` | `5672` | AMQP without TLS (ADR-0071: no TLS on the internal network yet). |
| `RABBITMQ_VHOST` | `skylab` | The vhost ops sets up. |
| `RABBITMQ_USERNAME` | none | Core's user, `core_a` or `core_b` (the rotation alternates them). Required when on. |
| `RABBITMQ_PASSWORD` | none | Its password. Required when on. |

In a deployment all five are OpenBao references (`kv/<env>/core/RABBITMQ_*`,
ADR-0049). The password is sent as SASL PLAIN on the connection, never put in a
URL, and never written to a log line, an error or a metric (an error that
quoted it would show `[redacted]`). A setting core cannot read stops startup
with the variable's name, never its value.

## What core may do on the broker

Core's user has no `configure` and no `write` permission, only `read` of
`^core\.` (ADR-0071 §4.1). Core declares nothing: ops sets up the exchanges,
the queues, their bindings, the DLQs and the policies from a file. A queue
that is missing closes its channel (`404`); core logs it, says
`skylab_events_consuming{queue} 0` and tries again with a growing wait, and the
other queues go on.

## Connection

amqp091-go (the RabbitMQ team's client) does not reconnect by itself (its
experimental recovery is left off), so core does:

- one connection (`core-backend` in the management UI), heartbeat 10 s, a
  dial bounded at 10 s;
- lost or refused: core waits and dials again, 1 s doubling to a minute, with
  up to a fifth more at random so replicas do not come back in step; the wait
  starts again from 1 s after a connection succeeds;
- a channel per queue, with its own prefetch of 20; a channel the broker
  closes is opened again with the same growing wait while the connection
  stays;
- each queue's deliveries are handled two at a time (a database transaction
  each; core's pool is small); the rest wait in the prefetch.

When a connection or a channel is lost, the handlers running on it are
stopped: their transactions roll back, their deliveries were not acked, and
the broker delivers them again. A shutdown does the same: core stops the
consumer after the HTTP drain and before the database pool closes
([health-and-shutdown.md](health-and-shutdown.md)).

## A delivery

1. **Decode.** A queue's decoder reads the message; the default is CloudEvents
   1.0 in structured JSON mode (ADR-0071 §5): `specversion` `1.0`, a non-empty
   `id`, `source` and `type` (each at most 255 bytes), `time` RFC 3339 when
   present, JSON `data` (no `data_base64`, `datacontenttype` absent or
   `application/json`), the AMQP content type absent or
   `application/cloudevents+json`, the body at most 64 KiB. Attributes core
   does not know are ignored. A queue whose publisher has its own format
   (Keycloak's provider) registers its own decoder. A message that does not
   decode is rejected without requeue, to the queue's DLQ.
2. **Claim and handle, in one transaction.** Core inserts
   `(consumer, source, event_id)` into `consumed_events` and runs the handler
   with the same transaction. The consumer is the queue's name unless the
   subscription names another.
   - The row is already there: the event was handled before (a publisher's
     retry, a redelivery after a lost ack). Nothing runs; acked as a
     duplicate. Two replicas handed the same event at once: the second waits
     on the first's insert and finds the row.
   - The handler returns nil: the transaction commits (its writes and the
     record together), then the delivery is acked.
   - `ErrUnrelated` (an event core has no business with, a form core does not
     know): rolled back, acked, nothing recorded.
   - `Permanent(err)` (data that fails validation, an unknown schema
     version): rolled back, nacked without requeue, to the DLQ.
   - A panic: recovered, rolled back, to the DLQ (a bug: the message waits
     there for the fix).
   - Any other error is transient (the database, a peer's 5xx or 429): rolled
     back and tried again after 1, 5 and 30 seconds; after the fourth failure,
     to the DLQ.
3. **Never requeued at once.** A quorum queue does not count an explicit
   requeue against its `delivery-limit`, so `requeue=true` would loop on a
   poison message. A core that crashes while it holds a delivery leaves it
   to the broker, which returns it to the queue; the queue's `delivery-limit`
   (5, ops' policy, ADR-0071 §3.2) is what then sends a message that keeps
   failing that way to the DLQ. That is the broker's behaviour and its
   topology's: core's tests do not cover it.

A handler that calls another service must be idempotent by itself as well: a
rollback does not take back a call that went out. Its errors reach the log
line, so they carry ids, never personal data.

DLQ messages are put back by an ops command (hub ticket R06); nothing does it
automatically.

## `consumed_events`

Migration `20261021153000` (expand only):

| Column | |
| --- | --- |
| `consumer` | the subscription's consumer name, 1 to 100 bytes |
| `source`, `event_id` | the CloudEvents source and id, 1 to 255 bytes each |
| `processed_at` | when it was handled |

The primary key is the first three; an index on `processed_at` serves the
cleanup. Ids only, no personal data.

A row goes 30 days after it was handled (ADR-0071 §2.7): longer than a
publisher's outbox (7 days) and a DLQ (14 days) can hand the same event over
again. The cleanup runs at startup and then hourly on every replica, 1,000 rows
a statement.

## What it tells

Log lines are JSON with an `event` name and carry the queue, the event's type
and id, and the error, never the event's data or the password:
`events_connected`, `events_connect_failed`, `events_connection_lost`,
`events_consuming`, `events_channel_closed`, `events_retry`,
`events_dead_lettered`, `events_settle_failed`,
`events_record_cleanup_failed`.

`/v1/metrics` (internal):

- `skylab_events_enabled` (0 when off; nothing else is reported then)
- `skylab_events_connected`, `skylab_events_connect_failures_total`
- `skylab_events_consuming{queue}`
- `skylab_events_consumed_total{queue,outcome="processed|duplicate|unrelated|dead_lettered"}`
- `skylab_events_redelivered_total{queue}`, `skylab_events_retries_total{queue}`
- `skylab_events_dlq_total{queue,reason="invalid_envelope|permanent|retries_exhausted|handler_panic"}`
- `skylab_events_records_deleted_total`, `skylab_events_record_cleanup_failures_total`

## Tests

`internal/events` has unit tests for the settings, the envelope, every
outcome of a delivery, the reconnection wait and a broker that cannot be
reached, and integration tests against PostgreSQL and a real RabbitMQ 4.2
(`internal/testrabbitmq`, the image e-skylab-keycloak's tests run). The broker
test sets up ADR-0071's topology (quorum queue, `delivery-limit`,
at-least-once dead-lettering to a DLQ) and a core user without `configure`
or `write`, and checks: an event handled once however often it comes, the four
ways to the DLQ with nothing written, unrelated events, a reconnection after
the broker drops the connection, a missing queue that leaves the others alone
and is picked up once declared, and a shutdown that returns the delivery in
hand to its queue.

## Against the standard

What matches: RabbitMQ's data safety guidance (manual acknowledgements after
the work is committed, a bounded prefetch, an idempotent consumer, no
immediate requeue on a quorum queue), the idempotent consumer pattern
(microservices.io: the message id recorded in the same transaction as the
business change), CloudEvents' `source` + `id` uniqueness, and Azure Service
Bus' model of a bounded delivery count with a dead-letter queue.

Deviations: the retries are in the process, not a delayed redelivery (no TTL
and DLX ring, no delayed-message plugin; ADR-0071 deviation 6); dropping
repeats is the consumer's, the broker has no duplicate detection window
(deviation 7); prefetch 20 is under RabbitMQ's suggested 100 to 300, for a
low volume with a database write each.

## Rolling back

`EVENTS_ENABLED=false` and a redeploy: core closes its connection and stops
consuming; the events wait in the queues. The table stays and is cleaned up as
before; the migration's down drops it, and the code before this change does
not read it.
