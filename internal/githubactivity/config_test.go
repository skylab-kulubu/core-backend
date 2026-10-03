package githubactivity_test

import (
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"strings"
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/githubactivity"
)

func env(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}

func TestConfigOffWhileNothingIsSet(t *testing.T) {
	t.Parallel()
	_, ok, err := githubactivity.ConfigFromEnv(env(map[string]string{githubactivity.WindowDaysEnv: "14"}))
	if ok || err != nil {
		t.Fatalf("ok %v err %v, want off", ok, err)
	}
}

func TestConfigReadsTheKeyInEveryForm(t *testing.T) {
	t.Parallel()
	pemKey := appKeyPEM(t)
	pkcs8, err := x509.MarshalPKCS8PrivateKey(appKey(t))
	if err != nil {
		t.Fatal(err)
	}
	forms := map[string]string{
		"PEM as GitHub hands it out": pemKey,
		"base64 on one line":         base64.StdEncoding.EncodeToString([]byte(pemKey)),
		"line breaks written as \\n": strings.ReplaceAll(pemKey, "\n", `\n`),
		"PKCS #8":                    string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8})),
	}
	for name, key := range forms {
		config, ok, err := githubactivity.ConfigFromEnv(env(map[string]string{
			githubactivity.OrgEnv: "skylab-kulubu", githubactivity.AppIDEnv: "4242", githubactivity.PrivateKeyEnv: key,
		}))
		if err != nil || !ok {
			t.Fatalf("%s: ok %v err %v", name, ok, err)
		}
		if !config.PrivateKey.Equal(appKey(t)) {
			t.Fatalf("%s: a different key", name)
		}
		if config.Org != "skylab-kulubu" || config.AppID != 4242 || config.InstallationID != 0 || config.WindowDays != githubactivity.DefaultWindowDays {
			t.Fatalf("%s: %+v", name, config)
		}
	}
}

func TestConfigReadsInstallationAndWindow(t *testing.T) {
	t.Parallel()
	config, ok, err := githubactivity.ConfigFromEnv(env(map[string]string{
		githubactivity.OrgEnv: " skylab-kulubu ", githubactivity.AppIDEnv: "4242", githubactivity.InstallationIDEnv: "777",
		githubactivity.PrivateKeyEnv: appKeyPEM(t), githubactivity.WindowDaysEnv: "14",
	}))
	if err != nil || !ok || config.InstallationID != 777 || config.WindowDays != 14 || config.Org != "skylab-kulubu" {
		t.Fatalf("%+v ok %v err %v", config, ok, err)
	}
}

func TestConfigNamesWhatIsWrongNeverAValue(t *testing.T) {
	t.Parallel()
	const garbage = "-----BEGIN RSA PRIVATE KEY-----\nTOPSECRETMATERIAL\n-----END RSA PRIVATE KEY-----"
	cases := []struct {
		name   string
		values map[string]string
		want   []string
	}{
		{"only the org", map[string]string{githubactivity.OrgEnv: "skylab-kulubu"},
			[]string{githubactivity.AppIDEnv + " is required", githubactivity.PrivateKeyEnv + " is required"}},
		{"a key that is no key", map[string]string{githubactivity.OrgEnv: "skylab-kulubu", githubactivity.AppIDEnv: "1", githubactivity.PrivateKeyEnv: garbage},
			[]string{githubactivity.PrivateKeyEnv}},
		{"base64 of nothing useful", map[string]string{githubactivity.OrgEnv: "skylab-kulubu", githubactivity.AppIDEnv: "1", githubactivity.PrivateKeyEnv: "VE9QU0VDUkVUTUFURVJJQUw="},
			[]string{githubactivity.PrivateKeyEnv}},
		{"not base64 either", map[string]string{githubactivity.OrgEnv: "skylab-kulubu", githubactivity.AppIDEnv: "1", githubactivity.PrivateKeyEnv: "TOPSECRETMATERIAL!!"},
			[]string{githubactivity.PrivateKeyEnv}},
		{"a bad app id", map[string]string{githubactivity.OrgEnv: "skylab-kulubu", githubactivity.AppIDEnv: "-3", githubactivity.PrivateKeyEnv: appKeyPEM(t)},
			[]string{githubactivity.AppIDEnv + " must be a positive number"}},
		{"a bad installation id", map[string]string{githubactivity.OrgEnv: "skylab-kulubu", githubactivity.AppIDEnv: "1", githubactivity.InstallationIDEnv: "x", githubactivity.PrivateKeyEnv: appKeyPEM(t)},
			[]string{githubactivity.InstallationIDEnv}},
		{"an org that is no login", map[string]string{githubactivity.OrgEnv: "skylab kulubu", githubactivity.AppIDEnv: "1", githubactivity.PrivateKeyEnv: appKeyPEM(t)},
			[]string{githubactivity.OrgEnv}},
		{"no org", map[string]string{githubactivity.AppIDEnv: "1", githubactivity.PrivateKeyEnv: appKeyPEM(t)},
			[]string{githubactivity.OrgEnv + " is required"}},
		{"a window of nothing", map[string]string{githubactivity.OrgEnv: "skylab-kulubu", githubactivity.AppIDEnv: "1", githubactivity.PrivateKeyEnv: appKeyPEM(t), githubactivity.WindowDaysEnv: "0"},
			[]string{githubactivity.WindowDaysEnv}},
		{"a window too long", map[string]string{githubactivity.OrgEnv: "skylab-kulubu", githubactivity.AppIDEnv: "1", githubactivity.PrivateKeyEnv: appKeyPEM(t), githubactivity.WindowDaysEnv: "91"},
			[]string{githubactivity.WindowDaysEnv}},
	}
	for _, tc := range cases {
		_, ok, err := githubactivity.ConfigFromEnv(env(tc.values))
		if !ok || err == nil {
			t.Fatalf("%s: ok %v err %v, want an error", tc.name, ok, err)
		}
		for _, want := range tc.want {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("%s: %q does not say %q", tc.name, err, want)
			}
		}
		if strings.Contains(err.Error(), "TOPSECRET") || strings.Contains(err.Error(), "BEGIN") {
			t.Fatalf("%s: the error quotes the value: %q", tc.name, err)
		}
	}
}
