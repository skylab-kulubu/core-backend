package identity

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGroupDeleteKeycloakUsesExactDeleteEndpoint(t *testing.T) {
	var deletes []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/protocol/openid-connect/token"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"local-test-token","expires_in":300}`))
		case r.Method == http.MethodDelete:
			deletes = append(deletes, r.URL.Path)
			if strings.HasSuffix(r.URL.Path, "/missing") {
				w.WriteHeader(http.StatusNotFound)
			} else {
				w.WriteHeader(http.StatusNoContent)
			}
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer srv.Close()
	dir := NewKeycloak(KeycloakConfig{URL: srv.URL, Realm: "test", ClientID: "core", ClientSecret: "local-fixture"})
	if err := dir.DeleteGroup(context.Background(), "group-id"); err != nil {
		t.Fatal(err)
	}
	if err := dir.DeleteGroup(context.Background(), "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing group: %v", err)
	}
	if strings.Join(deletes, ",") != "/admin/realms/test/groups/group-id,/admin/realms/test/groups/missing" {
		t.Fatalf("delete requests: %v", deletes)
	}
}
