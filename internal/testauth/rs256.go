package testauth

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/skylab-kulubu/core-backend/internal/authn"
)

const Issuer = "https://auth.example.test/realms/e-skylab"
const Audience = "core"

type Bundle struct {
	Key     *rsa.PrivateKey
	JWKSURL string
	Issuer  string
	Verify  func(string) error
}

func New(t *testing.T) *Bundle {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	n := base64.RawURLEncoding.EncodeToString(key.N.Bytes())
	e := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes())
	body, err := json.Marshal(map[string]any{
		"keys": []map[string]string{{
			"kty": "RSA", "kid": "k1", "alg": "RS256", "use": "sig", "n": n, "e": e,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	v := authn.NewJWKS(srv.URL)
	return &Bundle{Key: key, JWKSURL: srv.URL, Issuer: Issuer, Verify: v.Verify}
}

func (b *Bundle) Token(t *testing.T, claims jwt.MapClaims) string {
	t.Helper()
	if claims == nil {
		claims = jwt.MapClaims{}
	}
	if _, ok := claims["iss"]; !ok {
		claims["iss"] = b.Issuer
	}
	if _, ok := claims["aud"]; !ok {
		claims["aud"] = Audience
	}
	if _, ok := claims["exp"]; !ok {
		claims["exp"] = time.Now().Add(time.Hour).Unix()
	}
	return b.Sign(t, claims)
}

func (b *Bundle) Sign(t *testing.T, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = "k1"
	signed, err := tok.SignedString(b.Key)
	if err != nil {
		t.Fatal(err)
	}
	return signed
}

func (b *Bundle) Parse() func(string) (authn.Identity, error) {
	return func(token string) (authn.Identity, error) {
		return authn.ParseAndVerify(token, b.Verify, b.Issuer, Audience)
	}
}
