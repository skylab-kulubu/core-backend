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
