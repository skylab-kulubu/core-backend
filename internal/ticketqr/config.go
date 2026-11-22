package ticketqr

import (
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
	"time"
)

const (
	// PreviousJWKEnv is the Ticket QR public key before a SkyPass key
	// rotation, as the JWKS published it (one JWK, kid tk-e1). Empty, the
	// default: only the current key is accepted.
	PreviousJWKEnv = "TICKET_QR_PREVIOUS_JWK"
	// PreviousUntilEnv is when the previous key stops being accepted
	// (RFC 3339). Required with PreviousJWKEnv; at most MaxPreviousWindow
	// ahead.
	PreviousUntilEnv = "TICKET_QR_PREVIOUS_UNTIL"
	// MaxPreviousWindow bounds the previous key's window: the longest a
	// Ticket QR sent before a rotation has to keep working.
	MaxPreviousWindow = 120 * 24 * time.Hour
)

// Config is the Ticket QR's settings: only the previous key of a planned
// SkyPass key rotation (docs/ticket-qr.md, "Key rotation").
type Config struct {
	Previous      *ecdsa.PublicKey
	PreviousUntil time.Time
}

// ConfigFromEnv reads the settings. Both unset is the default (no previous
// key). A value that does not parse, one without the other, or a window
// longer than MaxPreviousWindow is an error: core does not start.
func ConfigFromEnv(getenv func(string) string, now time.Time) (Config, error) {
	rawJWK := strings.TrimSpace(getenv(PreviousJWKEnv))
	rawUntil := strings.TrimSpace(getenv(PreviousUntilEnv))
	if rawJWK == "" && rawUntil == "" {
		return Config{}, nil
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
	pub, err := ParseJWK([]byte(rawJWK))
	if err != nil {
		return Config{}, fmt.Errorf("%s: %w", PreviousJWKEnv, err)
	}
	return Config{Previous: pub, PreviousUntil: until}, nil
}

// ParseJWK reads one Ticket QR public key as the JWKS publishes it.
func ParseJWK(raw []byte) (*ecdsa.PublicKey, error) {
	var k JWK
	if err := json.Unmarshal(raw, &k); err != nil {
		return nil, fmt.Errorf("not a JWK")
	}
	if k.Kty != "EC" || k.Crv != "P-256" || k.Kid != Kid {
		return nil, fmt.Errorf("want an EC P-256 key with kid %s", Kid)
	}
	x, errX := b64.DecodeString(k.X)
	y, errY := b64.DecodeString(k.Y)
	if errX != nil || errY != nil || len(x) != 32 || len(y) != 32 {
		return nil, fmt.Errorf("x and y are not 32-byte base64url values")
	}
	// The point must lie on the curve.
	if _, err := ecdh.P256().NewPublicKey(append(append([]byte{4}, x...), y...)); err != nil {
		return nil, fmt.Errorf("not a point on P-256")
	}
	return &ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).SetBytes(x), Y: new(big.Int).SetBytes(y)}, nil
}
