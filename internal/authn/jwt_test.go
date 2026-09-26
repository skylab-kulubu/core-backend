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

func TestParseAccessTokenReadsYTUClaims(t *testing.T) {
	t.Parallel()
	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	cases := []struct {
		name, claims, university, department string
	}{
		{"strings", `"university":"Yıldız Teknik Üniversitesi","department":"011"`, "Yıldız Teknik Üniversitesi", "011"},
		// A multivalued attribute mapper writes a JSON array; the first value is the attribute.
		{"arrays", `"university":["Yıldız Teknik Üniversitesi"],"department":["Bilgisayar Mühendisliği"]`, "Yıldız Teknik Üniversitesi", "Bilgisayar Mühendisliği"},
		{"absent", `"email":"yk@example.com"`, "", ""},
		{"not text", `"university":7,"department":{"x":1}`, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := authn.ParseAccessToken(unsignedJWT(`{"sub":"` + id.String() + `",` + tc.claims + `}`))
			if err != nil {
				t.Fatal(err)
			}
			if got.Profile.University != tc.university || got.Profile.Department != tc.department {
				t.Fatalf("profile %+v", got.Profile)
			}
		})
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

// A client-credentials token names its client twice: azp, and client_id,
// which Keycloak writes only for a service account. A person's token, even
// one issued to a service's client, has no client_id.
func TestParseAccessTokenTellsAServiceAccountFromAPerson(t *testing.T) {
	t.Parallel()
	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	for name, tc := range map[string]struct {
		claims  string
		service bool
	}{
		"service account":           {`"azp":"forms","client_id":"forms"`, true},
		"person through the client": {`"azp":"forms"`, false},
		"client_id of another":      {`"azp":"frontend-main","client_id":"forms"`, false},
		"client_id without azp":     {`"client_id":"forms"`, false},
	} {
		got, err := authn.ParseAccessToken(unsignedJWT(`{"sub":"` + id.String() + `",` + tc.claims + `}`))
		if err != nil {
			t.Fatal(err)
		}
		if got.ServiceAccount != tc.service {
			t.Errorf("%s: service account %v, want %v", name, got.ServiceAccount, tc.service)
		}
	}
	got, _ := authn.ParseAccessToken(unsignedJWT(`{"sub":"` + id.String() + `","azp":"skycms"}`))
	if got.Client != "skycms" {
		t.Fatalf("client %q, want the azp", got.Client)
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
	now := time.Now().UTC().Truncate(time.Second)
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
		"wrong audience":          func(c jwt.MapClaims) { c["aud"] = "core" },
		"foreign audience set":    func(c jwt.MapClaims) { c["aud"] = []string{"core"} },
		"missing audience":        func(c jwt.MapClaims) { delete(c, "aud") },
		"empty audience set":      func(c jwt.MapClaims) { c["aud"] = []string{} },
		"non string audience":     func(c jwt.MapClaims) { c["aud"] = []any{1} },
		"wrong azp":               func(c jwt.MapClaims) { c["azp"] = "service-account" },
		"broad scope":             func(c jwt.MapClaims) { c["scope"] = "openid profile" },
		"expired":                 func(c jwt.MapClaims) { c["exp"] = now.Add(-time.Second).Unix() },
		"account audience prefix": func(c jwt.MapClaims) { c["aud"] = []string{"account-console"} },
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

// The reconciled Account Center client resolves more than one audience, so the
// intake reads `aud` as a set containing `account` instead of one exact string.
func TestParseSelfDeleteContextAcceptsAccountAudienceSet(t *testing.T) {
	t.Parallel()
	keys := testauth.New(t)
	now := time.Now().UTC().Truncate(time.Second)
	subject := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	idToken := keys.Sign(t, jwt.MapClaims{
		"sub": subject.String(), "iss": keys.Issuer, "aud": "account-center",
		"sid": "browser-session", "auth_time": now.Add(-2 * time.Minute).Unix(),
		"exp": now.Add(time.Hour).Unix(),
	})

	for name, audience := range map[string]any{
		"bare string":          "account",
		"single element array": []string{"account"},
		"reconciled audiences": []string{"account", "core"},
		"account listed last":  []string{"core", "account"},
	} {
		t.Run("access token "+name, func(t *testing.T) {
			accessToken := keys.Sign(t, jwt.MapClaims{
				"sub": subject.String(), "iss": keys.Issuer, "aud": audience,
				"azp": "account-center", "scope": "openid", "exp": now.Add(time.Hour).Unix(),
			})
			got, err := authn.ParseSelfDeleteContext(accessToken, idToken, keys.Verify, keys.Issuer, "account-center", now, 5*time.Minute)
			if err != nil {
				t.Fatalf("audience %v refused: %v", audience, err)
			}
			if got.ID != subject {
				t.Fatalf("identity = %+v", got)
			}
		})
	}

	// The ID token audience stays exclusive; only its JSON shape is relaxed.
	accessToken := keys.Sign(t, jwt.MapClaims{
		"sub": subject.String(), "iss": keys.Issuer, "aud": []string{"account", "core"},
		"azp": "account-center", "scope": "openid", "exp": now.Add(time.Hour).Unix(),
	})
	soleArrayID := keys.Sign(t, jwt.MapClaims{
		"sub": subject.String(), "iss": keys.Issuer, "aud": []string{"account-center"},
		"sid": "browser-session", "auth_time": now.Add(-2 * time.Minute).Unix(),
		"exp": now.Add(time.Hour).Unix(),
	})
	if _, err := authn.ParseSelfDeleteContext(accessToken, soleArrayID, keys.Verify, keys.Issuer, "account-center", now, 5*time.Minute); err != nil {
		t.Fatalf("single element ID token audience refused: %v", err)
	}
}
