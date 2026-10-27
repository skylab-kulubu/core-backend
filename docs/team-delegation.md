# Team delegation

A Leader manages their Team and every Sub-team below it through core: they
read its Groups and rosters, add and remove members (SPEC
`.scratch/ekip-yetki-devri`, ADR 0065, ticket 04), and open Sub-teams,
change their codes and delete them (ticket 05, [Team structure](#team-structure)).
name the Leaders of the Sub-teams below theirs (ticket 06,
[Leaders](#leaders)) and move Sub-teams between the Teams they manage
(ticket 07, [Moving a Team](#moving-a-team)). Editing a Team's profile
comes in ticket 08. Keycloak is only ever called by core's service account; the decision
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
  It also turns on the Owner team inheritance below.

Turn it on only after ticket 01 is in production and the tree report
(ticket 03) shows no duplicate codes and no names with a `/` (security
review of #227, Y3), and after the report's `--seed-retired` has written to
`retired_team_codes` the codes the CMS's Teams, Forms' templates and the
sites' `TEAM_META` name without a Group (review of #227, Y2): core reads
none of those systems, so a Sub-team code is checked against them only
through that table ([team-codes.md](team-codes.md) rule 3). Roll back with `off` and a redeploy; the changes made
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
  | `skymail` | `skymail:access`, `skymail:templates:read` |
  | `frontend-main`, `frontend-arge`, `frontend-artlab`, `frontend-yildizjam`, `frontend-skydays` (the Site clients) | `content:read` |

  The broker's `read-token` left the list (ekip-yetkileri ticket 15,
  YETKI-MATRISI §6.4): no app reads it. `skycloud:access` stays
  unclassified until it is known whether it opens other people's files.
  The admin panel assigns exactly these roles from Uygulamalar
  ([app-assignments.md](app-assignments.md)).

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

## Owner team inheritance (Events)

ADR-0065 decision 2 (ekip-yetkileri ticket 12): with `TEAM_DELEGATION=on`,
a person directly in a Team's Leader group is a Leader (`LEADER`, not
`MEMBER`) of every Owner team below that Team. Every Owner team decision
reads it from `ownerLevels`: Events, Event days and Sessions, drafts, the
applications and tickets of the Event, door check-in, competitions,
competitors, certificates and certificate templates. With delegation off
nothing changes.

- The token names the caller's Groups, not where the Owner team sits, so
  core reads the realm's Groups with its service account
  (`identity.TeamTree`, `ListGroups`) and finds the one Group named after
  the Owner team, compared without case; the decision then matches the
  name exactly, as before. No Group or more than one: no inheritance.
- Both ends are Teams as delegation reads them (`authz.TeamOf`): an area's
  `KOORDINATORLER`, `/UYELER`'s Leaders and a Group under a Privileged root
  lead nothing below them. A Sub-team's Leader stays a `MEMBER` of the Team
  above; a sibling Team's Leader gets nothing.
- The tree is read only for a caller in some Team's Leader group whose own
  Team is not the Owner team. A failed read (Keycloak down, 5 s) answers no
  inheritance for 10 s: the Leader leads only their own Team, as before.
- `GET /v1/users/me/capabilities` lists those Sub-teams in `teams` with
  `levels: ["LEADER"]`, so the panel finds their decisions.
- More people read an Event's applicants (participants' personal data).
  The Team audit record does not cover Event decisions; whether it should
  is open (ADR-0065 "Açık").

## Team settings

`PUT /v1/groups/{id}/settings` `{"membersManageCompetitors": bool}` writes a
Team's own settings as its Group's attributes and answers them. The Team's
Leader, a Leader of a Team above (delegation on, `Manages`) and Privileged
write them; a protected Group is Privileged's; only a Team takes them (a
Leader group, an area or a root: `400`). Each change is a
`group.attributes` row with the old and new value, a refusal a `denied`
row, as every delegated write. The panel reads the values from the Group's
attributes (`GET /v1/groups/{id}`).

- `membersManageCompetitors` (`members_manage_competitors`, Yusuf
  2026-10-25): whether the Team's members add, edit and remove its Events'
  competitors. Unset or anything but `false` is `true`, the rule before;
  `false` keeps them to the Team's Leaders (and Leaders of a Team above,
  "Owner team inheritance") and Privileged. Members still read them, and a
  person still enters or leaves as a competitor themselves. The decision
  (`competitor.create/update/delete` in the capabilities too) reads the
  Owner team's own Group from the realm's Groups kept a minute
  (`identity.TeamTree`); core's own write forgets them (a read already
  running then is not kept, and the next decision reads again), a write
  elsewhere (another core, the Keycloak console) is seen within the minute.
  A tree that cannot be read is the default, `true`. Since the setting, a
  member's capabilities and competitor writes read the tree too, with
  delegation off as well: at most one realm Group read a minute, and the
  first request after the minute waits for it (5 s at most). The setting does not depend on
  `TEAM_DELEGATION`; with it off only Privileged writes it.

## Role rules

The role cleanup (sky_lab_genel `.scratch/rol-temizligi`, ticket 06; Yusuf, 2026-10-12: "Microsoft gibi"): access comes through Groups, and a role is mapped only where the rule allows it. These hold for every caller, Privileged included.

- **No direct role on a person (K1).** `POST /v1/users/{id}/client-roles` for a person answers `403 direct_role_not_allowed`, with a `detail`. A client's service account (Keycloak's `serviceAccountClientId`) still takes one. `DELETE /v1/users/{id}/client-roles` stays: the direct roles given before are taken off (F2). Switch: `DIRECT_USER_ROLES=off|on`, `off` when unset (refused); any other value stops startup; `on` gives the route back as it was, as the way back.
- **Role placement (K4, S5).** `PUT /v1/groups/{id}/client-roles` adds a client role only to:
  - a Privileged root itself (`/ADMIN`, `/UYELER/YK`, …), not a Group under one (a YK duty group);
  - a top Team's Leader group (`/UYELER/<area>/<team>/LIDERLER` or `KOORDINATORLER`), not a Sub-team's;
  - a top Team itself (`/UYELER/<area>/<team>`), an **app role only** (the app roles list above: Forms' seat `skyforms:access`, SkyMail's `skymail:access` and `skymail:templates:read`, …; ekip-yetkileri ticket 04, ADR 0065 addendum). Every member of the Team and its Sub-teams gets it, and the Team stays delegable. An admin role or an unclassified one there is `409 role_placement` with `reason` `top_team_admin_role`: it would make the Team Protected. The class is read by the role's name;
  - a role group: a leaf right under `/ROLLER` (`/ROLLER/<APP>-<ROLE>`, S4).

  Anywhere else the write is `409 role_placement`, with a `reason` member and a `detail`: `members_root` (`/UYELER`), `area`, `team` (a Sub-team), `top_team_admin_role`, `sub_team_leader_group`, `privileged_subgroup`, `roles_root` (`/ROLLER` itself), `roles_group_not_leaf` (a role group with Groups under it), `roles_nested`, `outside_role_places` (any other Group). Only an add is refused: a write that keeps or takes off mappings made before the rule goes through, so they can be cleaned. When the Group's mappings cannot be read, every role counts as an add. One exception: with `APP_ASSIGNMENTS=on`, a write that adds or takes off an app role of a Team, a Sub-team or a Leader group is `409 role_placement` with `reason` `use_app_assignments`, before the other rules; those app roles are the Uygulamalar page's ([app-assignments.md](app-assignments.md)), and a mapping read that fails refuses the write. Realm roles are not this route's (they leave in F3).
- **No Keycloak administration through core (K3).** A role mapping write (`PUT /v1/groups/{id}/client-roles`) or a direct role (`POST /v1/users/{id}/client-roles`, a service account included) that adds a `realm-management` role, or a client role that holds one as a composite at any depth (8 levels; deeper counts as one), is `403 keycloak_admin_role`, before the placement rule. A composite that holds a realm role counts as one too: core cannot read into realm roles (no `view-realm`), so it fails closed (realm roles leave in F3). Keycloak administration is given in Keycloak's console only. Core's Keycloak layer refuses a `realm-management` add on its own as well.
- **Role groups stay leaves, and roles stay in place on a rename.** `POST /v1/groups` under a role group (`/ROLLER/<X>`) is `409 role_placement` (`roles_group_not_leaf`). A rename that would move a Group's client roles from an allowed place to a refused one (a top Team's `LIDERLER` renamed into a Team) is `409 role_placement` with the new place's reason; a Group mapped against the rule from before renames as before. Core has no Group move route; Keycloak's console stays outside these rules (rol-temizligi ticket 17 watches it).
- **No membership change in a Keycloak administration Group (K3).** A Privileged add or remove in a Group that holds a `realm-management` role, or under one, mapped or inside a composite (a client role's or a realm role's), is `403 keycloak_admin_group`: such a Group is changed in Keycloak's console only, by its writers. While K3 holds (no `realm-management` role on any e-skylab Group) this never happens; it is defence in depth against the `manage-users` → administration Group escalation (rol-temizligi spec §1.2). A delegated request never reaches such a Group: its role is an admin role, so it is `protected_group` first. A Group that cannot be read fails the change.

Errors are RFC 9457 problems with `code` (and `reason` for placement). The panel (core-frontend, rol-temizligi ticket 07) drops "ekstra rol ekle" and shows `detail` on a refusal.

**Microsoft Entra.** Matches: access through groups (group-based assignment), administrative roles only on dedicated groups (role-assignable groups), app roles handed out by group membership, and a directory's admin roles kept off ordinary groups. Deviations:

1. **No direct app role on a person through these routes.** Entra lets an app role be assigned to a user or a group; here only to a Group (a service account aside). Since ekip-yetkileri ticket 15 the panel's app assignments ([app-assignments.md](app-assignments.md), `APP_ASSIGNMENTS`) give an app role, and only an app role, to a Team, a Sub-team, a Leader group or a person, with an audit row each, as Entra's "Users and groups" does; these routes and their rules stay as they are.
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
| `PUT /v1/groups/{id}/settings` | `Manages` (a Team only) | the Team's own settings, below |
| `POST /v1/groups/{id}/children` `{code, displayNameTr, displayNameEn?}` | `Manages` of the Team | a Sub-team ([Team structure](#team-structure)) |
| `PUT /v1/groups/{id}/code` `{code}` | `ManagesStrict` | the Sub-team's code |
| `DELETE /v1/groups/{id}` | `ManagesStrict` | the Sub-team and every Group under it, all empty |
| `GET /v1/groups/{id}/leaders` | `Manages` | the Team's Leaders and Coordinators ([Leaders](#leaders)) |
| `POST /v1/groups/{id}/leaders` `{userId, kind?}` | `ManagesStrict` | names a Leader; opens the Leader group |
| `DELETE /v1/groups/{id}/leaders/{userId}` | `ManagesStrict` | `{lastLeader}` |
| `GET /v1/groups/{id}/move-preview?parentId=` | as the move | what the move would do ([Moving a Team](#moving-a-team)) |
| `POST /v1/groups/{id}/move` `{parentId}` | `ManagesStrict` of the Team, `Manages` of the new parent | the Team with everything under it |

`GET /v1/groups`, `POST /v1/groups`, `PATCH /v1/groups/{id}` and the
client-role routes stay Privileged's (Forms reads the list).

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

### Team structure

ADR 0065 ticket 05 (S9, S10). The same three routes serve Privileged on
every Team (top Teams included) and a Leader with delegation on; every
check and the Keycloak write run under the Team code lock, the Groups read
again **by id** inside it (a Group named by a path or a name is 404 for
Privileged, 403 for a Leader). A refusal of a Leader is a `denied` row with
its code; a change is a `team.create`, `team.code` or `team.delete` row.

- **Open a Sub-team**: `POST /v1/groups/{id}/children`
  `{code, displayNameTr, displayNameEn?}` under a Team (an area, a root, a
  Leader group or a Group outside `/UYELER` is `400`). The code passes the
  name rule ([team-codes.md](team-codes.md)) and must be free everywhere;
  `LIDERLER`, `KOORDINATORLER` and every name that starts like them
  (`LIDERLER-ESKI`) are `422 reserved_name` here for everyone: a Leader
  group is made only by naming a Leader (ticket 06; review of #227, Y1).
  The display names (Turkish required, at most 80 characters each) are
  written as `display_name_tr`/`display_name_en` in the same Keycloak write.
  Answers `201` with the Group.
- **Change a code**: `PUT /v1/groups/{id}/code` `{code}`, a Team only
  (`400` otherwise). The same name rules; a Leader only on a Sub-team they
  manage strictly, never their own Team or a top Team. While core's
  records name the old code it is `409 code_in_use` with `records`, the
  kinds (`event`, `certificate`, `certificate_template`,
  `certificate_template_binding`), never a record. The old code is retired
  (`renamed`); every Group under the Team moves with it; every kept Group
  read (Group overage, delegation, the Owner team tree) is forgotten. The
  same code is a no-op `200`.
- **Delete**: `DELETE /v1/groups/{id}`, for a Team, takes every Group under
  it too, once all of them are Sub-teams or Leader groups, nobody is in any
  of them (Leader groups included: `409 not_empty` with `members` and
  `children`), none maps a role (`403 group_delete_protected`) or is
  protected, and core's records name none of their codes
  (`409 code_in_use` with `records`). Every code given up is retired
  (`deleted`), Leader groups' aside. The rest is [group-deletion.md](group-deletion.md).
- **Limits** (SPEC §4.5, every caller, Privileged too): at most
  `TEAM_SUBTEAM_DEPTH_LIMIT` (2) levels of Sub-teams below a top Team and
  paths of at most `TEAM_PATH_LENGTH_LIMIT` (128) characters, a code
  change's longer paths below included (`409 depth_limit`); at most
  `TEAM_SUBTEAMS_LIMIT` (20) Sub-teams per Team, Leader groups aside
  (`409 children_limit`).

The managed tree's `can` says it per Team: `subteams.write` (`Manages`),
`code.write` and `team.delete` (`ManagesStrict`).

### Leaders

ADR 0065 ticket 06 (S2: a Leader names no co-Leader of their own Team).
Privileged on every Team, a Leader (delegation on) on a Team they manage
strictly: the Leaders of the Sub-teams below their own. A top Team's
Leaders are Privileged's alone, and nobody changes the Leaders of their own
Team (adds a co-Leader, makes themselves one, leaves).

- `GET /v1/groups/{id}/leaders` (`Manages`): `[{…person, kind, groupId}]`,
  `LIDERLER` first, then `KOORDINATORLER`, each by username. A Leader sees
  the person's id, names, e-mail address and profile picture (SPEC §6.4).
- `POST /v1/groups/{id}/leaders` `{userId, kind?}`, `kind` `LIDERLER` (the
  default) or `KOORDINATORLER` (anything else `400`), a Team only (`400`
  otherwise). When the Team has no such Leader group, core opens it right
  under the Team, under the Team code lock (a `team.create` row): **the only
  way a Leader group is made once delegation is on**. With delegation on,
  `POST /v1/groups` and a rename through `PATCH /v1/groups/{id}` refuse a
  leader subgroup's name (`422 reserved_name`), Privileged's too, and no
  Group is created under a Leader group (`400`); off, they work as before
  (ticket 01's exception). The person is an active account (`404`
  otherwise); a delegated add keeps them within `TEAM_DELEGATION_GROUP_LIMIT`
  (`409 group_limit`) and never reaches a protected Leader group
  (`403 protected_group`). A repeat add changes nothing. `204`.
- `DELETE /v1/groups/{id}/leaders/{userId}`: the person leaves every Leader
  group of the Team they are directly in (`404` when none). Answers
  `{"lastLeader": true}` when the Team has no Leader or Coordinator left;
  the panel warns, and the Team stays managed by the Team above.

Each change is a `leader.add` or `leader.remove` row on the **Team** (its id
and path) with the person and `kind` in `after` or `before`; a refusal of a
Leader is a `denied` row. The team membership mail goes as for a member
(`Role` `leader` or `coordinator`, from the Leader group).

### Moving a Team

ADR 0065 ticket 07 (K4; GitHub's "maintainer of both teams" rule). A Leader
moves a Sub-team they manage strictly under a Team they manage: in effect
under their common upper Leader. Privileged moves any Team, a top Team to
another area too. `parentId` is a Keycloak id only (a name or path is
`400`). Every rule is checked under the Team code lock, both Groups and
the Groups above them read again by id; each refusal has its own code:

| Code | When |
|---|---|
| `403` (`protected_group`) | a Leader who does not manage both sides; a protected new parent |
| `409 privileged_root` | a Privileged root, a Group under one or above one moves, the new parent is one or under one, or the move would make one (a Group named `ADMIN` moved under `/UYELER`): **for every caller** (review of #227, Y4) |
| `409 invalid_move_source` | the Group is no Team (an area, `/UYELER`, a Leader group, a Group outside `/UYELER`); a Leader moves Sub-teams only |
| `409 cycle` | the new parent is the Group or under it (by path and by the parent ids) |
| `409 leader_group_target` | the new parent is a Leader group |
| `409 invalid_move_target` | the new parent is no Team (Privileged: nor an area) |
| `409 same_parent` | the Group is right under it already |
| `409 depth_limit`, `409 children_limit` | the Team or a Group under it would break the structure limits |

The move is Keycloak's own (`POST /groups/{parentId}/children` with the
existing Group's id; its id, members and role mappings stay), a `team.move`
row with the path and parent id before and after; every kept Group read is
forgotten. Delegation follows at once: the new parent's Leaders manage the
Team, the old ones no longer do.

`GET /v1/groups/{id}/move-preview?parentId=` takes the same decision and
answers, without a change: `allowed` and the `refusal` code of a rule the
move breaks; `oldPath`, `newPath`; `members`, the people in the Team and
every Group under it, each once; `rolesGained` and `rolesLost`, the client
roles (`client:role`) the Groups above give it after and before the move;
`newParentLeaders`. What else a move changes is in
[team-codes.md](team-codes.md#when-a-path-or-a-code-changes).

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
children}`. `can` has `group.read`, `members.write`, `leaders.write`
(`ManagesStrict`: the Team's Leader groups), `subteams.write`, `code.write`
`team.delete` ([Team structure](#team-structure)) and `team.move` (the
Team's side of a move). Privileged gets every top Team
of every area with everything below; a Leader (delegation on) the Teams
they lead, each once (a Sub-team they also lead sits under the Team above);
anyone else `[]`. Walked down by id; kept 60 s per person and dropped by
core's own membership writes.

## Caches

| What | Kept | Dropped by |
|---|---|---|
| a caller's Leader groups (reads) | 60 s | core's membership writes of that person, renames, role mapping writes |
| a Group's protection (reads only) | 60 s | renames, role mapping writes |
| the realm's names (`team_code_not_unique`) | 60 s | renames, Sub-team creates, code changes, deletions |
| `managedTeams` | 60 s (a failure 10 s) | core's membership writes of that person |
| a managed tree | 60 s | every core membership write |
| the realm's Groups (Owner team inheritance, Team settings) | 60 s (a failure 10 s) | core's renames, attribute writes and Team settings writes |

Writes never use a kept caller, target or protection: they read all three
fresh.
