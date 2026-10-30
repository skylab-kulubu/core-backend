# Team structure report

`core-backend team-structure-report` reads the realm's Group tree and core's own records once, before team delegation is switched on (`TEAM_DELEGATION=on`, [team-delegation.md](team-delegation.md)), and lists what delegation would get wrong (sky_lab_genel `.scratch/ekip-yetki-devri`, ticket 03; SPEC §11). With `-seed-retired` it also writes to `retired_team_codes` the Team codes that are named somewhere but held by no Group ([team-codes.md](team-codes.md#retired-codes)), which is a condition for switching delegation on (review of #227, Y2).

It prints Group paths, Group ids, Team codes, role names and counts, never a person: section (a) counts members, it does not list them.

## Running it

On the production server, inside the core container, beside the server (working directory `/app`; the binary is not on `PATH`). Its environment already holds what it needs: core's Keycloak service account (`KEYCLOAK_*`), `DATABASE_URL`, `AUTHZ_PRIVILEGED_ROOTS` and the structure limits (`TEAM_SUBTEAM_DEPTH_LIMIT`, `TEAM_PATH_LENGTH_LIMIT`). No secret is typed.

```
core=$(sudo docker ps -q --filter label=com.docker.swarm.service.name=sky-lab-production-core-8qn0in | head -n1)
sudo docker exec "$core" ./core-backend team-structure-report
```

- Without `-seed-retired` nothing is written anywhere. The last line says so.
- `-codes source=path` gives the codes another system names, one per line. A byte order mark and anything after a `#` are dropped, blanks around the code trimmed, empty lines skipped. A line that is still no code in capitals (`^[A-Z][A-Z0-9-]{1,23}$`: `Web Lab`, `İZCİ`) is shown in (h) as skipped and never written: a new Group could not take it anyway, and a typo must not become a retired code for good. `source` is a lower-case label (`cms`, `forms`, `skylab-site`, `arge`) printed next to each code; `path` `-` reads the standard input (`docker exec -i`, once). Repeat it for each source. Core reads none of these systems itself: the CMS's Teams (inscribed's `teams` collection), Forms' templates' `ownerTeam`, and the sites' `TEAM_META` slugs come only from such lists. The CMS and Forms lists are not typed by hand: a read-only wizard takes them from those systems and runs this command with them. Without any list, `-seed-retired` says on standard error that only core's own codes were seeded.
- `-seed-retired` writes section (h)'s codes still to retire, reason `seeded`, no Group id. Give it the dry run's count with `-expect N` (the `to retire: N` of (h)), with the same lists: any other count, before the write or when the tree is read again inside the lock, writes nothing and exits 1. It takes the Group names lock (`core.group_names`, [team-codes.md](team-codes.md#the-lock)) and reads the tree again inside it, so a Group core creates meanwhile keeps its code; a create or rename on the server waits for it (at most 10 s, then `503`). The lock does not cover Keycloak's console: run it at a quiet time, with no Group work in the console. A code retired already, in any case, is not written again: a second run writes nothing new.

Exit 0: the report was read whole (and, with `-seed-retired`, the codes written), whatever it found. Exit 1: Keycloak or the database could not be read, or the write failed (nothing is written then). An incomplete Group list (a branch that answered 404 while the tree was read) is a failure, not a smaller tree. Exit 2: a flag or a setting is wrong.

Keycloak reads (all `GET`, the service account's `query-groups`, `view-users`, `view-clients`): the tree (`/groups`, `/groups/{id}/children`), the members of each section (a) Group (`/groups/{id}/members`, counted), and for the members' tree, every area, every Team and every Sub-team's Leader group the role mappings (`/groups/{id}/role-mappings`, `/groups/{id}/role-mappings/realm/composite`, composites by id). Database reads: `events`, `certificates`, `certificate_templates`, `certificate_template_bindings` (`scope = 'ownerTeam'`) and `retired_team_codes`.

## Sections

| | What | Why it matters |
|---|---|---|
| (a) | Groups named like a Privileged root's last segment (`ADMIN`, `YK`, `DK`) outside the roots, with direct member counts | Their members are not Privileged since ticket 01: rename them or map their roles (SPEC S12) |
| (b) | `LIDERLER`/`KOORDINATORLER` not right under a Team, or with Groups under them | Not a Leader group: no Leader is made by it |
| (b') | Names that, trimmed and in capitals, start like a Leader group's but are not one (`LIDERLER-ESKI`, `liderler`) | Only Privileged manages them and what is under them |
| (c) | Codes more than one Group holds, case and blanks aside (Leader groups and the Privileged roots aside) | A Leader could add someone to the other team's Group of the same name (`ownerLevels` matches by name). Delegation gate |
| (d) | Names outside `^[A-Z][A-Z0-9-]{1,23}$` | They stay; a rename follows the form |
| (e) | The members' tree, areas and Teams whose own role mappings make them Protected (a realm role, an admin or unclassified client role, an app role holding one), with the first reason and how many Teams are at or below | No delegation there or below. A realm role on `/UYELER` or an area protects every Team under it (#235 B1: no realm role is exempt) |
| (f) | Teams more levels below their top Team than `TEAM_SUBTEAM_DEPTH_LIMIT`, or with a longer path than `TEAM_PATH_LENGTH_LIMIT` | Moves and creates there are refused (`depth_limit`) |
| (g) | Top Teams without a Leader group | Delegation gives nobody their management; a Sub-team without one is managed by the Leaders above |
| (i) | Names with a `/` | The token carries the path unescaped: a Group `YK/BASKAN` under `/UYELER` reads as `/UYELER/YK/BASKAN`, marked when that is under a Privileged root or another Group's path. Delegation gate |
| (j) | The direct client role mappings of the members' tree, every area, every Team and every Sub-team's Leader group (top Teams' Leader groups aside), each with its class (`authz.ClassifyRole`: app, admin, unclassified); an app role that holds a protecting role as a composite says which. `!` marks admin, unclassified and such composites | Before delegation the list is clean or Yusuf leaves each `!` row knowingly (`cms:access`/`content:write` on a Site client, `client:admin`: ADR 0065 annex). Realm roles are in (e) |
| (h) | Codes core's records (and the `-codes` lists) name that no Group holds, compared without case and blanks, each with what names it (`core:event`, `core:certificate`, `core:certificate_template`, `core:certificate_template_binding`, or the list's source) and whether it is retired already | `-seed-retired` writes the ones to retire; list lines that are no code are shown as skipped |

The last lines are the delegation gate, `(c) n, (i) n` (both 0 or every row decided), and what the seed did.
