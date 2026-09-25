# Core's Keycloak Admin REST permissions

Core calls Keycloak Admin REST with the client-credentials token of its own client (`KEYCLOAK_CLIENT_ID`, `core`), i.e. as `service-account-core`. That service account holds `realm-management` roles for users and groups and only **read** roles for clients: `view-clients` and `query-clients`. It does not hold `manage-clients`. ADR-0048 rejected that role for core because it lets core rewrite every client's redirect URIs and secrets.

## Calls by resource (`internal/identity/keycloak.go`)

| Resource | Calls | Needs |
| --- | --- | --- |
| Clients | `GET /clients` (by `clientId` and list), `GET /clients/{id}/roles`, `GET /clients/{id}/roles/{role}/users`, `GET /clients/{id}/roles/{role}/groups` | `view-clients` / `query-clients` (role holders also `query-users` / `query-groups`) |
| Users | `GET /users`, `GET /users/{id}`, `POST /users`, `PUT /users/{id}`, `DELETE /users/{id}`, `GET /users/{id}/groups`, `PUT`/`DELETE /users/{id}/groups/{group}`, `POST /users/{id}/logout` | `manage-users` |
| Role mappings | `GET /users/{id}/role-mappings`, `POST`/`DELETE /users/{id}/role-mappings/clients/{client}`, `GET /groups/{id}/role-mappings`, `POST`/`DELETE /groups/{id}/role-mappings/clients/{client}` | `manage-users` |
| Groups | `GET /groups`, `GET /groups/{id}`, `GET /groups/{id}/children`, `GET /group-by-path/{path}`, `GET /groups/{id}/members`, `POST /groups`, `POST /groups/{id}/children`, `PUT /groups/{id}` | `manage-users` / `query-groups` |

Core never writes under `/clients`. `cmd/ldapimport` is a separate one-off tool: it reads users and their federated identities and deletes federated identity links (`manage-users`).

## Certificate roles

Certificate authorization (`internal/authz`) reads four client roles of core's client from tokens: `certificate:template:manage`, `certificate:binding:manage`, `certificate:issue` and `certificate:revoke`. Keycloak owns them. The operator script `config/identity-guardrails.sh` of e-skylab-keycloak creates missing ones (runbook `docs/v2-identity-reconcile-runbook.md` §8). The same run removes `manage-clients` from `service-account-core`, but only after it has confirmed that all four roles exist.

At startup core only checks them (`MissingClientRoles`, GET only). When a role is missing, or the roles cannot be read, core logs one line that starts with `certificate client roles` and names the missing roles and the operator command, then starts normally. Until the roles exist and are granted, certificate actions that need them are denied. A clean startup logs nothing about certificate roles.
