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
- **Protected group**: the Group or a Group above it maps a protected role:
  core's permission roles and `audit:read` (any client), any client's
  `client:admin`, any role of `realm-management`. Only Privileged manages
  it: `403 protected_group`. Role mappings are read per Group and kept 60 s;
  core's own role mapping writes drop them.

The target is read by its **Keycloak id** (`GET /groups/{id}` with its
`parentId`), and every Group above it the same way. The chain must agree
with the paths: each path is its parent's plus its own name, and no name
holds a `/`. Otherwise the request is `409 group_tree_mismatch` and logged
(ids and depth only). A Group named by its path or name is the Privileged
tool's shortcut: delegated, it is 403.

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
client-role routes stay Privileged's (Forms reads the list).

### Writes

A delegated write runs under the Team code lock (`teamcode.Ledger`, the
advisory lock creates and renames hold), with the caller's Groups and the
target read fresh from Keycloak inside it. Its refusals (`forbidden`,
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

`q` is at least 3 characters (400 otherwise). Core's active accounts whose
e-mail address, first name, last name or full name holds `q` (not the
school address, Sky number or username), at most 10, ordered by name, with
the address masked (`a***@example.com`). A `q` that is a whole address
answers only the accounts with that address, unmasked. Each item is
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
| a Group's protection | 60 s | renames, role mapping writes |
| the realm's names (`team_code_not_unique`) | 60 s | renames |
| `managedTeams` | 60 s (a failure 10 s) | core's membership writes of that person |
| a managed tree | 60 s | every core membership write |

Writes never use a kept caller or target: they read both fresh. A Group's
protection may be up to 60 s old, unless core itself changed the mappings.
