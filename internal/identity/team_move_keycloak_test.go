package identity

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// MoveGroup is Keycloak's own move: the existing Group's id and name to
// the new parent's child endpoint (the realm's top: POST /groups), then the
// Group read again. Ids only.
func TestMoveGroupKeycloakPostsTheExistingGroup(t *testing.T) {
	id, parent := uuid.NewString(), uuid.NewString()
	var posts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/protocol/openid-connect/token"):
			_, _ = w.Write([]byte(`{"access_token":"local-test-token","expires_in":300}`))
		case r.Method == http.MethodGet && r.URL.Path == "/admin/realms/test/groups/"+id:
			_, _ = w.Write([]byte(`{"id":"` + id + `","name":"FRONT","path":"/UYELER/ARGE/MOBILAB/FRONT"}`))
		case r.Method == http.MethodPost && (r.URL.Path == "/admin/realms/test/groups/"+parent+"/children" || r.URL.Path == "/admin/realms/test/groups"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["id"] != id || body["name"] != "FRONT" || len(body) != 2 {
				t.Errorf("body %v", body)
			}
			posts = append(posts, r.URL.Path)
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer srv.Close()
	dir := NewKeycloak(KeycloakConfig{URL: srv.URL, Realm: "test", ClientID: "core", ClientSecret: "local-fixture"})
	g, err := dir.MoveGroup(context.Background(), id, parent)
	if err != nil || g.Path != "/UYELER/ARGE/MOBILAB/FRONT" {
		t.Fatalf("%+v %v", g, err)
	}
	if _, err := dir.MoveGroup(context.Background(), id, ""); err != nil {
		t.Fatal(err)
	}
	if strings.Join(posts, ",") != "/admin/realms/test/groups/"+parent+"/children,/admin/realms/test/groups" {
		t.Fatalf("posts %v", posts)
	}
	if _, err := dir.MoveGroup(context.Background(), id, "/UYELER/ARGE"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a parent by path: %v", err)
	}
}
