package identity_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/identity"
)

// fakeGroupByID is a Keycloak that answers GET /groups/{id} the way 26.x
// does: the Group with its path and the id of its parent.
type fakeGroupByID struct {
	groups map[string]map[string]any
	status int

	mu    sync.Mutex
	calls []string
}

func (f *fakeGroupByID) directory(t *testing.T) *identity.Keycloak {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/protocol/openid-connect/token") {
			_, _ = w.Write([]byte(`{"access_token":"tok","expires_in":300,"token_type":"Bearer"}`))
			return
		}
		f.mu.Lock()
		f.calls = append(f.calls, r.Method+" "+r.URL.Path)
		f.mu.Unlock()
		id, ok := strings.CutPrefix(r.URL.Path, "/admin/realms/e-skylab/groups/")
		if r.Method != http.MethodGet || !ok || strings.Contains(id, "/") || r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("unexpected Keycloak call %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if f.status != 0 {
			w.WriteHeader(f.status)
			return
		}
		g, ok := f.groups[id]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"Could not find group by id"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(g)
	}))
	t.Cleanup(srv.Close)
	return identity.NewKeycloak(identity.KeycloakConfig{URL: srv.URL, Realm: "e-skylab", ClientID: "core", ClientSecret: "secret"})
}

// A Group read by id carries the id of the Group above it, which the
// delegated decisions walk up by.
func TestKeycloakGroupNodeCarriesTheParentID(t *testing.T) {
	t.Parallel()
	team, area := uuid.NewString(), uuid.NewString()
	fake := &fakeGroupByID{groups: map[string]map[string]any{
		team: {"id": team, "name": "WEBLAB", "path": "/UYELER/ARGE/WEBLAB", "parentId": area,
			"attributes": map[string][]string{"display_name_tr": {"Web"}}},
		area: {"id": area, "name": "ARGE", "path": "/UYELER/ARGE"},
	}}
	dir := fake.directory(t)
	g, parent, err := dir.GroupNode(context.Background(), team)
	if err != nil {
		t.Fatal(err)
	}
	if g.ID != team || g.Name != "WEBLAB" || g.Path != "/UYELER/ARGE/WEBLAB" || parent != area || g.Attributes["display_name_tr"] != "Web" {
		t.Fatalf("node = %+v, parent %q", g, parent)
	}
	if _, parent, err := dir.GroupNode(context.Background(), area); err != nil || parent != "" {
		t.Fatalf("top: parent %q, %v", parent, err)
	}
}

// Only an id names a Group here: a path or a name is not looked up (the
// delegated decisions never resolve a Group by its name), nor is an
// unknown id.
func TestKeycloakGroupNodeReadsOnlyByID(t *testing.T) {
	t.Parallel()
	fake := &fakeGroupByID{groups: map[string]map[string]any{}}
	dir := fake.directory(t)
	for _, ref := range []string{"/UYELER/ARGE/WEBLAB", "WEBLAB", "", "../users"} {
		if _, _, err := dir.GroupNode(context.Background(), ref); !errors.Is(err, identity.ErrNotFound) {
			t.Errorf("%q: %v, want ErrNotFound", ref, err)
		}
	}
	if len(fake.calls) != 0 {
		t.Fatalf("calls = %v, want none", fake.calls)
	}
	if _, _, err := dir.GroupNode(context.Background(), uuid.NewString()); !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("unknown id: %v", err)
	}
}

// A failure is an error, never a Group without a parent: a top Group is
// one Keycloak says has none.
func TestKeycloakGroupNodeFails(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusInternalServerError, http.StatusForbidden} {
		fake := &fakeGroupByID{status: status}
		if _, _, err := fake.directory(t).GroupNode(context.Background(), uuid.NewString()); err == nil || errors.Is(err, identity.ErrNotFound) {
			t.Errorf("%d: %v", status, err)
		}
	}
}

// The memory directory answers as Keycloak: by id only, with the parent
// the paths say, or the one a test sets.
func TestMemoryGroupNode(t *testing.T) {
	t.Parallel()
	m := identity.NewMemory()
	m.PutGroup(identity.Group{ID: "u", Name: "UYELER", Path: "/UYELER"})
	m.PutGroup(identity.Group{ID: "a", Name: "ARGE", Path: "/UYELER/ARGE"})
	m.PutGroup(identity.Group{ID: "w", Name: "WEBLAB", Path: "/UYELER/ARGE/WEBLAB"})
	for id, want := range map[string]string{"u": "", "a": "u", "w": "a"} {
		if _, parent, err := m.GroupNode(context.Background(), id); err != nil || parent != want {
			t.Errorf("%s: parent %q, %v", id, parent, err)
		}
	}
	if _, _, err := m.GroupNode(context.Background(), "/UYELER/ARGE"); !errors.Is(err, identity.ErrNotFound) {
		t.Errorf("by path: %v", err)
	}
	m.SetGroupParent("w", "u")
	if _, parent, _ := m.GroupNode(context.Background(), "w"); parent != "u" {
		t.Errorf("set parent: %q", parent)
	}
}

// fakeRoleMappings answers a Group's role mappings, composite roles by id
// and clients by UUID, as Keycloak 26.x does.
func fakeRoleMappings(t *testing.T, mappings map[string]any, composites map[string][]map[string]any, clients map[string]string) *identity.Keycloak {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.Path
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/protocol/openid-connect/token"):
			_, _ = w.Write([]byte(`{"access_token":"tok","expires_in":300,"token_type":"Bearer"}`))
		case strings.HasPrefix(path, "/admin/realms/e-skylab/groups/") && strings.HasSuffix(path, "/role-mappings/realm"):
			// Keycloak answers the realm mappings here, and leaves them out
			// of /role-mappings for a caller without view-realm.
			realm := mappings["realmMappings"]
			if realm == nil {
				realm = []any{}
			}
			_ = json.NewEncoder(w).Encode(realm)
		case strings.HasPrefix(path, "/admin/realms/e-skylab/groups/") && strings.HasSuffix(path, "/role-mappings"):
			out := map[string]any{}
			for k, v := range mappings {
				if k != "realmMappings" {
					out[k] = v
				}
			}
			_ = json.NewEncoder(w).Encode(out)
		case strings.HasPrefix(path, "/admin/realms/e-skylab/roles-by-id/") && strings.HasSuffix(path, "/composites"):
			id := strings.TrimSuffix(strings.TrimPrefix(path, "/admin/realms/e-skylab/roles-by-id/"), "/composites")
			_ = json.NewEncoder(w).Encode(composites[id])
		case strings.HasPrefix(path, "/admin/realms/e-skylab/clients/"):
			id := strings.TrimPrefix(path, "/admin/realms/e-skylab/clients/")
			_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "clientId": clients[id]})
		default:
			t.Errorf("unexpected Keycloak call %s %s", r.Method, path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return identity.NewKeycloak(identity.KeycloakConfig{URL: srv.URL, Realm: "e-skylab", ClientID: "core", ClientSecret: "secret"})
}

func clientMapping(client string, roles ...map[string]any) map[string]any {
	return map[string]any{"clientMappings": map[string]any{client: map[string]any{"client": client, "mappings": roles}}}
}

// Keycloak's answer: a realm role, a protected client role, or a composite
// that holds one (or a realm role) at any depth protects the Group; a
// composite of harmless roles does not.
func TestKeycloakGroupProtection(t *testing.T) {
	t.Parallel()
	clients := map[string]string{"c-core": "core", "c-rm": "realm-management", "c-site": "frontend-main", "c-forms": "forms"}
	for _, tc := range []struct {
		name       string
		mappings   map[string]any
		composites map[string][]map[string]any
		want       bool
	}{
		{"nothing", map[string]any{}, nil, false},
		{"a realm role", map[string]any{"realmMappings": []map[string]any{{"id": "r1", "name": "offline_access"}}}, nil, true},
		{"a protected client role", clientMapping("core", map[string]any{"id": "x", "name": "groups:manage"}), nil, true},
		{"client:admin", clientMapping("skymail", map[string]any{"id": "x", "name": "client:admin"}), nil, true},
		{"a composite with realm-management", clientMapping("skymail", map[string]any{"id": "staff", "name": "staff", "composite": true}),
			map[string][]map[string]any{"staff": {{"id": "mu", "name": "manage-users", "clientRole": true, "containerId": "c-rm"}}}, true},
		{"a composite two deep", clientMapping("skymail", map[string]any{"id": "outer", "name": "outer", "composite": true}),
			map[string][]map[string]any{
				"outer": {{"id": "inner", "name": "inner", "clientRole": true, "containerId": "c-forms", "composite": true}},
				"inner": {{"id": "gm", "name": "groups:manage", "clientRole": true, "containerId": "c-core"}},
			}, true},
		{"a composite with a realm role", clientMapping("skymail", map[string]any{"id": "outer", "name": "outer", "composite": true}),
			map[string][]map[string]any{"outer": {{"id": "admin", "name": "admin", "clientRole": false, "containerId": "e-skylab"}}}, true},
		{"cms:access and content roles", clientMapping("frontend-main", map[string]any{"id": "cms", "name": "cms:access", "composite": true}),
			map[string][]map[string]any{"cms": {
				{"id": "cr", "name": "content:read", "clientRole": true, "containerId": "c-site"},
				{"id": "cw", "name": "content:write", "clientRole": true, "containerId": "c-site"},
			}}, false},
		{"a composite loop", clientMapping("skymail", map[string]any{"id": "a", "name": "a", "composite": true}),
			map[string][]map[string]any{
				"a": {{"id": "b", "name": "b", "clientRole": true, "containerId": "c-forms", "composite": true}},
				"b": {{"id": "a", "name": "a", "clientRole": true, "containerId": "c-forms", "composite": true}},
			}, false},
	} {
		got, err := fakeRoleMappings(t, tc.mappings, tc.composites, clients).GroupProtection(context.Background(), uuid.NewString())
		if err != nil || got.Protected != tc.want || (got.Protected && got.Reason == "") {
			t.Errorf("%s: %+v %v", tc.name, got, err)
		}
	}
}
