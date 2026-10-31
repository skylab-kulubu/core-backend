# Team audit record

Every change core makes to a Keycloak Group through its routes is written to
`team_audit_records` (migration `20261012121000`, package
`internal/teamaudit`, ADR 0065): who changed which Group, when, and how.
Keycloak's own admin events show every such change as made by
`service-account-core`; this record names the person.

## What is recorded

| Route | Action | Group of the row | `before` / `after` |
|---|---|---|---|
| `DELETE /v1/groups/:groupId` | `team.delete` | the empty leaf Group, or a Team deleted with every Group under it | before: id, path, code; one pending row opens before durable code retirement ([group-deletion.md](group-deletion.md)) |
| `POST /v1/groups/:groupId/children` | `team.create` | the new Sub-team (its id once Keycloak answers) | after: path, parent id, code, display names |
| `PUT /v1/groups/:groupId/code` | `team.code` | the Team | code and path |
| `POST /v1/groups/:groupId/leaders` | `leader.add` | the Team (not its Leader group) | after: the Team's id and path, `kind` (`LIDERLER` or `KOORDINATORLER`); `target_user_id` is the person. A Leader group opened for it is a `team.create` row first |
| `DELETE /v1/groups/:groupId/leaders/:userId` | `leader.remove` | the Team | before: as `leader.add`; one row per Leader group the person leaves |
| `POST /v1/groups/:groupId/move` | `team.move` | the Team, at its old path | id, path, parent id and code before and after |
| `POST /v1/groups` | `team.create` | the new Group (its id once Keycloak answers) | after: path, parent id, code |
| `PATCH /v1/groups/:groupId` with a new name | `team.code` | the Group | code and path; the attributes that differ |
| `PATCH /v1/groups/:groupId` otherwise | `group.attributes` | the Group | the attributes that differ |
| `POST /v1/groups/:groupId/members` | `member.add` | the Group | none; `target_user_id` is the person. An add to a Privileged Group of someone not YTÜ-linked: after `{"unverifiedAck": true\|false}` (below) |
| `DELETE /v1/groups/:groupId/members/:userId` | `member.remove` | the Group the membership is removed from: through a parent roster, the Source group the person sits in | none; `target_user_id` is the person |
| `PUT /v1/groups/:groupId/client-roles` | `group.roles` | the Group | the role mappings before and after, as `client:role` |
| `POST /v1/apps/:clientId/assignments` ([app-assignments.md](app-assignments.md)) | `app.assign` | the Group, or none for a person (`target_user_id`, empty `group_path`) | after: the Group's id and path, `client`, `role` |
| `DELETE /v1/apps/:clientId/assignments` | `app.unassign` | as `app.assign` | as `app.assign` |
| `POST /v1/management-memberships/:termId/extend` ([management-membership-terms.md](management-membership-terms.md)) | `membership.term.extend` | the term's Group | before and after: `endsAt`; `target_user_id` is the holder |
| `POST /v1/management-memberships/:termId/handover` | `membership.term.handover` | the term's Group | before: the holder's `endsAt`; after: the successor's `endsAt`, `successorId` and `handover` (`immediate` or `at_term_end`); `target_user_id` is the holder. The add and the removal it makes write their own `member.add` and `member.remove` rows |
| the daily term job | `membership.term.expired` | the term's Group | after: `endsAt`; `target_user_id` is the holder; actor `system` (below) |

Privileged's writes carry `actor_basis` `privileged`. With team delegation
on ([team-delegation.md](team-delegation.md)), a Leader's member add and
remove carry `leader` and, in `actor_via_group_id`, the Team whose Leader
group grants it (the nearest one at or above the Group). A Leader's Sub-team create, code change and deletion
(ticket 05, [team-delegation.md](team-delegation.md#team-structure)) carry
it too. So do a Leader's `leader.add` and `leader.remove` (ticket 06,
[team-delegation.md](team-delegation.md#leaders)) and `team.move` (ticket
07). `team.profile` is for ticket 08.
Reads are not recorded. A change made in Keycloak's own console is not
recorded either: core does not see it.

## Fields

| Column | Holds |
|---|---|
| `id`, `occurred_at` | the row; when the write began |
| `actor_id` | the caller's Keycloak subject (`sub`). No name, no e-mail |
| `actor_basis`, `actor_via_group_id` | `privileged`; or `leader` with the Team whose Leader group grants the change; or, on a `denied` row only, `none` (the caller leads no Team of the Group); or, on a `membership.term.expired` row only, `system`: core's daily job, with the nil subject (`00000000-…`) as `actor_id` and `system:term-expiry` as `client` (migration `20261027140100`) |
| `action` | one of the sixteen above (CHECK) |
| `group_id`, `group_path` | the target Group's Keycloak id and its path at the time. A `team.create` has no id until Keycloak answers; an `app.*` row for a person has neither (empty path) |
| `target_user_id` | the person of a member or leader action, as a subject; set exactly for those, and on an `app.*` row exactly when it has no Group (CHECK, migration `20261023120000`) |
| `before`, `after` | the Group's side of the change (below) |
| `outcome`, `error_code` | `pending`, `ok`, `failed` or `denied`; a code exactly for `failed` (`not_found`, `invalid`, `forbidden`, `keycloak_<4xx status>`) and `denied` |
| `correlation_id` | the request's id (`X-Request-ID`); empty when the caller sent one that is not a plain token |
| `client` | the client the caller's token was issued to (`azp`) |

`before` and `after` hold only a Group's id, path, parent id, code (its
Keycloak name), role names, the `kind` of a leader action, the
`unverifiedAck` of an unverified add
(below), and the values of the recorded attributes:
`display_name_tr/en`, `description_tr/en`, `logo_media_id`,
`public_listing`, `public_leaders`, `team_door_scan`,
`members_manage_competitors`, `team_competition_manage`. Of any other attribute
that changed only the name is kept (`otherAttributes`). Texts are cut to 280
characters, codes and ids to 255, and each column to 32 KiB. No name, e-mail
address, phone number, IP address or other personal field is written; the
column comments say so. A Team's display names and descriptions are free
text its administrators typed, and are kept as typed: they are Team fields,
not a person's, but one may happen to mention someone, and account erasure
does not look into them. An `app.*` row has no free text: the client and
the role only.

`before` is read just before the write, outside any lock but the Team code
lock of a create and a rename: when two people change the same Group's
attributes, members or roles at the same moment, a row's `before` may
already be out of date.

## Unverified account (grup-yonetimi 07)

Adding a person who is not YTÜ-linked (`users.ytu_linked`, CONTEXT
"YTÜ-linked"; a person core has no account of is not) to a Privileged root
or a Group under one (`AUTHZ_PRIVILEGED_ROOTS`) is **never refused**: a
graduate on the board may have no YTÜ account any more (GY9). The panel
warns instead, and the row keeps what the caller confirmed:

- `POST /v1/groups/:groupId/members` takes an optional `unverifiedAck`
  beside `userId`. For such an add, the row's `after` is
  `{"unverifiedAck": true}` when the request said so and
  `{"unverifiedAck": false}` when it did not. Any other add (a YTÜ-linked
  person, an ordinary Group) has no `after`, whatever the request says.
- `GET /v1/groups/:groupId/member-check/:userId` answers, for whoever may
  read the Group (Privileged, or a Leader who manages it), what the add
  would warn of, and writes nothing:

  ```json
  {"privileged": true, "ytuLinked": false, "warnings": ["not_ytu_verified"]}
  ```

  `warnings` is always an array: `["not_ytu_verified"]` exactly when the
  Group is Privileged and the person is not YTÜ-linked, `[]` otherwise.
  The panel learns from `privileged` whether a Group is Privileged, never
  from its name.
- `GET /v1/groups/:groupId/member-candidates` gives each candidate
  `ytuLinked` and the same `warnings`.

The mark is a decision on core's own flag, not on Keycloak's federated link
to YTÜ (CONTEXT "Verified YTÜ account"); in practice they are the same
people.

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
`teamaudit.Writer.Deny` from the delegated member routes (ticket 04), one
per refused write, with the refusal's code: `forbidden`, `protected_group`,
`leader_group_needs_parent`, `group_limit`, `group_tree_mismatch`,
`team_code_not_unique`, `team_code_privileged_only` (a Leader's code change
while `TEAM_DELEGATION_LEADER_CODES` is off). A Leader refused for a Group of their own Teams
(protected, a Leader group) keeps `leader` and their Team; anyone else is
`none` (migration `20261012150000`). Not recorded: a refusal with
delegation off (the route is Privileged's as before), a Group id Keycloak
does not know, a person who is no active account (404) and a write over
the minute's budget (429), which reads nothing. A denied row that cannot
be written is logged and the request is still refused.

## Who sees it

Only the `audit:read` role of core's client reads the record. Keycloak maps
it to the ADMIN, YK and DK roots (ticket 09; DK since grup-yonetimi GY1,
which replaced S11's "not DK": DK reads and changes nothing,
[authz-roles.md](authz-roles.md#privileged-readers-dk)). The role is read
directly in every authorization mode (`authz.TypeTeamAudit`): a Privileged
Group without it does not see the record, nor does `global:read`, nor does a
service account, nor do Leaders, not even their own Teams' history (S7, first
release). `can["audit.read"]` in `GET /v1/users/me/capabilities` is this same
decision. Team delegation's switch (`TEAM_DELEGATION`) does not change it:
the record is read whether delegation is on or off.

The app assignments read their own rows: `GET /v1/apps/:clientId/audit`
(the app's last hundred) and the "who, when" of each assignment, both only
for a caller who also holds `audit:read`
([app-assignments.md](app-assignments.md)).

### `GET /v1/team-audit-records`

The record's reading route (ticket 10, SPEC §8). Every filter is optional
and they combine:

| Query | Meaning |
|---|---|
| `groupId` | the row's Group (a Keycloak id) |
| `subtree=true` | with `groupId`: the Group and every Group under it now, and every row whose path at the time was the Group's path now or under it, so a Sub-team deleted since stays in its Team's history. For a Group that is gone, its own rows and the rows under the path of its newest row |
| `actorId` | who made the change (a subject) |
| `userId` | the person the row is about (`target_user_id`) |
| `action` | one or more actions: repeated (`action=member.add&action=member.remove`) or comma-separated |
| `outcome` | `pending`, `ok`, `failed` or `denied` |
| `from`, `to` | RFC 3339; `from` inclusive, `to` exclusive |
| `limit` | 1 to 100, default 100 |
| `cursor` | the `next` of the page before |

`subtree=true` matches a deleted Sub-team's rows by the path they were
written under. Once the Team above it is renamed (`team.code`) or moved
(`team.move`), that path is no longer under the Team's path now, so those
rows drop out of the Team's subtree; a Group that later takes the old path
picks them up instead. They stay readable without the filter or by the
deleted Group's own `groupId`. Every reader holds the club-wide
`audit:read`, so this is history missing or misplaced in one view, never a
row shown to someone who could not read it otherwise.

Rows come newest first (`occurred_at`, then `id`, both descending). The
answer is `{"items": [...], "next": "<cursor>" | null}`; `next` is null on
the last page. A filter the route cannot read is `400`, but only for a
caller who may read the record (anyone else is `403` first). Every answer
is `Cache-Control: no-store`.

Each item:

| Field | |
|---|---|
| `id`, `occurredAt`, `action`, `outcome`, `errorCode`, `correlationId` | the row's |
| `actor` | `{id, displayName, basis, viaGroupId, client}`: `basis` is `privileged`, `leader` (with `viaGroupId`, the Team whose Leader group granted it), `none` (a denied row) or `system` (core's job, nil id, `client` `system:term-expiry`); `client` is the token's `azp` |
| `target` | `{id, displayName}` of a member, leader, term or person app row; `null` otherwise |
| `successor` | `{id, displayName}` of a `membership.term.handover` row: `after.successorId`, named; `null` otherwise |
| `group` | `{id, path, currentPath}`: `path` at the time; `currentPath` the Group's path now while it exists, `null` once it is gone. `null` for an app row about a person |
| `before`, `after` | as the row holds them (the table above); `null` when empty |

Names are read from core's `users` when the page is built: a person whose
account is erased, blocked or marked for deletion shows as "Silinmiş
kullanıcı", a subject core holds no row of as an empty `displayName`. The
answer holds nothing else of a person: no address, no number, no username
(SPEC §6.4 is the delegated member list's, not this route's).

Every read, a refused or malformed one too, is one JSON log line:
`{"event":"team_audit_read","actor":"<sub>","client":"<azp>","filters":["groupId","action",...],"outcome":"ok|forbidden|invalid|error","rows":N}`.
It names the kinds of filter the query carries, never their values. Reads
themselves are not rows of the record.

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
