# App assignments (Uygulamalar → Kullanıcılar ve gruplar)

The admin panel's Uygulamalar page decides which Team, Sub-team, Leader group
or person may use an app: SkyForms, SkyMail, the panel's own short links and
certificates, a site's content reading. It is Microsoft Entra's Enterprise
apps → an app → **Users and groups** → **Add user/group** → **Select a role**
→ **Assign** (and **Remove**), on Keycloak client roles
(sky_lab_genel `.scratch/ekip-yetkileri-20261022`, ticket 15; PLAN §1.1,
YETKI-MATRISI §6).

## Switch

`APP_ASSIGNMENTS`: `off` (the default) or `on`; any other value stops
startup. Off, none of the routes below is served (404) and
`GET /v1/users/me/capabilities` answers `apps.read` and `apps.assign`
`false` for everyone, so the panel shows no Uygulamalar page. Nothing else
depends on it.

## The catalogue

`config/app-assignments.json`, carried in the binary and checked at startup
(`authz.LoadApps`): per app its client, the name the panel shows,
`assignmentRequired` (Entra's "Assignment required?", shown read-only), the
roles the panel assigns and the ones its add drawer selects first (SkyMail's
`skymail:access` + `skymail:templates:read`, "send for approval"). The roles
are exactly core's app roles (`authz.AppRoles`, [team-delegation.md](team-delegation.md#role-classes));
`TestAppCatalogueIsAppRoles` fails when the two lists differ. Until ticket 03
writes e-skylab-keycloak's `config/app-assignments.json` this file is core's
own copy; then it is copied from there.

"Assignment required" is enforced by each app through its own seat role
(Forms' `skyforms:access`, SkyMail's `skymail:access`); Keycloak has no such
switch for an OIDC client. The panel only shows it; it is changed in code
(ticket 03).

## Routes

Every route is for a person (never a service account or a product token).

| Route | Who | Answer |
|---|---|---|
| `GET /v1/apps` | readers | each app: `clientId`, `name`, `assignmentRequired`, `roles`, `suggestedRoles`, and how many Groups (`groups`) and people (`users`) one of its roles is mapped to directly |
| `GET /v1/apps/:clientId/assignments?type=group\|user&role=` | readers | `{app, auditVisible, assignments}`: every direct mapping of the app's roles, Groups first by path, then people by name (below) |
| `POST /v1/apps/:clientId/assignments` `{type, id, role}` | writers | `201` with the new row; `200` with it when the target held the role directly already (nothing changes, no audit row) |
| `DELETE /v1/apps/:clientId/assignments` (same body) | writers | `204`; `404 not_assigned` when the target does not hold the role directly (no audit row) |
| `GET /v1/apps/:clientId/audit` | readers who hold `audit:read` | the app's last hundred `app.assign` / `app.unassign` rows, newest first: `occurredAt`, `action`, `outcome`, `errorCode`, `actor {id, name}`, `type`, `targetId`, `path` or `name`, `role` |
| `GET /v1/apps/:clientId/targets?type=group&q=` | writers | the Groups an app role may be assigned to whose path, code or Turkish name holds `q` (Turkish letters folded; empty `q` lists all), at most 20 by path, each with `inheritsTo`, `members` and the app's roles it holds directly (`assignedRoles`) |
| `GET /v1/apps/:clientId/targets?type=user&q=` | writers | people by the member-candidates rule: at least three letters (else `400`), at most ten, the address masked unless `q` is the whole address |

**Readers** are the `group.read` decision of a person (every Privileged
root's member in the groups mode, DK included; `groups:manage` or
`global:read` in the roles mode). **Writers** are the `group.write`
decision (ADMIN and YK in the groups mode; DK never, in any mode). Leaders
and members get `403`: in Entra the app's owner may assign too, here not yet
(PLAN §1.10, deviation 4).

An assignment row:

| Field | Holds |
|---|---|
| `type`, `id`, `role` | `group` or `user`, the Keycloak id, the app role |
| `area` | `panel`, or `code` for a mapping the panel cannot change (below), with its `reason` |
| `path`, `name` | a Group's path and Turkish display name (its code without one); a person's name |
| `email` | a person's primary address, whole (every reader is Privileged and reads full cards) |
| `inheritsTo`, `members` | a Group's: how many Groups are below it (Keycloak hands the role down to them) and how many enabled people it and they hold, each once; a person's `members` is 1 |
| `assignedBy`, `assignedAt` | the last `app.assign` that ended ok, when no later `app.unassign` did; `null` without one: made before the panel or in Keycloak's console, or `auditVisible` is false (the caller lacks `audit:read`) |

The counts are read from Keycloak (the Group, its subgroups and their
members, each once per answer, four at a time) and kept a minute per Group,
for the list and the add drawer's Group search alike: a list of SkyForms on
every top Team would otherwise read most of the Group tree on each opening.
A membership, Group role mapping or app assignment change through core drops
them at once; one made in Keycloak's console shows within the minute, as in
the seat search. Kept per replica.

## Rules

Checked in this order; every refusal from the target on is a `denied` row
(below).

1. The caller reads the assignments, else `403` with nothing read; the app
   is in the catalogue, else `404`; the body names a `type` of `group` or
   `user`, an id and a role, else `400` (there is no free-text field: a
   `note` an older client sends is ignored and not kept); the target exists,
   else `404`.
2. The caller changes the assignments, else `403` (DK: a `denied` row with
   basis `none`).
3. The role is one of the app's roles, else `409 role_placement` with
   `reason` `admin_role`: an admin role, one nobody has classified, or
   another app's.
4. On an add, the role is not and does not hold a Keycloak administration
   role (`realm-management`), else `403 keycloak_admin_role` (K3).
5. On an add, the role holds, as a composite at any depth, no realm role and
   no client role that is no app role (an admin role of its own app or of
   another, or one nobody has classified), else `409 role_placement` with
   `reason` `admin_role` and the composite in `detail`. Such a role would
   hand an admin right to a person directly (K1) or to a whole Team tree.
   The walk is the one that makes a Group Protected (`RoleProtection`,
   `GroupProtection`).
6. A Group target is a Team, a Sub-team, or a Leader group of either
   (`authz.AppAssignmentTarget`), else `409 role_placement` with `reason`:
   `code_area` (a Privileged root or a Group under one, `/ROLLER` or a Group
   under it), `members_root` (`/UYELER`: every member, a default role),
   `area`, `outside_role_places`.
7. A person target is no service account (`409`, `service_account`) and, on
   an add, is enabled in Keycloak and an active account in core when core
   has a row of them (`409`, `inactive_user`).

The change is then made under the Team code lock (no create, rename or move
runs between the checks and the write), with its audit row: no row, no
change (`503 audit_unavailable`). A Group's role is added or removed alone
(`AddGroupClientRole`, `RemoveGroupClientRole`); a person's as an extra role
(`AddUserExtraRole`, `RemoveUserExtraRole`).

The **code** area is what the reconciler owns (YETKI-MATRISI §6.1): the
Privileged roots, `/ROLLER`, service accounts. Its mappings of an app's roles
are listed (`area: code`) and the panel cannot take them off (`409`, same
reasons as above).

The older routes: `PUT /v1/groups/:groupId/client-roles` keeps ticket 04's
placement rule (a top Team takes app roles only, a Sub-team none) and its
`group.roles` row. With the switch on it changes no app role of a Group this
page assigns to (a Team, a Sub-team, a Leader group of either): a write that
adds one or takes one off is `409 role_placement` with `reason`
`use_app_assignments`, and nothing is written. Their other roles (a Leader
group's admin roles) and the code's places (a Privileged root, `/ROLLER`)
stay on it. So an app role of a Team has one way in and out, with its
`app.*` row, and a full list sent from an old tab cannot take a panel
assignment off unrecorded. Off, the route writes app roles as before.
`POST /v1/users/:id/client-roles` stays refused for a person
(`DIRECT_USER_ROLES` off, K1). A person gets an app role directly only here,
and only an app role: no person gets an admin role.

## Audit record

Each change is an `app.assign` or `app.unassign` row of the Team audit
record ([team-audit.md](team-audit.md)): actor, `actor_basis` `privileged`,
the target (a Group's id and path, or the person's subject with an empty
path), and in `after` the client and the role. There is no free text: an
Entra app role assignment has no reason field, and the record keeps no free
personal data (ADR 0065). A refused change is
a `denied` row with the refusal's code (`forbidden`, `role_placement`,
`keycloak_admin_role`). A repeat of an existing assignment and a removal of
a missing one write nothing. Kept three years like every row.

## What core forgets on a change

The write drops, even when it fails (it may have reached Keycloak), what
core keeps of role holders and protection: the seat search's minute (Forms'
people picker, ticket 04), team delegation's protection reads and the list's
Group counts. A person given Forms' seat is found by the picker at once.

## Not here

- The assignment mail to a person (GY05): `club.role-membership` speaks of a
  Privileged Group's management rights; an app assignment needs its own
  template variables (app, role) in SkyMail. A follow-up.
- "Assignment required" and the role definitions: in code (ticket 03).
- Access reviews: no review engine. YK looks at each app's people and Team
  assignments at the end of a season (GY13, ticket 15 §C); the "people only"
  filter (`type=user`) is the list a review starts from.
- A role change reaches the token at the next refresh (ticket 05).

## Microsoft Entra

Matches: one page per app; assignment to a group or a person, the group
first; the role is the app's own; "Assignment required" per app; each
assignment and removal in the audit log ("Add app role assignment to
group", "Remove app role assignment from user"); no free-text reason on an
assignment; admin roles apart, on role-assignable groups only. Deviations
(PLAN §1.10): a Keycloak subgroup inherits the assignment (Entra's nested
groups do not, 1); the app enforces "Assignment required", Keycloak issues
the token anyway (2); the setting is in code, not on a portal (3); only
ADMIN and YK assign, no app owner (4); a change made in Keycloak's console
is not in core's record (7); the row shows who assigned it and when (Entra's
"Users and groups" list does not; there it is read from the audit log); the
rows are kept three years (Entra's audit log keeps 7 days on Free, 30 on
P1/P2); the code's rows are locked on the page (Entra's portal does not stop
removing a role-assignable group's app assignment).
