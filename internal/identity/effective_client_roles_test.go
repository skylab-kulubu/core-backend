package identity_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/identity"
)

// fakeClientRoles is a Keycloak that knows some clients by clientId and,
// per client UUID, the client roles one user holds in effect, and answers
// GET /clients?clientId= and GET /users/{id}/role-mappings/clients/{client}/composite
// the way Keycloak does. clientsStatus and rolesStatus fail those calls (0:
// no failure).
type fakeClientRoles struct {
	user          uuid.UUID
	clients       map[string]string   // clientId → client UUID
	roles         map[string][]string // client UUID → role names
	clientsStatus int
	rolesStatus   int

	mu    sync.Mutex
	paths []string
}

func (f *fakeClientRoles) directory(t *testing.T) *identity.Keycloak {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/protocol/openid-connect/token") {
			_, _ = w.Write([]byte(`{"access_token":"tok","expires_in":300,"token_type":"Bearer"}`))
			return
		}
		if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("unexpected Keycloak call %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		f.mu.Lock()
		f.paths = append(f.paths, r.URL.Path)
		f.mu.Unlock()
		const realm = "/admin/realms/e-skylab"
		switch {
		case r.URL.Path == realm+"/clients":
			if f.clientsStatus != 0 {
				w.WriteHeader(f.clientsStatus)
				return
			}
			out := []map[string]string{}
			if id, ok := f.clients[r.URL.Query().Get("clientId")]; ok {
				out = append(out, map[string]string{"id": id, "clientId": r.URL.Query().Get("clientId")})
			}
			_ = json.NewEncoder(w).Encode(out)
		case strings.HasPrefix(r.URL.Path, realm+"/users/") && strings.HasSuffix(r.URL.Path, "/composite"):
			if f.rolesStatus != 0 {
				// Keycloak's error bodies may carry anything, the person included.
				w.WriteHeader(f.rolesStatus)
				_, _ = w.Write([]byte(`{"error":"` + f.user.String() + ` kisi@example.com"}`))
				return
			}
			rest := strings.TrimPrefix(r.URL.Path, realm+"/users/")
			user, rest, _ := strings.Cut(rest, "/role-mappings/clients/")
			client := strings.TrimSuffix(rest, "/composite")
			names, known := f.roles[client]
			if user != f.user.String() || !known {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"error":"not found"}`))
				return
			}
			out := []map[string]any{}
			for _, name := range names {
				out = append(out, map[string]any{"id": uuid.NewString(), "name": name, "clientRole": true, "containerId": client})
			}
			_ = json.NewEncoder(w).Encode(out)
		default:
			t.Errorf("unexpected Keycloak call %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return identity.NewKeycloak(identity.KeycloakConfig{URL: srv.URL, Realm: "e-skylab", ClientID: "core", ClientSecret: "secret"})
}

// The person's roles on one client come from Keycloak's composite role
// mappings of that client: what they hold directly, through their Groups and
// through composite roles, which is what a token for the client carries.
func TestKeycloakEffectiveClientRolesReadsTheCompositeMappingsOfTheClient(t *testing.T) {
	t.Parallel()

	id := uuid.New()
	fake := &fakeClientRoles{
		user:    id,
		clients: map[string]string{"frontend-artlab": "c-artlab", "frontend-main": "c-main"},
		roles:   map[string][]string{"c-artlab": {"cms:access", "content:read", "content:write"}, "c-main": {}},
	}
	dir := fake.directory(t)

	got, err := dir.EffectiveClientRoles(context.Background(), id, "frontend-artlab")
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(got)
	if want := []string{"cms:access", "content:read", "content:write"}; !slices.Equal(got, want) {
		t.Fatalf("roles = %v, want %v", got, want)
	}
	got, err = dir.EffectiveClientRoles(context.Background(), id, "frontend-main")
	if err != nil || len(got) != 0 {
		t.Fatalf("roles = %v, %v; want none", got, err)
	}
	want := "/admin/realms/e-skylab/users/" + id.String() + "/role-mappings/clients/c-artlab/composite"
	if !slices.Contains(fake.paths, want) {
		t.Fatalf("calls = %v, want %s", fake.paths, want)
	}
}

// A client the realm does not have, or a person it does not know, is
// ErrNotFound: nobody holds a role there.
func TestKeycloakEffectiveClientRolesOfAnUnknownClientOrUserIsNotFound(t *testing.T) {
	t.Parallel()

	id := uuid.New()
	fake := &fakeClientRoles{user: id, clients: map[string]string{"frontend-main": "c-main"}, roles: map[string][]string{"c-main": {"cms:access"}}}
	dir := fake.directory(t)

	if _, err := dir.EffectiveClientRoles(context.Background(), id, "frontend-skydays"); !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("unknown client: error = %v, want ErrNotFound", err)
	}
	if _, err := dir.EffectiveClientRoles(context.Background(), uuid.New(), "frontend-main"); !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("unknown user: error = %v, want ErrNotFound", err)
	}
}

// Any other answer is an error, never an empty list, and no error names the
// person, so a caller may log it. 403 is what a service account without
// view-users (or view-clients) gets.
func TestKeycloakEffectiveClientRolesFailsWithoutNamingThePerson(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		fake *fakeClientRoles
		want string
	}{
		{name: "role mappings forbidden", fake: &fakeClientRoles{rolesStatus: http.StatusForbidden}, want: "403"},
		{name: "role mappings server error", fake: &fakeClientRoles{rolesStatus: http.StatusInternalServerError}, want: "500"},
		{name: "clients forbidden", fake: &fakeClientRoles{clientsStatus: http.StatusForbidden}, want: "403"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			id := uuid.New()
			fake := tc.fake
			fake.user = id
			fake.clients = map[string]string{"frontend-main": "c-main"}
			fake.roles = map[string][]string{"c-main": {"cms:access"}}

			got, err := fake.directory(t).EffectiveClientRoles(context.Background(), id, "frontend-main")
			if err == nil || got != nil {
				t.Fatalf("roles = %v, %v; want only an error", got, err)
			}
			if errors.Is(err, identity.ErrNotFound) {
				t.Fatalf("error = %v, want a failure, not an unknown client or user", err)
			}
			msg := err.Error()
			if strings.Contains(msg, id.String()) || strings.Contains(msg, "kisi@example.com") || !strings.Contains(msg, tc.want) {
				t.Fatalf("error = %q, want %q and neither the person nor an address", msg, tc.want)
			}
		})
	}
}
