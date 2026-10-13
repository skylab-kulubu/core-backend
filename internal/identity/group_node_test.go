package identity_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
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
// and clients by UUID as Keycloak 26.7.4 answers core's service account,
// which holds no view-realm: /role-mappings leaves the realm mappings out,
// /role-mappings/realm has the direct ones, /role-mappings/realm/composite
// the effective ones (through client composites too), and a role's
// composites leave its realm children out. failing names path suffixes
// that answer 500.
func fakeRoleMappings(t *testing.T, mappings map[string]any, composites map[string][]map[string]any, clients map[string]string, failing ...string) *identity.Keycloak {
	t.Helper()
	realmDirect, _ := mappings["realmMappings"].([]map[string]any)
	// effective are the realm roles the Group holds in effect: its own and
	// every realm role reachable through its composites.
	effective := []map[string]any{}
	seen := map[string]bool{}
	var walk func(role map[string]any)
	walk = func(role map[string]any) {
		id, _ := role["id"].(string)
		if seen[id] {
			return
		}
		seen[id] = true
		if client, _ := role["clientRole"].(bool); !client {
			effective = append(effective, role)
		}
		for _, child := range composites[id] {
			walk(child)
		}
	}
	for _, r := range realmDirect {
		walk(r)
	}
	if cms, ok := mappings["clientMappings"].(map[string]any); ok {
		for _, cm := range cms {
			for _, r := range cm.(map[string]any)["mappings"].([]map[string]any) {
				r["clientRole"] = true
				walk(r)
			}
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.Path
		for _, suffix := range failing {
			if strings.HasSuffix(path, suffix) {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
		}
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/protocol/openid-connect/token"):
			_, _ = w.Write([]byte(`{"access_token":"tok","expires_in":300,"token_type":"Bearer"}`))
		case strings.HasPrefix(path, "/admin/realms/e-skylab/groups/") && strings.HasSuffix(path, "/role-mappings/realm/composite"):
			_ = json.NewEncoder(w).Encode(effective)
		case strings.HasPrefix(path, "/admin/realms/e-skylab/groups/") && strings.HasSuffix(path, "/role-mappings/realm"):
			if realmDirect == nil {
				realmDirect = []map[string]any{}
			}
			_ = json.NewEncoder(w).Encode(realmDirect)
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
			// The realm children are left out, without an error.
			visible := []map[string]any{}
			for _, child := range composites[id] {
				if client, _ := child["clientRole"].(bool); client {
					visible = append(visible, child)
				}
			}
			_ = json.NewEncoder(w).Encode(visible)
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

// deepChain is a composite app role n levels deep (core's
// certificate:issue, a new id at each level) that ends in an app role.
func deepChain(n int) map[string][]map[string]any {
	out := map[string][]map[string]any{}
	for i := 0; i < n; i++ {
		next := "l" + strconv.Itoa(i+1)
		out["l"+strconv.Itoa(i)] = []map[string]any{{"id": next, "name": "certificate:issue", "clientRole": true, "containerId": "c-core", "composite": i+1 < n}}
	}
	return out
}

// Keycloak's answer: a realm role (mapped, or inside a client composite,
// which Keycloak hides from a role's composites), a protected client role,
// or a composite that holds one at any depth protects the Group; a
// composite of harmless roles does not; a composite deeper than the limit
// does.
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
		{"a client composite holding a realm role", clientMapping("skymail", map[string]any{"id": "outer", "name": "outer", "composite": true}),
			map[string][]map[string]any{"outer": {{"id": "admin", "name": "admin", "clientRole": false, "containerId": "e-skylab"}}}, true},
		// client → realm → realm-management: the realm role is hidden from
		// the composite walk; only the Group's effective realm roles show it.
		{"client, realm, realm-management", clientMapping("skymail", map[string]any{"id": "bridge", "name": "bridge", "composite": true}),
			map[string][]map[string]any{
				"bridge":  {{"id": "rmadmin", "name": "rmadmin", "clientRole": false, "composite": true, "containerId": "e-skylab"}},
				"rmadmin": {{"id": "ra", "name": "realm-admin", "clientRole": true, "containerId": "c-rm"}},
			}, true},
		{"a mixed composite", clientMapping("skymail", map[string]any{"id": "mixed", "name": "mixed", "composite": true}),
			map[string][]map[string]any{"mixed": {
				{"id": "h", "name": "harmless", "clientRole": true, "containerId": "c-forms"},
				{"id": "rmadmin", "name": "rmadmin", "clientRole": false, "containerId": "e-skylab"},
			}}, true},
		{"cms:access and content roles", clientMapping("frontend-main", map[string]any{"id": "cms", "name": "cms:access", "composite": true}),
			map[string][]map[string]any{"cms": {
				{"id": "cr", "name": "content:read", "clientRole": true, "containerId": "c-site"},
				{"id": "cw", "name": "content:write", "clientRole": true, "containerId": "c-site"},
			}}, true},
		// A Site editor's write (ADR 0065 decision 10, addendum of
		// 2026-10-12): content:write, mapped or inside cms:access, on any
		// Site client; content:read alone is reading.
		{"content:write", clientMapping("frontend-arge", map[string]any{"id": "cw", "name": "content:write"}), nil, true},
		{"content:read alone", clientMapping("frontend-main", map[string]any{"id": "cr", "name": "content:read"}), nil, false},
		// cms:access is an admin role by its own name: protected even if
		// Keycloak answered its composites without content:write.
		{"cms:access with content:read only", clientMapping("frontend-main", map[string]any{"id": "cms", "name": "cms:access", "composite": true}),
			map[string][]map[string]any{"cms": {
				{"id": "cr", "name": "content:read", "clientRole": true, "containerId": "c-site"},
			}}, true},
		// Role classes (ADR 0065 decision 10, addendum of 2026-10-12).
		{"an app role", clientMapping("core", map[string]any{"id": "ci", "name": "certificate:issue"}), nil, false},
		{"an unclassified role", clientMapping("forms", map[string]any{"id": "fe", "name": "forms:editor"}), nil, true},
		{"an app role's name on another client", clientMapping("forms", map[string]any{"id": "ci", "name": "certificate:issue"}), nil, true},
		{"a composite app role holding an unclassified role", clientMapping("core", map[string]any{"id": "ci", "name": "certificate:issue", "composite": true}),
			map[string][]map[string]any{"ci": {{"id": "fe", "name": "forms:editor", "clientRole": true, "containerId": "c-forms"}}}, true},
		{"a composite app role holding app roles", clientMapping("core", map[string]any{"id": "ci", "name": "certificate:issue", "composite": true}),
			map[string][]map[string]any{"ci": {
				{"id": "cr", "name": "content:read", "clientRole": true, "containerId": "c-site"},
				{"id": "cv", "name": "certificate:revoke", "clientRole": true, "containerId": "c-core"},
			}}, false},
		{"a composite loop", clientMapping("core", map[string]any{"id": "a", "name": "certificate:issue", "composite": true}),
			map[string][]map[string]any{
				"a": {{"id": "b", "name": "certificate:revoke", "clientRole": true, "containerId": "c-core", "composite": true}},
				"b": {{"id": "a", "name": "certificate:issue", "clientRole": true, "containerId": "c-core", "composite": true}},
			}, false},
		{"eight levels, app roles", clientMapping("core", map[string]any{"id": "l0", "name": "certificate:issue", "composite": true}), deepChain(8), false},
		{"eleven levels, app roles", clientMapping("core", map[string]any{"id": "l0", "name": "certificate:issue", "composite": true}), deepChain(11), true},
	} {
		got, err := fakeRoleMappings(t, tc.mappings, tc.composites, clients).GroupProtection(context.Background(), uuid.NewString())
		if err != nil || got.Protected != tc.want || (got.Protected && got.Reason == "") {
			t.Errorf("%s: %+v %v", tc.name, got, err)
		}
	}
}

// A read that fails is an error, never "not protected": the write it
// guards is refused.
func TestKeycloakGroupProtectionFailsClosed(t *testing.T) {
	t.Parallel()
	mapping := clientMapping("core", map[string]any{"id": "staff", "name": "certificate:issue", "composite": true})
	composites := map[string][]map[string]any{"staff": {{"id": "x", "name": "skyforms:access", "clientRole": true, "containerId": "c-forms"}}}
	for _, suffix := range []string{"/role-mappings/realm/composite", "/role-mappings", "/composites", "/clients/c-forms"} {
		got, err := fakeRoleMappings(t, mapping, composites, map[string]string{"c-forms": "forms"}, suffix).GroupProtection(context.Background(), uuid.NewString())
		if err == nil {
			t.Errorf("%s failing: %+v, want an error", suffix, got)
		}
	}
}
