package main

import (
	"strings"

	"github.com/skylab-kulubu/core-backend/internal/identity"
)

// keycloakFromEnv reads core's Keycloak service account from the variables
// the server reads, and names the ones that are empty. The server needs all
// four (it also derives the token issuer from the URL and the realm), and
// so do the commands that run beside it. KEYCLOAK_ADMIN_URL is optional: when
// set, Admin REST and the service account's token for it go there instead of
// KEYCLOAK_URL; the error says why a value set there cannot be used.
func keycloakFromEnv(getenv func(string) string) (identity.KeycloakConfig, []string, error) {
	adminURL, err := identity.ParseKeycloakAdminURL(getenv("KEYCLOAK_ADMIN_URL"))
	config := identity.KeycloakConfig{
		URL:          getenv("KEYCLOAK_URL"),
		Realm:        getenv("KEYCLOAK_REALM"),
		AdminURL:     adminURL,
		ClientID:     getenv("KEYCLOAK_CLIENT_ID"),
		ClientSecret: getenv("KEYCLOAK_CLIENT_SECRET"),
	}
	var missing []string
	for _, variable := range []struct{ name, value string }{
		{"KEYCLOAK_URL", config.URL}, {"KEYCLOAK_REALM", config.Realm},
		{"KEYCLOAK_CLIENT_ID", config.ClientID}, {"KEYCLOAK_CLIENT_SECRET", config.ClientSecret},
	} {
		if strings.TrimSpace(variable.value) == "" {
			missing = append(missing, variable.name)
		}
	}
	return config, missing, err
}

// keycloakRealmFromEnv answers KEYCLOAK_URL without trailing slashes or a
// /realms/<realm> suffix, and the realm: KEYCLOAK_REALM, or the one
// KEYCLOAK_URL names. The issuer and the realm token endpoints (core's
// SkyMail and core-erasure tokens) are built on them; KEYCLOAK_ADMIN_URL
// plays no part.
func keycloakRealmFromEnv(getenv func(string) string) (base, realm string) {
	base = strings.TrimRight(getenv("KEYCLOAK_URL"), "/")
	realm = getenv("KEYCLOAK_REALM")
	if parts := strings.SplitN(base, "/realms/", 2); len(parts) == 2 {
		base = parts[0]
		if realm == "" {
			realm = parts[1]
		}
	}
	return base, realm
}

// tokenVerificationFromEnv answers the JWKS URL and the issuer core verifies
// every bearer token against: KEYCLOAK_JWKS_URL, or the certs of the realm at
// KEYCLOAK_URL; the issuer is KEYCLOAK_URL's realm, or the realm the JWKS URL
// names. KEYCLOAK_ADMIN_URL plays no part: Keycloak names its public address
// in iss whichever address issued the token. Either is "" when it cannot be
// derived.
func tokenVerificationFromEnv(getenv func(string) string) (jwksURL, issuer string) {
	jwksURL = getenv("KEYCLOAK_JWKS_URL")
	base, realm := keycloakRealmFromEnv(getenv)
	if jwksURL == "" && base != "" && realm != "" {
		jwksURL = base + "/realms/" + realm + "/protocol/openid-connect/certs"
	}
	if base != "" && realm != "" {
		issuer = base + "/realms/" + realm
	} else if i := strings.Index(jwksURL, "/realms/"); i >= 0 {
		host := jwksURL[:i]
		rest := jwksURL[i+len("/realms/"):]
		realmPart, _, _ := strings.Cut(rest, "/")
		if host != "" && realmPart != "" {
			issuer = host + "/realms/" + realmPart
		}
	}
	return jwksURL, issuer
}
