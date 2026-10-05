# Privileged as roles, and what the caller may do

ADR-0059 (e-skylab) splits what being Privileged (a member of `ADMIN`, `YK` or `DK`, or of a subgroup) allows into per-resource client roles of core's Keycloak client `core`. Keycloak maps the roles once to whichever of the six Privileged group paths exist (`/ADMIN`, `/UYELER/ADMIN`, `/YK`, `/UYELER/YK`, `/DK`, `/UYELER/DK`; spec, ticket 03; e-skylab-keycloak, `KEYCLOAK_RECONCILE_ONLY=core-roles`), and subgroups inherit them; from then on the SKY LAB admin UI owns the mappings. Tokens carry the roles in `resource_access.core.roles`. Leader and Owner team decisions stay on group paths (ADR-0014).

Every Privileged check is in `internal/authz` and goes through `authorizer.privileged(p, role)`, or for short links and form links, whose roles are read on their own, `authorizer.privilegedGroupOnly`. No other code reads a Privileged Group from a token.

Core's Privileged Group test (`privilegedGroup`, unchanged) matches a group named `ADMIN`, `YK` or `DK` anywhere in the tree, not only the six paths above: a member of, say, `/UYELER/ARGE/YK` is Privileged in the `groups` mode but gets no role from the seeding, so the `both` mode counts them under `granted_by="group"`. Map the roles to such a group in the admin UI, or accept that they lose it in the `roles` mode.

**Service accounts.** A client's service account token (client credentials, `client_id` claim) never holds a role of the table: no role makes it Privileged, in any mode. Its own paths are unchanged: `url:forms` and `url:moderator` on form links, `media:attach` for a product, the short-link roles read below the Privileged shortcut. The contact consent roles `consent:record` (Forms, Place, Guessr) and `consent:audience:read` (SkyMail) are service-account roles too, read in every mode and only from a service account's token ([contact-consents.md](contact-consents.md)).

## Roles

The names are a contract with Keycloak: the table "Sözleşme: core'un kaynak rolleri" of the admin-token-authz spec (sky_lab_genel `.scratch/admin-token-authz/spec.md`). Whoever renames a role changes that table first. In code: `authz.PermissionRoles()`.

| Role (`core`) | Check sites (`internal/authz/authorizer.go`) | Requests behind them (examples) |
|---|---|---|
| `event:manage` | `allowEvent`: Create, Update, Delete of `TypeEvent`, `TypeEventDay`, `TypeSession` for every Owner team and none; `Assign` (door staff) | `POST/PUT/DELETE /v1/events…`, `/v1/event-days…`, `/v1/sessions…`; Event media uploads (upload rule `event_editor`); Guest apply trust (Event update) |
| `season:manage` | `Allow` `TypeSeason`: every action but Read | `POST/PUT/DELETE /v1/seasons…`, restore, archived Seasons (assigning an Event to a Season is the Event update decision) |
| `ticket:manage` | `allowTicket`: Read and Assign | `GET /v1/events/{id}/tickets`, `GET /v1/events/{id}/assignable-users`, `POST …/applications/users/{id}`, dashboard summary scope, Guest apply trust (Ticket assign) |
| `ticket:validate` | `allowDoorCheckIn` (`Validate`) | door check-in routes, guest door QR (`/v1/sessions/{id}/door-qr`), `POST /v1/skypass/verify`, `GET /v1/skypass/card` |
| `competitor:manage` | `allowCompetitor`: every action | `/v1/competitors…` |
| `media:manage` | `allowMedia`: List, Delete | `GET /v1/media`, `DELETE /v1/media/{id}` |
| `media:private:read` | `TypeMediaReadLink` Read, people only | a private core Media opened by an admin (certificate assets) |
| `certificate:manage` | `allowCertificate` (Read; Issue/Revoke), `allowCertificateTemplate` (every action) for every Owner team and none | `/v1/events/{id}/certificates…`, `/v1/certificate-templates…`, `/v1/certificate-template-bindings…`; certificate template media uploads |
| `users:manage` | `TypeUser`: every action (`users:read` still reads) | `/v1/users…` incl. `DELETE /v1/users/{id}` (account erasure request), user client roles; dashboard Members with every team |
| `groups:manage` | `TypeGroup`: every action | `/v1/groups…` incl. group members and group client-role mappings |
| `github:activity:read` | `TypeGithubActivity` Read, people only | `GET /v1/dashboard/github-activity` |
| `url:moderator` (existed) | `allowURL` (with `url:access`), `TypeFormLink` | everyone's short links; form links |
| `url:access` (existed) | `allowURL` (with `url:moderator`) | one's own short links |

Short links and form links have no new role: `url:moderator` and `url:access`, read below the Privileged shortcut as before, together allow every short-link action (Create, ReadMe, Read, Update, Delete) and `url:moderator` every form-link action. The shortcut itself is the Privileged Group in the `groups` and `both` modes and nothing in the `roles` mode. The team-bound certificate roles (`certificate:issue`, `certificate:revoke`, `certificate:template:manage`, `certificate:binding:manage`), `url:create`/`url:get`/`url:update`/`url:delete`, `url:forms`, `users:read` and `media:attach` are unchanged and read in every mode.

`groups:manage` lets its holder change group role mappings, so it can grant every role here. Map it as narrowly as `ADMIN` is today.

## AUTHZ_ROLE_MODE

Where a Privileged decision comes from while the roles roll out. Core refuses to start on any other value.

| Mode | A Privileged check allows when | Disagreements |
|---|---|---|
| `groups` (default, unset) | the person is in a Privileged Group. A new role alone allows nothing. Exactly the decisions before this change. | not counted |
| `both` | the Group **or** the role allows | every check where the two differ is counted and logged |
| `roles` | the role allows. A Privileged Group alone allows nothing. | not counted |

`url:moderator` and `url:access` existed before the roles, so they allow in every mode, and holding one outside the Privileged Groups is no disagreement.

Order (spec, "Sıra"):

1. Release core. Nothing changes.
2. Keycloak creates and maps the roles in production (ticket 03).
3. Finish narrowing the full-scope login clients (tickets 16–19: `frontend-main`, `frontend-arge`, `skymail`, `skyforms`). A full-scope client's tokens carry every core role of the person; a role mapped to a group for one purpose then shows up wherever that client's tokens reach core.
4. Before setting `both`, list the role holders per role in Keycloak and make sure each one is meant to have it: `both` already lets a role holder through. Set `AUTHZ_ROLE_MODE=both` on the core application.
5. Watch the counter per `client`, both sides: `granted_by="group"` is a Privileged member who would lose the action in `roles` (missing mapping, or a client whose tokens lack core's roles); `granted_by="role"` is someone who has it only through a role (a new mapping, or a full-scope client carrying a role nobody meant for that client). Each must be explained before going on.
6. `AUTHZ_ROLE_MODE=roles` once no `granted_by="group"` disagreement is left and every `granted_by="role"` one is intended.

Going back is setting the previous value and redeploying.

Before `roles`, every Keycloak client whose people's tokens reach core must carry core's roles in its tokens (full scope, or a scope mapping with `core`'s roles, as the `admin` client has since ticket 02). The `client` label of the counter names the client of every disagreeing token, so `both` shows which client still lacks them (for example the `skyforms` client of forms-frontend, which reads `/v1/groups` and `/v1/users`, or `sky-app` for the door).

### Metrics and logs

On `/v1/metrics`, no person and no path named:

- `skylab_authz_role_mode{mode="groups"|"both"|"roles"}`: 1 for the running mode, 0 for the others.
- `skylab_authz_role_disagreements_total{permission,granted_by,client}`: in the `both` mode, Privileged checks where the Group and the role disagreed. `granted_by="group"`: the person is in a Privileged Group but lacks the role, and loses this in the `roles` mode. `granted_by="role"`: the person holds the role outside the Privileged Groups, and keeps it. `client` is the token's `azp` (`none` when absent; past 32 distinct clients, `other`). It counts checks, not requests: one request can check several times. `GET /v1/users/me/capabilities` is not counted.

Logs: one JSON line per permission, side and client a minute, `{"event":"authz_role_disagreement","mode":"both","permission":"event:manage","granted_by":"group","group":"YK","client":"admin"}`. `group` is the Privileged Group that allowed (`ADMIN`, `YK` or `DK`) and appears only for `granted_by="group"`. No `sub`, e-mail or path.

At startup (Keycloak configured), core reads whether the roles exist on the `core` client, the one tokens carry them for, whatever `KEYCLOAK_CLIENT_ID` says (`GET /clients` and `GET /clients/{id}/roles`, the certificate role check's calls; no new Keycloak permission) and logs one line naming the missing ones and what that means in the running mode. It does not stop core.

## GET /v1/users/me/capabilities

What the caller may do, for the admin panel (ticket 10) to decide which menus and buttons to show without reading the token (ADR-0059). Every value is the answer `authz.Authorizer.Allow` or `Permitted` gives the caller's real requests, in the running mode; there is no second copy of a rule (`authz.Capabilities`). Bearer as for every route; 401 without one. `Cache-Control: no-store`.

```json
{
  "permissions": ["event:manage", "season:manage"],
  "can": {"season.write": true, "group.read": false, "...": false},
  "teams": [
    {"team": "WEBLAB", "levels": ["LEADER", "MEMBER"], "can": {"event.create": true, "...": true}}
  ],
  "otherTeams": {"can": {"event.create": true, "...": false}},
  "noOwnerTeam": {"can": {"event.create": true, "...": false}}
}
```

- `permissions`: the roles of the table the caller holds as the mode reads them, in table order. In the `groups` mode a Privileged member holds them all.
- `can`: decisions that name no Owner team. Every key is always present.
- `teams`: the Owner teams the caller is a member or Leader of, by name: every segment of their group paths, `LIDERLER`/`KOORDINATORLER` aside, exactly the teams core's decisions consider (so `UYELER` and `ARGE` appear for `/UYELER/ARGE/WEBLAB`). `levels` are `LEADER` and/or `MEMBER`.
- `otherTeams`: the decisions for a record of any Owner team not in `teams`. Use it for a record whose team is not listed.
- `noOwnerTeam`: the decisions for a record without an Owner team.

Look-up for a record of Owner team `t`: `teams.find(x => x.team === t)?.can ?? (t ? otherTeams.can : noOwnerTeam.can)`.

`can` keys: `season.write` (Season create/update/delete), `group.read`, `group.write` (Groups, members, group role mappings), `user.read`, `user.write` (full user card, profile, extra roles, logout), `user.create`, `user.delete` (erasure request), `media.list`, `media.delete`, `media.readPrivate`, `media.uploadEventMedia`, `media.uploadCertificateTemplateMedia`, `url.create`, `url.listMine`, `url.moderate` (everyone's links), `formLink.read`, `formLink.write`, `githubActivity.read`.

Team keys (in `teams[].can`, `otherTeams.can`, `noOwnerTeam.can`): `event.create`, `event.update`, `event.delete`, `event.assignDoorStaff`, `ticket.read` (applicant list), `ticket.assign`, `door.checkIn`, `competitor.read`, `competitor.create`, `competitor.update`, `competitor.delete`, `certificate.read`, `certificate.issue`, `certificate.revoke`, `certificateTemplate.read`, `certificateTemplate.create`, `certificateTemplate.update`, `certificateTemplate.assign` (bindings).

`door.checkIn` is the team's answer without the Event's own door staff and with Team door scan off; a person on an Event's door staff, or a member of a team with Team door scan on, may still check in there (`GET /v1/door/events` lists the caller's Events).

### The admin panel's rules today and their answer here

From core-frontend `src/lib/auth/groups.ts` and its callers (origin/main, 2026-10-03). "scope(t)" is the look-up above.

| Panel rule today | Answer |
|---|---|
| `isPrivileged` → Users page / user card editing, add user, roles | `can["user.write"]`, `can["user.create"]`, `can["user.delete"]` |
| `isPrivileged` → Groups, Teams menu | `can["group.read"]`, `can["group.write"]` |
| `isPrivileged` → Seasons (`canWriteSeason`) | `can["season.write"]` |
| `isPrivileged` → Event form `showSeason` | `scope(t).can["event.update"]` (core lets whoever may update the Event assign its Season, Leaders too) |
| `isPrivileged` → Media page actions | `can["media.list"]`, `can["media.delete"]` |
| `isPrivileged` → dashboard GitHub activity | `can["githubActivity.read"]` |
| `isPrivileged` → Event form `ownerOptional`, `lockOwner`, directory people | `noOwnerTeam.can["event.create"]`, `otherTeams.can["event.update"]`, `can["user.read"]` |
| `isPrivileged` → Event form `assignDoorStaff` | `scope(t).can["event.assignDoorStaff"]` |
| `isPrivileged` → Announcements (News) | not core's decision: the CMS decides (`news:write`, ticket 13) |
| `canManageHandoffTargets` (`/ADMIN`) | not core's decision: Keycloak's sky-handoff admin API answers 403 |
| `isLeader`, `leaderOwnerTeams`, `ownerLevels` | `teams[].levels` |
| `canWriteEvent(t, action)`; "Yeni etkinlik" | `scope(t).can["event.<action>"]`; any of `teams[].can`, `noOwnerTeam.can` `event.create` |
| `canCheckInForTeam(t)`, `canCheckInForEvent`, `canDeskCheckIn` | `scope(t).can["door.checkIn"]`, or the caller on the Event's door staff |
| `canManageCompetitors(t)` | `scope(t).can["competitor.update"]` (core also lets Owner team members, not only Leaders: the panel showed less than core allows) |
| `canListEventTickets(t)`, `canAssignEventTicket(t)` | `scope(t).can["ticket.read"]`, `scope(t).can["ticket.assign"]` |
| `canReadCertificates`, `canIssueCertificates`, `canRevokeCertificates` (t) | `scope(t).can["certificate.read" / "certificate.issue" / "certificate.revoke"]` |
| `canBindCertificateTemplates(t)` | `scope(t).can["certificateTemplate.assign"]` |
| `canEditCertificateTemplateForTeam(t)` | `scope(t).can["certificateTemplate.update"]` (`noOwnerTeam` for the club default) |
| `canManageCertificateTemplates`, `canUseCertificates`, `canUseCertificateWorkspace` | any scope with a `certificate.*` / `certificateTemplate.*` key true |
| sidebar certificate children (Şablonlar, Varsayılanlar, Verilenler, Üretim İşleri) | any scope's `certificateTemplate.read`, `certificateTemplate.assign`, `certificate.read`, `certificate.issue` |
| `canUseUrls`, `canModerateUrls` | `can["url.create"]` or `can["url.listMine"]` or `can["url.moderate"]`; `can["url.moderate"]` |
| Users menu (`privileged \|\| users:read`) | `can["user.read"]` |
| `canSeeSchedulingNav`, Competitors/Media menus (`privileged \|\| leader`) | `can["media.list"]`, any `LEADER` level, or any scope's `event.create` |
