package ticketqr

import (
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"encoding/json"
	"fmt"
	"math/big"
	"regexp"
	"strings"
	"time"
)

const (
	// KidEnv is the kid the Ticket QR key signs under: tk-e1 (the default)
	// until the SkyPass key is first rotated, then tk-e2, and so on
	// (ADR 0082). It never goes back.
	KidEnv = "TICKET_QR_KID"
	// PreviousJWKEnv is the Ticket QR public key before a SkyPass key
	// rotation, as the JWKS published it (one JWK with its kid, another than
	// TICKET_QR_KID). Empty, the default: only the current key is accepted.
	PreviousJWKEnv = "TICKET_QR_PREVIOUS_JWK"
	// PreviousUntilEnv is when the previous key stops being accepted
	// (RFC 3339): the latest expiry of the codes it signed. Required with
	// PreviousJWKEnv; at most MaxPreviousWindow ahead.
	PreviousUntilEnv = "TICKET_QR_PREVIOUS_UNTIL"
	// MaxPreviousWindow bounds the previous key's window: a code of an Event
	// with no date lasts a year from its Ticket (ADR 0082), and a day more.
	MaxPreviousWindow = 366 * 24 * time.Hour
)

var kidShape = regexp.MustCompile(`^tk-e[1-9][0-9]{0,3}$`)

// ValidKid reports whether kid is a Ticket QR kid: tk-e1, tk-e2, …
func ValidKid(kid string) bool { return kidShape.MatchString(kid) }

// Config is the Ticket QR's settings (docs/ticket-qr.md, "Key rotation").
type Config struct {
	// Kid is the current key's kid; empty is Kid.
	Kid string
	// Previous, PreviousKid and PreviousUntil are the key before the last
	// rotation, accepted under its kid until PreviousUntil.
	Previous      *ecdsa.PublicKey
	PreviousKid   string
	PreviousUntil time.Time
}

// ConfigFromEnv reads the settings. All unset is the default: tk-e1, no
// previous key. A value that does not parse, the previous key without its
// end (or the other way round), a previous key under the current kid, or a
// window longer than MaxPreviousWindow is an error: core does not start.
func ConfigFromEnv(getenv func(string) string, now time.Time) (Config, error) {
	cfg := Config{Kid: Kid}
	if raw := strings.TrimSpace(getenv(KidEnv)); raw != "" {
		if !ValidKid(raw) {
			return Config{}, fmt.Errorf("%s: want tk-e1, tk-e2, …", KidEnv)
		}
		cfg.Kid = raw
	}
	rawJWK := strings.TrimSpace(getenv(PreviousJWKEnv))
	rawUntil := strings.TrimSpace(getenv(PreviousUntilEnv))
	if rawJWK == "" && rawUntil == "" {
		return cfg, nil
	}
	if rawJWK == "" || rawUntil == "" {
		return Config{}, fmt.Errorf("%s and %s go together", PreviousJWKEnv, PreviousUntilEnv)
	}
	until, err := time.Parse(time.RFC3339, rawUntil)
	if err != nil {
		return Config{}, fmt.Errorf("%s: want an RFC 3339 time", PreviousUntilEnv)
	}
	if until.After(now.Add(MaxPreviousWindow)) {
		return Config{}, fmt.Errorf("%s: at most %s ahead", PreviousUntilEnv, MaxPreviousWindow)
	}
	pub, kid, err := ParseJWK([]byte(rawJWK))
	if err != nil {
		return Config{}, fmt.Errorf("%s: %w", PreviousJWKEnv, err)
	}
	if kid == cfg.Kid {
		return Config{}, fmt.Errorf("%s: its kid is %s's (%s); a rotation gives the new key the next kid", PreviousJWKEnv, KidEnv, cfg.Kid)
	}
	cfg.Previous, cfg.PreviousKid, cfg.PreviousUntil = pub, kid, until
	return cfg, nil
}

// ParseJWK reads one Ticket QR public key as the JWKS publishes it, with
// its kid.
func ParseJWK(raw []byte) (*ecdsa.PublicKey, string, error) {
	var k JWK
	if err := json.Unmarshal(raw, &k); err != nil {
		return nil, "", fmt.Errorf("not a JWK")
	}
	if k.Kty != "EC" || k.Crv != "P-256" || !ValidKid(k.Kid) {
		return nil, "", fmt.Errorf("want an EC P-256 key with a Ticket QR kid (tk-e1, tk-e2, …)")
	}
	x, errX := b64.DecodeString(k.X)
	y, errY := b64.DecodeString(k.Y)
	if errX != nil || errY != nil || len(x) != 32 || len(y) != 32 {
		return nil, "", fmt.Errorf("x and y are not 32-byte base64url values")
	}
	// The point must lie on the curve.
	if _, err := ecdh.P256().NewPublicKey(append(append([]byte{4}, x...), y...)); err != nil {
		return nil, "", fmt.Errorf("not a point on P-256")
	}
	return &ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).SetBytes(x), Y: new(big.Int).SetBytes(y)}, k.Kid, nil
}
