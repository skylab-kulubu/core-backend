package erasure_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/skylab-kulubu/core-backend/internal/erasure"
)

type tokenEndpoint struct {
	mu        sync.Mutex
	server    *httptest.Server
	requests  int
	forms     []map[string]string
	status    int
	expiresIn int
	// secret, when set, is the only client secret Keycloak accepts: the one
	// the nightly rotation (ADR-0050) wrote last.
	secret string
}

func newTokenEndpoint(t *testing.T) *tokenEndpoint {
	t.Helper()
	endpoint := &tokenEndpoint{status: http.StatusOK, expiresIn: 300}
	endpoint.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		endpoint.mu.Lock()
		defer endpoint.mu.Unlock()
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		endpoint.requests++
		form := map[string]string{}
		for key := range r.PostForm {
			form[key] = r.PostForm.Get(key)
		}
		endpoint.forms = append(endpoint.forms, form)
		status := endpoint.status
		if endpoint.secret != "" && form["client_secret"] != endpoint.secret {
			status = http.StatusUnauthorized // Keycloak's invalid_client
		}
		if status != http.StatusOK {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":"unauthorized_client","error_description":"s3cr3t-value-never-printed"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "token-" + string(rune('a'+endpoint.requests-1)),
			"expires_in":   endpoint.expiresIn,
			"token_type":   "Bearer",
		})
	}))
	t.Cleanup(endpoint.server.Close)
	return endpoint
}

func (e *tokenEndpoint) count() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.requests
}

func (e *tokenEndpoint) set(status, expiresIn int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.status, e.expiresIn = status, expiresIn
}

func (e *tokenEndpoint) rotate(secret string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.secret = secret
}

func (e *tokenEndpoint) secretSent(i int) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.forms[i]["client_secret"]
}

// configuredSecret stands in for core's configuration: it holds whatever the
// deployment currently says and counts how often it was read.
type configuredSecret struct {
	mu    sync.Mutex
	value string
	err   error
	reads int
}

func fixedSecret(value string) *configuredSecret { return &configuredSecret{value: value} }

func (s *configuredSecret) read() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads++
	return s.value, s.err
}

func (s *configuredSecret) set(value string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.value, s.err = value, err
}

func (s *configuredSecret) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reads
}

func TestClientCredentialsAsksForTheServiceScopeAndCachesUntilThirtySecondsBeforeExpiry(t *testing.T) {
	t.Parallel()

	endpoint := newTokenEndpoint(t)
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	tokens := &erasure.ClientCredentials{
		TokenURL: endpoint.server.URL, ClientID: "core-erasure", Secret: fixedSecret("s3cr3t-value-never-printed").read,
		Scope: "account-erase-cms", Now: func() time.Time { return now },
	}
	ctx := context.Background()

	first, err := tokens.Token(ctx)
	if err != nil || first != "token-a" {
		t.Fatalf("first token=%q err=%v", first, err)
	}
	form := endpoint.forms[0]
	if form["grant_type"] != "client_credentials" || form["client_id"] != "core-erasure" ||
		form["client_secret"] != "s3cr3t-value-never-printed" || form["scope"] != "openid account-erase-cms" {
		t.Fatalf("token form = %v", form)
	}

	now = now.Add(269 * time.Second)
	if cached, err := tokens.Token(ctx); err != nil || cached != "token-a" || endpoint.count() != 1 {
		t.Fatalf("cached token=%q err=%v requests=%d", cached, err, endpoint.count())
	}
	now = now.Add(time.Second)
	if renewed, err := tokens.Token(ctx); err != nil || renewed != "token-b" || endpoint.count() != 2 {
		t.Fatalf("renewed token=%q err=%v requests=%d", renewed, err, endpoint.count())
	}
}

func TestClientCredentialsNeverCachesAShortLivedTokenAndForgetsOnInvalidate(t *testing.T) {
	t.Parallel()

	endpoint := newTokenEndpoint(t)
	endpoint.set(http.StatusOK, 30)
	tokens := &erasure.ClientCredentials{
		TokenURL: endpoint.server.URL, ClientID: "core-erasure", Secret: fixedSecret("secret").read, Scope: "account-erase-forms",
	}
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if _, err := tokens.Token(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if endpoint.count() != 2 {
		t.Fatalf("a token living 30s was cached: requests=%d", endpoint.count())
	}

	endpoint.set(http.StatusOK, 300)
	if _, err := tokens.Token(ctx); err != nil {
		t.Fatal(err)
	}
	tokens.Invalidate()
	if _, err := tokens.Token(ctx); err != nil {
		t.Fatal(err)
	}
	if endpoint.count() != 4 {
		t.Fatalf("invalidated token was reused: requests=%d", endpoint.count())
	}
}

func TestClientCredentialsFailureNamesTheStatusButNoSecret(t *testing.T) {
	t.Parallel()

	endpoint := newTokenEndpoint(t)
	endpoint.set(http.StatusUnauthorized, 300)
	tokens := &erasure.ClientCredentials{
		TokenURL: endpoint.server.URL, ClientID: "core-erasure", Secret: fixedSecret("s3cr3t-value-never-printed").read, Scope: "account-erase-skymail",
	}
	_, err := tokens.Token(context.Background())
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("token error = %v", err)
	}
	if strings.Contains(err.Error(), "s3cr3t") {
		t.Fatalf("token error leaks the secret: %v", err)
	}

	// A rejected secret may have just rotated (ADR-0050): nothing is cached, so
	// the next call asks again with whatever the configuration now holds.
	endpoint.set(http.StatusOK, 300)
	if token, err := tokens.Token(context.Background()); err != nil || token == "" {
		t.Fatalf("token after recovery=%q err=%v", token, err)
	}
}

func TestClientCredentialsReadsTheSecretFromConfigurationForEveryTokenRequest(t *testing.T) {
	t.Parallel()

	endpoint := newTokenEndpoint(t)
	endpoint.rotate("secret-monday")
	secret := fixedSecret("secret-monday")
	now := time.Date(2026, 9, 25, 3, 0, 0, 0, time.UTC)
	tokens := &erasure.ClientCredentials{
		TokenURL: endpoint.server.URL, ClientID: "core-erasure", Secret: secret.read,
		Scope: "account-erase-skymail", Now: func() time.Time { return now },
	}
	ctx := context.Background()

	if token, err := tokens.Token(ctx); err != nil || token != "token-a" || endpoint.secretSent(0) != "secret-monday" {
		t.Fatalf("first token=%q err=%v", token, err)
	}
	// A cached token is still good at Keycloak after the secret rotates; it
	// is reused until 30 seconds before it expires, without reading the
	// secret again.
	secret.set("secret-tuesday", nil)
	endpoint.rotate("secret-tuesday")
	now = now.Add(4 * time.Minute)
	if token, err := tokens.Token(ctx); err != nil || token != "token-a" || secret.count() != 1 {
		t.Fatalf("cached token=%q err=%v secret reads=%d", token, err, secret.count())
	}
	// Past that point the next request reads the rotated secret.
	now = now.Add(30 * time.Second)
	if token, err := tokens.Token(ctx); err != nil || token != "token-b" || endpoint.secretSent(1) != "secret-tuesday" {
		t.Fatalf("token after expiry=%q err=%v", token, err)
	}
	// So does the request after a service refused the token.
	secret.set("secret-wednesday", nil)
	endpoint.rotate("secret-wednesday")
	tokens.Invalidate()
	if token, err := tokens.Token(ctx); err != nil || token != "token-c" || endpoint.secretSent(2) != "secret-wednesday" {
		t.Fatalf("token after invalidate=%q err=%v", token, err)
	}
	if secret.count() != 3 {
		t.Fatalf("secret read %d times for 3 token requests", secret.count())
	}
}

func TestClientCredentialsRecoversFromARotatedSecretOnTheNextRequest(t *testing.T) {
	t.Parallel()

	endpoint := newTokenEndpoint(t)
	endpoint.rotate("secret-tuesday")
	secret := fixedSecret("secret-monday")
	tokens := &erasure.ClientCredentials{
		TokenURL: endpoint.server.URL, ClientID: "core-erasure", Secret: secret.read, Scope: "account-erase-cms",
	}
	ctx := context.Background()

	_, err := tokens.Token(ctx)
	if err == nil || !strings.Contains(err.Error(), "401") || strings.Contains(err.Error(), "secret-monday") {
		t.Fatalf("stale secret error = %v", err)
	}
	secret.set("secret-tuesday", nil)
	if token, err := tokens.Token(ctx); err != nil || token != "token-b" || endpoint.secretSent(1) != "secret-tuesday" {
		t.Fatalf("token after the configuration caught up=%q err=%v", token, err)
	}
}

func TestClientCredentialsWithoutAReadableSecretAsksNothingAndCachesNothing(t *testing.T) {
	t.Parallel()

	endpoint := newTokenEndpoint(t)
	secret := fixedSecret("")
	secret.set("", errors.New("ACCOUNT_ERASURE_CLIENT_SECRET is required when ACCOUNT_ERASURE_WORKER_ENABLED=true"))
	tokens := &erasure.ClientCredentials{
		TokenURL: endpoint.server.URL, ClientID: "core-erasure", Secret: secret.read, Scope: "account-erase-forms",
	}
	if _, err := tokens.Token(context.Background()); err == nil || !strings.Contains(err.Error(), "ACCOUNT_ERASURE_CLIENT_SECRET") {
		t.Fatalf("unreadable secret error = %v", err)
	}
	if endpoint.count() != 0 {
		t.Fatal("token endpoint asked without a secret")
	}

	tokens.Secret = nil
	if _, err := tokens.Token(context.Background()); err == nil || endpoint.count() != 0 {
		t.Fatalf("no secret source: err=%v requests=%d", err, endpoint.count())
	}

	secret.set("s3cr3t-value-never-printed", nil)
	tokens.Secret = secret.read
	if token, err := tokens.Token(context.Background()); err != nil || token != "token-a" {
		t.Fatalf("token once the secret is readable=%q err=%v", token, err)
	}
}
