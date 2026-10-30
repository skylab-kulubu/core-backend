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

// CreateSubgroup is one POST /groups/{parent}/children with the name and
// the attributes, then the new Group read by the id Keycloak answered; a
// parent that is no id is never looked up.
func TestCreateSubgroupKeycloakWritesNameAndAttributesOnce(t *testing.T) {
	parent, child := uuid.NewString(), uuid.NewString()
	var posts []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/protocol/openid-connect/token"):
			_, _ = w.Write([]byte(`{"access_token":"local-test-token","expires_in":300}`))
		case r.Method == http.MethodPost && r.URL.Path == "/admin/realms/test/groups/"+parent+"/children":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			posts = append(posts, body)
			w.Header().Set("Location", "http://"+r.Host+"/admin/realms/test/groups/"+child)
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodGet && r.URL.Path == "/admin/realms/test/groups/"+child:
			_, _ = w.Write([]byte(`{"id":"` + child + `","name":"BACK","path":"/UYELER/ARGE/WEBLAB/BACK","attributes":{"display_name_tr":["Arka"]}}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer srv.Close()
	dir := NewKeycloak(KeycloakConfig{URL: srv.URL, Realm: "test", ClientID: "core", ClientSecret: "local-fixture"})
	g, err := dir.CreateSubgroup(context.Background(), parent, "BACK", map[string]string{"display_name_tr": "Arka"})
	if err != nil {
		t.Fatal(err)
	}
	if g.ID != child || g.Path != "/UYELER/ARGE/WEBLAB/BACK" || g.Attributes["display_name_tr"] != "Arka" {
		t.Fatalf("group %+v", g)
	}
	if len(posts) != 1 || posts[0]["name"] != "BACK" {
		t.Fatalf("posts %v", posts)
	}
	attrs, _ := posts[0]["attributes"].(map[string]any)
	if values, _ := attrs["display_name_tr"].([]any); len(values) != 1 || values[0] != "Arka" {
		t.Fatalf("attributes %v", posts[0]["attributes"])
	}
	if _, err := dir.CreateSubgroup(context.Background(), "/UYELER/ARGE/WEBLAB", "BACK", nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a parent by path: %v", err)
	}
}
