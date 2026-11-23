# A person's account from the person card

The admin panel's person card reads and changes the person's Keycloak account through core (P7, 2026-11-22: "Keycloak'taki tüm özellikleri burdan da yönetebilmeliyiz"). Keycloak stays the identity store; core calls its Admin REST with `service-account-core` ([keycloak-admin-permissions.md](keycloak-admin-permissions.md)). Code: `internal/identity/user_accounts*.go`, `internal/handlers/user_accounts.go`.

## Who

| | Read (`GET …/account`) | Write |
| --- | --- | --- |
| `users:manage` (ADMIN, YK) | yes | yes, except below |
| Privileged reader (DK, `global:read`) | yes | no |
| anyone else | no (403) | no (403) |

- **A privileged person's account** is changed only by someone in the ADMIN root: 403 `privileged_target`. Privileged is Entra's sense: only a Privileged Authentication Administrator resets the credentials of any admin role's holder. Here that is a Privileged root (`/ADMIN`, `/UYELER/YK`, `/UYELER/DK` or a Group under one; `AUTHZ_PRIVILEGED_ROOTS`), or a role the person holds **in effect**, as Keycloak resolves it (Groups above the person's, realm and client composites and the default role included: `GET /users/{id}/role-mappings/realm/composite` and, for each client but `account`, `account-console`, `admin-cli` and `security-admin-console`, `GET /users/{id}/role-mappings/clients/{client}/composite`, the client list kept ten minutes): every client role that is no app role (core's Protected classification, `authz.ClassifyRole`: `/ROLLER/FORMS-ALL`, SkyMail's sending, approving and reading roles, `place:admin`, `skycloud/admin`, a role nobody has classified …) and every realm role but `default-roles-<realm>`, `offline_access`, `uma_authorization` and the Java era's `USER` (everyone's). `broker/read-token` (one's own identity provider token) counts for nobody. Until the role cleanup's F3 removes the Java era's other realm roles (team and `<TEAM>_LEADER` roles), the people who still hold one are ADMIN's alone too. A read that fails refuses the write.
- **One's own account** is never changed here: 409 `own_account`. The person uses the Account center, which asks for Sudo mode (ADR-0043).
- A client's **service account**: 409 `service_account`. A person **erased or being erased**: 409 `account_not_active`; the read answers only `id` and `status`.
- The answer's `can` says the same for the card: `{"manage": false, "reason": "own_account"}`.
- Every write, and every write refused for the caller (forbidden, own account, privileged target), is a Team audit record row (`user.*`, [team-audit.md](team-audit.md)). No row, no change (503 `audit_unavailable`). The same refusal repeated within a minute writes no second row: the same caller, person, action and code; for `forbidden` the person does not count, so random ids do not fill the record. The caller is decided before the request's own input: a reader's bad input is still 403.

## Routes

| Route | Does |
| --- | --- |
| `GET /v1/users/{id}/account` | username, `enabled`, `createdAt`, `email` (`primary`, `primaryVerified`, `primaryKind` school/personal/none, `school`, `personal`, `personalVerified`), `ytuVerified`, `requiredActions`, `credentials` (id, type, label, date; no secret), `identityProviders` (alias, user name, `ytu`), `sessions` (start, last access, applications; no address), `lockout`, `logins` (the newest ten sign-in events and the last successful sign-in, or `null` without `view-events`), `can`. `Cache-Control: no-store` |
| `PUT /v1/users/{id}/enabled {enabled}` | sign-in on or off. Off also ends every session. Sent as `{"enabled": …}` alone, without the erasure header, so Keycloak does not mark the person as being erased |
| `POST /v1/users/{id}/password {password?}` | a temporary password (Keycloak adds `UPDATE_PASSWORD`). Without `password` core makes one up (`hT7k-Pq3m-Zx9r-Wd2n`, about 92 bits) and answers it once: `{"generated": true, "password": …, "temporary": true, "sessionsClosed": true}`, `Cache-Control: no-store`. Every session ends. A password the realm's policy refuses: 422 `password_policy` with `policy` (Keycloak's message key). The password is in no log, row or error |
| `POST /v1/users/{id}/password-reset-mail` | Keycloak's execute-actions mail with `UPDATE_PASSWORD` (SkyMail `keycloak.generic`). The link lives as long as Keycloak's realm setting for admin-made action tokens (`actionTokenGeneratedByAdminLifespan`; Keycloak's default 12 hours, not managed by the reconciler); core sends no `lifespan`. No primary e-mail: 409 `no_email`; the mail not sent: 502 `mail_not_sent` |
| `PUT /v1/users/{id}/email {address, verified, primary}` | a new Personal e-mail (ADR-0044), trimmed and lower-cased; with `verified` its proof stamp `personalEmailVerifiedAt` too; with `primary` (or when the old Personal e-mail was the primary, or there was none) it becomes Keycloak's `email` with `emailVerified = verified`. A school-domain address: 422 `school_domain`; held by another account (primary, school or personal): 409 `email_taken`. The School e-mail is never written |
| `POST /v1/users/{id}/email/verify {kind?}` | without `kind`: marks the Primary e-mail verified; a Personal primary gets its stamp; a primary from before the two e-mails becomes the proven Personal e-mail (as sky-account's A1c) unless another account holds it (then only the flag). `kind: "personal"`: stamps the Personal e-mail, and sets Keycloak's flag when it is the primary; this also stamps a Personal primary a verification link proved (Keycloak's flag, no stamp) |
| `POST /v1/users/{id}/email/verification-mail` | Keycloak's verify mail (SkyMail `keycloak.verify-email`) to the Primary e-mail |
| `PUT /v1/users/{id}/email/primary {kind}` | `school` or `personal` becomes the primary; an unproven Personal e-mail: 409 `email_not_verified`; none: 409 `no_address` |
| `PUT /v1/users/{id}/required-actions {actions}` | the panel's actions (`UPDATE_PASSWORD`, `CONFIGURE_TOTP`, `webauthn-register-passwordless`, `VERIFY_EMAIL`) set to exactly these; any other the account carries stays. Another alias: 422 `unknown_required_action` |
| `DELETE /v1/users/{id}/credentials/{credentialId}` | one password, authenticator app or passkey |
| `DELETE /v1/users/{id}/identity-providers/{provider}` | the link to an identity provider. Unlinking YTÜ ends the Verified YTÜ account's lock; core's `ytu_linked` stays |
| `DELETE /v1/users/{id}/lockout` | lifts the brute-force protection's temporary lock |
| `PATCH /v1/users/{id}` | as before, and `firstName` / `lastName` are written to Keycloak first (`user.profile` row with the field names), so tokens, mail and the Account center show them. A name that changes follows the account's rules above (not one's own account: 409 `own_account`; a privileged person's only by ADMIN: 403 `privileged_target`; refusals are denied rows); a PATCH whose names Keycloak already holds goes on as before. A name Keycloak's User Profile refuses: 422 `name_invalid` (an address: `email_invalid`, anything else: `account_invalid`). The Account center's own profile is another route and is not touched. A Verified YTÜ account keeps the name written here: the YTÜ identity provider syncs `LEGACY`, which does not write names on later logins (e-skylab-keycloak `BrokerLinkCommitGuard`); only a new YTÜ link sets Microsoft's name again |

## Not here

- The username: the realm keeps `editUsernameAllowed=false`, and Admin REST cannot change it; only sky-account's person route does.
- Impersonation, a permanent delete (deletion is the KVKK request, `DELETE /v1/users/{id}`), a client role given to a person (ADR-0076), consents, ending one session alone.
