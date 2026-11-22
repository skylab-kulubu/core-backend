package httpx_test

import (
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/httpx"
	"github.com/skylab-kulubu/core-backend/internal/mapregion"
	"github.com/skylab-kulubu/core-backend/internal/testauth"
)

// The map region routes read the caller from the token like every signed-in
// route (docs/event-map.md, "Regions"): a valid token reaches the service,
// which lets ADMIN ask for a region and refuses a member. Registered ahead
// of Bearer, they answered 401 to every token.
func TestMapRegionRoutesReadTheToken(t *testing.T) {
	keys := testauth.New(t)
	deps := memoryDeps()
	deps.ParseToken = keys.Parse()
	// No store: every answer below is decided before the service reads one.
	deps.MapRegions = &mapregion.Service{Authz: deps.Authz}
	app := httpx.New(deps)
	admin := groupToken(t, keys, uuid.NewString(), "/ADMIN")
	member := groupToken(t, keys, uuid.NewString(), "/UYELER/ARGE/WEBLAB")
	id := uuid.NewString()
	for _, c := range []struct {
		name, token, path, body string
		want                    int
	}{
		{"ADMIN asks with no box", admin, "/v1/map-regions", `{"name":"Kampüs"}`, fiber.StatusBadRequest},
		{"member asks", member, "/v1/map-regions", `{"name":"Kampüs","bbox":[28.88,41.02,28.9,41.03]}`, fiber.StatusForbidden},
		{"ADMIN retries no id", admin, "/v1/map-regions/x/retry", "", fiber.StatusNotFound},
		{"member retries", member, "/v1/map-regions/" + id + "/retry", "", fiber.StatusForbidden},
		{"member retires", member, "/v1/map-regions/" + id + "/retire", "", fiber.StatusForbidden},
	} {
		got := audienceCall(t, app, c.token, fiber.MethodPost, c.path, c.body)
		if got.status != c.want {
			t.Fatalf("%s: %d %s, want %d", c.name, got.status, got.raw, c.want)
		}
	}
}
