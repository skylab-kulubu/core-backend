package identity_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/identity"
)

// addressKeycloak answers the token and GET /users, recording each query.
func addressKeycloak(t *testing.T, status int, rows []map[string]any) (*identity.Keycloak, func() []url.Values) {
	t.Helper()
	var mu sync.Mutex
	var queries []url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/protocol/openid-connect/token") {
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 300, "token_type": "Bearer"})
			return
		}
		if r.URL.Path != "/admin/realms/e-skylab/users" || r.Header.Get("Authorization") != "Bearer tok" {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		queries = append(queries, r.URL.Query())
		mu.Unlock()
		if status != http.StatusOK {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":"ada@example.com"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(rows)
	}))
	t.Cleanup(server.Close)
	dir := identity.NewKeycloak(identity.KeycloakConfig{URL: server.URL, Realm: "e-skylab", ClientID: "core", ClientSecret: "secret"})
	return dir, func() []url.Values {
		mu.Lock()
		defer mu.Unlock()
		return queries
	}
}

// One page in full representation: the Primary e-mail with its flag, the
// first value of the School and Personal e-mail attributes and of the
// stamp, raw; a service account by its client link or its username; a row
// that is no user counted and left out.
func TestKeycloakAddressHoldersPage(t *testing.T) {
	t.Parallel()
	person, service, named := uuid.New(), uuid.New(), uuid.New()
	dir, queries := addressKeycloak(t, http.StatusOK, []map[string]any{
		{"id": person.String(), "username": "ada", "enabled": true, "email": "ada@example.com", "emailVerified": true,
			"attributes": map[string][]string{
				"schoolEmail":             {" Ada@STD.yildiz.edu.tr ", "second@std.yildiz.edu.tr"},
				"personalEmail":           {"ada@example.org"},
				"personalEmailVerifiedAt": {"2026-09-21T14:13:20Z"},
			}},
		{"id": service.String(), "username": "service-account-forms", "enabled": true, "serviceAccountClientId": "forms"},
		{"id": named.String(), "username": "service-account-x", "enabled": false},
		{"id": "not-a-uuid", "enabled": true},
	})
	page, err := dir.AddressHoldersPage(context.Background(), 1000, 500)
	if err != nil {
		t.Fatal(err)
	}
	q := queries()
	if len(q) != 1 || q[0].Get("first") != "1000" || q[0].Get("max") != "500" || q[0].Get("briefRepresentation") != "false" || q[0].Has("idpAlias") {
		t.Fatalf("queries %v", q)
	}
	if page.Rows != 4 || len(page.Holders) != 3 {
		t.Fatalf("page %+v", page)
	}
	want := identity.AddressHolder{
		ID: person, Enabled: true, Email: "ada@example.com", EmailVerified: true,
		SchoolEmail: " Ada@STD.yildiz.edu.tr ", PersonalEmail: "ada@example.org", PersonalEmailVerifiedAt: "2026-09-21T14:13:20Z",
	}
	if page.Holders[0] != want {
		t.Fatalf("person %+v", page.Holders[0])
	}
	if !page.Holders[1].ServiceAccount || !page.Holders[2].ServiceAccount || page.Holders[2].Enabled {
		t.Fatalf("service accounts %+v", page.Holders[1:])
	}
}

// The linked accounts: briefRepresentation with idpAlias, the ids only.
func TestKeycloakIdentityProviderLinkedPage(t *testing.T) {
	t.Parallel()
	a, b := uuid.New(), uuid.New()
	dir, queries := addressKeycloak(t, http.StatusOK, []map[string]any{
		{"id": a.String(), "username": "a"}, {"id": "x"}, {"id": b.String()},
	})
	page, err := dir.IdentityProviderLinkedPage(context.Background(), "OBS", 0, 3)
	if err != nil {
		t.Fatal(err)
	}
	q := queries()
	if len(q) != 1 || q[0].Get("idpAlias") != "OBS" || q[0].Get("briefRepresentation") != "true" || q[0].Get("first") != "0" || q[0].Get("max") != "3" {
		t.Fatalf("queries %v", q)
	}
	if page.Rows != 3 || len(page.IDs) != 2 || page.IDs[0] != a || page.IDs[1] != b {
		t.Fatalf("page %+v", page)
	}
	if _, err := dir.IdentityProviderLinkedPage(context.Background(), " ", 0, 3); err == nil {
		t.Fatal("blank alias read")
	}
}

// An error names the status, never what Keycloak's body said.
func TestKeycloakAddressReadsFail(t *testing.T) {
	t.Parallel()
	dir, _ := addressKeycloak(t, http.StatusForbidden, nil)
	_, err := dir.AddressHoldersPage(context.Background(), 0, 10)
	if err == nil || !strings.Contains(err.Error(), "403") || strings.Contains(err.Error(), "ada@") {
		t.Fatalf("holders: %v", err)
	}
	_, err = dir.IdentityProviderLinkedPage(context.Background(), "OBS", 0, 10)
	if err == nil || !strings.Contains(err.Error(), "403") || strings.Contains(err.Error(), "ada@") {
		t.Fatalf("links: %v", err)
	}
	if _, err := dir.AddressHoldersPage(context.Background(), -1, 10); err == nil {
		t.Fatal("negative first read")
	}
}

func TestMemoryAddressDirectory(t *testing.T) {
	t.Parallel()
	m := identity.NewMemory()
	a := uuid.MustParse("00000000-0000-4000-8000-000000000001")
	b := uuid.MustParse("00000000-0000-4000-8000-000000000002")
	c := uuid.MustParse("00000000-0000-4000-8000-000000000003")
	m.PutUser(identity.Person{ID: a, Email: "a@example.test", SchoolEmail: "a@std.yildiz.edu.tr"})
	m.PutUser(identity.Person{ID: b, Email: "b@example.test", Username: "service-account-forms"})
	m.PutUser(identity.Person{ID: c, Email: "c@example.test"})
	m.SetPersonalEmail(c, "c@example.org", "2026-10-01T00:00:00Z")
	m.LinkIdentityProvider("OBS", a)
	if err := m.DisableUser(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	first, err := m.AddressHoldersPage(context.Background(), 0, 2)
	if err != nil || first.Rows != 2 || first.Holders[0].ID != a || first.Holders[0].SchoolEmail != "a@std.yildiz.edu.tr" || !first.Holders[1].ServiceAccount {
		t.Fatalf("first %+v %v", first, err)
	}
	second, _ := m.AddressHoldersPage(context.Background(), 2, 2)
	if second.Rows != 1 || second.Holders[0].Enabled || second.Holders[0].PersonalEmail != "c@example.org" || second.Holders[0].PersonalEmailVerifiedAt == "" {
		t.Fatalf("second %+v", second)
	}
	linked, _ := m.IdentityProviderLinkedPage(context.Background(), "OBS", 0, 10)
	if linked.Rows != 1 || linked.IDs[0] != a {
		t.Fatalf("linked %+v", linked)
	}
	if other, _ := m.IdentityProviderLinkedPage(context.Background(), "OTHER", 0, 10); other.Rows != 0 {
		t.Fatalf("other %+v", other)
	}
}
