# Team delegation

A Leader manages their Team and every Sub-team below it through core: they
read its Groups and rosters, and add and remove members (SPEC
`.scratch/ekip-yetki-devri`, ADR 0065, ticket 04). Creating, renaming,
moving and deleting Sub-teams and naming their Leaders come in tickets
05–08. Keycloak is only ever called by core's service account; the decision
is made in `internal/authz` (`Policy.Manages`, `ManagesStrict`,
`ManagesMembership`), from what core reads from Keycloak, never from the
caller's token.

## Switch: TEAM_DELEGATION

`TEAM_DELEGATION=off|on`, `off` when unset; any other value stops startup.

- **off**: no delegated way. Every Group route is Privileged's, exactly as
  before: a Leader gets the same 403, nothing is read for them and no row is
  written. `managedTeams` is `[]`; `GET /v1/users/me/managed-groups` answers
  `[]` to everyone but Privileged.
- **on**: a person who is not Privileged for Groups reaches the routes
  below for a Group they manage.

Turn it on only after ticket 01 is in production and the tree report
(ticket 03) shows no duplicate codes and no names with a `/` (security
review of #227, Y3). Roll back with `off` and a redeploy; the changes made
stay in Keycloak and in the audit record.

## Who manages what

- **Leader**: directly in a Team's `LIDERLER` or `KOORDINATORLER` (S3),
  as Keycloak says now (`GET /users/{id}/groups`, then each such Group by id
  for its parent). A token issued before the person left the Group grants
  nothing.
- **Team**: a Group at least three deep under `/UYELER`, not under a
  Privileged root, not a Leader group (`authz.TeamOf`). `/UYELER` and its
  areas (`/UYELER/ARGE`) are no Team.
- `Manages(G)`: Privileged (`groups:manage`), or delegation on, G a Team or
  a Team's Leader group, and the caller a Leader of G's Team or of a Team
  above it.
- `ManagesStrict(G)`: G a Sub-team whose parent the caller manages. A Leader
  never changes their own Team's Leaders.
- **Membership** of G: `Manages(G)` for a Team; `ManagesStrict` of its Team
  for a Leader group, refused with `leader_group_needs_parent` when the
  caller manages the Team but not strictly. Nobody makes themselves a
  Leader, names a co-Leader (S2) or leaves their Team without one.
- **Protected group**: the Group or a Group above it holds **any realm
  role** in effect (mapped, or inside a client composite: Keycloak hides a
  composite's realm roles from core's service account, so they are read as
  the Group's effective realm roles), or maps a client role that is **no
  app role** (see [Role classes](#role-classes)), or a composite app role
  that holds, at any depth (8 levels; deeper counts as protected), a realm
  role or a client role that is no app role. Only Privileged manages it:
  `403 protected_group`. A protection that cannot be read refuses the
  write. No realm role is exempt (Yusuf, 2026-10-12): a harmless one
  (`offline_access`) on `/UYELER` or an area makes every Team under it
  Privileged's alone; core logs each protected Group with its reason
  (`team delegation: group <id> is protected (realm role …)`), and ticket
  03's report lists the mappings. Production's Java-era realm roles (`USER`
  on `/UYELER`, a Team's own name on it, `<TEAM>_LEADER` on its Leader
  groups; measurement of 2026-10-12) therefore keep delegation shut until
  the role cleanup's F3 takes them off the groups (hub
  `.scratch/rol-temizligi`). **Prerequisite for switching delegation on:
  the role cleanup's F3** (no realm role mapped to `/UYELER`, an area or a
  Team). A write reads protection fresh; a read
  keeps each Group's answer 60 s.
- **Look-alike names**: a Group whose name, blanks trimmed and case
  folded, starts with `LIDERLER` or `KOORDINATORLER` without being one
  (`LIDERLER-ESKI`, `LIDERLER `, `liderler`) is managed by Privileged only,
  as is everything under it: paths read by substring elsewhere could take
  its members for Leaders.

The target is read by its **Keycloak id** (`GET /groups/{id}` with its
`parentId`), and every Group above it the same way. The chain must agree
with the paths: each path is its parent's plus its own name, and no name
holds a `/`. Otherwise the request is `409 group_tree_mismatch` and logged
(ids and depth only). A Group named by its path or name is the Privileged
tool's shortcut: delegated, it is 403.

### Role classes

ADR 0065 decision 10, addendum of 2026-10-12 (Yusuf: Microsoft Entra's
model). A client role is classed by its **(client, role) pair**, never by
its name alone (`authz.ClassifyRole`):

- **App roles** (Entra's app roles): what they grant is bound to the
  holder or to the holder's Teams, so a Leader may hand them out by
  changing a Group's membership. The list (`authz.AppRoles`):

  | Client | Roles |
  |---|---|
  | `core` | `certificate:issue`, `certificate:revoke`, `certificate:template:manage`, `certificate:binding:manage` |
  | `core` | `url:create`, `url:get`, `url:update`, `url:delete`, `url:access` |
  | `forms` | `skyforms:access`, `skyforms:form:manage`, `skyforms:response:manage`, `skyforms:feedback:manage` (screens only: forms-backend decides each form by its collaborators or `skyforms:*`) |
  | `broker` | `read-token` (one's own identity provider token) |
  | `skymail` | `skymail:access`, `skymail:templates:read` |
  | `frontend-main`, `frontend-arge`, `frontend-artlab`, `frontend-yildizjam`, `frontend-skydays` (the Site clients) | `content:read` |

- **Admin roles** (Entra's directory roles): every other client role. A
  Group that maps one, or under a Group that does, is a Protected group.
  The known ones (`authz.AdminRoles`): core's permission roles but
  `url:access`, `audit:read`, `users:read` and its service roles;
  `client:admin`, `content:write`, `cms:access` and `schema:sync` on any
  client; every role of `realm-management`; Forms' `skyforms:*`,
  `skyforms:feedback:*`, `skyforms:read-all` (DK's read-only role: every
  form and answer), `skyforms:account:erase`; SkyMail's
  `templates:write`, `lists:*`, `mails:*`, `system-mails:read`,
  `announcements:send`, `account:erase`; Place's `place:admin` and
  `place:moderator`; `skycms`'s `cms:account:erase`. A pair on both lists
  is an admin role. `cms:access` is named on its own, so a Site editor's
  Group stays protected even if Keycloak answered its composites without
  `content:write`.
- **Unclassified**: a role on neither list counts as an admin role
  (fail-closed). Each protection read that meets one logs
  `team delegation: client role <client>:<role> is unclassified and counts
  as an admin role …` and counts it on `/v1/metrics` as
  `skylab_team_delegation_unclassified_roles_total{role="<client>:<role>"}`
  (role names only, at most 64, the rest as `other`).

**Realm roles** are never classified: every one protects, and
`authz.ClassifyRealmRole` answers `admin` for each (for callers that name a
role's class, such as the membership notice).

A composite app role is delegable only when every role it holds is too.
The same role name on another client is another pair: `content:read` on
the panel's `admin` client, or `skyforms:access` on the old `dotnet`
client, is unclassified. Changing either list is Yusuf's decision; a new
role stays protected until then. Keycloak roles flow down to subgroups
(Entra's app roles do not), so an ancestor's mapping protects the Groups
under it.

## Role rules

The role cleanup (sky_lab_genel `.scratch/rol-temizligi`, ticket 06; Yusuf, 2026-10-12: "Microsoft gibi"): access comes through Groups, and a role is mapped only where the rule allows it. These hold for every caller, Privileged included.

- **No direct role on a person (K1).** `POST /v1/users/{id}/client-roles` for a person answers `403 direct_role_not_allowed`, with a `detail`. A client's service account (Keycloak's `serviceAccountClientId`) still takes one. `DELETE /v1/users/{id}/client-roles` stays: the direct roles given before are taken off (F2). Switch: `DIRECT_USER_ROLES=off|on`, `off` when unset (refused); any other value stops startup; `on` gives the route back as it was, as the way back.
- **Role placement (K4, S5).** `PUT /v1/groups/{id}/client-roles` adds a client role only to:
  - a Privileged root itself (`/ADMIN`, `/UYELER/YK`, …), not a Group under one (a YK duty group);
  - a top Team's Leader group (`/UYELER/<area>/<team>/LIDERLER` or `KOORDINATORLER`), not a Sub-team's;
  - a top Team itself (`/UYELER/<area>/<team>`), an **app role only** (the app roles list above: Forms' seat `skyforms:access`, SkyMail's `skymail:access` and `skymail:templates:read`, …; ekip-yetkileri ticket 04, ADR 0065 addendum). Every member of the Team and its Sub-teams gets it, and the Team stays delegable. An admin role or an unclassified one there is `409 role_placement` with `reason` `top_team_admin_role`: it would make the Team Protected. The class is read by the role's name;
  - a role group: a leaf right under `/ROLLER` (`/ROLLER/<APP>-<ROLE>`, S4).

  Anywhere else the write is `409 role_placement`, with a `reason` member and a `detail`: `members_root` (`/UYELER`), `area`, `team` (a Sub-team), `top_team_admin_role`, `sub_team_leader_group`, `privileged_subgroup`, `roles_root` (`/ROLLER` itself), `roles_group_not_leaf` (a role group with Groups under it), `roles_nested`, `outside_role_places` (any other Group). Only an add is refused: a write that keeps or takes off mappings made before the rule goes through, so they can be cleaned. When the Group's mappings cannot be read, every role counts as an add. Realm roles are not this route's (they leave in F3).
- **No Keycloak administration through core (K3).** A role mapping write (`PUT /v1/groups/{id}/client-roles`) or a direct role (`POST /v1/users/{id}/client-roles`, a service account included) that adds a `realm-management` role, or a client role that holds one as a composite at any depth (8 levels; deeper counts as one), is `403 keycloak_admin_role`, before the placement rule. A composite that holds a realm role counts as one too: core cannot read into realm roles (no `view-realm`), so it fails closed (realm roles leave in F3). Keycloak administration is given in Keycloak's console only. Core's Keycloak layer refuses a `realm-management` add on its own as well.
- **Role groups stay leaves, and roles stay in place on a rename.** `POST /v1/groups` under a role group (`/ROLLER/<X>`) is `409 role_placement` (`roles_group_not_leaf`). A rename that would move a Group's client roles from an allowed place to a refused one (a top Team's `LIDERLER` renamed into a Team) is `409 role_placement` with the new place's reason; a Group mapped against the rule from before renames as before. Core has no Group move route; Keycloak's console stays outside these rules (rol-temizligi ticket 17 watches it).
- **No membership change in a Keycloak administration Group (K3).** A Privileged add or remove in a Group that holds a `realm-management` role, or under one, mapped or inside a composite (a client role's or a realm role's), is `403 keycloak_admin_group`: such a Group is changed in Keycloak's console only, by its writers. While K3 holds (no `realm-management` role on any e-skylab Group) this never happens; it is defence in depth against the `manage-users` → administration Group escalation (rol-temizligi spec §1.2). A delegated request never reaches such a Group: its role is an admin role, so it is `protected_group` first. A Group that cannot be read fails the change.

Errors are RFC 9457 problems with `code` (and `reason` for placement). The panel (core-frontend, rol-temizligi ticket 07) drops "ekstra rol ekle" and shows `detail` on a refusal.

**Microsoft Entra.** Matches: access through groups (group-based assignment), administrative roles only on dedicated groups (role-assignable groups), app roles handed out by group membership, and a directory's admin roles kept off ordinary groups. Deviations:

1. **No direct app role on a person at all.** Entra lets an app role be assigned to a user or a group; here only to a Group (a service account aside), so every grant has a Group, a membership mail and a review.
2. **Placement is by place in the tree, not by a group flag.** Entra marks a group role-assignable when it is created (`isAssignableToRole`) and the mark cannot change; here the rule reads the Group's path (Privileged root, top Leader group, `/ROLLER` leaf). A Group moved or renamed into a place gets its rights; core's name rule ([team-codes.md](team-codes.md)) reserves `ROLLER` for `/ROLLER` itself and keeps the roots from being moved or made by a rename.
3. **Keycloak roles flow down to subgroups** (Entra's do not), so a role on a Group with Groups under it is refused, and a Leader group takes roles only at the top Team. A top Team takes app roles on purpose: one mapping reaches the whole Team tree (Entra's nested groups would not get it).
4. **The administration blocks are ours.** Entra keeps directory roles off non-role-assignable groups by itself; Keycloak does not, so core refuses mapping administration roles and changing membership where they are, and the console's writers keep them.

## Forms' people picker

`GET /v1/users?q=…&clientId=forms&role=skyforms:access` is Forms' people picker (forms-frontend `useUser.js`, with the person's own `skyforms` token). The answer stays a plain array of the safe card (`id`, `email`, `firstName`, `lastName`, `profilePictureUrl`, `profilePictureSizes`, `status`, `displayName`).

- **With `users:read` or Privileged** (`users:manage`, a reader root): as before, every seat holder who matches, the full card for a full reader.
- **A member without `users:read`** (ekip-yetkileri ticket 04): a person (no service account, no product token) whose token's `groups` name a member's Group: a Team, a Sub-team or a Leader group (`/UYELER/<area>/<team>…`), or a Privileged root or a Group under one. `/UYELER` itself (where an account that signed itself up lands), an area, `/ROLLER` and anything else are no member: `403`, as is a token without `groups` (or an overage core could not read). For them:
  - only `role=skyforms:access` (any other role `403`); no `role` stays `403` (never the whole list);
  - `q` of at least three letters; a shorter one answers `200 []` without reading anything;
  - at most ten people, sorted by last and first name; matched by the names only, so a part of an address finds nobody; a query that is a whole address answers its holder with the address unmasked; every other answer masks the address (`a***@example.com`), as the candidate search does;
  - only members: a seat holder through a member's Group or a role group (`/ROLLER/FORMS-ACCESS`), or one whose own Groups make them a member (a seat through the default roles alone, on an account in `/UYELER` only, is not found);
  - 30 searches a minute per person, then `429 rate_limited` with `Retry-After: 60`.

  Switch: `SEAT_SEARCH_MEMBERS=on|off`, `on` when unset; `off` answers such a member `403` as before; any other value stops startup.
- **Every seat search**: the holders through a direct mapping (on a person, or on a Group and every Group under it, Keycloak's inheritance) are read whole and kept a minute, so a short query finds every one of them, not only those in Keycloak's first 50 matches. They are matched by core's profile names (a name changed in core only is found), Turkish letters folded (`ı`, `I`, `İ`, `i` match each other, so do `ş`/`s`, `ğ`/`g`, `ü`/`u`, `ö`/`o`, `ç`/`c`). People who may hold the seat through a composite or the default roles are found as before through Keycloak's search (its first 50) and core's profile search, and the first 50 of them that match have their effective roles read (kept a minute). A membership or role mapping written through core drops what is kept at once; one made in Keycloak's console shows within the minute.

**Microsoft Entra.** Entra lets a member enumerate every user and read their public properties; a guest cannot. Here a member finds only the seat holders who are members, ten at a time, by name, with the address masked, and an account outside the Teams (Entra's guest) finds nobody: stricter than Entra.

## Routes

| Route | Delegated rule | Notes |
|---|---|---|
| `GET /v1/users/me/capabilities` | everyone | `managedTeams` (docs/authz-roles.md), `can["audit.read"]` |
| `GET /v1/users/me/managed-groups` | everyone | the tree below |
| `GET /v1/groups/{id}` | `Manages` | the Group and its attributes |
| `GET /v1/groups/{id}/members` | `Manages` | the roster with Source groups; a Leader sees id, names, e-mail, profile picture (SPEC §6.4) |
| `GET /v1/groups/{id}/member-candidates?q=` | `Manages` | below |
| `POST /v1/groups/{id}/members` `{userId}` | membership of the Group | an active account (S4); limits below |
| `DELETE /v1/groups/{id}/members/{userId}` | `Manages` of the Group, membership of the Source group | the person leaves the Group they sit in |

`GET /v1/groups`, `POST /v1/groups`, `PATCH /v1/groups/{id}` and the
client-role routes stay Privileged's (Forms reads the list). So does
`DELETE /v1/groups/{id}`, which removes only an empty leaf under the stricter
[Group deletion guards](group-deletion.md); delegated subtree deletion remains
ticket 05.

### Writes

A delegated write has 10 s in all (past that it is 503), one at a time per
person (a second is `429` with `Retry-After: 1`; the minute's budget
answers `Retry-After: 60`). Before anything else it checks the **canary**:
a Privileged root that exists must read as protected (its core role
mappings). Keycloak answers a service account without `view-clients` an
empty role mapping with no error, so without it core would read every Group
as unprotected. When no root reads as protected, every delegated write is
`503 protection_unverified` and core logs an `ERROR team delegation:
protection cannot be verified …` line; a pass is kept 60 s. Core runs the
check at startup too when delegation is on. The caller's Groups,
the target and its protection are read fresh and decided first, outside
any lock: a caller who may not write (most often one who leads no Team of
the Group, or whose own account is no longer active) is refused there and
never waits for the lock or holds a database connection for it. Then the
write runs under the Team code lock (`teamcode.Ledger`, the advisory lock
creates and renames hold), with everything read fresh again inside it. A
core takes the lock for one request at a time: the others wait in the
process holding no connection, so queued writes never use up the pool;
the wait for another core's lock is bounded too (`lock_timeout` 10 s). Its refusals (`forbidden`,
`protected_group`, `leader_group_needs_parent`, `group_limit`,
`group_tree_mismatch`, `team_code_not_unique`) are recorded as `denied` rows
([team-audit.md](team-audit.md)); a change is recorded as Privileged's are,
with `actor_basis` `leader` and the Team that grants it. No row, no change:
503 `audit_unavailable`.

- `409 group_limit`: the add would put the person directly in more than
  `TEAM_DELEGATION_GROUP_LIMIT` Groups (25; token size, SPEC §4.5). A repeat
  add of a member is no new Group.
- `409 team_code_not_unique`: a Group on the way to the target (Leader
  groups aside) shares its name, without case, with another Group of the
  realm, so the member would also count in the other Team (Owner team
  decisions read names). Only Privileged writes there. The realm's names
  are read once a minute.
- `429 rate_limited` (`Retry-After: 60`): more than
  `TEAM_DELEGATION_WRITES_PER_MINUTE` (60) delegated writes of one person in
  a minute. Privileged has no budget.
- `404`: the person is no active account of core (erased, being erased,
  never signed in).

The team membership mail goes as before; its `LeaderName` ("İşlemi yapan")
is now whoever made the change, a Leader too.

### Member candidates

`q` is at least 3 characters (400 otherwise, checked after the caller is
allowed). Core's active accounts whose first name, last name or full name
holds `q`, at most 10, ordered by name, with the address masked
(`a***@example.com`). An address matches only whole: a `q` that is a whole
address answers only the accounts with that address, unmasked, and a part
of an address matches nothing, so the mask cannot be read back one letter
at a time. The school address, Sky number and username are never searched.
A Leader has 30 searches a minute (`429 rate_limited`); Privileged has no
budget. Each item is
`{id, firstName, lastName, email, profilePictureUrl?, alreadyMember}`;
`alreadyMember` is a direct membership of the Group.

### Managed tree

`GET /v1/users/me/managed-groups` answers a list of Team nodes:
`{id, code, path, displayName, memberCount, leaderCount, protected, can,
children}`. `can` has `group.read`, `members.write` and `leaders.write`
(`ManagesStrict`: the Team's Leader groups). Privileged gets every top Team
of every area with everything below; a Leader (delegation on) the Teams
they lead, each once (a Sub-team they also lead sits under the Team above);
anyone else `[]`. Walked down by id; kept 60 s per person and dropped by
core's own membership writes.

## Caches

| What | Kept | Dropped by |
|---|---|---|
| a caller's Leader groups (reads) | 60 s | core's membership writes of that person, renames, role mapping writes |
| a Group's protection (reads only) | 60 s | renames, role mapping writes |
| the realm's names (`team_code_not_unique`) | 60 s | renames |
| `managedTeams` | 60 s (a failure 10 s) | core's membership writes of that person |
| a managed tree | 60 s | every core membership write |

Writes never use a kept caller, target or protection: they read all three
fresh.
