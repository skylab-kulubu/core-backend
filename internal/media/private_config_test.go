package media_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/media"
)

// privateEnv is a complete private Media configuration next to the public
// bucket's.
func privateEnv() map[string]string {
	return map[string]string{
		"R2_ENDPOINT":             "https://account.r2.cloudflarestorage.com",
		"R2_BUCKET":               "skylab-cdn",
		"R2_ACCESS_KEY":           "public-access",
		"R2_SECRET_KEY":           "public-secret",
		"MEDIA_PRIVATE_ENABLED":   "true",
		"MEDIA_OPENBAO_ADDR":      "http://openbao:8200",
		"MEDIA_TRANSIT_MOUNT":     "transit/sandbox",
		"MEDIA_TRANSIT_KEY":       "media",
		"MEDIA_OPENBAO_ROLE_ID":   "role-id-value",
		"MEDIA_OPENBAO_SECRET_ID": "secret-id-value",
		"R2_PRIVATE_BUCKET":       "skylab-private-sandbox",
		"R2_PRIVATE_ACCESS_KEY":   "private-access-value",
		"R2_PRIVATE_SECRET_KEY":   "private-secret-value",
		// 32 bytes, unpadded base64url.
		"MEDIA_LINK_SIGNING_KEY": "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8",
		"PUBLIC_API_ORIGIN":      "https://sandbox-api.yildizskylab.com",
	}
}

func lookup(env map[string]string) func(string) string {
	return func(name string) string { return env[name] }
}

func TestPrivateMediaOffNeedsNoPrivateSetting(t *testing.T) {
	t.Parallel()
	for name, env := range map[string]map[string]string{
		"flag unset": {"R2_BUCKET": "skylab-cdn"},
		"flag false with broken private settings": {
			"MEDIA_PRIVATE_ENABLED": "false", "MEDIA_OPENBAO_ADDR": "not an address", "MEDIA_LINK_SIGNING_KEY": "short",
		},
	} {
		config, err := media.PrivateConfigFromEnv(lookup(env))
		if err != nil || config.Enabled {
			t.Errorf("%s: config %+v, err %v", name, config, err)
		}
	}
}

func TestPrivateMediaFlagIsTrueOrFalse(t *testing.T) {
	t.Parallel()
	env := privateEnv()
	env["MEDIA_PRIVATE_ENABLED"] = "yes"
	_, err := media.PrivateConfigFromEnv(lookup(env))
	if err == nil || !strings.Contains(err.Error(), "MEDIA_PRIVATE_ENABLED") {
		t.Fatalf("err = %v", err)
	}
}

func TestPrivateMediaOnReadsEverySetting(t *testing.T) {
	t.Parallel()
	config, err := media.PrivateConfigFromEnv(lookup(privateEnv()))
	if err != nil {
		t.Fatal(err)
	}
	if !config.Enabled {
		t.Fatal("not enabled")
	}
	transit := config.Transit
	if transit.Addr != "http://openbao:8200" || transit.Mount != "transit/sandbox" || transit.Key != "media" ||
		transit.RoleID != "role-id-value" || transit.SecretID != "secret-id-value" {
		t.Fatalf("transit %+v", transit)
	}
	bucket := config.Bucket
	if bucket.Endpoint != "https://account.r2.cloudflarestorage.com" || bucket.Bucket != "skylab-private-sandbox" ||
		bucket.AccessKey != "private-access-value" || bucket.SecretKey != "private-secret-value" {
		t.Fatalf("bucket %+v", bucket)
	}
	want := make([]byte, 32)
	for i := range want {
		want[i] = byte(i)
	}
	if !bytes.Equal(config.LinkKey, want) {
		t.Fatalf("link key %x", config.LinkKey)
	}
	if config.LinkOrigin != "https://sandbox-api.yildizskylab.com" {
		t.Fatalf("links point at %q", config.LinkOrigin)
	}
}

func TestReadLinksPointAtThePublicAPIOriginWithoutItsSlash(t *testing.T) {
	t.Parallel()
	env := privateEnv()
	env["PUBLIC_API_ORIGIN"] = "https://api.sandbox.example.test/"
	config, err := media.PrivateConfigFromEnv(lookup(env))
	if err != nil || config.LinkOrigin != "https://api.sandbox.example.test" {
		t.Fatalf("link origin %q, err %v", config.LinkOrigin, err)
	}
}

func TestPrivateMediaOnRefusesAMissingSettingByName(t *testing.T) {
	t.Parallel()
	for _, name := range []string{
		"MEDIA_OPENBAO_ADDR", "MEDIA_TRANSIT_MOUNT", "MEDIA_TRANSIT_KEY", "MEDIA_OPENBAO_ROLE_ID", "MEDIA_OPENBAO_SECRET_ID",
		"R2_PRIVATE_BUCKET", "R2_PRIVATE_ACCESS_KEY", "R2_PRIVATE_SECRET_KEY", "R2_ENDPOINT", "MEDIA_LINK_SIGNING_KEY",
		// Without it read links would silently point at production.
		"PUBLIC_API_ORIGIN",
	} {
		env := privateEnv()
		delete(env, name)
		_, err := media.PrivateConfigFromEnv(lookup(env))
		if err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("without %s: err = %v", name, err)
		}
	}
}

func TestPrivateMediaOnRefusesAnInvalidSettingWithoutItsValue(t *testing.T) {
	t.Parallel()
	for name, value := range map[string]string{
		"MEDIA_OPENBAO_ADDR":     "bao.internal:8200",
		"MEDIA_TRANSIT_MOUNT":    "transit/../sys",
		"MEDIA_TRANSIT_KEY":      "media/other",
		"MEDIA_LINK_SIGNING_KEY": "c2hvcnQta2V5LXZhbHVl",
		// The private bucket is never the public one, nor reached with
		// the public bucket's key.
		"R2_PRIVATE_BUCKET":     "skylab-cdn",
		"R2_PRIVATE_ACCESS_KEY": "public-access",
		"PUBLIC_API_ORIGIN":     "api.sandbox.example.test",
	} {
		env := privateEnv()
		env[name] = value
		_, err := media.PrivateConfigFromEnv(lookup(env))
		if err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("%s=%q: err = %v", name, value, err)
			continue
		}
		if strings.Contains(err.Error(), value) {
			t.Errorf("%s: the error shows the value: %v", name, err)
		}
	}
}
