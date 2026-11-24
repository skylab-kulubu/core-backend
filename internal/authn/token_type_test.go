package authn_test

import (
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/testauth"
)

// Only an access token is a Bearer. Keycloak writes typ "Bearer" on every
// access token (sign-in, a service account's client credentials, token
// exchange; "bearer" for a client set to lower case) and "ID" or "Refresh"
// on the others, all signed by the same realm key. An ID token minted for a
// client whose audience names core differs from an access token only in typ.
func TestParseAndVerifyAcceptsOnlyAccessTokens(t *testing.T) {
	t.Parallel()
	keys := testauth.New(t)
	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	for typ, accepted := range map[string]bool{
		"Bearer":  true,
		"bearer":  true,
		"ID":      false,
		"Refresh": false,
		"Logout":  false,
		"DPoP":    false,
		"":        false,
	} {
		tok := keys.Token(t, jwt.MapClaims{"sub": id.String(), "typ": typ, "azp": "core"})
		_, err := authn.ParseAndVerify(tok, keys.Verify, keys.Issuer, testauth.Audience)
		if (err == nil) != accepted {
			t.Fatalf("typ %q: err %v", typ, err)
		}
	}
	noType := keys.Token(t, jwt.MapClaims{"sub": id.String()})
	untyped := keys.Sign(t, jwt.MapClaims{"sub": id.String(), "iss": keys.Issuer, "aud": testauth.Audience, "exp": time.Now().Add(time.Hour).Unix()})
	if _, err := authn.ParseAndVerify(noType, keys.Verify, keys.Issuer, testauth.Audience); err != nil {
		t.Fatalf("testauth default: %v", err)
	}
	if _, err := authn.ParseAndVerify(untyped, keys.Verify, keys.Issuer, testauth.Audience); err == nil {
		t.Fatal("token without typ accepted")
	}
	numeric := keys.Token(t, jwt.MapClaims{"sub": id.String(), "typ": 1})
	if _, err := authn.ParseAndVerify(numeric, keys.Verify, keys.Issuer, testauth.Audience); err == nil {
		t.Fatal("non-string typ accepted")
	}
}
