package skypass

import (
	"crypto/rsa"
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
	SkyNumber string `json:"skyNumber"`
	Name      string `json:"name"`
	jwt.RegisteredClaims
}

type Signer struct {
	key *rsa.PrivateKey
	ttl time.Duration
	Now func() time.Time
}

func NewSigner(key *rsa.PrivateKey, ttl time.Duration) *Signer {
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

func ParseRSAPrivateKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, ErrInvalid
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, ErrInvalid
	}
	k, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, ErrInvalid
	}
	return k, nil
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
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, passClaims{
		SkyNumber: u.SkyNumber,
		Name:      strings.TrimSpace(u.FirstName + " " + u.LastName),
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    Issuer,
			Subject:   u.ID.String(),
			Audience:  jwt.ClaimStrings{Audience},
			IssuedAt:  jwt.NewNumericDate(now),
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
		jwt.WithValidMethods([]string{jwt.SigningMethodRS256.Alg()}),
		jwt.WithTimeFunc(s.clock),
		jwt.WithExpirationRequired(),
		jwt.WithIssuer(Issuer),
		jwt.WithAudience(Audience),
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
	n := base64.RawURLEncoding.EncodeToString(s.key.N.Bytes())
	e := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(s.key.E)).Bytes())
	return JWKS{Keys: []JWK{{
		Kty: "RSA",
		Kid: Kid,
		Alg: "RS256",
		Use: "sig",
		N:   n,
		E:   e,
	}}}
}
