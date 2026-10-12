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
