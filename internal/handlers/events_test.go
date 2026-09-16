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
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

func eventApp(t *testing.T, ident authn.Identity, store event.Store) *fiber.App {
	t.Helper()
	svc := event.NewService(store, authz.NewAuthorizer(authz.DefaultPolicy()))
	h := NewEventHandler(svc)
	app := fiber.New()
	app.Use(func(c fiber.Ctx) error {
		if ident.ID != uuid.Nil || len(ident.Groups) > 0 {
			c.Locals(authn.LocalsIdentity, ident)
		}
		return c.Next()
	})
	app.Get("/v1/events", h.List)
	app.Get("/v1/events/:id", h.Get)
	app.Post("/v1/events", h.Create)
	app.Put("/v1/events/:id", h.Update)
	app.Delete("/v1/events/:id", h.Delete)
	return app
}

func weblabLeader() authn.Identity {
	return authn.Identity{
		ID:      uuid.MustParse("11111111-1111-1111-1111-111111111111"),
		Profile: user.Profile{Email: "lead@example.com"},
		Groups:  []string{"/UYELER/ARGE/WEBLAB/LIDERLER"},
	}
}

func TestEventCreateReadablePublic(t *testing.T) {
	t.Parallel()
	store := event.NewMemoryStore()
	app := eventApp(t, weblabLeader(), store)

	req := httptest.NewRequest(fiber.MethodPost, "/v1/events", strings.NewReader(
		`{"name":"Hack","location":"YTÜ","ownerTeam":"WEBLAB","active":true}`,
	))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}
	var created event.Event
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}

	public := eventApp(t, authn.Identity{}, store)
	resp, err = public.Test(httptest.NewRequest(fiber.MethodGet, "/v1/events/"+created.ID.String(), nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("get status %d", resp.StatusCode)
	}
	var got event.Event
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Name != "Hack" || got.OwnerTeam != "WEBLAB" {
		t.Fatalf("got %+v", got)
	}

	resp, err = public.Test(httptest.NewRequest(fiber.MethodGet, "/v1/events", nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("list status %d", resp.StatusCode)
	}
}

func TestEventWrongTeamProblemJSON(t *testing.T) {
	t.Parallel()
	app := eventApp(t, weblabLeader(), event.NewMemoryStore())
	req := httptest.NewRequest(fiber.MethodPost, "/v1/events", strings.NewReader(
		`{"name":"CTF","location":"YTÜ","ownerTeam":"SKYSEC"}`,
	))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusForbidden {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "application/problem+json") {
		t.Fatalf("content-type %s", ct)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"status":403`) {
		t.Fatalf("body %s", body)
	}
}

func TestEventCreateUnauthorized(t *testing.T) {
	t.Parallel()
	app := eventApp(t, authn.Identity{}, event.NewMemoryStore())
	req := httptest.NewRequest(fiber.MethodPost, "/v1/events", strings.NewReader(
		`{"name":"Hack","location":"YTÜ","ownerTeam":"WEBLAB"}`,
	))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusUnauthorized {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestEventCreateCoverImage(t *testing.T) {
	t.Parallel()
	store := event.NewMemoryStore()
	app := eventApp(t, weblabLeader(), store)
	cover := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	req := httptest.NewRequest(fiber.MethodPost, "/v1/events", strings.NewReader(
		`{"name":"Hack","location":"YTÜ","ownerTeam":"WEBLAB","coverImageId":"`+cover.String()+`"}`,
	))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}
	var created event.Event
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	if created.CoverImageID == nil || *created.CoverImageID != cover {
		t.Fatalf("cover %+v", created.CoverImageID)
	}

	public := eventApp(t, authn.Identity{}, store)
	resp, err = public.Test(httptest.NewRequest(fiber.MethodGet, "/v1/events/"+created.ID.String(), nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("get status %d", resp.StatusCode)
	}
	var got event.Event
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.CoverImageID == nil || *got.CoverImageID != cover {
		t.Fatalf("public cover %+v", got.CoverImageID)
	}
}
