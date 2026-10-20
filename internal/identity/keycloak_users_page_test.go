package identity_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/identity"
)

// UsersPage asks Keycloak for one brief page and keeps what the user sync
// decides on: the account's enabled flag, whether it is a service account and
// when Keycloak created it.
func TestKeycloakUsersPageReadsOneBriefPage(t *testing.T) {
	t.Parallel()
	created := time.Date(2026, 10, 19, 21, 5, 0, 0, time.UTC)
	person := uuid.New()
	var query string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/protocol/openid-connect/token") {
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 300, "token_type": "Bearer"})
			return
		}
		if r.URL.Path != "/admin/realms/e-skylab/users" {
			http.NotFound(w, r)
			return
		}
		query = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"id": person.String(), "username": "ada.lovelace", "email": "ada@example.com", "firstName": "Ada", "lastName": "Lovelace",
				"enabled": true, "createdTimestamp": created.UnixMilli()},
			{"id": uuid.NewString(), "username": "service-account-forms", "enabled": true, "serviceAccountClientId": "forms"},
			{"id": "not-a-uuid", "enabled": true},
		})
	}))
	t.Cleanup(server.Close)
	dir := identity.NewKeycloak(identity.KeycloakConfig{URL: server.URL, Realm: "e-skylab", ClientID: "core", ClientSecret: "secret"})

	people, err := dir.UsersPage(context.Background(), 200, 100)
	if err != nil {
		t.Fatal(err)
	}
	if query != "first=200&max=100&briefRepresentation=true" {
		t.Fatalf("query %q", query)
	}
	if len(people) != 2 {
		t.Fatalf("people %+v", people)
	}
	got := people[0]
	if got.ID != person || got.Email != "ada@example.com" || got.FirstName != "Ada" || got.LastName != "Lovelace" ||
		got.Username != "ada.lovelace" || !got.Enabled || got.ServiceAccount || got.CreatedAt == nil || !got.CreatedAt.Equal(created) {
		t.Fatalf("person %+v", got)
	}
	if !people[1].ServiceAccount || people[1].CreatedAt != nil {
		t.Fatalf("service account %+v", people[1])
	}
}

func TestKeycloakUsersPageRefusals(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/protocol/openid-connect/token") {
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 300, "token_type": "Bearer"})
			return
		}
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"unknown_error"}`))
	}))
	t.Cleanup(server.Close)
	dir := identity.NewKeycloak(identity.KeycloakConfig{URL: server.URL, Realm: "e-skylab", ClientID: "core", ClientSecret: "secret"})

	if _, err := dir.UsersPage(context.Background(), 0, 100); err == nil {
		t.Fatal("a refused page must be an error")
	}
	for _, bad := range [][2]int{{-1, 100}, {0, 0}} {
		if _, err := dir.UsersPage(context.Background(), bad[0], bad[1]); !errors.Is(err, identity.ErrInvalid) {
			t.Fatalf("%v: %v", bad, err)
		}
	}
}
