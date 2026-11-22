# A person's account from the person card

The admin panel's person card reads and changes the person's Keycloak account through core (P7, 2026-11-22: "Keycloak'taki tüm özellikleri burdan da yönetebilmeliyiz"). Keycloak stays the identity store; core calls its Admin REST with `service-account-core` ([keycloak-admin-permissions.md](keycloak-admin-permissions.md)). Code: `internal/identity/user_accounts*.go`, `internal/handlers/user_accounts.go`.

## Who

| | Read (`GET …/account`) | Write |
| --- | --- | --- |
| `users:manage` (ADMIN, YK) | yes | yes, except below |
| Privileged reader (DK, `global:read`) | yes | no |
| anyone else | no (403) | no (403) |

- **A Privileged person's account** (in `/ADMIN`, `/UYELER/YK`, `/UYELER/DK` or under one; `AUTHZ_PRIVILEGED_ROOTS`) is changed only by someone in the ADMIN root: 403 `privileged_target`. Entra's split between the Authentication Administrator and the Privileged Authentication Administrator.
- **One's own account** is never changed here: 409 `own_account`. The person uses the Account center, which asks for Sudo mode (ADR-0043).
- A client's **service account**: 409 `service_account`. A person **erased or being erased**: 409 `account_not_active`; the read answers only `id` and `status`.
- The answer's `can` says the same for the card: `{"manage": false, "reason": "own_account"}`.
- Every write, and every write refused for the caller (forbidden, own account, Privileged target), is a Team audit record row (`user.*`, [team-audit.md](team-audit.md)). No row, no change (503 `audit_unavailable`).

## Routes

| Route | Does |
| --- | --- |
| `GET /v1/users/{id}/account` | username, `enabled`, `createdAt`, `email` (`primary`, `primaryVerified`, `primaryKind` school/personal/none, `school`, `personal`, `personalVerified`), `ytuVerified`, `requiredActions`, `credentials` (id, type, label, date; no secret), `identityProviders` (alias, user name, `ytu`), `sessions` (start, last access, applications; no address), `lockout`, `logins` (the newest ten sign-in events and the last successful sign-in, or `null` without `view-events`), `can`. `Cache-Control: no-store` |
| `PUT /v1/users/{id}/enabled {enabled}` | sign-in on or off. Off also ends every session. Sent as `{"enabled": …}` alone, without the erasure header, so Keycloak does not mark the person as being erased |
| `POST /v1/users/{id}/password {password?}` | a temporary password (Keycloak adds `UPDATE_PASSWORD`). Without `password` core makes one up (`hT7k-Pq3m-Zx9r-Wd2n`, about 92 bits) and answers it once: `{"generated": true, "password": …, "temporary": true, "sessionsClosed": true}`, `Cache-Control: no-store`. Every session ends. A password the realm's policy refuses: 422 `password_policy` with `policy` (Keycloak's message key). The password is in no log, row or error |
| `POST /v1/users/{id}/password-reset-mail` | Keycloak's execute-actions mail with `UPDATE_PASSWORD` (SkyMail `keycloak.generic`). No primary e-mail: 409 `no_email`; the mail not sent: 502 `mail_not_sent` |
| `PUT /v1/users/{id}/email {address, verified, primary}` | a new Personal e-mail (ADR-0044), trimmed and lower-cased; with `verified` its proof stamp `personalEmailVerifiedAt` too; with `primary` (or when the old Personal e-mail was the primary, or there was none) it becomes Keycloak's `email` with `emailVerified = verified`. A school-domain address: 422 `school_domain`; held by another account (primary, school or personal): 409 `email_taken`. The School e-mail is never written |
| `POST /v1/users/{id}/email/verify` | marks the Primary e-mail verified; a Personal primary gets its stamp; a primary from before the two e-mails becomes the proven Personal e-mail (as sky-account's A1c) |
| `POST /v1/users/{id}/email/verification-mail` | Keycloak's verify mail (SkyMail `keycloak.verify-email`) to the Primary e-mail |
| `PUT /v1/users/{id}/email/primary {kind}` | `school` or `personal` becomes the primary; an unproven Personal e-mail: 409 `email_not_verified`; none: 409 `no_address` |
| `PUT /v1/users/{id}/required-actions {actions}` | the panel's actions (`UPDATE_PASSWORD`, `CONFIGURE_TOTP`, `webauthn-register-passwordless`, `VERIFY_EMAIL`) set to exactly these; any other the account carries stays. Another alias: 422 `unknown_required_action` |
| `DELETE /v1/users/{id}/credentials/{credentialId}` | one password, authenticator app or passkey |
| `DELETE /v1/users/{id}/identity-providers/{provider}` | the link to an identity provider. Unlinking YTÜ ends the Verified YTÜ account's lock; core's `ytu_linked` stays |
| `DELETE /v1/users/{id}/lockout` | lifts the brute-force protection's temporary lock |
| `PATCH /v1/users/{id}` | as before, and `firstName` / `lastName` are written to Keycloak first (`user.profile` row with the field names), so tokens, mail and the Account center show them |

## Not here

- The username: the realm keeps `editUsernameAllowed=false`, and Admin REST cannot change it; only sky-account's person route does.
- Impersonation, a permanent delete (deletion is the KVKK request, `DELETE /v1/users/{id}`), a client role given to a person (ADR-0076), consents, ending one session alone.
