# Team codes: the names of new and renamed Groups

A Group's Keycloak name is its **Team code** (`WEBLAB`). Core uses it as a key: an Event's, a certificate's and a certificate template's Owner team, a template's Owner team binding, the CMS's Teams record and Forms' template match all name the code, and core's Leader and Owner team decisions read it from the group paths in the token. Whoever could give a Group any name could take another team's records, make themselves its Leader, or (before the Privileged roots, [authz-roles.md](authz-roles.md#privileged-roots-authz_privileged_roots)) make themselves Privileged. So every Group core creates or renames passes one rule, for every caller, Privileged included (`internal/teamcode`; spec: sky_lab_genel `.scratch/ekip-yetki-devri` §4.4, ticket 01).

Routes: `POST /v1/groups` (`name`, `parentId`) and `PATCH /v1/groups/{id}` when `name` is a new name. A `PATCH` whose `name` is the Group's own name, blanks aside, is no rename: the panel sends it with every save, and a name from before the rule stays as it is. Only a rename follows the rule.

## The rule

1. **Form.** `^[A-Z][A-Z0-9-]{1,23}$`: 2 to 24 ASCII capital letters, digits and hyphens, starting with a letter. No Turkish letters (`İ`, `Ş`, …), no look-alikes, no `/`, `_`, `~` or blanks. The team's Turkish name is its display name (`display_name_tr`).
2. **Reserved names**, compared without case: `ADMIN`, `YK`, `DK`, `UYELER`, `LIDERLER`, `KOORDINATORLER`, `TAKIMLAR`, `_DEFAULT`, `ROLLER`, the last segment of every Privileged root, and every Owner team the Event permission table names (`GECEKODU`). Three of them have one place where they are allowed:
   - a Privileged root's name, for the Group **at** that root (`/ADMIN`, `/UYELER/YK`, …, as `AUTHZ_PRIVILEGED_ROOTS` says);
   - `ROLLER`, for `/ROLLER` itself: the root of the role groups `/ROLLER/<APP>-<ROLE>` (sky_lab_genel `.scratch/rol-temizligi`, S4). No Team is named `ROLLER`, and nothing under `/ROLLER` is a Team (`TeamOf` answers nothing there: it is outside `/UYELER`), so no Leader group or delegation reaches it. Role mappings go only to its leaves ([team-delegation.md](team-delegation.md#role-rules));
   - `LIDERLER` and `KOORDINATORLER`, for a Group right under a **Team** (a Group at least three deep under `/UYELER` and not under a Privileged root, `teamcode.TeamOf`), with no Group under it. Until delegated Leader management (ticket 06) only a Privileged caller creates one.

   There the name only has to be free at that path: `/UYELER/ADMIN` sits beside `/ADMIN`, every Team has its own `LIDERLER`.
3. **Taken nowhere.** A new code is refused when, compared without case, any of these holds it:
   - the name of any other Group in the realm;
   - `events.owner_team`, `certificates.owner_team`, `certificate_templates.owner_team`, or `certificate_template_bindings.scope_key` with `scope = 'ownerTeam'`, archived and deleted rows included;
   - `retired_team_codes`.
4. **Privileged roots stay put.** A rename of a Privileged root, of a Group above one (`/UYELER`), or to a root's path is refused: its members would gain or lose Privileged with a name.

A rename that only changes case (`artlab` → `ARTLAB`) keeps the code: only the path must be free, and nothing is retired.

## Retired codes

`retired_team_codes(code, retired_at, group_id, reason)`. A rename writes the Group's old name there (`reason = 'renamed'`, with the Group's Keycloak id), in the same transaction as the check, committed only once Keycloak took the rename. A reserved old name (`LIDERLER`) is not written. A retired code is never given to a new Group, nor back to the Group that gave it up. Codes and Group ids only, no person, so the table has no retention limit and account erasure does not touch it. An empty-leaf deletion durably retires the code with reason `deleted` before asking Keycloak; a failed deletion can be retried but never frees the code ([group-deletion.md](group-deletion.md)). Ticket 03's report seeds the codes core's records name without a Group.

## The lock

Every create and rename runs its check and its Keycloak write in one transaction holding the Postgres advisory lock `core.group_names` (`pg_advisory_xact_lock`). Two creates of one name, on one core or two, cannot both pass the check; the second sees the first's Group. A core that dies mid-write gives the lock back with its connection.

The check reads every Group of the realm (`GET /groups` and `GET /groups/{id}/children`, the Group reads core already does) and the record tables above. A write that Keycloak took but whose commit failed answers 500 and its error names the code left unretired. When the database cannot give the lock or the check (`teamcode.ErrUnavailable`), Keycloak is not asked and the route answers 503 `audit_unavailable`, as the Team audit record does when it cannot open its row ([`team-audit.md`](team-audit.md)).

## Answers

RFC 7807 problems with a `code` member and a `detail` saying why:

| Status | `code` | When |
| --- | --- | --- |
| 422 | `invalid_name` | not a code (rule 1) |
| 422 | `reserved_name` | a reserved name outside its place (rule 2) |
| 409 | `name_taken` | another Group, a record or a retired code holds it (rule 3), or the path is taken |
| 409 | `privileged_root` | a rename would move or make a Privileged root (rule 4) |

A caller who may not manage Groups gets 403 first, whatever the name.

Empty-leaf deletion uses the same lock in two transactions: durable retirement first, then fresh checks and DELETE. Privileged member adds and Group role writes also take it, as delegated membership already does. This prevents a core write from populating a checked empty Group just before its deletion; see [group-deletion.md](group-deletion.md) for the failure and external-writer limits.
