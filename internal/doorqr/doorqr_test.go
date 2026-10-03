package doorqr

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"errors"
	"net/url"
	"strconv"
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

func admit(g *Gate, raw string) error {
	_, err := g.Admit(raw, sessionID, eventID)
	return err
}

func TestConfigFromEnvDefaults(t *testing.T) {
	cfg, err := ConfigFromEnv(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	want := Config{Mode: ModeOpen, TTL: 60 * time.Second, MaxUses: 20, SessionGrace: 30 * time.Minute}
	if cfg != want {
		t.Fatalf("defaults %+v", cfg)
	}
}

func TestConfigFromEnvReadsEverySetting(t *testing.T) {
	env := map[string]string{
		ModeEnv:         " qr ",
		TTLEnv:          "90s",
		MaxUsesEnv:      "30",
		GuestURLEnv:     "https://example.test/kapi/{sessionId}",
		SessionGraceEnv: "0s",
	}
	cfg, err := ConfigFromEnv(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	want := Config{Mode: ModeQR, TTL: 90 * time.Second, MaxUses: 30, GuestURL: env[GuestURLEnv], SessionGrace: 0}
	if cfg != want {
		t.Fatalf("config %+v", cfg)
	}
}

func TestConfigFromEnvRefusesTyposAndOutOfRangeValues(t *testing.T) {
	cases := map[string]map[string]string{
		"mode typo":        {ModeEnv: "qrr"},
		"mode upper":       {ModeEnv: "QR"},
		"ttl not duration": {TTLEnv: "120"},
		"ttl too short":    {TTLEnv: "5s"},
		"ttl too long":     {TTLEnv: "1h"},
		"uses not number":  {MaxUsesEnv: "many"},
		"uses zero":        {MaxUsesEnv: "0"},
		"url not https":    {GuestURLEnv: "http://example.test/{sessionId}"},
		"url no session":   {GuestURLEnv: "https://example.test/kapi"},
		"grace negative":   {SessionGraceEnv: "-1m"},
		"grace too long":   {SessionGraceEnv: "24h"},
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
	if pass.RefreshAfter != 15*time.Second {
		t.Fatalf("refresh %s", pass.RefreshAfter)
	}
	c.now = c.now.Add(DefaultTTL - time.Second)
	if err := admit(g, pass.Token); err != nil {
		t.Fatal(err)
	}
}

// The token is small enough for a QR of about 41 modules: v1, base-36
// expiry, 6-byte id and 12-byte MAC.
func TestTokenIsCompact(t *testing.T) {
	g, _ := newGate(t, Config{})
	pass, err := g.Mint(sessionID, eventID)
	if err != nil {
		t.Fatal(err)
	}
	if len(pass.Token) > 40 || !strings.HasPrefix(pass.Token, "v1.") || strings.Count(pass.Token, ".") != 3 {
		t.Fatalf("token %q (%d)", pass.Token, len(pass.Token))
	}
	if strings.Contains(pass.Token, sessionID.String()) || strings.Contains(pass.Token, eventID.String()) {
		t.Fatal("ids sent in the token")
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
	c.now = c.now.Add(DefaultTTL)
	if err := admit(g, pass.Token); !errors.Is(err, ErrExpired) {
		t.Fatalf("expired: %v", err)
	}
}

func TestAdmitAllowsAFewSecondsOfClockSkew(t *testing.T) {
	g, c := newGate(t, Config{})
	ahead := NewGate(testKey, Config{})
	ahead.Now = func() time.Time { return c.now.Add(ClockLeeway) }
	pass, err := ahead.Mint(sessionID, eventID)
	if err != nil {
		t.Fatal(err)
	}
	if err := admit(g, pass.Token); err != nil {
		t.Fatalf("5 s ahead: %v", err)
	}
	farAhead := NewGate(testKey, Config{})
	farAhead.Now = func() time.Time { return c.now.Add(ClockLeeway + 2*time.Second) }
	pass, err = farAhead.Mint(sessionID, eventID)
	if err != nil {
		t.Fatal(err)
	}
	if err := admit(g, pass.Token); !errors.Is(err, ErrInvalid) {
		t.Fatalf("7 s ahead: %v", err)
	}
}

func TestAdmitBindsSessionAndEvent(t *testing.T) {
	g, _ := newGate(t, Config{})
	pass, err := g.Mint(sessionID, eventID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.Admit(pass.Token, uuid.New(), eventID); !errors.Is(err, ErrInvalid) {
		t.Fatalf("other session: %v", err)
	}
	if _, err := g.Admit(pass.Token, sessionID, uuid.New()); !errors.Is(err, ErrInvalid) {
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
	exp, _ := strconv.ParseInt(parts[1], 36, 64)
	flip := func(s string) string {
		b := []byte(s)
		if b[0] == 'A' {
			b[0] = 'B'
		} else {
			b[0] = 'A'
		}
		return string(b)
	}
	forged := map[string]string{
		"empty":         "",
		"garbage":       "not-a-token",
		"foreign key":   foreign.Token,
		"other version": "v2." + parts[1] + "." + parts[2] + "." + parts[3],
		"later expiry":  parts[0] + "." + strconv.FormatInt(exp+1, 36) + "." + parts[2] + "." + parts[3],
		"padded expiry": parts[0] + ".0" + parts[1] + "." + parts[2] + "." + parts[3],
		"other id":      parts[0] + "." + parts[1] + "." + flip(parts[2]) + "." + parts[3],
		"other mac":     parts[0] + "." + parts[1] + "." + parts[2] + "." + flip(parts[3]),
		"short mac":     parts[0] + "." + parts[1] + "." + parts[2] + "." + parts[3][:8],
		"extra part":    pass.Token + ".x",
		"over 1 KB":     pass.Token + strings.Repeat(" ", MaxTokenBytes),
	}
	// JWTs of every kind are refused, whatever key or algorithm signs them
	// and whatever their header claims: the format has no algorithm to
	// choose.
	jwtClaims := jwt.MapClaims{"sid": sessionID.String(), "eid": eventID.String(), "jti": "x", "exp": c.now.Add(time.Minute).Unix()}
	sign := func(method jwt.SigningMethod, key any) string {
		t.Helper()
		tok := jwt.NewWithClaims(method, jwtClaims)
		tok.Header["kid"] = Version
		tok.Header["typ"] = "door-qr+jwt"
		raw, err := tok.SignedString(key)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	forged["jwt none"] = sign(jwt.SigningMethodNone, jwt.UnsafeAllowNoneSignatureType)
	forged["jwt es256"] = sign(jwt.SigningMethodES256, ecKey)
	forged["jwt hs256 right key"] = sign(jwt.SigningMethodHS256, g.key)
	forged["jwt hs384 right key"] = sign(jwt.SigningMethodHS384, g.key)
	for name, raw := range forged {
		if err := admit(g, raw); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := admit(g, pass.Token); err != nil {
		t.Fatalf("the genuine token after the forgeries: %v", err)
	}
}

func TestAdmitCapsUsesPerTokenAndReleaseGivesThemBack(t *testing.T) {
	g, c := newGate(t, Config{MaxUses: 2})
	pass, err := g.Mint(sessionID, eventID)
	if err != nil {
		t.Fatal(err)
	}
	first, err := g.Admit(pass.Token, sessionID, eventID)
	if err != nil {
		t.Fatal(err)
	}
	if err := admit(g, pass.Token); err != nil {
		t.Fatal(err)
	}
	if err := admit(g, pass.Token); !errors.Is(err, ErrUsedUp) {
		t.Fatalf("third use: %v", err)
	}
	first.Release()
	if err := admit(g, pass.Token); err != nil {
		t.Fatalf("after release: %v", err)
	}
	// A fresh token from the screen has its own budget.
	fresh, err := g.Mint(sessionID, eventID)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Token == pass.Token {
		t.Fatal("two mints gave the same token")
	}
	if err := admit(g, fresh.Token); err != nil {
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

func TestUseCountsAreBounded(t *testing.T) {
	g, c := newGate(t, Config{})
	g.maxTrack = 3
	var passes []Pass
	for range 4 {
		p, err := g.Mint(sessionID, eventID)
		if err != nil {
			t.Fatal(err)
		}
		passes = append(passes, p)
	}
	for _, p := range passes[:3] {
		if err := admit(g, p.Token); err != nil {
			t.Fatal(err)
		}
	}
	if err := admit(g, passes[3].Token); !errors.Is(err, ErrUsedUp) {
		t.Fatalf("over the bound: %v", err)
	}
	if n := g.trackedTokens(); n != 3 {
		t.Fatalf("tracked %d", n)
	}
	// Once the old tokens expire, room is made for new ones.
	c.now = c.now.Add(DefaultTTL)
	fresh, err := g.Mint(sessionID, eventID)
	if err != nil {
		t.Fatal(err)
	}
	if err := admit(g, fresh.Token); err != nil {
		t.Fatalf("after expiry: %v", err)
	}
}

func TestSessionOpen(t *testing.T) {
	g, c := newGate(t, Config{SessionGrace: 30 * time.Minute})
	at := func(d time.Duration) *time.Time { v := c.now.Add(d); return &v }
	cases := []struct {
		name       string
		start, end *time.Time
		cancelled  bool
		want       bool
	}{
		{"no schedule", nil, nil, false, true},
		{"running", at(-time.Hour), at(time.Hour), false, true},
		{"cancelled", at(-time.Hour), at(time.Hour), true, false},
		{"starts within grace", at(29 * time.Minute), at(2 * time.Hour), false, true},
		{"starts later", at(31 * time.Minute), at(2 * time.Hour), false, false},
		{"ended within grace", at(-2 * time.Hour), at(-29 * time.Minute), false, true},
		{"ended earlier", at(-2 * time.Hour), at(-31 * time.Minute), false, false},
		{"start only, started", at(-5 * time.Hour), nil, false, true},
		{"end only, ended", nil, at(-time.Hour), false, false},
	}
	for _, tc := range cases {
		if got := g.SessionOpen(tc.start, tc.end, tc.cancelled); got != tc.want {
			t.Errorf("%s: %v", tc.name, got)
		}
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
	g.Record(true, OutcomeSessionClosed)
	text := g.Prometheus()
	for _, want := range []string{
		`skylab_guest_self_checkin_total{door_qr="present",outcome="checked_in"} 1`,
		`skylab_guest_self_checkin_total{door_qr="absent",outcome="checked_in"} 2`,
		`skylab_guest_self_checkin_total{door_qr="absent",outcome="door_qr_required"} 1`,
		`skylab_guest_self_checkin_total{door_qr="present",outcome="session_closed"} 1`,
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
