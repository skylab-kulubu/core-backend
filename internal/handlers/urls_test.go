package handlers

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/shorturl"
)

func urlApp(t *testing.T, ident authn.Identity) *fiber.App {
	t.Helper()
	h := NewURLHandler(shorturl.NewService(shorturl.NewMemoryStore(), authz.NewAuthorizer(authz.DefaultPolicy())))
	app := fiber.New()
	app.Use(func(c fiber.Ctx) error {
		if ident.ID != uuid.Nil || len(ident.Roles) > 0 || len(ident.Groups) > 0 {
			c.Locals(authn.LocalsIdentity, ident)
		}
		return c.Next()
	})
	app.Get("/v1/go/:alias", h.Redirect)
	app.Post("/v1/urls", h.Create)
	app.Get("/v1/urls", h.ListMine)
	app.Get("/v1/urls/all", h.ListAll)
	app.Patch("/v1/urls/:id", h.Update)
	app.Delete("/v1/urls/:id", h.Delete)
	return app
}

func TestURLCreateRedirectAndListHTTP(t *testing.T) {
	t.Parallel()
	uid := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	app := urlApp(t, authn.Identity{ID: uid, Roles: []string{"skylapp:access"}})
	req := httptest.NewRequest(fiber.MethodPost, "/v1/urls", strings.NewReader(`{"url":"https://skylab.com","alias":"club"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d body %s", resp.StatusCode, b)
	}
	redir, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/v1/go/club", nil))
	if err != nil {
		t.Fatal(err)
	}
	if redir.StatusCode != fiber.StatusMovedPermanently {
		t.Fatalf("redirect %d", redir.StatusCode)
	}
	if loc := redir.Header.Get("Location"); loc != "https://skylab.com" {
		t.Fatalf("location %s", loc)
	}
	list, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/v1/urls", nil))
	if err != nil {
		t.Fatal(err)
	}
	if list.StatusCode != fiber.StatusOK {
		t.Fatalf("list %d", list.StatusCode)
	}
	var items []shorturl.URL
	if err := json.NewDecoder(list.Body).Decode(&items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].ClickCount != 1 {
		t.Fatalf("items %+v", items)
	}
}

func TestURLListAllForbiddenForMember(t *testing.T) {
	t.Parallel()
	uid := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	app := urlApp(t, authn.Identity{ID: uid, Roles: []string{"url:create"}})
	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/v1/urls/all", nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusForbidden {
		t.Fatalf("status %d", resp.StatusCode)
	}
}
