package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"strings"
	"testing"
	"time"

	"github.com/skylab-kulubu/core-backend/internal/accessgate"
	"github.com/skylab-kulubu/core-backend/internal/mail"
)

func TestLoadSkyPassKeyPrefersExplicitP256Environment(t *testing.T) {
	want, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(want)
	if err != nil {
		t.Fatal(err)
	}
	raw := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	t.Setenv("SKYPASS_EC_PRIVATE_KEY", string(raw))
	t.Setenv("SKYPASS_RSA_PRIVATE_KEY", "invalid legacy value")

	got, err := loadSkyPassKey()
	if err != nil {
		t.Fatal(err)
	}
	if got.Curve != elliptic.P256() || got.D.Cmp(want.D) != 0 {
		t.Fatal("loaded key does not match explicit P-256 key")
	}
}

func TestAccountErasureCannotStartWithoutEnforcedAccessGate(t *testing.T) {
	t.Parallel()

	if err := validateAccountErasureGate(false, accessgate.ModeOff); err != nil {
		t.Fatal(err)
	}
	if err := validateAccountErasureGate(true, accessgate.ModeOff); err == nil {
		t.Fatal("erasure worker started while the shared access gate was off")
	}
	if err := validateAccountErasureGate(true, accessgate.ModeEnforce); err != nil {
		t.Fatal(err)
	}
}

func TestDisabledAccountAccessGateRemainsNilAtHTTPBoundary(t *testing.T) {
	t.Parallel()

	var gate *accessgate.RedisGate
	if reader := optionalAccountAccessGate(gate); reader != nil {
		t.Fatal("disabled account access gate became a non-nil HTTP dependency")
	}
}

func TestLoadSkyPassKeyDerivesES256FromLegacyRSAEnvironment(t *testing.T) {
	legacy, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	raw := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(legacy)})
	t.Setenv("SKYPASS_EC_PRIVATE_KEY", "")
	t.Setenv("SKYPASS_RSA_PRIVATE_KEY", string(raw))

	key, err := loadSkyPassKey()
	if err != nil {
		t.Fatal(err)
	}
	if key.Curve != elliptic.P256() {
		t.Fatalf("curve = %v", key.Curve)
	}
}

func TestGotenbergURLDefaultsToComposeService(t *testing.T) {
	t.Setenv("GOTENBERG_URL", "")
	if got := gotenbergURL(); got != "http://gotenberg:3000" {
		t.Fatalf("gotenberg URL = %q", got)
	}

	t.Setenv("GOTENBERG_URL", " http://pdf.internal:3010/ ")
	if got := gotenbergURL(); got != "http://pdf.internal:3010/" {
		t.Fatalf("explicit gotenberg URL = %q", got)
	}
}

func TestAccountErasureWorkerIsDefaultOffAndRequiresFullKeycloakConfig(t *testing.T) {
	values := map[string]string{}
	getenv := func(key string) string { return values[key] }

	enabled, err := accountErasureWorkerEnabled(getenv)
	if err != nil || enabled {
		t.Fatalf("default enabled=%v err=%v", enabled, err)
	}
	values["KEYCLOAK_URL"] = "https://identity.example.test"
	enabled, err = accountErasureWorkerEnabled(getenv)
	if err != nil || enabled {
		t.Fatalf("Keycloak alone enabled=%v err=%v", enabled, err)
	}

	values["ACCOUNT_ERASURE_WORKER_ENABLED"] = "true"
	if enabled, err = accountErasureWorkerEnabled(getenv); err == nil || enabled {
		t.Fatalf("incomplete config enabled=%v err=%v", enabled, err)
	}
	values["KEYCLOAK_REALM"] = "e-skylab"
	values["KEYCLOAK_CLIENT_ID"] = "core"
	values["KEYCLOAK_CLIENT_SECRET"] = "secret"
	if enabled, err = accountErasureWorkerEnabled(getenv); err != nil || !enabled {
		t.Fatalf("full config enabled=%v err=%v", enabled, err)
	}

	values["ACCOUNT_ERASURE_WORKER_ENABLED"] = "yes"
	if enabled, err = accountErasureWorkerEnabled(getenv); err == nil || enabled {
		t.Fatalf("invalid flag enabled=%v err=%v", enabled, err)
	}
}

func TestAccountSelfDeletionReceiptKeyIsRequiredOnlyWhenFeatureEnabled(t *testing.T) {
	t.Parallel()
	values := map[string]string{}
	getenv := func(key string) string { return values[key] }

	disabled, err := accountSelfDeletionConfig(getenv, false)
	if err != nil || disabled.Enabled || len(disabled.ReceiptKey) != 0 {
		t.Fatalf("disabled config=%+v err=%v", disabled, err)
	}
	if _, err := accountSelfDeletionConfig(getenv, true); err == nil {
		t.Fatal("enabled self deletion accepted a missing receipt key")
	}
	values["ACCOUNT_DELETION_RECEIPT_KEY"] = base64.RawURLEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	enabled, err := accountSelfDeletionConfig(getenv, true)
	if err != nil || !enabled.Enabled || len(enabled.ReceiptKey) != 32 {
		t.Fatalf("enabled config=%+v err=%v", enabled, err)
	}
}

func TestTemplateKeyDefaultsToTheSeededKeyAndAnEmptyValueOptsOut(t *testing.T) {
	t.Parallel()
	values := map[string]string{}
	lookup := func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
	welcome := func() string {
		return templateKey(lookup, "SKYMAIL_WELCOME_TEMPLATE_KEY", mail.DefaultWelcomeTemplateKey)
	}

	if got := welcome(); got != "core.welcome" {
		t.Fatalf("unset key = %q", got)
	}
	if got := templateKey(lookup, "SKYMAIL_CERTIFICATE_TEMPLATE_KEY", mail.DefaultCertificateTemplateKey); got != "core.certificate" {
		t.Fatalf("unset certificate key = %q", got)
	}

	values["SKYMAIL_WELCOME_TEMPLATE_KEY"] = "  core.welcome.next  "
	if got := welcome(); got != "core.welcome.next" {
		t.Fatalf("explicit key = %q", got)
	}

	// An empty variable is the documented way back to the template id while the
	// keys are being seeded; it must not be read as "unset" and defaulted.
	values["SKYMAIL_WELCOME_TEMPLATE_KEY"] = ""
	if got := welcome(); got != "" {
		t.Fatalf("emptied key = %q", got)
	}
}

// The sudo proof is introspected with the realm issuer and Core's own
// confidential client; no new setting exists. Missing any part leaves the
// sudo proof unverifiable, and the intake then refuses it.
func TestSudoIntrospectionReusesIssuerAndCoreClientCredentials(t *testing.T) {
	t.Parallel()
	values := map[string]string{}
	getenv := func(key string) string { return values[key] }
	issuer := "https://identity.example.test/realms/e-skylab"

	if _, ok := sudoIntrospection(getenv, issuer); ok {
		t.Fatal("introspection configured without client credentials")
	}
	values["KEYCLOAK_CLIENT_ID"] = "core"
	if _, ok := sudoIntrospection(getenv, issuer); ok {
		t.Fatal("introspection configured without a client secret")
	}
	values["KEYCLOAK_CLIENT_SECRET"] = "secret"
	if _, ok := sudoIntrospection(getenv, ""); ok {
		t.Fatal("introspection configured without an issuer")
	}

	got, ok := sudoIntrospection(getenv, issuer)
	if !ok {
		t.Fatal("full configuration refused")
	}
	if got.URL != issuer+"/protocol/openid-connect/token/introspect" || got.ClientID != "core" || got.ClientSecret != "secret" {
		t.Fatalf("introspection = {URL:%q ClientID:%q}", got.URL, got.ClientID)
	}
	if got.HTTP == nil || got.HTTP.Timeout <= 0 || got.HTTP.Timeout > 5*time.Second {
		t.Fatalf("introspection client has no short timeout: %+v", got.HTTP)
	}
}

func TestAccountErasureStartupRefusesAMissingServiceSettingByName(t *testing.T) {
	t.Parallel()
	values := map[string]string{
		"KEYCLOAK_URL": "https://identity.example.test", "KEYCLOAK_REALM": "e-skylab",
		"KEYCLOAK_CLIENT_ID": "core", "KEYCLOAK_CLIENT_SECRET": "secret",
	}
	getenv := func(key string) string { return values[key] }

	enabled, config, err := accountErasureStartup(getenv)
	if err != nil || enabled || len(config.Endpoints) != 0 {
		t.Fatalf("worker off: enabled=%v config=%+v err=%v", enabled, config, err)
	}

	values["ACCOUNT_ERASURE_WORKER_ENABLED"] = "true"
	values["ACCOUNT_ERASURE_SKYMAIL_URL"] = "http://skymail-backend:8080"
	values["ACCOUNT_ERASURE_CMS_URL"] = "http://cms-backend:8080"
	values["ACCOUNT_ERASURE_FORMS_URL"] = "http://forms-backend:8080"
	values["ACCOUNT_ERASURE_CLIENT_ID"] = "core-erasure"
	_, _, err = accountErasureStartup(getenv)
	if err == nil || err.Error() != "ACCOUNT_ERASURE_CLIENT_SECRET is required when ACCOUNT_ERASURE_WORKER_ENABLED=true" {
		t.Fatalf("missing secret error = %v", err)
	}

	values["ACCOUNT_ERASURE_CLIENT_SECRET"] = "erasure-secret-value"
	enabled, config, err = accountErasureStartup(getenv)
	if err != nil || !enabled || len(config.Endpoints) != 3 || config.AlertAfter != 480*time.Hour {
		t.Fatalf("full config: enabled=%v endpoints=%d alert=%s err=%v", enabled, len(config.Endpoints), config.AlertAfter, err)
	}

	values["ACCOUNT_ERASURE_CMS_URL"] = "cms-backend"
	if _, _, err = accountErasureStartup(getenv); err == nil || strings.Contains(err.Error(), "cms-backend") ||
		!strings.HasPrefix(err.Error(), "ACCOUNT_ERASURE_CMS_URL ") {
		t.Fatalf("bad URL error = %v", err)
	}
}
