package ticketqr_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/ticketqr"
)

func skypassKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func claims() ticketqr.Claims {
	return ticketqr.Claims{
		Ticket: uuid.MustParse("3f1c2b6e-1111-4a2b-8c3d-000000000001"), Event: uuid.MustParse("3f1c2b6e-2222-4a2b-8c3d-000000000002"),
		Version: 1, ExpiresAt: time.Date(2026, 12, 13, 21, 0, 0, 0, time.UTC),
	}
}

// The key is derived, stable for one SkyPass key, different for another,
// and never the SkyPass key itself.
func TestDeriveKey(t *testing.T) {
	t.Parallel()
	pass := skypassKey(t)
	a, b := ticketqr.DeriveKey(pass), ticketqr.DeriveKey(pass)
	if a.D.Cmp(b.D) != 0 || !a.PublicKey.Equal(&b.PublicKey) {
		t.Fatal("not stable")
	}
	if a.D.Cmp(pass.D) == 0 {
		t.Fatal("the SkyPass key itself")
	}
	if ticketqr.DeriveKey(skypassKey(t)).D.Cmp(a.D) == 0 {
		t.Fatal("another SkyPass key, the same Ticket QR key")
	}
	if !a.Curve.IsOnCurve(a.X, a.Y) {
		t.Fatal("not on the curve")
	}
}

// The same claims give the same token (RFC 6979): mail, PDF, page and app
// show one code. Another QR version or expiry gives another.
func TestMintIsDeterministic(t *testing.T) {
	t.Parallel()
	s := ticketqr.NewSigner(ticketqr.DeriveKey(skypassKey(t)), ticketqr.Config{})
	first, err := s.Mint(claims())
	if err != nil {
		t.Fatal(err)
	}
	for range 5 {
		again, err := s.Mint(claims())
		if err != nil || again != first {
			t.Fatalf("again %q %v", again, err)
		}
	}
	c := claims()
	c.Version = 2
	if other, _ := s.Mint(c); other == first {
		t.Fatal("a new QR version, the same token")
	}
	c = claims()
	c.ExpiresAt = c.ExpiresAt.Add(24 * time.Hour)
	if other, _ := s.Mint(c); other == first {
		t.Fatal("a moved Event, the same token")
	}
	got, err := s.Verify(first)
	if err != nil || got != claims() {
		t.Fatalf("verify %+v %v", got, err)
	}
	// About 300 characters: QR version 12–13 at M, readable on a screen and
	// at 3 cm on paper.
	if len(first) > 320 {
		t.Fatalf("%d characters", len(first))
	}
}

func part(t *testing.T, token string, i int) map[string]any {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(strings.Split(token, ".")[i])
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]any{}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// The token is what docs/ticket-qr.md says, and nothing personal: two ids
// and two numbers.
func TestTokenShape(t *testing.T) {
	t.Parallel()
	s := ticketqr.NewSigner(ticketqr.DeriveKey(skypassKey(t)), ticketqr.Config{})
	token, err := s.Mint(claims())
	if err != nil {
		t.Fatal(err)
	}
	header, payload := part(t, token, 0), part(t, token, 1)
	if len(header) != 3 || header["alg"] != "ES256" || header["kid"] != "tk-e1" || header["typ"] != "JWT" {
		t.Fatalf("header %v", header)
	}
	if len(payload) != 4 || payload["sub"] != claims().Ticket.String() || payload["evt"] != claims().Event.String() ||
		payload["qv"] != float64(1) || payload["exp"] != float64(claims().ExpiresAt.Unix()) {
		t.Fatalf("payload %v", payload)
	}
}

// sky-app's scanner sends a code to core only when DoorService.isSkyPassToken
// says yes: three dot-separated parts, the header's alg ES256 and a
// non-empty string sub (lib/features/calendar/data/services/door_service.dart,
// sky-app origin/main). The rule, mirrored here, takes a Ticket QR: today's
// app scans it unchanged.
func TestTodaysScannerTakesATicketQR(t *testing.T) {
	t.Parallel()
	isSkyPassToken := func(raw string) bool {
		parts := strings.Split(strings.TrimSpace(raw), ".")
		if len(parts) != 3 {
			return false
		}
		decode := func(p string) map[string]any {
			if pad := len(p) % 4; pad != 0 {
				p += strings.Repeat("=", 4-pad)
			}
			raw, err := base64.URLEncoding.DecodeString(p)
			if err != nil {
				return nil
			}
			out := map[string]any{}
			if json.Unmarshal(raw, &out) != nil {
				return nil
			}
			return out
		}
		header, payload := decode(parts[0]), decode(parts[1])
		sub, ok := payload["sub"].(string)
		return header != nil && header["alg"] == "ES256" && ok && sub != ""
	}
	s := ticketqr.NewSigner(ticketqr.DeriveKey(skypassKey(t)), ticketqr.Config{})
	token, err := s.Mint(claims())
	if err != nil {
		t.Fatal(err)
	}
	if !isSkyPassToken(token) {
		t.Fatal("today's scanner refuses the Ticket QR")
	}
	if !ticketqr.IsTicketQR(token) || ticketqr.IsTicketQR("eyJhbGciOiJFUzI1NiIsImtpZCI6InNwLWUxIn0.e30.x") || ticketqr.IsTicketQR("SPW1:abc:12345678") {
		t.Fatal("IsTicketQR")
	}
}

// The JWKS key verifies the token with an independent JWT library: what a
// scanner that verifies on the device does (ADR 0022).
func TestTheJWKSKeyVerifiesTheToken(t *testing.T) {
	t.Parallel()
	s := ticketqr.NewSigner(ticketqr.DeriveKey(skypassKey(t)), ticketqr.Config{})
	token, err := s.Mint(claims())
	if err != nil {
		t.Fatal(err)
	}
	keys := s.JWKs()
	if len(keys) != 1 || keys[0].Kid != "tk-e1" || keys[0].Alg != "ES256" || keys[0].Crv != "P-256" {
		t.Fatalf("keys %+v", keys)
	}
	coordinate := func(v string) *big.Int {
		raw, err := base64.RawURLEncoding.DecodeString(v)
		if err != nil {
			t.Fatal(err)
		}
		return new(big.Int).SetBytes(raw)
	}
	pub := &ecdsa.PublicKey{Curve: elliptic.P256(), X: coordinate(keys[0].X), Y: coordinate(keys[0].Y)}
	parsed, err := jwt.Parse(token, func(tok *jwt.Token) (any, error) {
		if tok.Header["kid"] != keys[0].Kid {
			return nil, errors.New("kid")
		}
		return pub, nil
	}, jwt.WithValidMethods([]string{"ES256"}), jwt.WithTimeFunc(func() time.Time { return claims().ExpiresAt.Add(-time.Hour) }))
	if err != nil || !parsed.Valid {
		t.Fatalf("independent verify: %v", err)
	}
}

// Another key's token is refused, a SkyPass-key token with kid tk-e1
// included, and so is a token whose body was changed.
func TestVerifyRefuses(t *testing.T) {
	t.Parallel()
	pass := skypassKey(t)
	s := ticketqr.NewSigner(ticketqr.DeriveKey(pass), ticketqr.Config{})
	other := ticketqr.NewSigner(ticketqr.DeriveKey(skypassKey(t)), ticketqr.Config{})
	foreign, err := other.Mint(claims())
	if err != nil {
		t.Fatal(err)
	}
	withSkyPassKey := ticketqr.NewSigner(pass, ticketqr.Config{})
	signedBySkyPass, err := withSkyPassKey.Mint(claims())
	if err != nil {
		t.Fatal(err)
	}
	good, _ := s.Mint(claims())
	parts := strings.Split(good, ".")
	c := claims()
	c.Version = 9
	tamperedBody, _ := s.Mint(c)
	tampered := parts[0] + "." + strings.Split(tamperedBody, ".")[1] + "." + parts[2]
	for name, token := range map[string]string{
		"another key":           foreign,
		"the SkyPass key":       signedBySkyPass,
		"body changed":          tampered,
		"empty":                 "",
		"two parts":             parts[0] + "." + parts[1],
		"too long":              good + strings.Repeat("A", 1024),
		"unsigned (alg none)":   "eyJhbGciOiJub25lIiwia2lkIjoidGstZTEifQ." + parts[1] + ".",
		"signature not 64":      parts[0] + "." + parts[1] + ".AAAA",
		"wallet code":           "SPW1:abc:12345678",
		"a SkyPass-shaped JWT ": "eyJhbGciOiJFUzI1NiIsImtpZCI6InNwLWUxIn0." + parts[1] + "." + parts[2],
	} {
		if _, err := s.Verify(token); !errors.Is(err, ticketqr.ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// After a planned SkyPass key rotation the previous Ticket QR key is
// accepted, and published, until its window ends; then neither.
func TestPreviousKeyWindow(t *testing.T) {
	t.Parallel()
	before := ticketqr.NewSigner(ticketqr.DeriveKey(skypassKey(t)), ticketqr.Config{})
	sent, err := before.Mint(claims())
	if err != nil {
		t.Fatal(err)
	}
	previous, err := json.Marshal(before.JWKs()[0])
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	env := map[string]string{
		ticketqr.PreviousJWKEnv:   string(previous),
		ticketqr.PreviousUntilEnv: now.Add(30 * 24 * time.Hour).Format(time.RFC3339),
	}
	cfg, err := ticketqr.ConfigFromEnv(func(k string) string { return env[k] }, now)
	if err != nil {
		t.Fatal(err)
	}
	after := ticketqr.NewSigner(ticketqr.DeriveKey(skypassKey(t)), cfg)
	clock := now
	after.Now = func() time.Time { return clock }
	if _, err := after.Verify(sent); err != nil {
		t.Fatalf("within the window: %v", err)
	}
	if len(after.JWKs()) != 2 {
		t.Fatalf("published %d keys within the window", len(after.JWKs()))
	}
	clock = now.Add(31 * 24 * time.Hour)
	if _, err := after.Verify(sent); !errors.Is(err, ticketqr.ErrInvalid) {
		t.Fatalf("after the window: %v", err)
	}
	if len(after.JWKs()) != 1 {
		t.Fatal("the previous key is still published")
	}
}

func TestConfigFromEnv(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	key, _ := json.Marshal(ticketqr.NewSigner(ticketqr.DeriveKey(skypassKey(t)), ticketqr.Config{}).JWKs()[0])
	soon := now.Add(time.Hour).Format(time.RFC3339)
	for name, tc := range map[string]struct {
		env map[string]string
		ok  bool
	}{
		"unset":            {map[string]string{}, true},
		"both":             {map[string]string{ticketqr.PreviousJWKEnv: string(key), ticketqr.PreviousUntilEnv: soon}, true},
		"key alone":        {map[string]string{ticketqr.PreviousJWKEnv: string(key)}, false},
		"until alone":      {map[string]string{ticketqr.PreviousUntilEnv: soon}, false},
		"not a time":       {map[string]string{ticketqr.PreviousJWKEnv: string(key), ticketqr.PreviousUntilEnv: "yarın"}, false},
		"too far":          {map[string]string{ticketqr.PreviousJWKEnv: string(key), ticketqr.PreviousUntilEnv: now.Add(200 * 24 * time.Hour).Format(time.RFC3339)}, false},
		"not a JWK":        {map[string]string{ticketqr.PreviousJWKEnv: "{}", ticketqr.PreviousUntilEnv: soon}, false},
		"SkyPass kid":      {map[string]string{ticketqr.PreviousJWKEnv: strings.Replace(string(key), "tk-e1", "sp-e1", 1), ticketqr.PreviousUntilEnv: soon}, false},
		"not on the curve": {map[string]string{ticketqr.PreviousJWKEnv: `{"kty":"EC","kid":"tk-e1","crv":"P-256","x":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","y":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}`, ticketqr.PreviousUntilEnv: soon}, false},
	} {
		_, err := ticketqr.ConfigFromEnv(func(k string) string { return tc.env[k] }, now)
		if (err == nil) != tc.ok {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestExpiresAt(t *testing.T) {
	t.Parallel()
	end := time.Date(2026, 12, 12, 21, 0, 0, 0, time.UTC)
	created := time.Date(2026, 11, 22, 9, 30, 0, 0, time.UTC)
	if got := ticketqr.ExpiresAt(&end, created); !got.Equal(end.Add(24 * time.Hour)) {
		t.Fatalf("dated %v", got)
	}
	if got := ticketqr.ExpiresAt(nil, created); !got.Equal(created.Add(365 * 24 * time.Hour)) {
		t.Fatalf("undated %v", got)
	}
}
