package identity_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/identity"
)

// An app assignment adds or removes one role on a Group and touches no
// other mapping; the list reads exactly the role asked, not skyforms:*.
func TestKeycloakAppRoleWritesTouchOneRole(t *testing.T) {
	t.Parallel()
	const group = "11111111-1111-1111-1111-111111111111"
	var mu sync.Mutex
	var writes, reads []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		base := "/admin/realms/e-skylab"
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/protocol/openid-connect/token"):
			_, _ = w.Write([]byte(`{"access_token":"tok","expires_in":300,"token_type":"Bearer"}`))
		case r.URL.Path == base+"/clients" && r.URL.Query().Get("clientId") == "forms":
			_ = json.NewEncoder(w).Encode([]any{map[string]any{"id": "forms-uuid", "clientId": "forms"}})
		case r.URL.Path == base+"/clients":
			_ = json.NewEncoder(w).Encode([]any{})
		case r.URL.Path == base+"/clients/forms-uuid/roles" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode([]any{
				map[string]any{"id": "seat-id", "name": "skyforms:access"},
				map[string]any{"id": "all-id", "name": "skyforms:*"},
			})
		case r.URL.Path == base+"/groups/"+group && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"id": group, "name": "WEBLAB", "path": "/UYELER/ARGE/WEBLAB"})
		case r.URL.Path == base+"/groups/"+group+"/role-mappings/clients/forms-uuid" && (r.Method == http.MethodPost || r.Method == http.MethodDelete):
			body, _ := io.ReadAll(r.Body)
			mu.Lock()
			writes = append(writes, r.Method+" "+strings.TrimSpace(string(body)))
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		case strings.HasPrefix(r.URL.Path, base+"/clients/forms-uuid/roles/") && r.Method == http.MethodGet:
			mu.Lock()
			reads = append(reads, strings.TrimPrefix(r.URL.Path, base+"/clients/forms-uuid/roles/"))
			mu.Unlock()
			if strings.HasSuffix(r.URL.Path, "/groups") {
				_ = json.NewEncoder(w).Encode([]any{map[string]any{"id": group, "name": "WEBLAB", "path": "/UYELER/ARGE/WEBLAB"}})
				return
			}
			_ = json.NewEncoder(w).Encode([]any{})
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	dir := identity.NewKeycloak(identity.KeycloakConfig{URL: srv.URL, Realm: "e-skylab", ClientID: "core", ClientSecret: "secret"})
	ctx := context.Background()
	seat := identity.ClientRole{ClientID: "forms", Role: "skyforms:access"}
	if err := dir.AddGroupClientRole(ctx, group, seat); err != nil {
		t.Fatal(err)
	}
	if err := dir.RemoveGroupClientRole(ctx, group, seat); err != nil {
		t.Fatal(err)
	}
	if err := dir.AddGroupClientRole(ctx, group, identity.ClientRole{ClientID: identity.KeycloakAdminClient, Role: "manage-users"}); !errors.Is(err, identity.ErrForbidden) {
		t.Errorf("realm-management: %v", err)
	}
	if len(writes) != 2 || !strings.HasPrefix(writes[0], "POST ") || !strings.HasPrefix(writes[1], "DELETE ") {
		t.Fatalf("writes %v", writes)
	}
	for _, w := range writes {
		if !strings.Contains(w, `"skyforms:access"`) || strings.Contains(w, `skyforms:*`) {
			t.Errorf("write %s", w)
		}
	}
	groups, people, err := dir.ClientRoleMappings(ctx, "forms", "skyforms:access")
	if err != nil || len(groups) != 1 || groups[0].Path != "/UYELER/ARGE/WEBLAB" || len(people) != 0 {
		t.Fatalf("mappings %v %v %v", groups, people, err)
	}
	if strings.Join(reads, ",") != "skyforms:access/groups,skyforms:access/users" {
		t.Errorf("reads %v", reads)
	}
	// A realm without the client maps the role to nobody.
	groups, people, err = dir.ClientRoleMappings(ctx, "skymail", "skymail:access")
	if err != nil || len(groups)+len(people) != 0 {
		t.Errorf("no client: %v %v %v", groups, people, err)
	}
}
