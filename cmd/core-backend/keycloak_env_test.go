package main

import (
	"strings"
	"testing"
)

func TestKeycloakFromEnvReadsTheOptionalAdminURL(t *testing.T) {
	t.Parallel()
	values := map[string]string{
		"KEYCLOAK_URL": "https://e.yildizskylab.com", "KEYCLOAK_REALM": "e-skylab",
		"KEYCLOAK_CLIENT_ID": "core", "KEYCLOAK_CLIENT_SECRET": "secret",
	}
	getenv := func(key string) string { return values[key] }

	config, missing, err := keycloakFromEnv(getenv)
	if err != nil || len(missing) != 0 || config.AdminURL != "" {
		t.Fatalf("unset: AdminURL=%q missing=%v err=%v", config.AdminURL, missing, err)
	}

	values["KEYCLOAK_ADMIN_URL"] = " http://sky-lab-production-keycloak-cfrcp6:8080/ "
	config, missing, err = keycloakFromEnv(getenv)
	if err != nil || len(missing) != 0 {
		t.Fatalf("set: missing=%v err=%v", missing, err)
	}
	if config.AdminURL != "http://sky-lab-production-keycloak-cfrcp6:8080" || config.URL != "https://e.yildizskylab.com" {
		t.Fatalf("set: URL=%q AdminURL=%q", config.URL, config.AdminURL)
	}

	values["KEYCLOAK_ADMIN_URL"] = "sky-lab-production-keycloak-cfrcp6:8080"
	if _, _, err = keycloakFromEnv(getenv); err == nil || !strings.Contains(err.Error(), "KEYCLOAK_ADMIN_URL") {
		t.Fatalf("a value without a scheme: err=%v", err)
	}

	// It is optional: the four required variables are still what is missing.
	values = map[string]string{"KEYCLOAK_ADMIN_URL": "http://keycloak:8080"}
	if _, missing, err = keycloakFromEnv(getenv); err != nil || strings.Join(missing, ",") != "KEYCLOAK_URL,KEYCLOAK_REALM,KEYCLOAK_CLIENT_ID,KEYCLOAK_CLIENT_SECRET" {
		t.Fatalf("only the admin URL: missing=%v err=%v", missing, err)
	}
}

// KEYCLOAK_ADMIN_URL moves Admin REST only. The issuer core checks every
// token against, the keys it verifies them with and the sudo proof's
// introspection stay on KEYCLOAK_URL: Keycloak names its public address in
// iss whichever address a token was fetched from.
func TestKeycloakAdminURLLeavesTokenVerificationOnKeycloakURL(t *testing.T) {
	t.Parallel()
	for _, admin := range []string{"", "http://sky-lab-production-keycloak-cfrcp6:8080"} {
		values := map[string]string{
			"KEYCLOAK_URL": "https://e.yildizskylab.com/", "KEYCLOAK_REALM": "e-skylab",
			"KEYCLOAK_CLIENT_ID": "core", "KEYCLOAK_CLIENT_SECRET": "secret",
			"KEYCLOAK_ADMIN_URL": admin,
		}
		getenv := func(key string) string { return values[key] }

		jwksURL, issuer := tokenVerificationFromEnv(getenv)
		if issuer != "https://e.yildizskylab.com/realms/e-skylab" {
			t.Fatalf("admin %q: issuer = %q", admin, issuer)
		}
		if jwksURL != "https://e.yildizskylab.com/realms/e-skylab/protocol/openid-connect/certs" {
			t.Fatalf("admin %q: JWKS = %q", admin, jwksURL)
		}
		// core's SkyMail and core-erasure tokens come from the realm token
		// endpoint built on these.
		if base, realm := keycloakRealmFromEnv(getenv); base != "https://e.yildizskylab.com" || realm != "e-skylab" {
			t.Fatalf("admin %q: realm base %q realm %q", admin, base, realm)
		}
		introspection, ok := sudoIntrospection(getenv, issuer)
		if !ok || introspection.URL != "https://e.yildizskylab.com/realms/e-skylab/protocol/openid-connect/token/introspect" {
			t.Fatalf("admin %q: introspection = %q, %v", admin, introspection.URL, ok)
		}
	}
}

func TestTokenVerificationFromEnvKeepsTodaysDerivation(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name                 string
		env                  map[string]string
		wantJWKS, wantIssuer string
	}{
		{"nothing", map[string]string{}, "", ""},
		{"realm in the URL", map[string]string{"KEYCLOAK_URL": "https://e.yildizskylab.com/realms/e-skylab"},
			"https://e.yildizskylab.com/realms/e-skylab/protocol/openid-connect/certs", "https://e.yildizskylab.com/realms/e-skylab"},
		{"explicit JWKS, issuer from the URL", map[string]string{
			"KEYCLOAK_URL": "https://e.yildizskylab.com", "KEYCLOAK_REALM": "e-skylab",
			"KEYCLOAK_JWKS_URL": "http://keycloak:8080/realms/e-skylab/protocol/openid-connect/certs"},
			"http://keycloak:8080/realms/e-skylab/protocol/openid-connect/certs", "https://e.yildizskylab.com/realms/e-skylab"},
		{"issuer from the JWKS URL alone", map[string]string{
			"KEYCLOAK_JWKS_URL": "https://e.yildizskylab.com/realms/e-skylab/protocol/openid-connect/certs"},
			"https://e.yildizskylab.com/realms/e-skylab/protocol/openid-connect/certs", "https://e.yildizskylab.com/realms/e-skylab"},
	} {
		getenv := func(key string) string { return tc.env[key] }
		jwksURL, issuer := tokenVerificationFromEnv(getenv)
		if jwksURL != tc.wantJWKS || issuer != tc.wantIssuer {
			t.Errorf("%s: JWKS %q issuer %q; want %q %q", tc.name, jwksURL, issuer, tc.wantJWKS, tc.wantIssuer)
		}
	}
}
