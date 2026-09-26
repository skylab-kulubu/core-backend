package main

import (
	"strings"

	"github.com/skylab-kulubu/core-backend/internal/identity"
)

// keycloakFromEnv reads core's Keycloak service account from the variables
// the server reads, and names the ones that are empty. The server needs all
// four (it also derives the token issuer from the URL and the realm), and
// so do the commands that run beside it.
func keycloakFromEnv(getenv func(string) string) (identity.KeycloakConfig, []string) {
	config := identity.KeycloakConfig{
		URL:          getenv("KEYCLOAK_URL"),
		Realm:        getenv("KEYCLOAK_REALM"),
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
	return config, missing
}
