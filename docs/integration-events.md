# Integration events

Core consumes Integration events (CONTEXT "Integration event", ADR-0071): facts
another service publishes through RabbitMQ ("a response was created", "a form
closed") without knowing who reads them. Code: `internal/events`, wired in
`cmd/core-backend/integration_events.go`.

Queues core reads: `core.keycloak`, whose events wake the user sync (below;
hub ticket R09). `core.forms`, which writes Tickets and feedback, comes with
hub ticket R13.

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
  each; core's pool is small); the rest wait in the prefetch. A channel that
  closes, or a consumer the broker cancels, while both are busy (two handlers
  in their 36 seconds of retries) stops them at once, not after their retries.

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
   does not know are ignored. The optional `traceparent` extension is read
   (below); an invalid one is ignored, never a reason to refuse. A queue
   whose publisher has its own format (Keycloak's provider, `core.keycloak`
   below) registers its own decoder. A
   message that does not decode is rejected without requeue, to the queue's
   DLQ.
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
line, so they carry ids, never personal data. Core masks what a handler may
wrap without meaning to: a PostgreSQL error is written as its SQLSTATE and
constraint (its message and detail can quote a value), a panic as its type (a
Go runtime error keeps its text), e-mail and IPv4 addresses as `[e-mail]` and
`[ip]`, the password as `[redacted]`.

DLQ messages are put back by an ops command (hub ticket R06); nothing does it
automatically.

### Envelope extension: `traceparent`

| Attribute | Required | Format | Core |
| --- | --- | --- | --- |
| `traceparent` | no | W3C Trace Context version `00`: `00-<32 hex>-<16 hex>-<2 hex>`, lower case, trace id and parent id not all zeros (CloudEvents Distributed Tracing extension). A later version (`01`..`fe`) is read by its first 55 characters in version `00`'s layout, followed by nothing or `-`; the rest is dropped | Its trace id goes into the event's log lines as `trace_id`. Missing, invalid (upper case, wrong length, all-zero ids, version `ff`, version `00` with more after it, not a string): empty, the event is handled as before. Not stored in `consumed_events`, not a metric label. |

The publisher sets it from the request that caused the event (Forms: .NET's
`Activity.Current` when its id is W3C), and leaves it out in background work;
`tracestate` is not used. Keycloak's events (`core.keycloak`) never carry it:
the provider's format is not CloudEvents. Passing it on as a `traceparent`
header when a handler calls another service is not done yet.

## `core.keycloak`: Keycloak's events

Keycloak's RabbitMQ provider (keycloak-to-rabbitmq, on `keycloak.events`)
publishes Keycloak's own event JSON, not CloudEvents (ADR-0071 §5). Ops binds
`core.keycloak` to the `e-skylab` realm's successful `REGISTER`,
`UPDATE_EMAIL`, `UPDATE_PROFILE` and `DELETE_ACCOUNT` user events, to every
admin event on a `USER`, and to the admin `GROUP DELETE`
(`KK.EVENT.ADMIN.e-skylab.SUCCESS.GROUP.DELETE`; the provider publishes no
other `GROUP` event).

Its decoder (`events.DecodeKeycloak`) reads:

- an admin event (an `operationType` and a `resourceType`) or a user event (a
  `type`), each an upper-case Keycloak name; anything else goes to the DLQ;
- the id: Keycloak's event id, else the message-id, else the body's SHA-256
  (the provider before SKY LAB's fork, hub ticket R07, sends none; the same
  body is the same event). The source is `/keycloak`;
- the type as `keycloak.register`, `keycloak.admin.user.update`;
- `realmId`, `clientId`, `userId`, whether Keycloak sent an `error`, and
  `time` when it is Keycloak's milliseconds;
- an admin event's `resourcePath` when it is `users/<id>` or `groups/<id>`
  (any other path is left out; the event is still read);
- the realm's name from the routing key's fourth word
  (`KK.EVENT.ADMIN.<realm>.…`, `KK.EVENT.CLIENT.<realm>.…`): `realmId` is
  Keycloak's internal id of the realm, which core does not know;
- the AMQP content type absent or `application/json`, the body at most
  64 KiB.

It keeps nothing else: the IP address, the details (a sign-up's e-mail, a
change's old and new address), the session, an admin event's representation
and auth details are dropped when the provider still sends them, and fields
it does not know are ignored.

An event only wakes something: what changes is decided on a fresh read of
Keycloak, the order of events does not matter, and an event that was lost
(Keycloak publishes after its commit, without an outbox; ADR-0071 sapma 4) is
caught by the periodic passes, which stay. The handler:

- **Wakes the user sync** ([user-sync.md](user-sync.md)) on the user events
  above and an admin's `USER` `CREATE`, `UPDATE` or `DELETE`: it reads
  Keycloak and brings core's row in line, a pass at most every 30 seconds;
  the `USER_SYNC_INTERVAL` pass catches the rest.
- **Removes a person deleted outside the erasure saga.** On an admin's
  `USER DELETE` (the person from `resourcePath`) or a `DELETE_ACCOUNT` (from
  `userId`): when core has an erasure request for the person
  (`account_deletion_requests`, any state), it is the saga's and nothing else
  is done. Otherwise core reads the person from Keycloak: still there, nothing
  is deleted; "not found", their communication preferences go in the event's
  transaction (the same rows as the saga's `erase_communication_preferences`
  step and the daily deleted people cleanup), with the warning
  `keycloak_user_deleted_outside_erasure` (the event id and the person's id,
  no address) and a count: a deletion from the Keycloak console skips the
  saga, and should be seen. Any other Keycloak error is transient (tried
  again, then the DLQ).
- **Wakes the deleted teams cleanup** on a `GROUP DELETE`
  ([communication-preferences.md](communication-preferences.md)): it reads
  the whole Group list, so a deleted Group's sub-Groups, which get no event,
  go too. The first event runs it at once; events that come within 30
  seconds of a run make one more run when the 30 seconds are up (that run
  sees a deletion the first one may have read before). The handler does not
  wait for either. Core's in-process Group caches are not emptied: the event
  reaches one replica of competing consumers, and the caches refresh within a
  minute. What else a deleted Group leaves in core is in
  [group-deletion.md](group-deletion.md#what-a-deletion-leaves-in-core).

A `DELETE_ACCOUNT` (deletion from Keycloak's account console, off in our
realm) is also logged as a warning, `keycloak_account_deleted_by_user` with
the event id. An event of another realm, a failed event and any other event
(a sign-in, an admin's `ACTION` e-mail, a `GROUP` `UPDATE`) are acked
unrecorded. So is an event whose part is off: the user sync's with
`USER_SYNC_ENABLED=false`, the deletions' without Keycloak. The startup log
says which parts are on.

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
and id, the error, and `trace_id` when the event has a valid `traceparent`,
never the event's data or the password:
`events_connected`, `events_connect_failed`, `events_connection_lost`,
`events_consuming`, `events_channel_closed`, `events_handled` (with its
outcome: processed, duplicate or unrelated), `events_retry`,
`events_dead_lettered`, `events_settle_failed`,
`events_record_cleanup_failed`, `keycloak_account_deleted_by_user`,
`keycloak_user_deleted_outside_erasure`.

`/v1/metrics` (internal):

- `skylab_events_enabled` (0 when off; the consumer has no other series then,
  the cleanup's two counters at the end stay)
- `skylab_events_connected`, `skylab_events_connect_failures_total`
- `skylab_events_consuming{queue}`
- `skylab_events_consumed_total{queue,outcome="processed|duplicate|unrelated|dead_lettered"}`
- `skylab_events_redelivered_total{queue}`, `skylab_events_retries_total{queue}`
- `skylab_events_dlq_total{queue,reason="invalid_envelope|permanent|retries_exhausted|handler_panic"}`
- `skylab_events_records_deleted_total`, `skylab_events_record_cleanup_failures_total`
- `skylab_keycloak_users_deleted_outside_erasure_total` (with the consumer on)

## Tests

`internal/events` has unit tests for the settings, the envelope and its
`traceparent`, Keycloak's
format, every outcome of a delivery, the masking of a handler's error in the
log, the reconnection wait and a broker that cannot be reached, and integration tests against PostgreSQL and a real RabbitMQ 4.2
(`internal/testrabbitmq`, the image e-skylab-keycloak's tests run). The broker
test sets up ADR-0071's topology (quorum queue, `delivery-limit`,
at-least-once dead-lettering to a DLQ) and a core user without `configure`
or `write`, and checks: an event handled once however often it comes, the four
ways to the DLQ with nothing written, unrelated events, a reconnection after
the broker drops the connection, a missing queue that leaves the others alone
and is picked up once declared, a shutdown that returns the delivery in
hand to its queue, Keycloak's provider format (no id; the same body twice is
one event), and a consumer the broker cancels while both handlers are busy.
`cmd/core-backend` tests which Keycloak events wake the user sync.

## Against the standard

What matches: RabbitMQ's data safety guidance (manual acknowledgements after
the work is committed, a bounded prefetch, an idempotent consumer, no
immediate requeue on a quorum queue), the idempotent consumer pattern
(microservices.io: the message id recorded in the same transaction as the
business change), CloudEvents' `source` + `id` uniqueness, Azure Service
Bus' model of a bounded delivery count with a dead-letter queue, and for
`core.keycloak` Microsoft Entra provisioning's model: a periodic sync is the
truth, a change notification only brings the next pass forward.

Deviations: the retries are in the process, not a delayed redelivery (no TTL
and DLX ring, no delayed-message plugin; ADR-0071 deviation 6); dropping
repeats is the consumer's, the broker has no duplicate detection window
(deviation 7); prefetch 20 is under RabbitMQ's suggested 100 to 300, for a
low volume with a database write each; `traceparent` follows CloudEvents'
Distributed Tracing extension, but core has no tracing (no OpenTelemetry
spans or collector): the trace id only joins log lines.

## Rolling back

`EVENTS_ENABLED=false` and a redeploy: core closes its connection and stops
consuming; the events wait in the queues. The table stays and is cleaned up as
before; the migration's down drops it, and the code before this change does
not read it.
