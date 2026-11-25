# Keycloak console changes in the audit record

Keycloak's own console (and its Admin REST API) can change Groups, role
mappings and clients without core. The club keeps the console for
emergencies (grup-yonetimi GY3/GY4); this is the detection: with
`KEYCLOAK_CONSOLE_AUDIT=on`, core reads Keycloak's admin events back, writes
each change made outside core to the [Team audit record](team-audit.md), and
sends the [privilege notice](privilege-change-mail.md) and the
[membership mail](team-membership-mail.md) for it as it does for the same
change made in the panel. Nothing is undone.

## How it reads

- Every `KEYCLOAK_CONSOLE_AUDIT_INTERVAL` (default 1m, 15s to 10m) core asks
  `GET /admin/realms/{realm}/admin-events` (`realm-management/view-events`,
  which core's service account holds) for the events from where it stopped,
  oldest first, 200 a page, at most 10 pages a pass. Resource types:
  `GROUP`, `GROUP_MEMBERSHIP`, `REALM_ROLE_MAPPING`, `CLIENT_ROLE_MAPPING`,
  `CLIENT`, `CLIENT_ROLE`, `REALM_ROLE`.
- One core reads at a time (a Postgres session advisory lock); another
  core's pass is skipped (`outcome="locked"`).
- Each pass starts two minutes before where it stopped. Keycloak stamps an
  admin event when the change is made, before its transaction commits, so
  an event that commits after a later one was read would otherwise fall
  behind the cursor. What the overlap reads again is skipped: by the
  process's memory of the events it handled, and after a restart by the
  record's unique `keycloak_event_id` (a `duplicate`, no second row, no
  second mail).
- Where it stopped is `console_audit_cursor` (one row: the newest event time
  read, in epoch milliseconds, and the ids read at that millisecond). The
  first pass after the switch is turned on only sets it to now: the 30 days
  Keycloak already keeps are not read, so no old change is mailed.
- An event made by core's own service account (the `sub` of core's token)
  is skipped: core's write has its row already, naming the person. An event
  of a write that failed (`error`) is skipped.
- Keycloak keeps admin events 30 days (ADR-0062). A cursor older than 29 days
  (core off that long) logs a WARN and counts
  `skylab_console_audit_gap_total`: changes may have been missed.
- Keycloak unreachable, or a row that cannot be written: the pass stops,
  the cursor stays before the event, the next pass reads it again.

## What is written

Every row: `via` `console`, `actor_basis` `console`, `outcome` `ok`,
`occurred_at` Keycloak's time of the change, `actor_id` the subject Keycloak
names (an account of the master realm or of the club's), `client`
`keycloak:reconcile` (a reconciler, below), `keycloak:master` (the author
signed in to another realm) or `keycloak:realm`, `keycloak_event_id` the admin event's id (unique: an event
is written once, so reading it again writes and mails nothing). Never a
name, an e-mail address, an IP address, a user name or a client secret;
Keycloak's `details` are not read.

| Admin event | Row | Mail |
|---|---|---|
| `GROUP_MEMBERSHIP` `CREATE` / `DELETE` (`users/{u}/groups/{g}`) | `member.add` / `member.remove`, the Group and the person | the person's membership mail; for a Privileged Group the privilege notice (ADMIN and the root's members) |
| `*_ROLE_MAPPING` on a Group (`groups/{g}/role-mappings/…`) | `group.roles`: `after.roles` the roles mapped (`CREATE`), or `before.roles` those unmapped (`DELETE`); only the change, not the Group's whole mapping | the privilege notice to ADMIN |
| `*_ROLE_MAPPING` on a person (`users/{u}/role-mappings/…`), a service account's included | `user.roles`, the person as target, no Group | none yet: a WARN line without the person and `skylab_console_audit_direct_user_roles_total` (the notice's template has no wording for a direct role; the groups-only model has none) |
| `GROUP` `CREATE` / `UPDATE` / `DELETE` | `team.create` / `group.attributes` / `team.delete` with the Group's id, path and code | none |
| `CLIENT` | `keycloak.client`: the client's id and client id, the operation, and the kind of a sub-resource (its name only) | none |
| `CLIENT_ROLE`, `REALM_ROLE` (a role made, changed, deleted; composites added or removed) | `keycloak.role`: client, role, and the composites' names | none |

Role names are `client:role`, a realm role `:ROLE` (the notice shows
`realm:ROLE`); a client Keycloak cannot name any more is `?`. A Group gone
and not named by the event keeps the path `/` (unknown) and its notice is
not sent. Rows and notices go through the same writers as the panel's
([team-audit.md](team-audit.md), [privilege-change-mail.md](privilege-change-mail.md)):
the notice of a console change names the console ("Keycloak konsolu") and
keeps Keycloak's time.

## Reconcilers

The realm's reconcilers (e-skylab-keycloak `config/app-assignments.sh`,
`leader-app-roles.sh`, `dk-read-only.sh` and the others run from the release
wizards) write with kcadm: an operator of the master realm signs in through
the master realm's `admin-cli` client. The browser console signs in through
`security-admin-console`. An admin event names the client by its internal id
(`authDetails.clientId`, a UUID of the master realm that core cannot read),
so `KEYCLOAK_CONSOLE_AUDIT_AUTOMATION_CLIENTS` lists it; on the server:

```sh
docker exec <keycloak container> /opt/keycloak/bin/kcadm.sh get clients -r master -q clientId=admin-cli --fields id --config <kcadm config>
```

A change made through such a client is recorded with `client`
`keycloak:reconcile` and **sends no mail** (no membership mail, no privilege
notice, no direct-role warning: the reconcilers map service accounts' roles
by design; Yusuf, 2026-11-25). The management terms still hear of a
membership. A kcadm command typed by hand goes through the same client and
is recorded the same way. Without the setting a reconciler's changes are
recorded and mailed as the console's.

## Configuration

| Variable | Unset | Values |
|---|---|---|
| `KEYCLOAK_CONSOLE_AUDIT` | `off` | `on`, `off`; anything else stops core |
| `KEYCLOAK_CONSOLE_AUDIT_INTERVAL` | `1m` | a Go duration from `15s` to `10m` |
| `KEYCLOAK_CONSOLE_AUDIT_AUTOMATION_CLIENTS` | none | the reconcilers' client ids (UUIDs), comma separated (above); anything else stops core |

The startup line says `keycloak console audit: off (…)` or
`keycloak console audit: on (every 1m0s; GROUP,…)`. On without Keycloak (the
in-memory directory) it says OFF and reads nothing.

## Metrics (`/v1/metrics`, internal)

- `skylab_console_audit_enabled`
- `skylab_console_audit_events_total{outcome}`: `recorded`, `duplicate`,
  `skipped_self`, `skipped_error`, `unsupported` (an event the record does
  not keep, or one without an author), `write_failed`
- `skylab_console_audit_pass_total{outcome}`: `ok`, `failed`, `locked`
- `skylab_console_audit_gap_total`, `skylab_console_audit_direct_user_roles_total`
- `skylab_console_audit_last_success_timestamp_seconds`,
  `skylab_console_audit_lag_seconds`

No label names a person or a path.

`skylab_console_audit_lag_seconds` is the age of the newest event read: it
grows in a quiet period (nobody changes anything), so it is no alarm alone.
Alarm on `skylab_console_audit_last_success_timestamp_seconds` older than a
few intervals, or `pass_total{outcome="failed"}` rising.

## Guarantees

- A row is written once per admin event (the unique `keycloak_event_id`).
- The mail is queued after the row: if core stops between the two, the
  row stands and its mail is lost. Reading the event again finds the row
  and sends nothing. This is the panel's guarantee too (its mail is queued
  after Keycloak answered).
- An event older than Keycloak keeps (30 days) is gone: see the gap
  metric.

## Rollout and rollback

1. Release core (migration `20261126110000`, after `20261126101700`) with
   the switch off.
2. `KEYCLOAK_CONSOLE_AUDIT_AUTOMATION_CLIENTS` (the master realm's
   `admin-cli` id), then `KEYCLOAK_CONSOLE_AUDIT=on`. The first pass sets
   the cursor to now.
3. Check: add someone to a test Group in the console; within a minute the
   panel's audit tab shows a `via` `console` row.

Rollback: `KEYCLOAK_CONSOLE_AUDIT=off` (the cursor stays; turned on again
within 30 days, it goes on from there). **The down migration deletes the
console rows** (the record's narrowed checks cannot keep them), the cursor
and the notice marker; export them first if they must be kept. An older core runs on the
new schema: it writes none of the new values and sends a queued console
notice without the console's name.

## Compared with Microsoft Entra

- **Matches:** the directory's audit log keeps every change with its author
  (Entra audit logs, read through Graph `directoryAudits`); a privileged
  assignment tells the administrators and the person (PIM's "role
  assigned" notification); the record cannot be changed (the finish-only
  trigger).
- **Deviation 1:** the record is up to one interval late (Entra writes it
  with the change).
- **Deviation 2:** the console's write is not prevented, only recorded and
  told (GY4: prevention is policy, this is detection).
- **Deviation 3:** a master realm account is not named in the mail
  ("Keycloak konsolu"); its subject is in the record.
- **Deviation 4:** a role mapped to a person directly is recorded and logged
  but not mailed until the template has wording for it (PIM mails it).
- **Deviation 5:** the 30 days Keycloak kept before the switch was turned
  on are not imported.
