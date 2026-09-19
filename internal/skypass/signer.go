package skypass

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"math/big"
	"strings"
	"time"
	"unicode"

	"github.com/golang-jwt/jwt/v5"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

type passClaims struct {
	jwt.RegisteredClaims
}

type Signer struct {
	key *ecdsa.PrivateKey
	ttl time.Duration
	Now func() time.Time
}

func NewSigner(key *ecdsa.PrivateKey, ttl time.Duration) *Signer {
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	return &Signer{key: key, ttl: ttl, Now: time.Now}
}

func (s *Signer) clock() time.Time {
	if s != nil && s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func ParseSigningKey(pemBytes []byte) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, ErrInvalid
	}
	if key, err := x509.ParseECPrivateKey(block.Bytes); err == nil {
		if key.Curve != elliptic.P256() {
			return nil, ErrInvalid
		}
		return key, nil
	}
	if parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		switch key := parsed.(type) {
		case *ecdsa.PrivateKey:
			if key.Curve != elliptic.P256() {
				return nil, ErrInvalid
			}
			return key, nil
		case *rsa.PrivateKey:
			return deriveP256Key(key), nil
		}
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return deriveP256Key(key), nil
	}
	return nil, ErrInvalid
}

func deriveP256Key(legacy *rsa.PrivateKey) *ecdsa.PrivateKey {
	material := append([]byte("skypass-es256-v1\x00"), x509.MarshalPKCS1PrivateKey(legacy)...)
	digest := sha256.Sum256(material)
	curve := elliptic.P256()
	maxScalar := new(big.Int).Sub(curve.Params().N, big.NewInt(1))
	d := new(big.Int).SetBytes(digest[:])
	d.Mod(d, maxScalar)
	d.Add(d, big.NewInt(1))
	x, y := curve.ScalarBaseMult(d.Bytes())
	return &ecdsa.PrivateKey{
		PublicKey: ecdsa.PublicKey{Curve: curve, X: x, Y: y},
		D:         d,
	}
}

func NormalizeUID(raw string) (string, error) {
	var b strings.Builder
	for _, r := range strings.TrimSpace(raw) {
		switch {
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r >= 'a' && r <= 'f':
			b.WriteRune(unicode.ToUpper(r))
		case r >= 'A' && r <= 'F':
			b.WriteRune(r)
		case r == ':' || r == ' ' || r == '-':
		default:
			return "", ErrInvalid
		}
	}
	s := b.String()
	if s == "" {
		return "", nil
	}
	if len(s)%2 != 0 || len(s) < 8 || len(s) > 20 {
		return "", ErrInvalid
	}
	return s, nil
}

func (s *Signer) Mint(u user.User) (Token, error) {
	if s == nil || s.key == nil {
		return Token{}, ErrInvalid
	}
	now := s.clock().UTC()
	exp := now.Add(s.ttl)
	tok := jwt.NewWithClaims(jwt.SigningMethodES256, passClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   u.ID.String(),
			ExpiresAt: jwt.NewNumericDate(exp),
		},
	})
	tok.Header["kid"] = Kid
	signed, err := tok.SignedString(s.key)
	if err != nil {
		return Token{}, err
	}
	return Token{Value: signed, Exp: exp}, nil
}

func (s *Signer) Verify(raw string) (passClaims, error) {
	if s == nil || s.key == nil {
		return passClaims{}, ErrInvalid
	}
	raw = strings.TrimSpace(raw)
	if raw == "" || !strings.Contains(raw, ".") || strings.HasPrefix(raw, "SKYPASS:") {
		return passClaims{}, ErrInvalid
	}
	parser := jwt.NewParser(
		jwt.WithValidMethods([]string{jwt.SigningMethodES256.Alg()}),
		jwt.WithTimeFunc(s.clock),
		jwt.WithExpirationRequired(),
	)
	var claims passClaims
	_, err := parser.ParseWithClaims(raw, &claims, func(t *jwt.Token) (any, error) {
		return &s.key.PublicKey, nil
	})
	if err != nil {
		if errors.Is(err, jwt.ErrTokenExpired) {
			return passClaims{}, ErrExpired
		}
		return passClaims{}, ErrInvalid
	}
	if claims.Subject == "" {
		return passClaims{}, ErrInvalid
	}
	return claims, nil
}

func (s *Signer) JWKS() JWKS {
	if s == nil || s.key == nil {
		return JWKS{Keys: []JWK{}}
	}
	if s.key.Curve != elliptic.P256() {
		return JWKS{Keys: []JWK{}}
	}
	return JWKS{Keys: []JWK{{
		Kty: "EC",
		Kid: Kid,
		Alg: "ES256",
		Use: "sig",
		Crv: "P-256",
		X:   base64.RawURLEncoding.EncodeToString(s.key.X.FillBytes(make([]byte, 32))),
		Y:   base64.RawURLEncoding.EncodeToString(s.key.Y.FillBytes(make([]byte, 32))),
	}}}
}
