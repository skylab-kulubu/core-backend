package authn_test

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
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authn"
)

func TestJWKSRejectsUnsigned(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"keys":[]}`))
	}))
	t.Cleanup(srv.Close)
	v := authn.NewJWKS(srv.URL)
	if err := v.Verify(unsignedJWT(`{"sub":"11111111-1111-1111-1111-111111111111"}`)); err == nil {
		t.Fatal("expected reject")
	}
}

func TestJWKSAcceptsSigned(t *testing.T) {
	t.Parallel()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	n := base64.RawURLEncoding.EncodeToString(key.N.Bytes())
	e := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes())
	jwks, err := json.Marshal(map[string]any{
		"keys": []map[string]string{{
			"kty": "RSA", "kid": "k1", "alg": "RS256", "use": "sig", "n": n, "e": e,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(jwks)
	}))
	t.Cleanup(srv.Close)

	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"sub":    id.String(),
		"email":  "yk@example.com",
		"groups": []string{"/UYELER/YK"},
		"exp":    time.Now().Add(time.Hour).Unix(),
	})
	tok.Header["kid"] = "k1"
	signed, err := tok.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	v := authn.NewJWKS(srv.URL)
	if err := v.Verify(signed); err != nil {
		t.Fatal(err)
	}
	got, err := authn.ParseAndVerify(signed, v.Verify)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != id || got.Groups[0] != "/UYELER/YK" {
		t.Fatalf("got %+v", got)
	}
}
