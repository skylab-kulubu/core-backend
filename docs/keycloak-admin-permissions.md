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

## Group count report

`core-backend group-count-report` measures how many Group paths each user carries in a token, for the Group overage threshold of ADR-0059 (30 paths). It runs instead of the server, inside the running core container, whose environment already holds core's service account. It only reads: `GET /users`, then `GET /users/{id}/groups` for every enabled user. Disabled users get no token and are only counted.

It counts each user's direct memberships with their full paths, which is what Keycloak's Group Membership mapper writes into the `groups` claim. The report goes to standard output: the number of users, how many users have each number of paths, the most paths any user has, how many users are above 30, the size of the largest `groups` claim, and the average path length with the size a 30-path claim of that length would have. A claim's size is its JSON (`"groups":["/A","/B"]`: each path's UTF-8 bytes in quotes, commas between them) and about four thirds of that in the token, whose payload is base64url. Against Keycloak 26.7.4 with a full-path Group Membership mapper, the JSON size matched real access tokens byte for byte (2 and 105 paths, 2026-09-29).

By default the report names nobody and prints no Group path. `-list-overage` adds the users above 30 paths by `sub` (their Keycloak id); that part is personal data.

A user whose Groups cannot be read is not counted as a user without Groups: the report says how many could not be read, the first error goes to standard error (it names nobody), and the command exits 1. A user deleted between the listing and the lookup is left out. Missing settings exit 2.

```sh
docker exec <core container> ./core-backend group-count-report
docker exec <core container> ./core-backend group-count-report -list-overage
```
