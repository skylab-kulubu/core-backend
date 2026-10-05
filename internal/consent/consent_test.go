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
	on, err := ConfigFromEnv(env(map[string]string{KeyEnv: key, "PUBLIC_API_ORIGIN": "https://api.example.test/", TextURLEnv: "https://yildizskylab.com/acik-riza"}))
	if err != nil || !on.Enabled || on.LinkOrigin != "https://api.example.test" || on.ConfirmTemplateKey != DefaultConfirmTemplateKey {
		t.Fatalf("on: %+v %v", on, err)
	}
	// Nobody may assert a verified address unless named; the aydınlatma
	// metni defaults to the published one.
	if len(on.VerifiedClients) != 0 || on.TextURL != "https://yildizskylab.com/acik-riza" || on.NoticeURL != DefaultNoticeURL {
		t.Fatalf("on: %+v", on)
	}
	if on.ServiceSources["forms"] != SourceForms || on.ServiceSources["place"] != SourcePlace || on.ServiceSources["guessr"] != SourceGuessr {
		t.Fatalf("default sources %v", on.ServiceSources)
	}
	text := "https://a.test/acik-riza"
	for name, values := range map[string]map[string]string{
		"short key":           {KeyEnv: "abc", "PUBLIC_API_ORIGIN": "https://api.example.test", TextURLEnv: text},
		"no origin":           {KeyEnv: key, TextURLEnv: text},
		"bad source":          {KeyEnv: key, "PUBLIC_API_ORIGIN": "https://a.test", TextURLEnv: text, ServiceSourcesEnv: "skymail:skymail"},
		"source twice":        {KeyEnv: key, "PUBLIC_API_ORIGIN": "https://a.test", TextURLEnv: text, ServiceSourcesEnv: "forms:a,forms:b"},
		"client twice":        {KeyEnv: key, "PUBLIC_API_ORIGIN": "https://a.test", TextURLEnv: text, ServiceSourcesEnv: "forms:a,place:a"},
		"not source:pair":     {KeyEnv: key, "PUBLIC_API_ORIGIN": "https://a.test", TextURLEnv: text, ServiceSourcesEnv: "forms"},
		"no consent text":     {KeyEnv: key, "PUBLIC_API_ORIGIN": "https://a.test"},
		"consent text no url": {KeyEnv: key, "PUBLIC_API_ORIGIN": "https://a.test", TextURLEnv: "yildizskylab.com/acik-riza"},
		"notice no url":       {KeyEnv: key, "PUBLIC_API_ORIGIN": "https://a.test", TextURLEnv: text, NoticeURLEnv: "javascript:alert(1)"},
		"verified unknown":    {KeyEnv: key, "PUBLIC_API_ORIGIN": "https://a.test", TextURLEnv: text, VerifiedClientsEnv: "skymail"},
	} {
		if _, err := ConfigFromEnv(env(values)); err == nil {
			t.Errorf("%s: accepted", name)
		} else if strings.Contains(err.Error(), key) {
			t.Errorf("%s: error carries the key", name)
		}
	}
	none, err := ConfigFromEnv(env(map[string]string{KeyEnv: key, "PUBLIC_API_ORIGIN": "https://a.test", TextURLEnv: text, ServiceSourcesEnv: "none"}))
	if err != nil || len(none.ServiceSources) != 0 {
		t.Fatalf("none: %v %v", none.ServiceSources, err)
	}
	verified, err := ConfigFromEnv(env(map[string]string{KeyEnv: key, "PUBLIC_API_ORIGIN": "https://a.test", TextURLEnv: text,
		VerifiedClientsEnv: " place , guessr", NoticeURLEnv: "https://yildizskylab.com/kvkk"}))
	if err != nil || !verified.VerifiedClients["place"] || !verified.VerifiedClients["guessr"] || verified.VerifiedClients["forms"] ||
		verified.NoticeURL != "https://yildizskylab.com/kvkk" {
		t.Fatalf("verified: %+v %v", verified, err)
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

func TestEveryPurposeHasATextASourceAndALabel(t *testing.T) {
	for purpose, spec := range purposes {
		if _, ok := CurrentText(purpose); !ok {
			t.Errorf("%s has no text", purpose)
		}
		for _, version := range spec.texts {
			if Text(version) == "" {
				t.Errorf("%s: version %s has no box text", purpose, version)
			}
		}
		if len(spec.sources) == 0 || spec.label == "" {
			t.Errorf("%s has no source or label", purpose)
		}
	}
}

// The recruitment pool is known (its text is the Açık Rıza Metni's
// alim-havuzu-v1) but not taken: its scope is not agreed with Forms.
func TestOnlyTheInvitationsPurposeIsEnabled(t *testing.T) {
	if p, err := ParsePurpose(" event_invitations "); err != nil || p != PurposeEventInvitations {
		t.Fatalf("event_invitations: %q %v", p, err)
	}
	if _, err := ParsePurpose("recruitment_pool"); !errors.Is(err, ErrPurposeNotEnabled) {
		t.Fatalf("recruitment_pool: %v", err)
	}
	if _, err := ParsePurpose("newsletter"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("newsletter: %v", err)
	}
	if EnabledPurposes() != 1 {
		t.Fatalf("enabled purposes %d", EnabledPurposes())
	}
	if v, _ := CurrentText(PurposeRecruitmentPool); v != "alim-havuzu-v1" {
		t.Fatalf("recruitment pool text %q", v)
	}
	if v, _ := CurrentText(PurposeEventInvitations); v != "davet-v1" {
		t.Fatalf("invitations text %q", v)
	}
}
