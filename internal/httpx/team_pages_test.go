package httpx_test

import (
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/skylab-kulubu/core-backend/internal/httpx"
	"github.com/skylab-kulubu/core-backend/internal/identity"
)

// The public team routes share one budget per client address
// (perClientLimit): their reads reach Keycloak.
func TestPublicTeamRoutesShareAPerClientBudget(t *testing.T) {
	t.Parallel()
	dir := identity.NewMemory()
	dir.PutGroup(identity.Group{ID: "g-weblab", Name: "WEBLAB", Path: "/UYELER/ARGE/WEBLAB", Attributes: map[string]string{"public_listing": "true"}})
	app := httpx.New(memoryDepsOver(dir, identity.Options{}))
	paths := []string{"/v1/teams", "/v1/teams/WEBLAB/members", "/v1/teams/WEBLAB/leaders"}
	limited := -1
	for n := range 130 {
		resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, paths[n%len(paths)], nil))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode == fiber.StatusTooManyRequests {
			limited = n
			break
		}
		if resp.StatusCode != fiber.StatusOK {
			t.Fatalf("%s: %d", paths[n%len(paths)], resp.StatusCode)
		}
	}
	if limited != 120 {
		t.Fatalf("limited at request %d, want 120", limited)
	}
}
