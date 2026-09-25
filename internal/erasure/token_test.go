package erasure_test

import (
	"context"
	"encoding/json"
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
		if endpoint.status != http.StatusOK {
			w.WriteHeader(endpoint.status)
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

func TestClientCredentialsAsksForTheServiceScopeAndCachesUntilThirtySecondsBeforeExpiry(t *testing.T) {
	t.Parallel()

	endpoint := newTokenEndpoint(t)
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	tokens := &erasure.ClientCredentials{
		TokenURL: endpoint.server.URL, ClientID: "core-erasure", ClientSecret: "s3cr3t-value-never-printed",
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
		TokenURL: endpoint.server.URL, ClientID: "core-erasure", ClientSecret: "secret", Scope: "account-erase-forms",
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
		TokenURL: endpoint.server.URL, ClientID: "core-erasure", ClientSecret: "s3cr3t-value-never-printed", Scope: "account-erase-skymail",
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
