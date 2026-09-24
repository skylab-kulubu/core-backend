package authn_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/testauth"
)

const (
	testSudoToken    = "eyJhbGciOiJIUzUxMiJ9.sudo-payload.sudo-signature"
	testCoreClientID = "core"
	testCoreSecret   = "core-client-secret-value"
	testSessionID    = "browser-session"
)

// realmIntrospection mimics Keycloak's RFC 7662 endpoint for Core's own
// confidential client: it authenticates the caller with client_secret_post,
// answers `{"active":false}` for any token it does not know and otherwise
// writes the status and body the test hands it.
type realmIntrospection struct {
	*httptest.Server
	calls atomic.Int32
}

func newRealmIntrospection(t *testing.T, status int, answer map[string]any) *realmIntrospection {
	t.Helper()
	body, err := json.Marshal(answer)
	if err != nil {
		t.Fatal(err)
	}
	realm := &realmIntrospection{}
	realm.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		realm.calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodPost || r.URL.RawQuery != "" ||
			!strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_request"}`))
			return
		}
		if err := r.ParseForm(); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if r.PostForm.Get("client_id") != testCoreClientID || r.PostForm.Get("client_secret") != testCoreSecret {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"invalid_client"}`))
			return
		}
		if r.PostForm.Get("token") != testSudoToken {
			_, _ = w.Write([]byte(`{"active":false}`))
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(realm.Close)
	return realm
}

func (r *realmIntrospection) client() authn.Introspection {
	return authn.Introspection{
		URL:          r.URL + "/realms/e-skylab/protocol/openid-connect/token/introspect",
		ClientID:     testCoreClientID,
		ClientSecret: testCoreSecret,
		HTTP:         r.Client(),
	}
}

// activeSudoIntrospection is what Keycloak answers for a live sky-account
// sudo token once K3e names Core in its audience.
func activeSudoIntrospection(issuer string, subject uuid.UUID, now time.Time) map[string]any {
	return map[string]any{
		"active":    true,
		"typ":       "sky-sudo",
		"iss":       issuer,
		"sub":       subject.String(),
		"azp":       "account-center",
		"aud":       []string{"sky-account", "core"},
		"sid":       testSessionID,
		"amr":       []string{"pwd"},
		"jti":       "sudo-token-id",
		"iat":       now.Add(-time.Minute).Unix(),
		"nbf":       now.Add(-time.Minute).Unix(),
		"exp":       now.Add(4 * time.Minute).Unix(),
		"client_id": "account-center",
	}
}

func selfDeleteBearer(t *testing.T, keys *testauth.Bundle, subject uuid.UUID, now time.Time, mutate func(jwt.MapClaims)) string {
	t.Helper()
	claims := jwt.MapClaims{
		"sub": subject.String(), "iss": keys.Issuer, "aud": []string{"account", "core"},
		"azp": "account-center", "scope": "openid", "sid": testSessionID,
		"exp": now.Add(time.Hour).Unix(),
	}
	if mutate != nil {
		mutate(claims)
	}
	return keys.Sign(t, claims)
}

func TestParseSelfDeleteSudoContextAcceptsIntrospectedSudoProof(t *testing.T) {
	t.Parallel()
	keys := testauth.New(t)
	now := time.Now().UTC().Truncate(time.Second)
	subject := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	bearer := selfDeleteBearer(t, keys, subject, now, nil)

	for name, mutate := range map[string]func(map[string]any){
		"as issued": func(map[string]any) {},
		"audience order reversed": func(c map[string]any) {
			c["aud"] = []string{"core", "sky-account"}
		},
		// Freshness comes from the token's own five-minute life; an old sign-in
		// is exactly what an in-product sudo proof does not change.
		"auth_time rule not applied": func(c map[string]any) {
			c["auth_time"] = now.Add(-3 * time.Hour).Unix()
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			answer := activeSudoIntrospection(keys.Issuer, subject, now)
			mutate(answer)
			realm := newRealmIntrospection(t, http.StatusOK, answer)
			got, err := authn.ParseSelfDeleteSudoContext(t.Context(), bearer, testSudoToken, keys.Verify, realm.client(), keys.Issuer, "account-center", now)
			if err != nil {
				t.Fatal(err)
			}
			if got.ID != subject {
				t.Fatalf("identity = %+v", got)
			}
			if realm.calls.Load() != 1 {
				t.Fatalf("introspection calls = %d", realm.calls.Load())
			}
		})
	}
}

func TestParseSelfDeleteSudoContextRefusesForeignOrStaleProof(t *testing.T) {
	t.Parallel()
	keys := testauth.New(t)
	now := time.Now().UTC().Truncate(time.Second)
	subject := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	bearer := selfDeleteBearer(t, keys, subject, now, nil)

	for name, mutate := range map[string]func(map[string]any){
		"inactive":                func(c map[string]any) { c["active"] = false },
		"active not a boolean":    func(c map[string]any) { c["active"] = "true" },
		"active missing":          func(c map[string]any) { delete(c, "active") },
		"wrong typ":               func(c map[string]any) { c["typ"] = "Bearer" },
		"missing typ":             func(c map[string]any) { delete(c, "typ") },
		"wrong iss":               func(c map[string]any) { c["iss"] = "https://other.example/realms/e-skylab" },
		"wrong azp":               func(c map[string]any) { c["azp"] = "skyforms" },
		"aud missing core":        func(c map[string]any) { c["aud"] = []string{"sky-account"} },
		"aud bare sky-account":    func(c map[string]any) { c["aud"] = "sky-account" },
		"aud missing sky-account": func(c map[string]any) { c["aud"] = []string{"core"} },
		"aud bare core":           func(c map[string]any) { c["aud"] = "core" },
		"aud missing":             func(c map[string]any) { delete(c, "aud") },
		"sub mismatch":            func(c map[string]any) { c["sub"] = uuid.NewString() },
		"sub missing":             func(c map[string]any) { delete(c, "sub") },
		"sid mismatch":            func(c map[string]any) { c["sid"] = "another-browser-session" },
		"sid missing":             func(c map[string]any) { delete(c, "sid") },
		"expired":                 func(c map[string]any) { c["exp"] = now.Add(-time.Second).Unix() },
		"expires now":             func(c map[string]any) { c["exp"] = now.Unix() },
		"exp missing":             func(c map[string]any) { delete(c, "exp") },
		"exp not an integer":      func(c map[string]any) { c["exp"] = float64(now.Add(time.Minute).Unix()) + 0.5 },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			answer := activeSudoIntrospection(keys.Issuer, subject, now)
			mutate(answer)
			realm := newRealmIntrospection(t, http.StatusOK, answer)
			_, err := authn.ParseSelfDeleteSudoContext(t.Context(), bearer, testSudoToken, keys.Verify, realm.client(), keys.Issuer, "account-center", now)
			if !errors.Is(err, authn.ErrInvalidToken) {
				t.Fatalf("err = %v, want ErrInvalidToken", err)
			}
		})
	}

	t.Run("token the realm does not know", func(t *testing.T) {
		t.Parallel()
		realm := newRealmIntrospection(t, http.StatusOK, activeSudoIntrospection(keys.Issuer, subject, now))
		_, err := authn.ParseSelfDeleteSudoContext(t.Context(), bearer, "some.other.token", keys.Verify, realm.client(), keys.Issuer, "account-center", now)
		if !errors.Is(err, authn.ErrInvalidToken) {
			t.Fatalf("err = %v, want ErrInvalidToken", err)
		}
	})
}

// The sudo proof is bound to the bearer that carries it. A bearer that fails
// the self-delete rules is refused before Keycloak is asked anything, so an
// anonymous caller cannot spend Core's introspection budget.
func TestParseSelfDeleteSudoContextVerifiesBearerBeforeIntrospecting(t *testing.T) {
	t.Parallel()
	keys := testauth.New(t)
	now := time.Now().UTC().Truncate(time.Second)
	subject := uuid.MustParse("11111111-1111-1111-1111-111111111111")

	for name, mutate := range map[string]func(jwt.MapClaims){
		"missing sid":      func(c jwt.MapClaims) { delete(c, "sid") },
		"blank sid":        func(c jwt.MapClaims) { c["sid"] = " " },
		"foreign audience": func(c jwt.MapClaims) { c["aud"] = []string{"core"} },
		"wrong azp":        func(c jwt.MapClaims) { c["azp"] = "skyforms" },
		"broad scope":      func(c jwt.MapClaims) { c["scope"] = "openid profile" },
		"expired":          func(c jwt.MapClaims) { c["exp"] = now.Add(-time.Second).Unix() },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			realm := newRealmIntrospection(t, http.StatusOK, activeSudoIntrospection(keys.Issuer, subject, now))
			bearer := selfDeleteBearer(t, keys, subject, now, mutate)
			_, err := authn.ParseSelfDeleteSudoContext(t.Context(), bearer, testSudoToken, keys.Verify, realm.client(), keys.Issuer, "account-center", now)
			if !errors.Is(err, authn.ErrInvalidToken) {
				t.Fatalf("err = %v, want ErrInvalidToken", err)
			}
			if realm.calls.Load() != 0 {
				t.Fatalf("introspected %d times for a refused bearer", realm.calls.Load())
			}
		})
	}

	t.Run("unsigned bearer", func(t *testing.T) {
		t.Parallel()
		realm := newRealmIntrospection(t, http.StatusOK, activeSudoIntrospection(keys.Issuer, subject, now))
		bearer := unsignedJWT(`{"sub":"` + subject.String() + `","iss":"` + keys.Issuer + `","aud":"account","azp":"account-center","scope":"openid","sid":"` + testSessionID + `"}`)
		_, err := authn.ParseSelfDeleteSudoContext(t.Context(), bearer, testSudoToken, keys.Verify, realm.client(), keys.Issuer, "account-center", now)
		if !errors.Is(err, authn.ErrInvalidToken) || realm.calls.Load() != 0 {
			t.Fatalf("err = %v calls = %d", err, realm.calls.Load())
		}
	})
}

// A realm that cannot answer is not a realm that said no: the person's proof
// may be perfectly good, so the caller is told to try again rather than that
// the proof was refused. Neither the sudo token nor the client secret may
// travel in the error, because the composition root logs it.
func TestParseSelfDeleteSudoContextReportsUnreachableRealmAsUnavailable(t *testing.T) {
	t.Parallel()
	keys := testauth.New(t)
	now := time.Now().UTC().Truncate(time.Second)
	subject := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	bearer := selfDeleteBearer(t, keys, subject, now, nil)

	unreachable := newRealmIntrospection(t, http.StatusOK, activeSudoIntrospection(keys.Issuer, subject, now))
	unreachableClient := unreachable.client()
	unreachable.Close()

	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	t.Cleanup(slow.Close)

	garbled := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html>proxy error</html>"))
	}))
	t.Cleanup(garbled.Close)

	cases := map[string]authn.Introspection{
		"unreachable": unreachableClient,
		"server error": newRealmIntrospection(t, http.StatusInternalServerError,
			map[string]any{"error": "unknown_error"}).client(),
		"bad gateway": newRealmIntrospection(t, http.StatusBadGateway,
			map[string]any{"error": "bad_gateway"}).client(),
		"service unavailable": newRealmIntrospection(t, http.StatusServiceUnavailable,
			map[string]any{"error": "unavailable"}).client(),
		"not json": {URL: garbled.URL, ClientID: testCoreClientID, ClientSecret: testCoreSecret, HTTP: garbled.Client()},
		"timeout": {
			URL: slow.URL, ClientID: testCoreClientID, ClientSecret: testCoreSecret,
			HTTP: slow.Client(), Timeout: 50 * time.Millisecond,
		},
	}
	// Core's own client being refused is Core's misconfiguration, not the
	// person's stale proof; it must not read as "re-authenticate".
	misconfigured := newRealmIntrospection(t, http.StatusOK, activeSudoIntrospection(keys.Issuer, subject, now)).client()
	misconfigured.ClientSecret = "rotated-away"
	cases["core client refused"] = misconfigured

	for name, client := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			started := time.Now()
			_, err := authn.ParseSelfDeleteSudoContext(t.Context(), bearer, testSudoToken, keys.Verify, client, keys.Issuer, "account-center", now)
			if !errors.Is(err, authn.ErrIntrospectionUnavailable) {
				t.Fatalf("err = %v, want ErrIntrospectionUnavailable", err)
			}
			if errors.Is(err, authn.ErrInvalidToken) {
				t.Fatalf("unavailable realm reported as a refused proof: %v", err)
			}
			if elapsed := time.Since(started); elapsed > 2*time.Second {
				t.Fatalf("waited %s for the realm", elapsed)
			}
			for _, secret := range []string{testSudoToken, testCoreSecret, "rotated-away", bearer} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("error carried a credential: %v", err)
				}
			}
		})
	}
}

func TestParseSelfDeleteSudoContextFailsClosedWithoutConfiguration(t *testing.T) {
	t.Parallel()
	keys := testauth.New(t)
	now := time.Now().UTC().Truncate(time.Second)
	subject := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	bearer := selfDeleteBearer(t, keys, subject, now, nil)
	realm := newRealmIntrospection(t, http.StatusOK, activeSudoIntrospection(keys.Issuer, subject, now))

	for name, call := range map[string]func() error{
		"nil introspector": func() error {
			_, err := authn.ParseSelfDeleteSudoContext(t.Context(), bearer, testSudoToken, keys.Verify, nil, keys.Issuer, "account-center", now)
			return err
		},
		"nil verify": func() error {
			_, err := authn.ParseSelfDeleteSudoContext(t.Context(), bearer, testSudoToken, nil, realm.client(), keys.Issuer, "account-center", now)
			return err
		},
		"empty issuer": func() error {
			_, err := authn.ParseSelfDeleteSudoContext(t.Context(), bearer, testSudoToken, keys.Verify, realm.client(), "", "account-center", now)
			return err
		},
		"empty client": func() error {
			_, err := authn.ParseSelfDeleteSudoContext(t.Context(), bearer, testSudoToken, keys.Verify, realm.client(), keys.Issuer, "", now)
			return err
		},
		"empty sudo token": func() error {
			_, err := authn.ParseSelfDeleteSudoContext(t.Context(), bearer, "", keys.Verify, realm.client(), keys.Issuer, "account-center", now)
			return err
		},
	} {
		if err := call(); !errors.Is(err, authn.ErrInvalidToken) {
			t.Fatalf("%s: err = %v, want ErrInvalidToken", name, err)
		}
	}
	if realm.calls.Load() != 0 {
		t.Fatalf("introspected %d times without a usable configuration", realm.calls.Load())
	}
}
