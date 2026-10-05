package identity_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/identity"
)

func TestParseKeycloakAdminURL(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ raw, want string }{
		{"", ""},
		{"   ", ""},
		{"http://sky-lab-production-keycloak-cfrcp6:8080", "http://sky-lab-production-keycloak-cfrcp6:8080"},
		{"http://keycloak:8080/", "http://keycloak:8080"},
		{"  http://keycloak:8080//  ", "http://keycloak:8080"},
		{"http://keycloak:8080/realms/e-skylab", "http://keycloak:8080"},
		{"http://keycloak:8080/realms/e-skylab/", "http://keycloak:8080"},
		{"https://kc-admin.yildizskylab.com", "https://kc-admin.yildizskylab.com"},
		{"http://keycloak:8080/auth/", "http://keycloak:8080/auth"},
	} {
		got, err := identity.ParseKeycloakAdminURL(tc.raw)
		if err != nil || got != tc.want {
			t.Errorf("ParseKeycloakAdminURL(%q) = %q, %v; want %q", tc.raw, got, err, tc.want)
		}
	}
	for _, raw := range []string{
		"keycloak:8080",
		"sky-lab-production-keycloak-cfrcp6",
		"/keycloak",
		"ftp://keycloak:8080",
		"http://",
		"http://keycloak:8080?x=1",
		"http://keycloak:8080#x",
		"http://operator:secret@keycloak:8080",
		"http://keycloak:8080/admin",
		"http://keycloak:8080/admin/",
		"http://keycloak:8080/admin/realms/e-skylab",
		"http://keycloak:8080/realms/",
		"http://key cloak:8080",
	} {
		got, err := identity.ParseKeycloakAdminURL(raw)
		if err == nil {
			t.Errorf("ParseKeycloakAdminURL(%q) = %q, want an error", raw, got)
			continue
		}
		// The value may carry credentials; the error never repeats it.
		if strings.Contains(err.Error(), "secret") || !strings.Contains(err.Error(), "KEYCLOAK_ADMIN_URL") {
			t.Errorf("ParseKeycloakAdminURL(%q) error %q: want it to name the variable and not the value", raw, err)
		}
	}
}

// Without KEYCLOAK_ADMIN_URL core does what it always did: the service
// account's token and every Admin REST call go to KEYCLOAK_URL.
func TestKeycloakAdminRESTUsesKeycloakURLWithoutAdminURL(t *testing.T) {
	t.Parallel()
	public := newRecordingKeycloak(t, "")
	internal := newRecordingKeycloak(t, "")

	dir := identity.NewKeycloak(identity.KeycloakConfig{
		URL: public.srv.URL, Realm: "e-skylab", ClientID: "core", ClientSecret: "secret",
	})
	exerciseAdminREST(t, dir)

	if got := internal.paths(); len(got) != 0 {
		t.Fatalf("the unset admin URL sent requests elsewhere: %v", got)
	}
	public.assertServedEveryFamily(t, "")
}

// With KEYCLOAK_ADMIN_URL every Admin REST call, and the service account's
// token those calls carry, goes to it; KEYCLOAK_URL (the issuer's host) gets
// nothing. The realm still comes from KEYCLOAK_URL / KEYCLOAK_REALM.
func TestKeycloakAdminRESTAndItsTokenGoToAdminURL(t *testing.T) {
	t.Parallel()
	public := newRecordingKeycloak(t, "")
	internal := newRecordingKeycloak(t, "")

	dir := identity.NewKeycloak(identity.KeycloakConfig{
		URL:          public.srv.URL + "/realms/e-skylab/",
		AdminURL:     internal.srv.URL + "/",
		ClientID:     "core",
		ClientSecret: "secret",
	})
	exerciseAdminREST(t, dir)

	if got := public.paths(); len(got) != 0 {
		t.Fatalf("KEYCLOAK_URL got requests although KEYCLOAK_ADMIN_URL is set: %v", got)
	}
	internal.assertServedEveryFamily(t, "")
}

// A Keycloak served under a context path keeps it: the path is part of the
// base, and core adds /admin/realms/<realm> and /realms/<realm> after it.
func TestKeycloakAdminURLKeepsItsContextPath(t *testing.T) {
	t.Parallel()
	public := newRecordingKeycloak(t, "")
	internal := newRecordingKeycloak(t, "/auth")

	dir := identity.NewKeycloak(identity.KeycloakConfig{
		URL: public.srv.URL, Realm: "e-skylab", AdminURL: internal.srv.URL + "/auth//",
		ClientID: "core", ClientSecret: "secret",
	})
	exerciseAdminREST(t, dir)

	if got := public.paths(); len(got) != 0 {
		t.Fatalf("KEYCLOAK_URL got requests although KEYCLOAK_ADMIN_URL is set: %v", got)
	}
	internal.assertServedEveryFamily(t, "/auth")
}

const recordedUserID = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"

// exerciseAdminREST calls at least one method of each way the adapter talks
// to Admin REST: gocloak's own calls, gocloak's raw resty requests and the
// adapter's net/http requests; reads, writes and the erasure steps.
func exerciseAdminREST(t *testing.T, dir *identity.Keycloak) {
	t.Helper()
	ctx := context.Background()
	id := uuid.MustParse(recordedUserID)
	steps := []struct {
		name string
		run  func() error
	}{
		{"ListGroups", func() error { _, err := dir.ListGroups(ctx); return err }},
		{"ListUsers", func() error { _, err := dir.ListUsers(ctx); return err }},
		{"SearchUsers", func() error { _, err := dir.SearchUsers(ctx, "ada", 5); return err }},
		{"GetUser", func() error { _, err := dir.GetUser(ctx, id); return err }},
		{"UserAddresses", func() error { _, err := dir.UserAddresses(ctx, id); return err }},
		{"GroupsForUser", func() error { _, err := dir.GroupsForUser(ctx, id); return err }},
		{"WriteSkyNumber", func() error { return dir.WriteSkyNumber(ctx, id, "SKY-1") }},
		{"MissingClientRoles", func() error { _, err := dir.MissingClientRoles(ctx, "core", []string{"event:manage"}); return err }},
		{"UsersWithClientRole", func() error { _, err := dir.UsersWithClientRole(ctx, "core", "event:manage"); return err }},
		{"LogoutAllSessions", func() error { return dir.LogoutAllSessions(ctx, id) }},
		{"DisableUser", func() error { return dir.DisableUser(ctx, id) }},
		{"DeleteUser", func() error { return dir.DeleteUser(ctx, id) }},
	}
	for _, step := range steps {
		if err := step.run(); err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
	}
}

type recordingKeycloak struct {
	srv    *httptest.Server
	prefix string

	mu       sync.Mutex
	requests []string
}

// newRecordingKeycloak is a fake Keycloak served under prefix that answers
// the calls exerciseAdminREST makes and records each request as
// "METHOD path".
func newRecordingKeycloak(t *testing.T, prefix string) *recordingKeycloak {
	t.Helper()
	kc := &recordingKeycloak{prefix: prefix}
	user := map[string]any{
		"id": recordedUserID, "username": "ada", "email": "ada@example.com", "enabled": true,
		"attributes": map[string][]string{"schoolEmail": {"ada@std.example.edu"}},
	}
	kc.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		kc.mu.Lock()
		kc.requests = append(kc.requests, r.Method+" "+r.URL.Path)
		kc.mu.Unlock()
		path, ok := strings.CutPrefix(r.URL.Path, prefix)
		if !ok {
			http.NotFound(w, r)
			return
		}
		admin := "/admin/realms/e-skylab"
		userPath := admin + "/users/" + recordedUserID
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && path == "/realms/e-skylab/protocol/openid-connect/token":
			_, _ = w.Write([]byte(`{"access_token":"tok","expires_in":300,"token_type":"Bearer"}`))
		case r.Header.Get("Authorization") != "Bearer tok":
			w.WriteHeader(http.StatusUnauthorized)
		case r.Method == http.MethodGet && (path == admin+"/groups" || path == admin+"/users" || path == userPath+"/groups"):
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodGet && path == userPath:
			_ = json.NewEncoder(w).Encode(user)
		case (r.Method == http.MethodPut || r.Method == http.MethodDelete) && path == userPath,
			r.Method == http.MethodPost && path == userPath+"/logout":
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && path == admin+"/clients":
			_, _ = w.Write([]byte(`[{"id":"core-uuid","clientId":"core"}]`))
		case r.Method == http.MethodGet && path == admin+"/clients/core-uuid/roles":
			_, _ = w.Write([]byte(`[{"name":"event:manage"}]`))
		case r.Method == http.MethodGet && (path == admin+"/clients/core-uuid/roles/event:manage/groups" ||
			path == admin+"/clients/core-uuid/roles/event:manage/users"):
			_, _ = w.Write([]byte(`[]`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(kc.srv.Close)
	return kc
}

func (kc *recordingKeycloak) paths() []string {
	kc.mu.Lock()
	defer kc.mu.Unlock()
	return append([]string(nil), kc.requests...)
}

// assertServedEveryFamily checks that the token request and the Admin REST
// families exerciseAdminREST touches all reached this server under prefix,
// and that nothing else did.
func (kc *recordingKeycloak) assertServedEveryFamily(t *testing.T, prefix string) {
	t.Helper()
	got := kc.paths()
	admin := prefix + "/admin/realms/e-skylab"
	user := admin + "/users/" + recordedUserID
	for _, want := range []string{
		"POST " + prefix + "/realms/e-skylab/protocol/openid-connect/token",
		"GET " + admin + "/groups",
		"GET " + admin + "/users",
		"GET " + user,
		"GET " + user + "/groups",
		"PUT " + user,
		"DELETE " + user,
		"POST " + user + "/logout",
		"GET " + admin + "/clients",
		"GET " + admin + "/clients/core-uuid/roles",
		"GET " + admin + "/clients/core-uuid/roles/event:manage/groups",
		"GET " + admin + "/clients/core-uuid/roles/event:manage/users",
	} {
		if !containsString(got, want) {
			t.Errorf("no %q among %v", want, got)
		}
	}
	for _, request := range got {
		_, path, _ := strings.Cut(request, " ")
		if !strings.HasPrefix(path, admin+"/") && path != prefix+"/realms/e-skylab/protocol/openid-connect/token" {
			t.Errorf("request outside Admin REST and the token endpoint: %q", request)
		}
	}
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
