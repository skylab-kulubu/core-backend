# Which tokens are a Bearer

Core's Bearer is a Keycloak access token: signature (RS256, JWKS), `iss`,
`aud` containing `core`, and the `typ` claim `Bearer` (any case: Keycloak
writes `Bearer` on every access token, sign-in, a service account's client
credentials and token exchange alike, and `bearer` for a client set to lower
case its token response).

An ID token (`typ: ID`) of the same realm is signed by the same key and, for
a client whose audience names core, differs only in `typ`; it answers `401`,
as do a refresh token, a DPoP-bound token (core checks no proof) and a token
with no `typ`. Keycloak does not write RFC 9068's `at+jwt` header, so the
claim is the check. The self-deletion intake's proofs keep their own rules
([account-self-delete.md](account-self-delete.md)).
