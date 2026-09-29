package identity_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/identity"
)

// fakeUserGroups is a Keycloak that knows one user and answers
// GET /users/{id}/groups the way Keycloak does: the direct memberships,
// paged by first and max. A status fails that call (0: no failure); with
// failFrom set, only the pages that start at or after that offset fail, so
// failFrom 1 lets the first page through. drop closes the connection instead
// of answering.
type fakeUserGroups struct {
	id       uuid.UUID
	groups   []map[string]any
	status   int
	failFrom int
	drop     bool

	mu    sync.Mutex
	pages []string
}

func (f *fakeUserGroups) serve(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/protocol/openid-connect/token") {
			_, _ = w.Write([]byte(`{"access_token":"tok","expires_in":300,"token_type":"Bearer"}`))
			return
		}
		if r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/groups") || !strings.HasPrefix(r.URL.Path, "/admin/realms/e-skylab/users/") {
			t.Errorf("unexpected Keycloak call %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		first, limit := pageParam(r, "first", 0), pageParam(r, "max", -1)
		f.mu.Lock()
		f.pages = append(f.pages, fmt.Sprintf("%d/%d", first, limit))
		f.mu.Unlock()
		switch {
		case f.drop:
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
			return
		case f.status != 0 && first >= f.failFrom:
			// Keycloak's error bodies may carry anything, the person included.
			w.WriteHeader(f.status)
			_, _ = w.Write([]byte(`{"error":"` + f.id.String() + ` kisi@example.com"}`))
			return
		case r.URL.Path != "/admin/realms/e-skylab/users/"+f.id.String()+"/groups":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"User not found"}`))
			return
		}
		page := f.groups[min(first, len(f.groups)):]
		if limit >= 0 && limit < len(page) {
			page = page[:limit]
		}
		_ = json.NewEncoder(w).Encode(page)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func pageParam(r *http.Request, name string, fallback int) int {
	value, err := strconv.Atoi(r.URL.Query().Get(name))
	if err != nil {
		return fallback
	}
	return value
}

func (f *fakeUserGroups) directory(t *testing.T) *identity.Keycloak {
	t.Helper()
	return identity.NewKeycloak(identity.KeycloakConfig{URL: f.serve(t).URL, Realm: "e-skylab", ClientID: "core", ClientSecret: "secret"})
}

// The person's groups come back as the token's groups claim carries them:
// the direct memberships, each with its full path. A subgroup nested in
// Keycloak's answer is not a membership of the person.
func TestKeycloakGroupsForUserReturnsTheDirectGroupsWithTheirFullPaths(t *testing.T) {
	t.Parallel()

	id := uuid.New()
	fake := &fakeUserGroups{id: id, groups: []map[string]any{
		{"id": "g1", "name": "LIDERLER", "path": "/UYELER/ARGE/WEBLAB/LIDERLER"},
		{"id": "g2", "name": "ARGE", "path": "/UYELER/ARGE", "attributes": map[string][]string{"public_listing": {"true"}},
			"subGroups": []map[string]any{{"id": "g3", "name": "YK", "path": "/UYELER/ARGE/YK"}}},
	}}

	got, err := fake.directory(t).GroupsForUser(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"/UYELER/ARGE/WEBLAB/LIDERLER", "/UYELER/ARGE"}; !slices.Equal(identity.GroupPaths(got), want) {
		t.Fatalf("paths = %v, want %v", identity.GroupPaths(got), want)
	}
	if got[0].ID != "g1" || got[0].Name != "LIDERLER" || got[1].Attributes["public_listing"] != "true" {
		t.Fatalf("groups = %+v", got)
	}
}

// Every membership comes back, however many pages Keycloak serves them in: a
// list cut short would read as fewer Groups.
func TestKeycloakGroupsForUserPagesThroughEveryGroup(t *testing.T) {
	t.Parallel()

	id := uuid.New()
	fake := &fakeUserGroups{id: id}
	var want []string
	for i := range 1005 {
		path := fmt.Sprintf("/UYELER/TAKIM-%04d", i)
		fake.groups = append(fake.groups, map[string]any{"id": fmt.Sprintf("g%d", i), "name": path[8:], "path": path})
		want = append(want, path)
	}

	got, err := fake.directory(t).GroupsForUser(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(identity.GroupPaths(got), want) {
		t.Fatalf("got %d paths, want all %d in order", len(got), len(want))
	}
	if len(fake.pages) < 2 {
		t.Fatalf("pages = %v, want the groups read in pages", fake.pages)
	}
}

// A Keycloak failure is an error, never an empty or partial list: a person
// whose Groups could not be read must not read as a person without Groups.
// No error names the person (their id is in the request address; the error
// body may hold anything), so a caller may log it.
func TestKeycloakGroupsForUserFailsWithoutNamingThePerson(t *testing.T) {
	t.Parallel()

	manyGroups := make([]map[string]any, 0, 250)
	for i := range 250 {
		manyGroups = append(manyGroups, map[string]any{"id": fmt.Sprintf("g%d", i), "path": fmt.Sprintf("/UYELER/T%d", i)})
	}
	for _, tc := range []struct {
		name string
		fake *fakeUserGroups
		want string
	}{
		{name: "server error", fake: &fakeUserGroups{status: http.StatusInternalServerError}, want: "500"},
		{name: "forbidden", fake: &fakeUserGroups{status: http.StatusForbidden}, want: "403"},
		{name: "failure on a later page", fake: &fakeUserGroups{groups: manyGroups, status: http.StatusServiceUnavailable, failFrom: 1}, want: "503"},
		{name: "connection dropped", fake: &fakeUserGroups{drop: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			id := uuid.New()
			fake := tc.fake
			fake.id = id

			got, err := fake.directory(t).GroupsForUser(context.Background(), id)
			if err == nil {
				t.Fatalf("groups = %v, want an error", identity.GroupPaths(got))
			}
			if got != nil {
				t.Fatalf("groups = %v alongside the error, want none", identity.GroupPaths(got))
			}
			if errors.Is(err, identity.ErrNotFound) {
				t.Fatalf("error = %v, want a failure, not an unknown user", err)
			}
			msg := err.Error()
			if strings.Contains(msg, id.String()) || strings.Contains(msg, "kisi@example.com") || !strings.Contains(msg, tc.want) {
				t.Fatalf("error = %q, want %q and neither the person nor an address", msg, tc.want)
			}
		})
	}
}

// A user Keycloak does not know (deleted since it was listed) is ErrNotFound.
func TestKeycloakGroupsForUserOfAnUnknownUserIsNotFound(t *testing.T) {
	t.Parallel()

	fake := &fakeUserGroups{id: uuid.New()}
	_, err := fake.directory(t).GroupsForUser(context.Background(), uuid.New())
	if !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
}
