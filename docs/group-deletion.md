# Deleting an empty Group

The management panel previously created Groups but offered no delete action,
and core had no Group DELETE route. `DELETE /v1/groups/{groupId}` now removes
an **empty leaf**, identified by its exact Keycloak id. A successful request is
204; another request for that id is 404, as for an unknown Group. It never
removes members, descendants, roles or domain records to make a deletion pass.

## Who and what

Core's existing `TypeGroup` / `Delete` authorization decides first, before a
directory read. This is the same Privileged `groups:manage` / `group.write`
route family as creating and editing a Group; a reader-only DK and a delegated
Leader cannot delete here. The delegated subtree feature of ADR 0065 ticket 05
remains separate.

A custom Group such as `/TANISMAYEMEGIGRUBU`, or an empty Team leaf, may be
deleted only after these fresh checks pass:

- No member and no subgroup. Remove or move them explicitly first.
- No live core Owner team reference: Events, certificates, certificate templates
  or Owner team template bindings, including archived/deleted records.
- No role mapped to the target, including app roles. No protected realm/admin/
  unclassified role on the target or any ancestor. The same GroupProtection
  classifier is used without its cache. A fresh role-visibility canary also
  requires a visible known admin client-role mapping on an existing Privileged
  root; a root’s realm role alone cannot prove client mappings are readable.
- The parent-id chain agrees with the paths, with no slash in a name or cycle.
- It is not a reserved/Leader Group, below a system/Leader container, a Privileged root, an ancestor or descendant
  of a Privileged root, an Area, or any Group in the `/ROLLER` tree.

The target's name alone never grants permission or makes it safe to delete.
The frontend uses core's `group.write`, asks for confirmation and displays core's
refusal inside that dialog. It neither retries automatically nor cascades.

## Audit, retirement and races

The existing AuditedDirectory opens **one pending `team.delete` record before
any retirement**. It records the id, path and code before deletion. No audit
row means no retirement and no Keycloak DELETE.

The existing `core.group_names` transaction advisory lock is taken twice:

1. Re-read the exact id, name, path, parent chain, protection, members, children
   and live references; retire the code with reason `deleted` and commit.
2. Re-read all checks again for that same id/name/path, then issue Keycloak's
   DELETE while holding the lock. A changed Group is refused; reload it first.

The first commit keeps the code unavailable even if Keycloak's result is lost
or the process stops. A retired code is deliberately distinct from a **live
reference**: retirement does not prevent retrying deletion of the same Group.
An unsuccessful deletion can therefore leave a live Group with a permanently
reserved code. Its existing members and access do not change. The record is
finished as `ok`/`failed`, or left `pending` when the outcome is uncertain, as
for other audited directory writes. No new state machine is introduced.

| Failure point | Group | Code | Audit / retry |
|---|---|---|---|
| Authorization or first guard | remains | no new retirement | refusal; fix the stated condition |
| Audit Begin | remains | no new retirement | 503; no directory mutation |
| First retirement commit fails | remains | possibly committed; never issue DELETE | error; inspect/retry |
| Between turns: member/child/role/ref/name/path changes | remains | retired | refusal; reload/fix condition, retry |
| Keycloak refuses DELETE | remains | retired | failed; same id may retry |
| Timeout, controlled abort or response loss | may be gone | retired | pending; read by id before retry |
| DELETE succeeds, second commit or audit Finish fails | gone | retired | pending/error; a new DELETE sees 404 |
| Success | gone | retired | ok; code cannot be reused |

Core creates/renames, delegated membership writes, and now Privileged member
adds and role mapping changes use this same lock. A create-child/member-add/
role-write cannot enter between the final check and DELETE; when deletion wins,
the waiting write sees the missing Group. No lock is nested. The in-process
queue and bounded PostgreSQL lock wait remain those of the existing ledger.
Overage and delegation caches are invalidated even for an uncertain DELETE.

Keycloak console/import operations do not take this core lock. Neither do other
domain writers that create Owner team records. Reference checks are fresh, but
are not a global transaction across those systems. The durable retired code
prevents a later Group from claiming that code even if such a reference races.

## Preferences and notices

Communication preferences use the Group id. The existing tree cache reflects
its removal within one minute; the startup/daily cleanup removes orphan rows
([communication-preferences.md](communication-preferences.md)). No preferences
are transferred to another Group and no account is erased. Since deletion
requires no members and changes no role mappings, it sends no membership or
privilege-change mail. Existing queued notices retain their existing contract.

## Validation and local operation

The tests cover authorization, structural and role protections, member/child/
reference refusals, malformed trees, audit unavailability, between-turn changes,
retry, code reuse and serialization of child/member/role writes. The Keycloak
adapter test checks the exact Admin REST DELETE endpoint with a local HTTP
fixture. PostgreSQL tests use the real ledger and audit tables, including
retirement surviving a timeout, refusal, controlled abort, second commit
cancellation and audit Finish failure. This is no claim of an actual OS crash
or a live Keycloak integration test.

CI uses the existing disposable PostgreSQL fixture. For a **fresh disposable
native database only**, `CORE_GROUP_DELETE_TEST_POSTGRES_URL` can replace that
fixture for `TestGroupDeletePostgresDurableRetirement`; migrations are applied.
Do not point it at an existing/shared database.

For the user's `TANISMAYEMEGIGRUBU`, deletion is a separate operational step:
verify its exact id/path, direct members, descendants, mapped/inherited roles
and Owner team references through authorized read-only access first. After the
backend and frontend changes are reviewed and released, use its Group card's
**Grubu sil**, confirm that same target, then verify the id returns 404 and its
`team.delete` audit outcome. A matching name alone is insufficient. No real
Group was read or deleted by the development tests, and no release is included
in these changes.
