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
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// seatRealm is a directory where every way Keycloak gives a person a role
// gives someone forms' skyforms:access or skyforms:*. Every person is named
// "Seat <who>", so one search finds them all.
type seatRealm struct {
	dir   *identity.Memory
	store *user.MemoryStore
	svc   identity.Service
	ids   map[string]uuid.UUID
}

func newSeatRealm(t *testing.T) seatRealm {
	t.Helper()
	dir, store, svc := setup(t)
	ctx := context.Background()
	r := seatRealm{dir: dir, store: store, svc: svc, ids: map[string]uuid.UUID{}}
	for _, who := range []string{"direct", "group", "subgroup", "realmcomposite", "clientcomposite", "star", "none", "inactive"} {
		id := uuid.New()
		r.ids[who] = id
		dir.PutUser(identity.Person{ID: id, Email: who + "@example.test", FirstName: "Seat", LastName: who})
	}
	dir.PutGroup(identity.Group{ID: "g-uyeler", Name: "UYELER", Path: "/UYELER"})
	dir.PutGroup(identity.Group{ID: "g-yk", Name: "YK", Path: "/UYELER/YK"})
	dir.PutGroup(identity.Group{ID: "g-realm", Name: "REALM", Path: "/REALM"})
	dir.PutGroup(identity.Group{ID: "g-other", Name: "OTHER", Path: "/OTHER"})
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	access := identity.ClientRole{ClientID: "forms", Role: "skyforms:access"}
	star := identity.ClientRole{ClientID: "forms", Role: "skyforms:*"}
	// Direct mapping.
	must(dir.AddUserExtraRole(ctx, r.ids["direct"], access))
	// Group mapping, and a member of its subgroup who inherits it.
	must(dir.SetGroupClientRoles(ctx, "g-uyeler", []identity.ClientRole{access}))
	must(dir.AddMember(ctx, "g-uyeler", r.ids["group"]))
	must(dir.AddMember(ctx, "g-yk", r.ids["subgroup"]))
	// A group holds a realm role that includes a panel role that includes
	// skyforms:access (realm → client → client).
	panel := identity.ClientRole{ClientID: "admin", Role: "forms-editor"}
	realmRole := identity.ClientRole{Role: "forms-seat"}
	dir.PutCompositeRole(realmRole, panel)
	dir.PutCompositeRole(panel, access)
	must(dir.SetGroupClientRoles(ctx, "g-realm", []identity.ClientRole{realmRole}))
	must(dir.AddMember(ctx, "g-realm", r.ids["realmcomposite"]))
	// A direct client role that includes another that includes skyforms:*.
	outer := identity.ClientRole{ClientID: "admin", Role: "superuser"}
	inner := identity.ClientRole{ClientID: "skycms", Role: "client:admin"}
	dir.PutCompositeRole(outer, inner)
	dir.PutCompositeRole(inner, star)
	must(dir.AddUserExtraRole(ctx, r.ids["clientcomposite"], outer))
	// skyforms:* held directly.
	must(dir.AddUserExtraRole(ctx, r.ids["star"], star))
	// A group with another forms role only.
	must(dir.SetGroupClientRoles(ctx, "g-other", []identity.ClientRole{{ClientID: "forms", Role: "skyforms:form:manage"}}))
	must(dir.AddMember(ctx, "g-other", r.ids["none"]))
	// Holds the seat through the group but is being erased.
	must(dir.AddMember(ctx, "g-uyeler", r.ids["inactive"]))
	if _, _, err := store.Upsert(ctx, user.User{ID: r.ids["inactive"], Email: "inactive@example.test", FirstName: "Seat", LastName: "inactive"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RequestDeletion(ctx, r.ids["inactive"], nil); err != nil {
		t.Fatal(err)
	}
	return r
}

func (r seatRealm) names(t *testing.T, people []identity.Person) []string {
	t.Helper()
	out := make([]string, 0, len(people))
	for _, p := range people {
		found := false
		for who, id := range r.ids {
			if id == p.ID {
				out = append(out, who)
				found = true
			}
		}
		if !found {
			t.Fatalf("unknown person in the answer: %+v", p)
		}
	}
	slices.Sort(out)
	return out
}

var seat = identity.ClientRole{ClientID: "forms", Role: "skyforms:access"}

func TestService_ListUsersSeatSearchFindsEveryEffectiveHolder(t *testing.T) {
	t.Parallel()
	r := newSeatRealm(t)
	got, err := r.svc.ListUsers(context.Background(), privileged(), "seat", seat)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"clientcomposite", "direct", "group", "realmcomposite", "star", "subgroup"}
	if names := r.names(t, got); !slices.Equal(names, want) {
		t.Fatalf("seat holders = %v, want %v", names, want)
	}
}

func TestService_ListUsersSeatSearchMatchesTheQuery(t *testing.T) {
	t.Parallel()
	r := newSeatRealm(t)
	ctx := context.Background()
	got, err := r.svc.ListUsers(ctx, privileged(), "subgroup", seat)
	if err != nil {
		t.Fatal(err)
	}
	if names := r.names(t, got); !slices.Equal(names, []string{"subgroup"}) {
		t.Fatalf("subgroup search = %v", names)
	}
	got, err = r.svc.ListUsers(ctx, privileged(), "seat none", seat)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("non-holder answered: %v", r.names(t, got))
	}
}

func TestService_ListUsersSeatSearchFindsDefaultRoleHolders(t *testing.T) {
	t.Parallel()
	r := newSeatRealm(t)
	defaults := identity.ClientRole{Role: "default-roles-e-skylab"}
	r.dir.PutCompositeRole(defaults, identity.ClientRole{ClientID: "forms", Role: "skyforms:access"})
	r.dir.SetDefaultRoles(defaults)
	got, err := r.svc.ListUsers(context.Background(), privileged(), "seat", seat)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"clientcomposite", "direct", "group", "none", "realmcomposite", "star", "subgroup"}
	if names := r.names(t, got); !slices.Equal(names, want) {
		t.Fatalf("seat holders = %v, want %v", names, want)
	}
}

func TestService_ListUsersSeatSearchKeepsTheSafeProjection(t *testing.T) {
	t.Parallel()
	r := newSeatRealm(t)
	reader := authz.Principal{ID: "forms", Roles: []string{"users:read"}}
	got, err := r.svc.ListUsers(context.Background(), reader, "seat", seat)
	if err != nil {
		t.Fatal(err)
	}
	full, err := r.svc.ListUsers(context.Background(), privileged(), "seat", seat)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(full) {
		t.Fatalf("safe answer has %d people, full %d", len(got), len(full))
	}
}

// failingRoles is the memory directory whose effective role read fails, as
// a Keycloak that answers 500 does.
type failingRoles struct{ *identity.Memory }

func (failingRoles) EffectiveClientRoles(context.Context, uuid.UUID, string) ([]string, error) {
	return nil, errors.New("keycloak effective client roles failed with status 500")
}

func TestService_ListUsersSeatSearchFailsWhenKeycloakFails(t *testing.T) {
	t.Parallel()
	r := newSeatRealm(t)
	svc := identity.NewService(failingRoles{r.dir}, r.store, authz.NewAuthorizer(authz.DefaultPolicy()))
	got, err := svc.ListUsers(context.Background(), privileged(), "seat", seat)
	if err == nil {
		t.Fatalf("a failed role read answered %v", r.names(t, got))
	}
}

// seatKeycloak is a Keycloak with client forms whose seat roles have no
// direct holders, two people a search for "seat" finds, and the effective
// forms roles of each (Keycloak's role-mappings/clients/{uuid}/composite).
// compositeStatus fails the effective role reads (0: no failure).
type seatKeycloak struct {
	effective       map[string][]string // user id → forms role names
	people          []map[string]any
	compositeStatus int

	mu       sync.Mutex
	searches []string
}

func (f *seatKeycloak) directory(t *testing.T) *identity.Keycloak {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/protocol/openid-connect/token") {
			_, _ = w.Write([]byte(`{"access_token":"tok","expires_in":300,"token_type":"Bearer"}`))
			return
		}
		const realm = "/admin/realms/e-skylab"
		path := r.URL.EscapedPath()
		switch {
		case path == realm+"/clients" && r.URL.Query().Get("clientId") == "forms":
			_, _ = w.Write([]byte(`[{"id":"c-forms","clientId":"forms"}]`))
		case strings.HasPrefix(path, realm+"/clients/c-forms/roles/"):
			// role/groups and role/users: direct mappings only, none here.
			_, _ = w.Write([]byte(`[]`))
		case path == realm+"/users" && r.URL.Query().Has("search"):
			f.mu.Lock()
			f.searches = append(f.searches, r.URL.Query().Get("search")+" max="+r.URL.Query().Get("max"))
			f.mu.Unlock()
			_ = json.NewEncoder(w).Encode(f.people)
		case strings.HasPrefix(path, realm+"/users/") && strings.HasSuffix(path, "/role-mappings/clients/c-forms/composite"):
			if f.compositeStatus != 0 {
				w.WriteHeader(f.compositeStatus)
				_, _ = w.Write([]byte(`{"error":"unknown_error"}`))
				return
			}
			id := strings.TrimSuffix(strings.TrimPrefix(path, realm+"/users/"), "/role-mappings/clients/c-forms/composite")
			out := []map[string]any{}
			for _, name := range f.effective[id] {
				out = append(out, map[string]any{"id": uuid.NewString(), "name": name, "clientRole": true, "containerId": "c-forms"})
			}
			_ = json.NewEncoder(w).Encode(out)
		default:
			t.Errorf("unexpected Keycloak call %s %s", r.Method, path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return identity.NewKeycloak(identity.KeycloakConfig{URL: srv.URL, Realm: "e-skylab", ClientID: "core", ClientSecret: "secret"})
}

func newSeatKeycloak() (*seatKeycloak, uuid.UUID, uuid.UUID) {
	holder, other := uuid.New(), uuid.New()
	return &seatKeycloak{
		effective: map[string][]string{holder.String(): {"skyforms:access", "skyforms:form:manage"}, other.String(): {"skyforms:form:manage"}},
		people: []map[string]any{
			{"id": holder.String(), "username": "holder", "email": "holder@example.test", "firstName": "Seat", "lastName": "Holder", "enabled": true},
			{"id": other.String(), "username": "other", "email": "other@example.test", "firstName": "Seat", "lastName": "Other", "enabled": true},
		},
	}, holder, other
}

// Against Keycloak itself: a person who holds the seat only through a Group
// above theirs or a composite role (not a direct mapping) is found through
// Keycloak's search and their effective roles.
func TestKeycloakSeatSearchKeepsEffectiveHoldersOnly(t *testing.T) {
	t.Parallel()
	fake, holder, _ := newSeatKeycloak()
	svc := identity.NewService(fake.directory(t), user.NewMemoryStore(), authz.NewAuthorizer(authz.DefaultPolicy()))
	got, err := svc.ListUsers(context.Background(), privileged(), "seat hol", seat)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != holder {
		t.Fatalf("seat search = %+v, want the holder only", got)
	}
	if want := []string{"*seat* max=50"}; !slices.Equal(fake.searches, want) {
		t.Fatalf("searches = %v, want %v", fake.searches, want)
	}
}

// A Keycloak that fails the effective role read fails the search: a 500
// that is logged, not an empty list.
func TestKeycloakSeatSearchFailsWhenTheRoleReadFails(t *testing.T) {
	t.Parallel()
	fake, _, _ := newSeatKeycloak()
	fake.compositeStatus = http.StatusInternalServerError
	svc := identity.NewService(fake.directory(t), user.NewMemoryStore(), authz.NewAuthorizer(authz.DefaultPolicy()))
	if got, err := svc.ListUsers(context.Background(), privileged(), "seat", seat); err == nil {
		t.Fatalf("seat search = %+v, want an error", got)
	}
}
