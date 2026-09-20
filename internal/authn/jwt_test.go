package authn_test

import (
	"encoding/base64"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/testauth"
)

func unsignedJWT(payload string) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	body := base64.RawURLEncoding.EncodeToString([]byte(payload))
	return header + "." + body + ".x"
}

func TestParseAccessTokenReadsGroups(t *testing.T) {
	t.Parallel()
	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	tok := unsignedJWT(`{"sub":"` + id.String() + `","email":"yk@example.com","given_name":"Y","family_name":"K","groups":["/UYELER/YK"]}`)
	got, err := authn.ParseAccessToken(tok)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != id || got.Profile.Email != "yk@example.com" {
		t.Fatalf("got %+v", got)
	}
	if len(got.Groups) != 1 || got.Groups[0] != "/UYELER/YK" {
		t.Fatalf("groups %+v", got.Groups)
	}
}

func TestParseAccessTokenReadsSkyNumber(t *testing.T) {
	t.Parallel()
	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	tok := unsignedJWT(`{"sub":"` + id.String() + `","sky_number":"SKY-0000007"}`)
	got, err := authn.ParseAccessToken(tok)
	if err != nil {
		t.Fatal(err)
	}
	if got.Profile.SkyNumber != "SKY-0000007" {
		t.Fatalf("profile %+v", got.Profile)
	}
}

func TestParseAccessTokenReadsPreferredUsername(t *testing.T) {
	t.Parallel()
	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	tok := unsignedJWT(`{"sub":"` + id.String() + `","preferred_username":"ada"}`)
	got, err := authn.ParseAccessToken(tok)
	if err != nil {
		t.Fatal(err)
	}
	if got.Profile.Username != "ada" {
		t.Fatalf("profile %+v", got.Profile)
	}
}

func TestParseAccessTokenReadsSchoolEmail(t *testing.T) {
	t.Parallel()
	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	tok := unsignedJWT(`{"sub":"` + id.String() + `","email":"yk@example.com","school_email":"yk@std.yildiz.edu.tr"}`)
	got, err := authn.ParseAccessToken(tok)
	if err != nil {
		t.Fatal(err)
	}
	if got.Profile.SchoolEmail != "yk@std.yildiz.edu.tr" {
		t.Fatalf("profile %+v", got.Profile)
	}
}

func TestParseAccessTokenReadsCoreRolesOnly(t *testing.T) {
	t.Parallel()
	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	tok := unsignedJWT(`{"sub":"` + id.String() + `","resource_access":{"core":{"roles":["url:create"]},"skylapp":{"roles":["skylapp:access","url:create"]}}}`)
	got, err := authn.ParseAccessToken(tok)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Roles) != 1 || got.Roles[0] != "url:create" {
		t.Fatalf("roles %+v", got.Roles)
	}
}

func TestParseAccessTokenRejectsBadSub(t *testing.T) {
	t.Parallel()
	_, err := authn.ParseAccessToken(unsignedJWT(`{"sub":"not-a-uuid"}`))
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestParseAccessTokenRejectsNonCanonicalSub(t *testing.T) {
	t.Parallel()
	_, err := authn.ParseAccessToken(unsignedJWT(`{"sub":"11111111-1111-1111-1111-AAAAAAAAAAAA"}`))
	if err == nil {
		t.Fatal("non-canonical subject was normalized instead of rejected")
	}
}

func TestParseAndVerifyRequiresAudienceAndIssuer(t *testing.T) {
	t.Parallel()
	keys := testauth.New(t)
	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	ok := keys.Token(t, jwt.MapClaims{"sub": id.String(), "email": "yk@example.com"})
	got, err := authn.ParseAndVerify(ok, keys.Verify, keys.Issuer, testauth.Audience)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != id {
		t.Fatalf("got %+v", got)
	}

	arrayAud := keys.Token(t, jwt.MapClaims{"sub": id.String(), "aud": []string{"account", "core"}})
	if _, err := authn.ParseAndVerify(arrayAud, keys.Verify, keys.Issuer, testauth.Audience); err != nil {
		t.Fatal(err)
	}

	unsigned := unsignedJWT(`{"sub":"` + id.String() + `","iss":"` + keys.Issuer + `","aud":"core"}`)
	if _, err := authn.ParseAndVerify(unsigned, keys.Verify, keys.Issuer, testauth.Audience); err == nil {
		t.Fatal("unsigned accepted")
	}

	wrongAud := keys.Token(t, jwt.MapClaims{"sub": id.String(), "aud": "account"})
	if _, err := authn.ParseAndVerify(wrongAud, keys.Verify, keys.Issuer, testauth.Audience); err == nil {
		t.Fatal("wrong aud accepted")
	}

	missingAud := keys.Sign(t, jwt.MapClaims{
		"sub": id.String(),
		"iss": keys.Issuer,
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	if _, err := authn.ParseAndVerify(missingAud, keys.Verify, keys.Issuer, testauth.Audience); err == nil {
		t.Fatal("missing aud accepted")
	}

	wrongIss := keys.Token(t, jwt.MapClaims{"sub": id.String(), "iss": "https://other.example/realms/e-skylab"})
	if _, err := authn.ParseAndVerify(wrongIss, keys.Verify, keys.Issuer, testauth.Audience); err == nil {
		t.Fatal("wrong iss accepted")
	}

	if _, err := authn.ParseAndVerify(ok, nil, keys.Issuer, testauth.Audience); err == nil {
		t.Fatal("nil verify accepted")
	}
	if _, err := authn.ParseAndVerify(ok, keys.Verify, "", testauth.Audience); err == nil {
		t.Fatal("empty issuer accepted")
	}
	if _, err := authn.ParseAndVerify(ok, keys.Verify, keys.Issuer, ""); err == nil {
		t.Fatal("empty audience accepted")
	}

	hs := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": id.String(), "iss": keys.Issuer, "aud": testauth.Audience, "exp": time.Now().Add(time.Hour).Unix(),
	})
	hsTok, err := hs.SignedString([]byte("not-an-rsa-key"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := authn.ParseAndVerify(hsTok, keys.Verify, keys.Issuer, testauth.Audience); err == nil {
		t.Fatal("HS256 accepted")
	}
}

func TestParseSelfDeleteContextRequiresMatchingAccountTokenAndFreshIDToken(t *testing.T) {
	t.Parallel()
	keys := testauth.New(t)
	now := time.Date(2026, 9, 20, 8, 0, 0, 0, time.UTC)
	subject := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	validAccessClaims := jwt.MapClaims{
		"sub": subject.String(), "iss": keys.Issuer, "aud": "account",
		"azp": "account-center", "scope": "openid", "exp": now.Add(time.Hour).Unix(),
	}
	validIDClaims := jwt.MapClaims{
		"sub": subject.String(), "iss": keys.Issuer, "aud": "account-center",
		"sid": "browser-session", "auth_time": now.Add(-2 * time.Minute).Unix(), "exp": now.Add(time.Hour).Unix(),
	}
	validAccess := keys.Sign(t, validAccessClaims)
	validID := keys.Sign(t, validIDClaims)
	got, err := authn.ParseSelfDeleteContext(validAccess, validID, keys.Verify, keys.Issuer, "account-center", now, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != subject {
		t.Fatalf("identity = %+v", got)
	}

	for name, mutate := range map[string]func(jwt.MapClaims){
		"wrong audience":    func(c jwt.MapClaims) { c["aud"] = "core" },
		"multiple audience": func(c jwt.MapClaims) { c["aud"] = []string{"account", "core"} },
		"wrong azp":         func(c jwt.MapClaims) { c["azp"] = "service-account" },
		"broad scope":       func(c jwt.MapClaims) { c["scope"] = "openid profile" },
	} {
		t.Run("access token "+name, func(t *testing.T) {
			claims := jwt.MapClaims{}
			for key, value := range validAccessClaims {
				claims[key] = value
			}
			mutate(claims)
			accessToken := keys.Sign(t, claims)
			if _, err := authn.ParseSelfDeleteContext(accessToken, validID, keys.Verify, keys.Issuer, "account-center", now, 5*time.Minute); err == nil {
				t.Fatal("invalid account access token accepted")
			}
		})
	}

	for name, mutate := range map[string]func(jwt.MapClaims){
		"wrong audience":     func(c jwt.MapClaims) { c["aud"] = "account" },
		"multiple audience":  func(c jwt.MapClaims) { c["aud"] = []string{"account-center", "core"} },
		"missing sid":        func(c jwt.MapClaims) { delete(c, "sid") },
		"missing auth time":  func(c jwt.MapClaims) { delete(c, "auth_time") },
		"stale auth time":    func(c jwt.MapClaims) { c["auth_time"] = now.Add(-6 * time.Minute).Unix() },
		"future auth time":   func(c jwt.MapClaims) { c["auth_time"] = now.Add(6 * time.Second).Unix() },
		"mismatched subject": func(c jwt.MapClaims) { c["sub"] = uuid.NewString() },
	} {
		t.Run("id token "+name, func(t *testing.T) {
			claims := jwt.MapClaims{}
			for key, value := range validIDClaims {
				claims[key] = value
			}
			mutate(claims)
			idToken := keys.Sign(t, claims)
			if _, err := authn.ParseSelfDeleteContext(validAccess, idToken, keys.Verify, keys.Issuer, "account-center", now, 5*time.Minute); err == nil {
				t.Fatal("invalid reauthentication ID token accepted")
			}
		})
	}
}
