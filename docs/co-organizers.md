# Co-organizers

An Event keeps **one** Owner team (for example YK). Beside it, a list of **Co-organizers** (Ortak düzenleyen) manages the Event: other teams and, where a team does not fit, single people. Keycloak is not touched: the list is core's data and core decides with it. Spec: `.scratch/etkinlik-ortak-yonetim/SPEC.md` (decisions of 2026-11-25); ADR 0092 in e-skylab.

This page is the data, the routes (ticket 01), the decisions (tickets 02 and 03), and the lists, the copy, retention and the Team code check (ticket 04). Every decision on an Event, its Tickets, certificates, Competitions, competitors, door, mail list, feedback and dashboard reads the Co-organizers (see "Decisions" below).

**`EVENT_CO_ORGANIZERS_WRITE`** (`on`/`off`, unset is `off`; any other value stops core at start): off, `PUT /v1/events/{id}/organizers` is `403 co_organizers_disabled` for everyone and `canManage` is `false`; `GET` still answers. It stays off until the decisions, the Team audit record and the mails (tickets 02–05) ship with it, so no list changes without its record; in particular it is not turned on before ticket 03 (every Event decision outside `internal/event` reading the Co-organizers) is merged.

## The rule

- A **Co-organizer team** is a Team code, as the Owner team is. Each team row carries two choices, both always stored:
  - `management`: `leaders` (its Leaders alone manage the Event) or `members` (every member, as the GECEKODU Owner team rule: what takes `Update` opens to members, what takes a Leader stays a Leader's). Left out, it is the Owner team's own rule (`ownerRule`): `members` for an Owner team whose `Update` row names `MEMBER` (GECEKODU), else `leaders`. A row keeps what was stored when the Owner team's rule changes later.
  - `visibility`: `co_organizer` (named outside the panel: the site, the Ticket page, the certificate check) or `helper` (Yardımcı ekip: seen in the panel alone). Left out, it is `co_organizer`.
- A **Co-organizer person** is an e-skylab account (user id). A person has no choices, is never shown outside the panel, and their rights end **30 days after the Event** (its end; its start plus a day when it has no end): `expiresAt`, derived, never stored. Who edits the Event as a Co-organizer person alone (`viewer.via` `coPerson`) may not lengthen that: an edit that clears the dates the end is read from, or moves it later, is `403 co_organizer_person_dates` (moving it earlier, and every other field, is fine). The Owner team and the Co-organizer teams are not bound by it.
- The Owner team is not a Co-organizer: an Event whose Owner team becomes one of its Co-organizer teams (an update of `ownerTeam`) drops that row in the same write.
- The decisions read the list so (ADR 0092): a person may do a job when, in **any** of the organizer teams (the Owner team and the Co-organizer teams), they meet the level that team asks for it. A Co-organizer person counts as a Leader. In a `leaders` team, being a member gives nothing (not the certificate roles, not Team door scan, not `team_competition_manage`); in a `members` team, a member who is no Leader does not scan at the door by membership (the door stays a Leader's, door staff's, or Team door scan's). Deleting, archiving, changing the Owner team, the list itself and assigning door staff stay the Owner team's.

## Decisions

`authz.Resource` carries `CoOwnerTeams` (`{Code, MembersManage}`), `CoOrganizerIDs` (the people whose rights have not ended) and `TeamDoorScanTeams` (the organizer teams whose Team door scan is on). `event.AuthzResource(ev, type, now)` is the one place they are filled from an Event; an architecture test (`internal/event/authz_resource_arch_test.go`) keeps any other place from building a decision out of an Event's Owner team alone. What reads an Owner team alone on purpose stays on its own list: creating an Event in a team, moving an Event between Owner teams, a team certificate template's own team, and a certificate whose Event can no longer be read.

For each organizer team the authorizer reads the person's levels in it (`ownerLevels`, a Leader of a Team above included), its **seat**: the Owner team's as before; a `members` Co-organizer team's Leader and member; a `leaders` Co-organizer team's Leader alone. A Co-organizer person has a Leader's seat.

| Decision | Who |
|---|---|
| Event `Update`, `ReadFull` (a draft), publish, unpublish, cancel, uncancel; days and Sessions (`Create`, `Update`, `Delete`) | a seat that holds the level its team asks: the Owner team's row of the Event permission table; a `members` team: Leader or member (`Delete` of a day or Session: Leader); a `leaders` team and a person: Leader |
| Event `Create`, `Delete` (delete, archive) | the Owner team's seat alone, as before |
| Changing the Owner team | `Update` in both teams through the Owner team's seat alone (a Co-organizer seat does not count) |
| `ManageOrganizers` (the list), `Assign` (door staff) | Privileged (`event:manage`) and the Owner team's Leaders (a Leader of a Team above included); on an Event without an Owner team, Privileged alone (Yusuf S3) |
| Ticket `Read` | a Leader's seat; a member's seat with `certificate:issue`; a seat that may edit the Event |
| Ticket `Assign`, certificate `Issue`/`Revoke` without a role, template binding (`CertificateTemplate.Assign`) | a Leader's seat |
| Certificate manager | a member's seat (Owner team, `members` team) with `certificate:issue` / `certificate:revoke` (`certificate:binding:manage` for a binding) |
| Door (`Validate`) | door staff; a Leader's seat; a member's seat while that team's Team door scan is on |
| Competitions | a Leader's seat (`leader` basis); a member's seat while that team's `team_competition_manage` is on (`member_setting`) |

**Around the decisions.**

- The door reads each organizer team's Team door scan (`team_door_scan`) only for the teams whose membership counts that the caller is in: the Owner team and the `members` Co-organizer teams (`TeamDoorScanTeams`).
- A Ticket write is recorded in the Team audit record whatever seat it rests on: a Co-organizer team's Leader as `leader` through their Team; a Co-organizer person, or a member, as `event_editor` through the Event's team. A resend and a link renewal (`ticket.resend`, `ticket.link.renew`) take `event_editor` too (migration `20261126190000`).
- The Media upload rules of an Event's editor (`event_editor`, `certificate_template_editor`) name no Event: they also let in who holds a Co-organizer seat on a current Event (`media.CoOrganizerSeats`, one query, read only when the caller's own teams let them in to none). A seat is a Co-organizer person, a Leader of a Co-organizer team (its `LIDERLER` or `KOORDINATORLER` subgroup), or a member of a `members` Co-organizer team; a plain member of a `leaders` team holds none. Every seat lasts as a person's rights do: `CoOrganizerPersonGrace` after the Event's end (or its start plus a day), never ending for an Event with no date. Which Event the Media ends up on is decided when it is linked.
- A certificate is revoked and reissued on its Event's organizers as they are now (the Event's current Owner team and its Co-organizers), not on the Owner team it was issued under (`Certificate.OwnerTeam`, its snapshot); the snapshot decides only when the Event cannot be read. A change from before: once an Event moves to another Owner team, the old team's Leaders no longer revoke or reissue its certificates and the new team's do.
- A team's competitor list (`GET …?ownerTeam=`) shows the team's members all of it, and a Co-organizer of one of the team's Events that Event's competitors (each Event read and decided once; a caller who is no person is refused with nothing read); the team's leaderboard stays the team's.
- The dashboard's to-dos, counts and charts decide per Event.

**Event-by-Event answers.** The Event's detail and list answer `viewer` to a signed-in caller:

```json
"viewer": {"via": "coTeam", "can": {"event.update": true, "event.delete": false, "event.publish": true, "event.manageOrganizers": false, "event.assignDoorStaff": false, "ticket.read": true, "ticket.assign": true, "certificate.issue": true, "certificate.revoke": true, "certificate.template.assign": true, "competition.manage": true}}
```

`via` is what the right to edit rests on, the strongest: `privileged`, `owner`, `coTeam`, `coPerson`, or `none`. Each `can` is the decision its endpoint takes on the Event: `event.manageOrganizers` also reads `EVENT_CO_ORGANIZERS_WRITE`; `event.delete` is false on a published or cancelled Event but for `event:manage` (only it archives one); `certificate.template.assign` is also true for who may edit the Event (they bind its template). The panel reads it instead of `teamCan(ownerTeam, …)`; `GET /v1/users/me/capabilities` keeps the decisions that name no Event ("for which team may I create one").

## Limits

| Check | Refusal (`400 event_organizers_invalid`) |
|---|---|
| At most 10 teams and 10 people | `reason: too_many`, no `index` |
| A team or person once | `duplicate` |
| A code that names a Group: no blank, no `/` | `invalid_code` |
| Not a Privileged root (YK, DK, ADMIN): they manage or read every Event already | `privileged_team` |
| An existing Team: the one Group of that name, a Team in the tree (`/UYELER/<area>/<team>` or below; not an area, a Leader group or a Privileged root) | `unknown_team` |
| Not the Owner team | `owner_team` |
| `management`: `leaders` or `members`; `visibility`: `co_organizer` or `helper` | `invalid_management`, `invalid_visibility` |
| A person is a non-nil id | `invalid_person` |
| A person is an active account (none, being erased or erased is refused; the database's `require_active_account_reference` guard) | `inactive_person` |

The problem names what to fix: `{"code": "event_organizers_invalid", "field": "teams" | "people", "index": 2, "reason": "unknown_team"}`. A team's code is stored as its Group is named. A body with a field not named below, or a person given as anything but an id, is a plain `400`.

## `GET /v1/events/{id}/organizers`

For who may edit the Event (`authz.Update`, its Co-organizers included); `403` to anyone else, `404` for a draft the caller may not see.

```json
{
  "ownerTeam": "YK",
  "ownerRule": "leaders",
  "teams": [{"code": "WEBLAB", "name": "WEBLAB · Web Geliştirme", "management": "leaders", "visibility": "co_organizer"}],
  "people": [{"id": "…", "name": "Ada Lovelace", "email": "ada@example.com", "expiresAt": "2027-01-14T18:00:00Z"}],
  "canManage": true
}
```

Teams by code, people by name. `name` is the team as shown outside the panel (`organizer.Name`). A person's `email` only to who may change the list (`canManage`); the Event's other editors see the name alone. `expiresAt` is left out for an Event with no date. `canManage` is whether the caller may change the list.

## `PUT /v1/events/{id}/organizers`

The whole lists; the answer is the GET's.

```json
{"teams": [{"code": "WEBLAB"}, {"code": "GECEKODU", "management": "members", "visibility": "helper"}], "people": ["<user id>"]}
```

Who: `ManageOrganizers` (Privileged and the Owner team's Leaders; never a Co-organizer), while `EVENT_CO_ORGANIZERS_WRITE` is on. The write is the difference, in one transaction under the Event's row lock: the rows that go are deleted, the new ones inserted, a team that stays has its choices updated in place. A row that stays keeps its `added_by` and `added_at`, and a person who stays is kept though their account is no longer active: `inactive_person` is about a new person alone. `409` when the Owner team changed since the request read it.

`PUT`/`PATCH /v1/events/{id}` and the Event's create never write the list (a field of it in their body is ignored); a copy carries it as below.

## Lists

`GET /v1/events` (current Events only; with `lifecycle=inactive|all` either filter is `400 event_list_filter_invalid`):

| Query | Answers |
|---|---|
| `?ownerTeam=X` | as before: the Events X owns |
| `?team=X` | the Events X owns **or** is a `co_organizer` team of; a `helper` team's Events never match (a helper team is not named outside the panel, nor by a list). Anyone may ask; drafts as for `ownerTeam` (signed in, `drafts=include`, those the caller may see); signed out, the active ones. With `ownerTeam` it is `400 event_list_filter_invalid` |
| `?managed=true` | the Events the caller may edit (`Update` on the Event, its Co-organizers included), drafts included, each decided on the Event: the Owner team's seat, a Co-organizer team's (a helper team's too), a Co-organizer person while their rights last, Privileged. `401` signed out. With `ownerTeam=X` or `team=X`, narrowed to those; with `active=true`, the active ones. `managed=false` is no filter; another value is `400 event_list_filter_invalid` |

`?managed=true` reads the club's current Events and decides each, as the dashboard does: the token alone does not name the Teams under the ones the caller leads (a Leader of a Team above). The archive (`lifecycle=inactive`) stays decided with `Delete`, the Owner team's: a Co-organizer never sees an archived Event there.

## Copy

`POST /v1/events/{id}/copy` (ADR 0066):

- The Co-organizer teams come along with their `management` and `visibility`, but the one that is the copy's Owner team, while `EVENT_CO_ORGANIZERS_WRITE` is on (off, a copy has none, as before).
- `includeCoOrganizerPeople: true` also copies the people whose accounts are active (not being erased), for who may change the copy's list (`ManageOrganizers` on the copy: Privileged and its Owner team's Leaders, with the switch on); anyone else gets `"coOrganizerPeople"` in `skipped` when the source has people. The copier is the rows' `added_by`.
- A Co-organizer of the source (`viewer.via` `coTeam` or `coPerson`) who may not create Events in its Owner team copies it **into the Owner team** (`ownerTeam` left out or the source's), never into their own (`403`), and only while they still manage the copy: a Co-organizer team's seat does (its team comes along), a Co-organizer person does not (their row comes along only for who manages the list), so their copy is `403`.
- The certificate template binding and the Competitions are decided on the copy as it will be (its Owner team and the Co-organizer teams it gets): a Co-organizer team's Leader brings them too.

## In the Event JSON

| Field | Who sees it |
|---|---|
| `coOrganizerTeams: ["WEBLAB"]` | anyone: the `co_organizer` teams alone, never a `helper` team or a person; left out when none |
| `coOrganizers: [{code, management, visibility}]` | who may edit the Event; left out when none |
| `coOrganizerIds: ["<user id>"]` | who may edit the Event; left out when none |

The stored lists are never in the JSON by themselves: `ProjectFor` copies them for who may edit, as the attendee note.

## Data

`event_co_organizers` (migration `20261126151000`): `event_id` (the Event's rows go with it), `team_code` or `user_id` (one of them), `management` and `visibility` (both on a team row, none on a person row; the checks enforce it), `added_by` (an id alone; the name is read when shown) and `added_at`. A team and a person are each once on an Event. `user_id` and `added_by` are current-identity links, guarded like `event_door_staff.user_id`. The audit record and the mails are ticket 05.

- **Retention** (`co_organizer_people`, [retention-sweep.md](retention-sweep.md); rule set v11): a person row goes 90 days after the Event's end, as door staff; an Event with no date is counted, never changed. The rights end 30 days after the Event, before. A team row lives with the Event.
- **Account erasure**: the person's rows go; `added_by` of the rows they added is cleared, the rows stay.
- **Team code changes** ([team-codes.md](team-codes.md)): a code an Event names as a Co-organizer team is one of core's records: changing or giving it up is `409 code_in_use` with `records` `event_co_organizer`, and a new Group may not take it.
