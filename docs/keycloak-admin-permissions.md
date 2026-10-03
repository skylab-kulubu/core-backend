# Core's Keycloak Admin REST permissions

Core calls Keycloak Admin REST with the client-credentials token of its own client (`KEYCLOAK_CLIENT_ID`, `core`), i.e. as `service-account-core`. That service account holds `realm-management` roles for users and groups and only **read** roles for clients: `view-clients` and `query-clients`. It does not hold `manage-clients`. ADR-0048 rejected that role for core because it lets core rewrite every client's redirect URIs and secrets.

## Calls by resource (`internal/identity/keycloak.go`)

| Resource | Calls | Needs |
| --- | --- | --- |
| Clients | `GET /clients` (by `clientId` and list), `GET /clients/{id}/roles`, `GET /clients/{id}/roles/{role}/users`, `GET /clients/{id}/roles/{role}/groups` | `view-clients` / `query-clients` (role holders also `query-users` / `query-groups`) |
| Users | `GET /users`, `GET /users/{id}`, `POST /users`, `PUT /users/{id}`, `DELETE /users/{id}`, `GET /users/{id}/groups`, `PUT`/`DELETE /users/{id}/groups/{group}`, `POST /users/{id}/logout` | `manage-users` |
| Role mappings | `GET /users/{id}/role-mappings`, `POST`/`DELETE /users/{id}/role-mappings/clients/{client}`, `GET /groups/{id}/role-mappings`, `POST`/`DELETE /groups/{id}/role-mappings/clients/{client}` | `manage-users` |
| Groups | `GET /groups`, `GET /groups/{id}`, `GET /groups/{id}/children`, `GET /group-by-path/{path}`, `GET /groups/{id}/members`, `POST /groups`, `POST /groups/{id}/children`, `PUT /groups/{id}` | `manage-users` / `query-groups` |

Core never writes under `/clients`.

## Certificate roles

Certificate authorization (`internal/authz`) reads four client roles of core's client from tokens: `certificate:template:manage`, `certificate:binding:manage`, `certificate:issue` and `certificate:revoke`. Keycloak owns them. The operator script `config/identity-guardrails.sh` of e-skylab-keycloak creates missing ones (runbook `docs/v2-identity-reconcile-runbook.md` §8). The same run removes `manage-clients` from `service-account-core`, but only after it has confirmed that all four roles exist.

At startup core only checks them (`MissingClientRoles`, GET only). When a role is missing, or the roles cannot be read, core logs one line that starts with `certificate client roles` and names the missing roles and the operator command, then starts normally. Until the roles exist and are granted, certificate actions that need them are denied. A clean startup logs nothing about certificate roles.

## Permission roles

The roles that stand for the Privileged Groups (`event:manage`, `season:manage`, … ; ADR-0059) are also Keycloak's: e-skylab-keycloak creates them and seeds their group mappings once (`KEYCLOAK_RECONCILE_ONLY=core-roles`). At startup core checks them with the same two reads as the certificate roles and logs one line that starts with `authz permission roles` when some are missing. Which mode reads them (`AUTHZ_ROLE_MODE`) and what each grants: [`authz-roles.md`](authz-roles.md).

## Dashboard summary

The Members section of `GET /v1/dashboard/summary` reads the `UYELER` tree: `GET /group-by-path/UYELER`, then `GET /groups/{id}/children` and `GET /groups/{id}/members` for each Group in it, about `1 + 2 × G` requests for `G` Groups. One read serves every caller for 5 minutes, runs one at a time with a 5 s limit, a failure stops new reads for 30 s, and only people who may read people trigger it. Nothing new is needed: these are the Group reads of the rosters. See [`dashboard-summary.md`](dashboard-summary.md).

## Group count report

`core-backend group-count-report` measures how many Group paths each user carries in a token, for the Group overage threshold of ADR-0059 (30 paths). It runs instead of the server, inside the running core container, whose environment already holds core's service account. It only reads: `GET /users`, then `GET /users/{id}/groups` for every enabled user. Disabled users get no token and are only counted.

It counts each user's direct memberships with their full paths, which is what Keycloak's Group Membership mapper writes into the `groups` claim. The report goes to standard output: the number of users, how many users have each number of paths, the most paths any user has, how many users are above 30, the size of the largest `groups` claim, and the average path length with the size a 30-path claim of that length would have. A claim's size is its JSON (`"groups":["/A","/B"]`: each path's UTF-8 bytes in quotes, commas between them) and about four thirds of that in the token, whose payload is base64url. Against Keycloak 26.7.4 with a full-path Group Membership mapper, the JSON size matched real access tokens byte for byte (2 and 105 paths, 2026-09-29).

By default the report names nobody and prints no Group path. `-list-overage` adds the users above 30 paths by `sub` (their Keycloak id); that part is personal data.

A user whose Groups cannot be read is not counted as a user without Groups: the report says how many could not be read, the first error goes to standard error (it names nobody), and the command exits 1. A user deleted between the listing and the lookup is left out. Missing settings exit 2.

```sh
docker exec <core container> ./core-backend group-count-report
docker exec <core container> ./core-backend group-count-report -list-overage
```

## Group overage

A person in more than 30 Group paths gets tokens without a `groups` claim (ADR-0059). SKY LAB's Keycloak mapper writes Microsoft's marker instead: `"_claim_names": {"groups": "src1"}` and `"_claim_sources": {"src1": {"endpoint": …}}`. Core looks only at whether `_claim_names` names `groups`; it never reads or calls the endpoint. A token with the marker has no Group paths of its own in core, even if it also carries a `groups` or `group` claim.

For such a token core reads the person's Groups with `GET /users/{id}/groups`, the call of the user card and the group count report, and decides from them exactly as from a `groups` claim. This happens after Bearer verification, the account access gate and the short-link hops (`/v1/go/…`, which use no Group), and before JIT and every other route. Tokens with their `groups` claim never cause a call.

- **Cache.** The answer is kept 60 s per person (`sub`), in the core process. The token is not part of the key: a person's Groups do not depend on which token asks, and the minute already bounds how old an answer can be. Requests of one person that arrive during a read share it. A read gives up after 5 s and is not cancelled by the request that started it.
- **Core's own writes.** Adding or removing a member (`POST /v1/groups/{id}/members`, `DELETE /v1/groups/{id}/members/{userId}`) forgets that person at once; renaming a Group (`PATCH /v1/groups/{id}` with a new name) forgets everyone, since every path under it changes. A read that started before such a write is not kept.
- **Freshness.** A change made through core is seen on the person's next request. A change made elsewhere (Keycloak's console, another core process) is seen within 60 s. A `groups` claim is as old as its token (the access token lifetime, 5 minutes today), so for a person in Group overage core is fresher than for everyone else, not staler.
- **Failure.** When Keycloak cannot be reached, times out or answers anything but success, the request gets `503` problem+json with `Retry-After: 1` and `Cache-Control: no-store`. The failure is not kept, and the person is never treated as having no Groups or someone else's. A subject Keycloak does not know gets `401` with `WWW-Authenticate: Bearer error="invalid_token"`. Without the cache wired (`httpx.Deps.GroupOverage`), every marked token gets `503`. So a person in Group overage cannot use core while Keycloak is unreachable; everyone else can.
- **Logs.** Each read writes one JSON line: `{"event":"group_overage","correlation_id":…,"outcome":"fetched"|"unavailable"|"unknown_subject","paths":N}`, with an `error` that names nobody on failure. Answers from the cache are not logged. No `sub`, path or token is logged.
- **Metrics** (`/v1/metrics`, unlabelled): `skylab_group_overage_requests_total` (requests with the marker), `skylab_group_overage_cache_hits_total`, `skylab_group_overage_lookups_total` (calls to Keycloak), `skylab_group_overage_lookup_failures_total`, `skylab_group_overage_unknown_subjects_total`, and the gauge `skylab_group_overage_people`: people whose Groups are cached now, that is, people in Group overage who used core in the last minute.
- **Permissions.** Nothing new: `service-account-core` already holds `view-users` and `manage-users`. Keycloak leaves out of each page the Groups the caller may not view, and these roles view every Group. If they are ever taken away, the call answers `403` and marked tokens get `503`; a narrower fine-grained permission could instead answer fewer Groups than the person has.

Nothing changes until the mapper is switched on for a client whose tokens reach core (the `admin` client first). Until then no token carries the marker.
