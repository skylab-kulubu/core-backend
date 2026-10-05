package consent

import (
	"bytes"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func env(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}

func TestConfigFromEnv(t *testing.T) {
	key := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))
	off, err := ConfigFromEnv(env(nil))
	if err != nil || off.Enabled {
		t.Fatalf("unset key: %+v %v", off, err)
	}
	on, err := ConfigFromEnv(env(map[string]string{KeyEnv: key, "PUBLIC_API_ORIGIN": "https://api.example.test/"}))
	if err != nil || !on.Enabled || on.LinkOrigin != "https://api.example.test" || on.ConfirmTemplateKey != DefaultConfirmTemplateKey {
		t.Fatalf("on: %+v %v", on, err)
	}
	if on.ServiceSources["forms"] != SourceForms || on.ServiceSources["place"] != SourcePlace || on.ServiceSources["guessr"] != SourceGuessr {
		t.Fatalf("default sources %v", on.ServiceSources)
	}
	for name, values := range map[string]map[string]string{
		"short key":       {KeyEnv: "abc", "PUBLIC_API_ORIGIN": "https://api.example.test"},
		"no origin":       {KeyEnv: key},
		"bad source":      {KeyEnv: key, "PUBLIC_API_ORIGIN": "https://a.test", ServiceSourcesEnv: "skymail:skymail"},
		"source twice":    {KeyEnv: key, "PUBLIC_API_ORIGIN": "https://a.test", ServiceSourcesEnv: "forms:a,forms:b"},
		"client twice":    {KeyEnv: key, "PUBLIC_API_ORIGIN": "https://a.test", ServiceSourcesEnv: "forms:a,place:a"},
		"not source:pair": {KeyEnv: key, "PUBLIC_API_ORIGIN": "https://a.test", ServiceSourcesEnv: "forms"},
	} {
		if _, err := ConfigFromEnv(env(values)); err == nil {
			t.Errorf("%s: accepted", name)
		} else if strings.Contains(err.Error(), key) {
			t.Errorf("%s: error carries the key", name)
		}
	}
	none, err := ConfigFromEnv(env(map[string]string{KeyEnv: key, "PUBLIC_API_ORIGIN": "https://a.test", ServiceSourcesEnv: "none"}))
	if err != nil || len(none.ServiceSources) != 0 {
		t.Fatalf("none: %v %v", none.ServiceSources, err)
	}
}

func TestLinkTokens(t *testing.T) {
	config := TestConfig(bytes.Repeat([]byte{2}, 32), "https://api.example.test")
	other := TestConfig(bytes.Repeat([]byte{3}, 32), "https://api.example.test")
	id := uuid.New()
	now := time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)

	withdraw := config.sign(linkWithdraw, id, time.Time{})
	if got, err := config.verify(withdraw, linkWithdraw, now.Add(20*365*24*time.Hour)); err != nil || got != id {
		t.Fatalf("withdraw link years later: %v %v", got, err)
	}
	if _, err := config.verify(withdraw, linkConfirm, now); !errors.Is(err, ErrLink) {
		t.Fatalf("a withdraw link confirmed: %v", err)
	}
	if _, err := other.verify(withdraw, linkWithdraw, now); !errors.Is(err, ErrLink) {
		t.Fatalf("another key accepted: %v", err)
	}
	confirm := config.sign(linkConfirm, id, now.Add(time.Hour))
	if _, err := config.verify(confirm, linkConfirm, now.Add(59*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := config.verify(confirm, linkConfirm, now.Add(61*time.Minute)); !errors.Is(err, ErrLinkExpired) {
		t.Fatalf("expired: %v", err)
	}
	for _, bad := range []string{"", "x", withdraw + "A", strings.ToUpper(withdraw)} {
		if _, err := config.verify(bad, linkWithdraw, now); !errors.Is(err, ErrLink) {
			t.Fatalf("%q: %v", bad, err)
		}
	}
	if strings.Contains(config.WithdrawURL(id), id.String()) {
		t.Fatal("the link shows the grant id in clear")
	}
}

func TestNormalizeEmail(t *testing.T) {
	for raw, want := range map[string]string{
		" Ada@Example.COM ": "ada@example.com",
		"a@b.c":             "a@b.c",
	} {
		if got, ok := NormalizeEmail(raw); !ok || got != want {
			t.Errorf("%q: %q %v", raw, got, ok)
		}
	}
	for _, raw := range []string{"", "ada", "a@b", "@b.c", "a@@b.c", "a b@c.d", "a@b.c, d@e.f"} {
		if _, ok := NormalizeEmail(raw); ok {
			t.Errorf("%q accepted", raw)
		}
	}
}

func TestEveryPurposeHasATextAndASource(t *testing.T) {
	for purpose := range purposes {
		if _, ok := CurrentText(purpose); !ok {
			t.Errorf("%s has no text", purpose)
		}
		if len(purposeSources[purpose]) == 0 {
			t.Errorf("%s has no source", purpose)
		}
	}
}
