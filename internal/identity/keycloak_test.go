package identity_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/identity"
)

func TestKeycloakDirectoryListsNestedGroups(t *testing.T) {
	t.Parallel()
	parent := map[string]any{"id": "p1", "name": "UYELER", "path": "/UYELER", "attributes": map[string][]string{"public_listing": {"true"}}}
	child := map[string]any{"id": "c1", "name": "YK", "path": "/UYELER/YK"}
	memberID := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	member := map[string]any{"id": memberID, "email": "yk@example.com", "firstName": "Y", "lastName": "K", "username": "yk"}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/protocol/openid-connect/token"):
			_, _ = w.Write([]byte(`{"access_token":"tok","expires_in":300,"token_type":"Bearer"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/admin/realms/e-skylab/groups":
			_ = json.NewEncoder(w).Encode([]any{parent})
		case r.Method == http.MethodGet && r.URL.Path == "/admin/realms/e-skylab/groups/p1/children":
			_ = json.NewEncoder(w).Encode([]any{child})
		case r.Method == http.MethodGet && r.URL.Path == "/admin/realms/e-skylab/groups/c1/children":
			_ = json.NewEncoder(w).Encode([]any{})
		case r.Method == http.MethodGet && r.URL.Path == "/admin/realms/e-skylab/groups/p1/members":
			_ = json.NewEncoder(w).Encode([]any{member})
		case r.Method == http.MethodGet && r.URL.Path == "/admin/realms/e-skylab/groups/p1":
			_ = json.NewEncoder(w).Encode(parent)
		case r.Method == http.MethodGet && r.URL.Path == "/admin/realms/e-skylab/groups/c1":
			_ = json.NewEncoder(w).Encode(child)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/admin/realms/e-skylab/group-by-path/"):
			path := strings.TrimPrefix(r.URL.Path, "/admin/realms/e-skylab/group-by-path/")
			switch path {
			case "UYELER":
				_ = json.NewEncoder(w).Encode(parent)
			case "UYELER/YK":
				_ = json.NewEncoder(w).Encode(child)
			default:
				http.NotFound(w, r)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	dir := identity.NewKeycloak(identity.KeycloakConfig{
		URL:          srv.URL,
		Realm:        "e-skylab",
		ClientID:     "core",
		ClientSecret: "secret",
	})
	ctx := context.Background()
	groups, err := dir.ListGroups(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 2 {
		t.Fatalf("groups %+v", groups)
	}
	g, err := dir.GetGroup(ctx, "/UYELER")
	if err != nil {
		t.Fatal(err)
	}
	if g.Attributes["public_listing"] != "true" {
		t.Fatalf("attrs %+v", g.Attributes)
	}
	people, err := dir.Members(ctx, "p1")
	if err != nil {
		t.Fatal(err)
	}
	if len(people) != 1 || people[0].ID != uuid.MustParse(memberID) {
		t.Fatalf("members %+v", people)
	}
	if _, err := dir.GetGroup(ctx, "missing"); !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("got %v", err)
	}
}

func TestKeycloakAccountLifecyclePreservesFederationMetadataAndTreatsDeleteRetryAsSuccess(t *testing.T) {
	t.Parallel()

	userID := uuid.MustParse("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
	deleteCalls := 0
	var disabledBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/protocol/openid-connect/token"):
			_, _ = w.Write([]byte(`{"access_token":"tok","expires_in":300,"token_type":"Bearer"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/admin/realms/e-skylab/users/"+userID.String():
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": userID.String(), "username": "ldap-user", "email": "ldap@example.com",
				"firstName": "LDAP", "lastName": "Member", "enabled": true,
				"federationLink": "ldap-provider-id", "attributes": map[string][]string{"sky_number": {"SKY-0000042"}},
			})
		case r.Method == http.MethodPut && r.URL.Path == "/admin/realms/e-skylab/users/"+userID.String():
			if err := json.NewDecoder(r.Body).Decode(&disabledBody); err != nil {
				t.Errorf("decode disable body: %v", err)
			}
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/admin/realms/e-skylab/users/"+userID.String()+"/logout":
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodDelete && r.URL.Path == "/admin/realms/e-skylab/users/"+userID.String():
			deleteCalls++
			if deleteCalls == 1 {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	directory := identity.NewKeycloak(identity.KeycloakConfig{
		URL: srv.URL, Realm: "e-skylab", ClientID: "core", ClientSecret: "secret",
	})
	lifecycle := identity.NewAccountIdentity(directory)
	ctx := context.Background()
	if err := lifecycle.EnsureDisabled(ctx, userID); err != nil {
		t.Fatal(err)
	}
	if disabledBody["enabled"] != false || disabledBody["federationLink"] != "ldap-provider-id" || disabledBody["username"] != "ldap-user" {
		t.Fatalf("federated representation was not preserved: %+v", disabledBody)
	}
	if err := lifecycle.EnsureLoggedOut(ctx, userID); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.EnsureDeleted(ctx, userID); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.EnsureDeleted(ctx, userID); err != nil {
		t.Fatalf("delete retry: %v", err)
	}
}
