package doorqr

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

var (
	sessionID = uuid.MustParse("11111111-1111-1111-1111-111111111111")
	eventID   = uuid.MustParse("22222222-2222-2222-2222-222222222222")
	testKey   = []byte("0123456789abcdef0123456789abcdef")
)

type clock struct{ now time.Time }

func (c *clock) Now() time.Time { return c.now }

func newGate(t *testing.T, cfg Config) (*Gate, *clock) {
	t.Helper()
	c := &clock{now: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)}
	g := NewGate(testKey, cfg)
	g.Now = c.Now
	return g, c
}

func TestConfigFromEnvDefaults(t *testing.T) {
	cfg, err := ConfigFromEnv(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Mode != ModeOpen || cfg.TTL != DefaultTTL || cfg.MaxUses != DefaultMaxUses || cfg.GuestURL != "" {
		t.Fatalf("defaults %+v", cfg)
	}
}

func TestConfigFromEnvReadsEverySetting(t *testing.T) {
	env := map[string]string{
		ModeEnv:     " qr ",
		TTLEnv:      "90s",
		MaxUsesEnv:  "20",
		GuestURLEnv: "https://example.test/kapi/{sessionId}",
	}
	cfg, err := ConfigFromEnv(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Mode != ModeQR || cfg.TTL != 90*time.Second || cfg.MaxUses != 20 || cfg.GuestURL != env[GuestURLEnv] {
		t.Fatalf("config %+v", cfg)
	}
}

func TestConfigFromEnvRefusesTyposAndOutOfRangeValues(t *testing.T) {
	cases := map[string]map[string]string{
		"mode typo":        {ModeEnv: "qrr"},
		"ttl not duration": {TTLEnv: "120"},
		"ttl too short":    {TTLEnv: "5s"},
		"ttl too long":     {TTLEnv: "1h"},
		"uses not number":  {MaxUsesEnv: "many"},
		"uses zero":        {MaxUsesEnv: "0"},
		"url not https":    {GuestURLEnv: "http://example.test/{sessionId}"},
		"url no session":   {GuestURLEnv: "https://example.test/kapi"},
	}
	for name, env := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ConfigFromEnv(func(k string) string { return env[k] }); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

func TestMintThenAdmit(t *testing.T) {
	g, c := newGate(t, Config{Mode: ModeQR})
	pass, err := g.Mint(sessionID, eventID)
	if err != nil {
		t.Fatal(err)
	}
	if !pass.IssuedAt.Equal(c.now) || !pass.ExpiresAt.Equal(c.now.Add(DefaultTTL)) {
		t.Fatalf("times %+v", pass)
	}
	if pass.RefreshAfter != DefaultTTL/4 {
		t.Fatalf("refresh %s", pass.RefreshAfter)
	}
	c.now = c.now.Add(DefaultTTL - time.Second)
	if err := g.Admit(pass.Token, sessionID, eventID); err != nil {
		t.Fatal(err)
	}
}

func TestPassURLDefaultsToTheSessionAddress(t *testing.T) {
	t.Setenv("PUBLIC_API_ORIGIN", "https://api.example.test/")
	g, _ := newGate(t, Config{})
	pass, err := g.Mint(sessionID, eventID)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(pass.URL)
	if err != nil {
		t.Fatal(err)
	}
	if u.Scheme+"://"+u.Host+u.Path != "https://api.example.test/v1/sessions/"+sessionID.String() {
		t.Fatalf("url %s", pass.URL)
	}
	if u.Query().Get(QueryParam) != pass.Token {
		t.Fatalf("token not in url %s", pass.URL)
	}
}

func TestPassURLUsesTheGuestPage(t *testing.T) {
	g, _ := newGate(t, Config{GuestURL: "https://panel.example.test/kapi/{sessionId}?lang=tr"})
	pass, err := g.Mint(sessionID, eventID)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(pass.URL)
	if err != nil {
		t.Fatal(err)
	}
	if u.Host != "panel.example.test" || u.Path != "/kapi/"+sessionID.String() || u.Query().Get("lang") != "tr" {
		t.Fatalf("url %s", pass.URL)
	}
	if u.Query().Get(QueryParam) != pass.Token {
		t.Fatalf("token not in url %s", pass.URL)
	}
}

func TestAdmitRefusesExpiredToken(t *testing.T) {
	g, c := newGate(t, Config{})
	pass, err := g.Mint(sessionID, eventID)
	if err != nil {
		t.Fatal(err)
	}
	c.now = c.now.Add(DefaultTTL + time.Second)
	if err := g.Admit(pass.Token, sessionID, eventID); !errors.Is(err, ErrExpired) {
		t.Fatalf("expired: %v", err)
	}
}

func TestAdmitBindsSessionAndEvent(t *testing.T) {
	g, _ := newGate(t, Config{})
	pass, err := g.Mint(sessionID, eventID)
	if err != nil {
		t.Fatal(err)
	}
	if err := g.Admit(pass.Token, uuid.New(), eventID); !errors.Is(err, ErrInvalid) {
		t.Fatalf("other session: %v", err)
	}
	if err := g.Admit(pass.Token, sessionID, uuid.New()); !errors.Is(err, ErrInvalid) {
		t.Fatalf("other event: %v", err)
	}
}

func TestAdmitRefusesForgedAndForeignTokens(t *testing.T) {
	g, c := newGate(t, Config{})
	other := NewGate([]byte("another key, another key, 32 byt"), Config{})
	other.Now = c.Now
	foreign, err := other.Mint(sessionID, eventID)
	if err != nil {
		t.Fatal(err)
	}
	pass, err := g.Mint(sessionID, eventID)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(pass.Token, ".")
	tampered := parts[0] + "." + parts[1] + "x." + parts[2]

	// A token signed with the right key but without the door QR type, as
	// another feature's HS256 token would be.
	untyped := jwt.NewWithClaims(jwt.SigningMethodHS256, claims{
		SessionID: sessionID.String(), EventID: eventID.String(),
		RegisteredClaims: jwt.RegisteredClaims{ID: "x", ExpiresAt: jwt.NewNumericDate(c.now.Add(time.Minute))},
	})
	untypedRaw, err := untyped.SignedString(testKey)
	if err != nil {
		t.Fatal(err)
	}
	// "none" and asymmetric algorithms are never accepted.
	none := jwt.NewWithClaims(jwt.SigningMethodNone, claims{
		SessionID: sessionID.String(), EventID: eventID.String(),
		RegisteredClaims: jwt.RegisteredClaims{ID: "x", ExpiresAt: jwt.NewNumericDate(c.now.Add(time.Minute))},
	})
	none.Header["typ"] = TokenType
	noneRaw, err := none.SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatal(err)
	}
	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	es := jwt.NewWithClaims(jwt.SigningMethodES256, claims{
		SessionID: sessionID.String(), EventID: eventID.String(),
		RegisteredClaims: jwt.RegisteredClaims{ID: "x", ExpiresAt: jwt.NewNumericDate(c.now.Add(time.Minute))},
	})
	es.Header["typ"] = TokenType
	esRaw, err := es.SignedString(ecKey)
	if err != nil {
		t.Fatal(err)
	}
	for name, raw := range map[string]string{
		"empty": "", "garbage": "not-a-token", "foreign key": foreign.Token, "tampered": tampered,
		"untyped": untypedRaw, "none": noneRaw, "es256": esRaw,
	} {
		if err := g.Admit(raw, sessionID, eventID); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestAdmitCapsUsesPerToken(t *testing.T) {
	g, c := newGate(t, Config{MaxUses: 2})
	pass, err := g.Mint(sessionID, eventID)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		if err := g.Admit(pass.Token, sessionID, eventID); err != nil {
			t.Fatalf("use %d: %v", i, err)
		}
	}
	if err := g.Admit(pass.Token, sessionID, eventID); !errors.Is(err, ErrUsedUp) {
		t.Fatalf("third use: %v", err)
	}
	// A fresh token from the screen has its own budget.
	fresh, err := g.Mint(sessionID, eventID)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Token == pass.Token {
		t.Fatal("two mints gave the same token")
	}
	if err := g.Admit(fresh.Token, sessionID, eventID); err != nil {
		t.Fatalf("fresh: %v", err)
	}
	// Spent tokens are forgotten once they could no longer be used.
	c.now = c.now.Add(DefaultTTL + time.Second)
	if _, err := g.Mint(sessionID, eventID); err != nil {
		t.Fatal(err)
	}
	if n := g.trackedTokens(); n != 0 {
		t.Fatalf("tracked after expiry: %d", n)
	}
}

func TestDeriveKeyIsStableAndSeparate(t *testing.T) {
	a := DeriveKey([]byte("secret one"))
	b := DeriveKey([]byte("secret one"))
	c := DeriveKey([]byte("secret two"))
	if len(a) != 32 || string(a) != string(b) || string(a) == string(c) {
		t.Fatal("derivation")
	}
	if string(a) == "secret one" {
		t.Fatal("key is the secret itself")
	}
}

func TestMetricsCountOutcomes(t *testing.T) {
	g, _ := newGate(t, Config{})
	g.Record(true, OutcomeCheckedIn)
	g.Record(false, OutcomeCheckedIn)
	g.Record(false, OutcomeCheckedIn)
	g.Record(false, OutcomeRequired)
	text := g.Prometheus()
	for _, want := range []string{
		`skylab_guest_self_checkin_total{door_qr="present",outcome="checked_in"} 1`,
		`skylab_guest_self_checkin_total{door_qr="absent",outcome="checked_in"} 2`,
		`skylab_guest_self_checkin_total{door_qr="absent",outcome="door_qr_required"} 1`,
		`skylab_guest_self_checkin_mode{mode="open"} 1`,
		`skylab_guest_self_checkin_mode{mode="qr"} 0`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %s in\n%s", want, text)
		}
	}
	var nilGate *Gate
	nilGate.Record(true, OutcomeCheckedIn)
	if nilGate.Prometheus() != "" {
		t.Fatal("nil gate metrics")
	}
}
