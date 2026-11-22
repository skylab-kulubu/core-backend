// Package ticketqr is the Ticket QR: a Ticket's signed, static door code
// (ADR 0082; docs/ticket-qr.md).
//
// A Ticket QR is an ES256 JWT, header {"alg":"ES256","kid":"tk-e1","typ":
// "JWT"}, payload {"sub":<ticket id>,"evt":<event id>,"qv":<QR version>,
// "exp":<unix seconds>}. It carries no name, e-mail or phone. The mail, the
// PDF, the ticket page and sky-app show the same code: it is signed
// deterministically (RFC 6979), so the same Ticket, Event dates and QR
// version always give the same string.
//
// The key is derived from the SkyPass signing key with HKDF-SHA256 (info
// "skylab ticket-qr es256 v1"): no new secret, and the SkyPass key itself
// never signs a Ticket QR. Its public half is published with SkyPass's in
// the same JWKS, under its kid: tk-e1, and tk-e2, tk-e3… after each
// rotation of the SkyPass key (ADR 0082; Config).
package ticketqr

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/asn1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	// Kid is the first Ticket QR key's kid, the default (TICKET_QR_KID).
	// Every Ticket QR kid is tk-e<n>; the door sends a token whose kid has
	// the tk- prefix to the Ticket QR path.
	Kid = "tk-e1"
	// KidPrefix marks a Ticket QR kid.
	KidPrefix = "tk-"
	// KeyInfo is the HKDF info the key is derived with.
	KeyInfo = "skylab ticket-qr es256 v1"
	// MaxTokenBytes is the longest token looked at.
	MaxTokenBytes = 1024
	// AfterEnd is how long after its Event ends a Ticket QR stays valid.
	AfterEnd = 24 * time.Hour
	// Undated is how long a Ticket QR of an Event with no date at all stays
	// valid, from the Ticket's creation (such an Event never ends, ADR 0066).
	Undated = 365 * 24 * time.Hour
)

var (
	// ErrInvalid is a token that is not a Ticket QR: malformed, another
	// kid, or not signed by the Ticket QR key.
	ErrInvalid = errors.New("ticketqr: invalid")
)

var b64 = base64.RawURLEncoding

// Claims are what a Ticket QR says.
type Claims struct {
	Ticket    uuid.UUID
	Event     uuid.UUID
	Version   int
	ExpiresAt time.Time
}

// ExpiresAt is a Ticket QR's expiry: a day after its Event ends (end date,
// else a day after its start: event.Event.EndsAt), or, for an Event with no
// date, a year after the Ticket was written.
func ExpiresAt(eventEnd *time.Time, ticketCreated time.Time) time.Time {
	if eventEnd != nil {
		return eventEnd.Add(AfterEnd).UTC().Truncate(time.Second)
	}
	return ticketCreated.Add(Undated).UTC().Truncate(time.Second)
}

// DeriveKey derives the Ticket QR key from the SkyPass signing key:
// HKDF-SHA256 over its private scalar, 40 bytes reduced to a P-256 scalar
// (FIPS 186-5 A.2.1, the extra 64 bits make the bias negligible).
func DeriveKey(skypass *ecdsa.PrivateKey) *ecdsa.PrivateKey {
	material, err := hkdf.Key(sha256.New, skypass.D.FillBytes(make([]byte, 32)), nil, KeyInfo, 40)
	if err != nil {
		// Only a length beyond 255 hash blocks fails.
		panic(err)
	}
	curve := elliptic.P256()
	n1 := new(big.Int).Sub(curve.Params().N, big.NewInt(1))
	d := new(big.Int).SetBytes(material)
	d.Mod(d, n1)
	d.Add(d, big.NewInt(1))
	x, y := curve.ScalarBaseMult(d.FillBytes(make([]byte, 32)))
	return &ecdsa.PrivateKey{PublicKey: ecdsa.PublicKey{Curve: curve, X: x, Y: y}, D: d}
}

type header struct {
	Alg string `json:"alg"`
	Kid string `json:"kid"`
	Typ string `json:"typ"`
}

// payload is the token's body; its field order is the order on the wire.
type payload struct {
	Sub string `json:"sub"`
	Evt string `json:"evt"`
	QV  int    `json:"qv"`
	Exp int64  `json:"exp"`
}

func encodeHeader(kid string) string {
	raw, err := json.Marshal(header{Alg: "ES256", Kid: kid, Typ: "JWT"})
	if err != nil {
		panic(err)
	}
	return b64.EncodeToString(raw)
}

// Signer mints and verifies Ticket QRs. It signs with its key under kid;
// previous is the Ticket QR public key before a rotation of the SkyPass
// key, still accepted under its own kid until previousUntil (Config).
type Signer struct {
	key           *ecdsa.PrivateKey
	kid           string
	header        string
	previous      *ecdsa.PublicKey
	previousKid   string
	previousUntil time.Time
	// Now is the clock for the previous key's window; nil is time.Now.
	Now func() time.Time
}

// NewSigner signs with key under cfg's kid (Kid when unset) and verifies
// with it, and with cfg's previous key, by its kid, while its window lasts.
func NewSigner(key *ecdsa.PrivateKey, cfg Config) *Signer {
	kid := cfg.Kid
	if kid == "" {
		kid = Kid
	}
	return &Signer{
		key: key, kid: kid, header: encodeHeader(kid),
		previous: cfg.Previous, previousKid: cfg.PreviousKid, previousUntil: cfg.PreviousUntil,
	}
}

// Kid is the kid the signer signs under.
func (s *Signer) Kid() string { return s.kid }

func (s *Signer) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Mint signs c. The same claims always give the same token (RFC 6979).
func (s *Signer) Mint(c Claims) (string, error) {
	if s == nil || s.key == nil || c.Ticket == uuid.Nil || c.Event == uuid.Nil || c.Version < 1 || c.ExpiresAt.IsZero() {
		return "", ErrInvalid
	}
	body, err := json.Marshal(payload{Sub: c.Ticket.String(), Evt: c.Event.String(), QV: c.Version, Exp: c.ExpiresAt.Unix()})
	if err != nil {
		return "", err
	}
	signing := s.header + "." + b64.EncodeToString(body)
	digest := sha256.Sum256([]byte(signing))
	// A nil random source makes the signature deterministic (RFC 6979).
	der, err := s.key.Sign(nil, digest[:], crypto.SHA256)
	if err != nil {
		return "", err
	}
	var sig struct{ R, S *big.Int }
	if _, err := asn1.Unmarshal(der, &sig); err != nil {
		return "", err
	}
	raw := make([]byte, 64)
	sig.R.FillBytes(raw[:32])
	sig.S.FillBytes(raw[32:])
	return signing + "." + b64.EncodeToString(raw), nil
}

// IsTicketQR reports whether token's header names a Ticket QR key (a kid
// with the tk- prefix): the door sends such a token to the Ticket QR path,
// any other to SkyPass's.
func IsTicketQR(token string) bool {
	token = strings.TrimSpace(token)
	if len(token) > MaxTokenBytes {
		return false
	}
	first, _, ok := strings.Cut(token, ".")
	if !ok {
		return false
	}
	raw, err := b64.DecodeString(first)
	if err != nil {
		return false
	}
	var h header
	if err := json.Unmarshal(raw, &h); err != nil {
		return false
	}
	return strings.HasPrefix(h.Kid, KidPrefix)
}

// Verify checks token's signature with the key its kid names: the current
// one, or the previous one while its window lasts; any other kid is
// refused. It answers the token's claims. It does not
// judge the expiry: online, the door does, by the Event's dates now (an
// Event moved later keeps the codes already sent valid; docs/ticket-qr.md).
func (s *Signer) Verify(token string) (Claims, error) {
	if s == nil || s.key == nil {
		return Claims{}, ErrInvalid
	}
	token = strings.TrimSpace(token)
	if token == "" || len(token) > MaxTokenBytes {
		return Claims{}, ErrInvalid
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return Claims{}, ErrInvalid
	}
	rawHeader, err := b64.DecodeString(parts[0])
	if err != nil {
		return Claims{}, ErrInvalid
	}
	var h header
	if err := decodeJSON(rawHeader, &h); err != nil || h.Alg != "ES256" {
		return Claims{}, ErrInvalid
	}
	var pub *ecdsa.PublicKey
	switch {
	case h.Kid == s.kid:
		pub = &s.key.PublicKey
	case s.previous != nil && h.Kid == s.previousKid && s.now().Before(s.previousUntil):
		pub = s.previous
	default:
		return Claims{}, ErrInvalid
	}
	sig, err := b64.DecodeString(parts[2])
	if err != nil || len(sig) != 64 {
		return Claims{}, ErrInvalid
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	r, sv := new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])
	if !ecdsa.Verify(pub, digest[:], r, sv) {
		return Claims{}, ErrInvalid
	}
	rawBody, err := b64.DecodeString(parts[1])
	if err != nil {
		return Claims{}, ErrInvalid
	}
	var p payload
	if err := decodeJSON(rawBody, &p); err != nil {
		return Claims{}, ErrInvalid
	}
	ticketID, err := uuid.Parse(p.Sub)
	if err != nil || ticketID == uuid.Nil {
		return Claims{}, ErrInvalid
	}
	eventID, err := uuid.Parse(p.Evt)
	if err != nil || eventID == uuid.Nil || p.QV < 1 || p.Exp <= 0 {
		return Claims{}, ErrInvalid
	}
	return Claims{Ticket: ticketID, Event: eventID, Version: p.QV, ExpiresAt: time.Unix(p.Exp, 0).UTC()}, nil
}

func decodeJSON(raw []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	return dec.Decode(v)
}

// JWK is one public key of the JWKS (RFC 7517), the shape SkyPass's has.
type JWK struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Alg string `json:"alg"`
	Use string `json:"use"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

// JWKs are the Ticket QR public keys to publish, each under its own kid:
// the current one, and the previous one while its window lasts. A verifier
// picks the key by the token's kid.
func (s *Signer) JWKs() []JWK {
	if s == nil || s.key == nil {
		return nil
	}
	out := []JWK{publicJWK(&s.key.PublicKey, s.kid)}
	if s.previous != nil && s.now().Before(s.previousUntil) {
		out = append(out, publicJWK(s.previous, s.previousKid))
	}
	return out
}

func publicJWK(pub *ecdsa.PublicKey, kid string) JWK {
	return JWK{
		Kty: "EC", Kid: kid, Alg: "ES256", Use: "sig", Crv: "P-256",
		X: b64.EncodeToString(pub.X.FillBytes(make([]byte, 32))),
		Y: b64.EncodeToString(pub.Y.FillBytes(make([]byte, 32))),
	}
}
