package erasure_test

import (
	"strings"
	"testing"
	"time"

	"github.com/skylab-kulubu/core-backend/internal/erasure"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

func TestRegistryListsTheThreeServicesInSagaOrder(t *testing.T) {
	t.Parallel()

	got := erasure.Registry()
	want := []erasure.Service{
		{Name: "skymail", Step: user.DeletionStepEraseSkyMail, URLVar: "ACCOUNT_ERASURE_SKYMAIL_URL", Scope: "account-erase-skymail"},
		{Name: "cms", Step: user.DeletionStepEraseCMS, URLVar: "ACCOUNT_ERASURE_CMS_URL", Scope: "account-erase-cms"},
		{Name: "forms", Step: user.DeletionStepEraseForms, URLVar: "ACCOUNT_ERASURE_FORMS_URL", Scope: "account-erase-forms"},
	}
	if len(got) != len(want) {
		t.Fatalf("registry = %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("registry[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
	got[0].Scope = "changed"
	if erasure.Registry()[0].Scope != "account-erase-skymail" {
		t.Fatal("registry is mutable through a returned slice")
	}
}

func completeEnv() map[string]string {
	return map[string]string{
		"ACCOUNT_ERASURE_SKYMAIL_URL":   "http://skymail-backend:8080/",
		"ACCOUNT_ERASURE_CMS_URL":       "http://cms-backend:8080",
		"ACCOUNT_ERASURE_FORMS_URL":     "https://forms-backend.internal",
		"ACCOUNT_ERASURE_CLIENT_ID":     "core-erasure",
		"ACCOUNT_ERASURE_CLIENT_SECRET": "s3cr3t-value-never-printed",
	}
}

func TestConfigIsNotReadWhileTheWorkerIsOff(t *testing.T) {
	t.Parallel()

	reads := 0
	getenv := func(string) string {
		reads++
		return "not a duration"
	}
	config, err := erasure.ConfigFromEnv(getenv, false)
	if err != nil {
		t.Fatal(err)
	}
	if reads != 0 {
		t.Fatalf("disabled config read %d variables", reads)
	}
	if len(config.Endpoints) != 0 || config.ClientID != "" || config.ClientSecret != nil {
		t.Fatalf("disabled config = %+v", config)
	}
	if config.AlertAfter != 480*time.Hour || config.PeriodicDestructionInterval != 90*24*time.Hour {
		t.Fatalf("disabled defaults = %+v", config)
	}
}

func TestConfigRequiresEveryServiceAndTheClientWhenEnabled(t *testing.T) {
	t.Parallel()

	for _, name := range []string{
		"ACCOUNT_ERASURE_SKYMAIL_URL",
		"ACCOUNT_ERASURE_CMS_URL",
		"ACCOUNT_ERASURE_FORMS_URL",
		"ACCOUNT_ERASURE_CLIENT_ID",
		"ACCOUNT_ERASURE_CLIENT_SECRET",
	} {
		values := completeEnv()
		values[name] = "   "
		_, err := erasure.ConfigFromEnv(func(key string) string { return values[key] }, true)
		if err == nil {
			t.Fatalf("missing %s accepted", name)
		}
		want := name + " is required when ACCOUNT_ERASURE_WORKER_ENABLED=true"
		if err.Error() != want {
			t.Fatalf("missing %s error = %q, want %q", name, err, want)
		}
	}
}

func TestConfigRejectsABadValueByNameOnly(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"ACCOUNT_ERASURE_SKYMAIL_URL":   "skymail-backend:8080",
		"ACCOUNT_ERASURE_CMS_URL":       "ftp://cms-backend",
		"ACCOUNT_ERASURE_FORMS_URL":     "http://forms-backend/?token=leak",
		"ACCOUNT_ERASURE_ALERT_AFTER":   "-1h",
		"PERIODIC_DESTRUCTION_INTERVAL": "4500h",
	}
	for name, bad := range cases {
		values := completeEnv()
		values[name] = bad
		_, err := erasure.ConfigFromEnv(func(key string) string { return values[key] }, true)
		if err == nil {
			t.Fatalf("%s=%q accepted", name, bad)
		}
		if !strings.HasPrefix(err.Error(), name+" ") {
			t.Fatalf("%s error does not lead with the name: %q", name, err)
		}
		if strings.Contains(err.Error(), bad) {
			t.Fatalf("%s error echoes the value: %q", name, err)
		}
	}
}

func TestConfigBuildsEndpointsInRegistryOrder(t *testing.T) {
	t.Parallel()

	values := completeEnv()
	values["ACCOUNT_ERASURE_ALERT_AFTER"] = "72h"
	values["PERIODIC_DESTRUCTION_INTERVAL"] = "4380h"
	config, err := erasure.ConfigFromEnv(func(key string) string { return values[key] }, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(config.Endpoints) != 3 {
		t.Fatalf("endpoints = %+v", config.Endpoints)
	}
	wantURLs := []string{"http://skymail-backend:8080", "http://cms-backend:8080", "https://forms-backend.internal"}
	for i, endpoint := range config.Endpoints {
		if endpoint.Service != erasure.Registry()[i] || endpoint.BaseURL != wantURLs[i] {
			t.Fatalf("endpoint[%d] = %+v", i, endpoint)
		}
	}
	if config.ClientID != "core-erasure" {
		t.Fatal("client id not read")
	}
	if secret, err := config.ClientSecret(); err != nil || secret != "s3cr3t-value-never-printed" {
		t.Fatal("client secret not read")
	}
	if config.AlertAfter != 72*time.Hour || config.PeriodicDestructionInterval != 4380*time.Hour {
		t.Fatalf("durations = %s %s", config.AlertAfter, config.PeriodicDestructionInterval)
	}

	delete(values, "ACCOUNT_ERASURE_ALERT_AFTER")
	delete(values, "PERIODIC_DESTRUCTION_INTERVAL")
	config, err = erasure.ConfigFromEnv(func(key string) string { return values[key] }, true)
	if err != nil {
		t.Fatal(err)
	}
	if config.AlertAfter != 480*time.Hour {
		t.Fatalf("default alert after = %s", config.AlertAfter)
	}
	if config.PeriodicDestructionInterval != 2160*time.Hour {
		t.Fatalf("default periodic destruction interval = %s", config.PeriodicDestructionInterval)
	}
}

func TestConfigReadsTheClientSecretAgainOnEveryCall(t *testing.T) {
	t.Parallel()

	values := completeEnv()
	config, err := erasure.ConfigFromEnv(func(key string) string { return values[key] }, true)
	if err != nil {
		t.Fatal(err)
	}
	// Config keeps no copy: a token request after the configuration changes
	// sends the new secret (ADR-0050 rotates it nightly).
	values["ACCOUNT_ERASURE_CLIENT_SECRET"] = " rotated-secret-value "
	if secret, err := config.ClientSecret(); err != nil || secret != "rotated-secret-value" {
		t.Fatal("rotated client secret not read")
	}
	values["ACCOUNT_ERASURE_CLIENT_SECRET"] = ""
	_, err = config.ClientSecret()
	if err == nil || err.Error() != "ACCOUNT_ERASURE_CLIENT_SECRET is required when ACCOUNT_ERASURE_WORKER_ENABLED=true" {
		t.Fatalf("emptied client secret error = %v", err)
	}
}
