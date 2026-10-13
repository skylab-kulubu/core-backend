package identity_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/identity"
)

// The audience reads a Group's direct members in full representation, page
// by page, with emailVerified and the service-account mark. Failures name a status, never a person or a Group.
func TestKeycloakAudienceMembers(t *testing.T) {
	t.Parallel()
	const members = 205 // more than one page
	ids := make([]string, members)
	for i := range ids {
		ids[i] = fmt.Sprintf("00000000-0000-4000-8000-%012d", i+1)
	}
	var pages atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/protocol/openid-connect/token"):
			_, _ = w.Write([]byte(`{"access_token":"tok","expires_in":300,"token_type":"Bearer"}`))
		case r.URL.Path == "/admin/realms/e-skylab/groups/g1/members":
			if r.URL.Query().Get("briefRepresentation") != "false" {
				http.Error(w, "brief", http.StatusBadRequest)
				return
			}
			pages.Add(1)
			first, _ := strconv.Atoi(r.URL.Query().Get("first"))
			max, _ := strconv.Atoi(r.URL.Query().Get("max"))
			var out []map[string]any
			for i := first; i < min(first+max, members); i++ {
				u := map[string]any{"id": ids[i], "username": "u" + strconv.Itoa(i), "email": "u" + strconv.Itoa(i) + "@example.test", "enabled": true, "emailVerified": i != 1}
				if i == 2 {
					u["username"] = "service-account-skymail"
					u["serviceAccountClientId"] = "skymail"
				}
				out = append(out, u)
			}
			_ = json.NewEncoder(w).Encode(out)
		case r.URL.Path == "/admin/realms/e-skylab/groups/broken/members":
			http.Error(w, "boom "+ids[4], http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	dir := identity.NewKeycloak(identity.KeycloakConfig{URL: srv.URL, Realm: "e-skylab", ClientID: "core", ClientSecret: "secret"})
	ctx := context.Background()

	people, err := dir.AudienceMembers(ctx, "g1")
	if err != nil || len(people) != members || pages.Load() != 2 {
		t.Fatalf("members %d pages %d %v", len(people), pages.Load(), err)
	}
	if !people[0].EmailVerified || !people[0].Enabled || people[0].ServiceAccount || people[0].Email != "u0@example.test" ||
		people[1].EmailVerified || !people[2].ServiceAccount {
		t.Fatalf("flags %+v %+v %+v", people[0], people[1], people[2])
	}
	if _, err := dir.AudienceMembers(ctx, "missing"); !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("missing group: %v", err)
	}
	if _, err := dir.AudienceMembers(ctx, "broken"); err == nil || strings.Contains(err.Error(), ids[4]) || strings.Contains(err.Error(), "broken") {
		t.Fatalf("broken group: %v", err)
	}
}
