package handlers

import (
	"context"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/mapregion"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
)

// POST /v1/map-regions answers 202 with the region for a manager, 409 for
// a box a live region has, 400 for a body that is not a request, 403 for
// anyone else and 401 signed out; GET lists them.
func TestPostgresMapRegionRoutes(t *testing.T) {
	pool := testpostgres.Start(t)
	if err := migrate.Apply(context.Background(), pool); err != nil {
		t.Fatal(err)
	}
	h := NewMapRegionHandler(&mapregion.Service{Store: mapregion.NewPostgresStore(pool), Authz: authz.NewAuthorizer(authz.DefaultPolicy()),
		Address: func(key string) string { return "https://cdn.example.test/" + key }})
	app := func(ident authn.Identity) *fiber.App {
		a := fiber.New()
		a.Use(func(c fiber.Ctx) error {
			if ident.ID != uuid.Nil {
				c.Locals(authn.LocalsIdentity, ident)
			}
			return c.Next()
		})
		a.Get("/v1/map-regions", h.List)
		a.Post("/v1/map-regions", h.Request)
		a.Post("/v1/map-regions/:id/retry", h.Retry)
		a.Post("/v1/map-regions/:id/retire", h.Retire)
		return a
	}
	send := func(a *fiber.App, method, path, body string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := a.Test(req)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(raw)
	}
	manager := app(yk())
	istanbul := `{"name":"İstanbul","bbox":[27.95,40.80,29.95,41.60]}`
	if status, body := send(manager, fiber.MethodPost, "/v1/map-regions", istanbul); status != fiber.StatusAccepted || !strings.Contains(body, `"status":"requested"`) {
		t.Fatalf("request %d %s", status, body)
	}
	for _, tc := range []struct {
		a      *fiber.App
		method string
		path   string
		body   string
		status int
		code   string
	}{
		{manager, fiber.MethodPost, "/v1/map-regions", istanbul, fiber.StatusConflict, "map_region_exists"},
		{manager, fiber.MethodPost, "/v1/map-regions", `{"name":"x","bbox":[29,41,28]}`, fiber.StatusBadRequest, "map_region_invalid"},
		{manager, fiber.MethodPost, "/v1/map-regions", `{"name":"x","bbox":[27,40,28,41],"maxZoom":16}`, fiber.StatusBadRequest, "map_region_invalid"},
		{app(weblabLeader()), fiber.MethodPost, "/v1/map-regions", `{"name":"x","bbox":[27,40,28,41]}`, fiber.StatusForbidden, ""},
		{app(authn.Identity{}), fiber.MethodGet, "/v1/map-regions", "", fiber.StatusUnauthorized, ""},
		{manager, fiber.MethodPost, "/v1/map-regions/" + uuid.NewString() + "/retry", "", fiber.StatusNotFound, ""},
	} {
		status, body := send(tc.a, tc.method, tc.path, tc.body)
		if status != tc.status || (tc.code != "" && !strings.Contains(body, tc.code)) {
			t.Errorf("%s %s: %d %s", tc.method, tc.path, status, body)
		}
	}
	if status, body := send(manager, fiber.MethodGet, "/v1/map-regions", ""); status != fiber.StatusOK || !strings.Contains(body, `"name":"İstanbul"`) {
		t.Fatalf("list %d %s", status, body)
	}
	if status, body := send(app(weblabLeader()), fiber.MethodGet, "/v1/map-regions", ""); status != fiber.StatusOK || body != "[]" {
		t.Fatalf("a leader's list %d %s", status, body)
	}
}
