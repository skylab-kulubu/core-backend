# Team audit record

Every change core makes to a Keycloak Group through its routes is written to
`team_audit_records` (migration `20261012121000`, package
`internal/teamaudit`, ADR 0065): who changed which Group, when, and how.
Keycloak's own admin events show every such change as made by
`service-account-core`; this record names the person.

## What is recorded

| Route | Action | Group of the row | `before` / `after` |
|---|---|---|---|
| `POST /v1/groups` | `team.create` | the new Group (its id once Keycloak answers) | after: path, parent id, code |
| `PATCH /v1/groups/:groupId` with a new name | `team.code` | the Group | code and path; the attributes that differ |
| `PATCH /v1/groups/:groupId` otherwise | `group.attributes` | the Group | the attributes that differ |
| `POST /v1/groups/:groupId/members` | `member.add` | the Group | none; `target_user_id` is the person |
| `DELETE /v1/groups/:groupId/members/:userId` | `member.remove` | the Group the membership is removed from: through a parent roster, the Source group the person sits in | none; `target_user_id` is the person |
| `PUT /v1/groups/:groupId/client-roles` | `group.roles` | the Group | the role mappings before and after, as `client:role` |

All of these are Privileged's routes: `actor_basis` is `privileged`. The
other actions the table accepts (`leader.add`, `leader.remove`,
`team.profile`, `team.move`, `team.delete`) and the `leader` basis are for
the delegated routes (SPEC tickets 04–08). Reads are not recorded. A change
made in Keycloak's own console is not recorded either: core does not see it.

## Fields

| Column | Holds |
|---|---|
| `id`, `occurred_at` | the row; when the write began |
| `actor_id` | the caller's Keycloak subject (`sub`). No name, no e-mail |
| `actor_basis`, `actor_via_group_id` | `privileged`; or `leader` with the Team whose Leader group grants the change |
| `action` | one of the eleven above (CHECK) |
| `group_id`, `group_path` | the target Group's Keycloak id and its path at the time. A `team.create` has no id until Keycloak answers |
| `target_user_id` | the person of a member or leader action, as a subject; set exactly for those (CHECK) |
| `before`, `after` | the Group's side of the change (below) |
| `outcome`, `error_code` | `pending`, `ok`, `failed` or `denied`; a code exactly for `failed` (`not_found`, `invalid`, `forbidden`, `keycloak_<4xx status>`) and `denied` |
| `correlation_id` | the request's id (`X-Request-ID`); empty when the caller sent one that is not a plain token |
| `client` | the client the caller's token was issued to (`azp`) |

`before` and `after` hold only a Group's id, path, parent id, code (its
Keycloak name), role names, and the values of the recorded attributes:
`display_name_tr/en`, `description_tr/en`, `logo_media_id`,
`public_listing`, `public_leaders`, `team_door_scan`. Of any other attribute
that changed only the name is kept (`otherAttributes`). Texts are cut to 280
characters, codes and ids to 255, and each column to 32 KiB. No name, e-mail
address, phone number, IP address or other personal field is written; the
column comments say so. A Team's display names and descriptions are free
text its administrators typed, and are kept as typed: they are Team fields,
not a person's, but one may happen to mention someone, and account erasure
does not look into them.

`before` is read just before the write, outside any lock but the Team code
lock of a create and a rename: when two people change the same Group's
attributes, members or roles at the same moment, a row's `before` may
already be out of date.

## Writing

1. The route reads what the row needs from Keycloak (the Group's path; for a
   change, its old name, attributes or role mappings).
2. core opens the row as `pending` (at most 3 seconds).
3. Only then does it ask Keycloak for the change.
4. It writes the outcome: `ok`, or `failed` with the code of Keycloak's
   refusal.

`failed` means Keycloak answered and refused: an unknown Group or person, an
invalid write, a 4xx. It made no change.

**`pending` means the outcome is unknown**: Keycloak may or may not have
made the change; look in Keycloak. A row stays pending when

- the write failed in a way that does not tell: a timeout, a request given
  up (the caller went away), a lost connection, a Keycloak 5xx. core logs
  `team audit: record <id> stays pending: outcome unknown (<timeout|canceled|directory_error|keycloak_5xx>)`
  and the route answers with the error as before;
- the outcome could not be written (the database went away after step 2):
  `team audit: record <id> stays pending: <code>`; the change stands;
- core stopped between steps 2 and 4.

If the row cannot be opened (the database is down, the table refuses it, or
the request carries no actor) the change is **not** made and the route
answers `503` with `code: audit_unavailable`, `Cache-Control: no-store` and
`Retry-After: 1`: without a record there is no change. A create and a rename
first take the Team code lock in the same database ([`team-codes.md`](team-codes.md));
when the database cannot give it, they answer the same 503 and Keycloak is
not asked.

A row changes once: a database trigger lets only a `pending` row turn into
its outcome (and a create get its Group id); nothing else updates a row.

`denied` rows (a write that delegation refuses) are written by
`teamaudit.Writer.Deny`; the delegated routes use it (ticket 04).

## Who sees it

The record has no reading route yet. `GET /v1/team-audit-records` (ticket
10) opens it to the `audit:read` role of core's client, which Keycloak maps
to the ADMIN and YK roots (ticket 09). The role is read directly in every
authorization mode, so DK and Leaders do not see the record. Names are read
from core's `users` when a row is shown; an erased person shows as
"Silinmiş kullanıcı".

## Retention and account erasure

- **Three years** (S6), after which the retention sweep's `team_audit_age`
  rule deletes the row, whatever its outcome
  ([`retention-sweep.md`](retention-sweep.md)). The sweep runs once a day,
  and only when `RETENTION_SWEEP_MODE` is `dry-run` (counts) or `apply`
  (deletes).
- **Account erasure** neither deletes nor changes a row, whether the person
  was the actor or the target ([`data-lifecycle.md`](data-lifecycle.md)).
  The rows hold only the subject id, which resolves to nobody once the
  Keycloak account is gone; there is nothing personal to clear. The
  precedent is the access log of private Media.

## Legal basis

Legitimate interest (KVKK art. 5/2-f): being able to account for
administrative changes to teams and roles and to look into their misuse. The
period follows ADR-0062's access-record precedent (three years). The
privacy notice and ADR-0062's retention table get a line for this record
(ADR 0065, consequences).
