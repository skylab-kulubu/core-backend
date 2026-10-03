# Dashboard summary

`GET /v1/dashboard/summary` answers the admin panel's dashboard in one request: counts and per-day figures computed in PostgreSQL, instead of the panel reading every Event and then each Event's applicant list. Code: `internal/dashboard`, handler `internal/handlers/dashboard.go`.

## Who sees what

The endpoint makes no permission decision of its own; it reuses the ones core already makes.

- **Signed in.** No bearer, `401` problem+json. Anyone signed in gets `200`; what it covers depends on them.
- **Events and applications.** The summary covers the current (not archived) Events whose applicant list the caller may read: the `authz` Ticket read decision of `GET /v1/events/{id}/tickets`, taken once per Owner team. Today that is Privileged people (every Event, those without an Owner team too), Leaders of the Event's Owner team, members of an Owner team whose members may edit its Events (GECEKODU), and Owner-team members holding `certificate:issue`. Anyone else gets a summary of nothing (zeros, empty lists). `ownerTeams` names the Owner teams covered.
- **Members.** Only for a person who may read people: the `authz` User read decision of `GET /v1/users` (Privileged, or the `users:read` role). A product's service account (a token `authz.ServiceClients` maps to a product) is not a person and never gets the section, whatever its roles. Anyone else gets `"members": null`.
- **Joiners' teams.** A caller who may also manage people (the User update decision: Privileged) sees every team a joiner sits in. A caller who may only read people (`users:read`) sees the teams with Public listing (`public_listing=true`) alone, as visitors of the public rosters do.
- **Group overage.** A token with the Group overage marker gets its Groups from Keycloak before the route, as for every route ([`keycloak-admin-permissions.md`](keycloak-admin-permissions.md#group-overage)).
- **When Privileged becomes a role (admin-token-authz ticket 04).** The decisions above come from `authz.Authorizer.Allow`, so when ticket 04 moves the Privileged checks from group paths to `core` client roles, the summary follows without a change here: a Privileged person without the role then sees only their Leader/Owner-team scope, and no Members section or only listed teams, depending on which roles they keep. Leader and Owner-team decisions stay on group paths.

The answer names people (recent joiners), so it carries `Cache-Control: no-store`.

## Answer

All times are UTC (RFC 3339). Days and months are counted in `Europe/Istanbul`.

```json
{
  "generatedAt": "2026-10-03T12:00:00Z",
  "timeZone": "Europe/Istanbul",
  "ownerTeams": ["GECEKODU", "WEBLAB"],
  "events": {
    "total": 42, "upcoming": 3, "live": 1,
    "byMonth": [{ "month": "2026-05", "count": 4 }, "… 6 months, oldest first, this one last"]
  },
  "applications": {
    "total": 1830, "members": 1204, "guests": 626, "checkedIn": 912,
    "daily": [{ "date": "2026-09-04", "count": 12 }, "… 30 days, oldest first, today last"]
  },
  "eventStats": [
    {
      "id": "…", "name": "WebLab Bootcamp", "ownerTeam": "WEBLAB", "location": "D-101",
      "startDate": "2026-10-08T12:00:00Z", "endDate": "2026-10-08T15:00:00Z",
      "active": true, "live": false, "capacity": 50,
      "coverImageUrl": "https://…",
      "hasApplicationForm": true, "hasCoverImage": true,
      "applications": 46, "members": 30, "guests": 16, "checkedIn": 0,
      "dailyApplications": [0, 0, 1, 0, 2, "… 14 days, oldest first, today last"]
    }
  ],
  "members": {
    "active": 412,
    "newByMonth": [{ "month": "2025-11", "count": 9 }, "… 12 months, oldest first"],
    "recentJoiners": [
      { "id": "…", "firstName": "Ada", "lastName": "Lovelace", "teams": ["WEBLAB"], "registeredAt": "2026-10-03T09:12:00Z" }
    ],
    "asOf": "2026-10-03T11:58:00Z"
  }
}
```

- **`events`**: the covered Events. `upcoming` have not started; `live` are running now. An Event without an end date runs for 12 hours from its start, as the panel counts it. Events without a start date count in `total` only.
- **`applications`**: every Ticket of the covered Events, all time. `members` are Member apply Tickets (`REGISTERED`), `guests` the rest (Guest apply, Walk-in). `checkedIn` are Tickets with at least one check-in (archiving a day or session keeps its check-ins, as the applicant list does). `daily` counts the Tickets created on each of the last 30 days.
- **`eventStats`**: the covered Events that are running, still to come, or ended within the last 30 days, by start date. The figures are the Event's own; `dailyApplications` are its last 14 days. `hasApplicationForm` is false when the Event links no form, main or extra; `hasCoverImage` when it has no cover.
- **`members`** (or `null`): see below. `membersUnavailable: true` (and `members: null`) means the caller may see the section but Keycloak could not be read; the rest of the answer stands.

Nothing in the answer carries an e-mail, phone or other contact, and no Ticket appears one by one.

## Members

A Member is a User in the `UYELER` tree (CONTEXT.md, Member). The glossary has no "active Member"; the summary counts as **active** a Member whose Keycloak account is enabled and whose core account is not being erased or erased (no account deletion request, and a row, when core has one, in `account_state = 'active'`; the rule `AttributionState` applies). This definition is a proposal until the glossary takes it or another.

- **`newByMonth`** counts the active Members by the month their Keycloak account was created (`createdTimestamp`), over 12 months. Keycloak keeps no date for joining a Group, so this is registration, not the day someone was put in the tree.
- **`recentJoiners`** are the 8 active Members who registered last: id, name, the teams they sit in (the last name of each Group under `UYELER`, leadership subgroups counting as their team; see **Who sees what** for which teams a caller sees) and the registration time. The name is the one core stores for the person when core has a row for them, as core's other people reads answer it (`overlayShadow`), and Keycloak's otherwise.
- **How fresh.** The erasure check (and the stored names) are read from the database on every request, so a person whose erasure starts disappears at once. Everything read from Keycloak is as old as the last read of the tree: a person disabled in Keycloak, a new Member, a team change or a Keycloak name shows **up to 5 minutes** later, and later still while Keycloak cannot be read (below).

## Reading the Members tree

- **Keycloak requests.** One read of the tree is `GET /group-by-path/UYELER`, then for every Group in the tree `GET /groups/{id}/children` (one request per 100 children) and `GET /groups/{id}/members`: about `1 + 2 × G` requests for `G` Groups in the tree, plus the service account's token when it has expired. All through core's service account; nothing new is needed ([`keycloak-admin-permissions.md`](keycloak-admin-permissions.md)).
- **One read for everyone.** A read serves every allowed caller for 5 minutes (`DefaultMembersTTL`). It holds ids, names, the enabled flag, the creation time, team names and whether each team is listed: no contact. Who may see it is decided on every request.
- **Shared, bounded.** Only one read runs at a time; requests that arrive meanwhile wait for it. The read is detached from the requests: one that goes away (client gone, request cancelled) stops waiting and gets `membersUnavailable`, without failing the read for the others. A read gives up after 5 s in all (`DefaultMembersReadTimeout`).
- **Refresh in the background.** Once a read is 5 minutes old, the next request starts a new one and is answered the old one at once.
- **Failure.** A failed read keeps the last good one in place, which is answered on, however old (`asOf` says when it was read). With no good read yet, the section is `null` with `membersUnavailable: true`. A failure is kept 30 s (`DefaultMembersFailureTTL`): no new read starts before that, so a Keycloak outage costs at most one read every 30 s, not one per request.
- **Log.** Each failed read logs one line, `{"event":"dashboard_members","outcome":"unavailable","stale":true|false,"error":"…"}`. The error is Keycloak's status or the timeout; no person, id, name or Group path is added. Successful reads and cached answers are not logged.

## Cost

Three queries for the Events part, whatever the number of Events: the current Events (the list query of `GET /v1/events`), the Ticket counts per Event, and the Tickets of the last 30 days per Event and day; one more (`Accounts`: erasure markers and stored names) for the Members part. Migration `20261003120000` adds `tickets (event_id, created_at)`; before it no index on `tickets` led with `event_id`.

Measured on a local PostgreSQL 18 (Docker, tmpfs), 30 runs each, the whole `Summary` call:

| Data | Caller | Without the index (p50 / p95) | With it (p50 / p95) |
| --- | --- | --- | --- |
| 400 Events, 40 000 Tickets, 20 000 check-ins | Privileged (400 Events) | 42 / 90 ms | 21 / 24 ms |
| same | Leader of one team (10 Events) | 25 / 60 ms | 8 / 13 ms |
| 2 000 Events, 300 000 Tickets, 150 000 check-ins | Privileged (2 000 Events) | 313 / 339 ms | 154 / 201 ms |
| same | Leader of one team (50 Events) | 69 / 140 ms | 39 / 61 ms |

The Events part is not cached: at the club's size it costs tens of milliseconds, and a cache would have to be keyed by the caller's scope. The Members part is cached as above because each read costs about two Keycloak requests per Group in the tree; its database part (`Accounts`) is one query per request over the tree's people.
