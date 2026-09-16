package handlers

import (
	"context"
	"io"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/middlewares"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

func testApp(t *testing.T, store user.Store) *fiber.App {
	t.Helper()
	svc := user.NewService(store)
	jit := middlewares.NewJIT(svc)
	me := NewMeHandler()

	app := fiber.New()
	app.Get("/v1/health", func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusNoContent)
	})
	app.Get("/v1/users/me", jit.Handle, me.GetMe)
	return app
}

func TestGetMeUnauthorizedWithoutIdentity(t *testing.T) {
	t.Parallel()
	store := user.NewMemoryStore()
	app := testApp(t, store)

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/v1/users/me", nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusUnauthorized {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestHealthDoesNotCreateUser(t *testing.T) {
	t.Parallel()
	store := user.NewMemoryStore()
	app := testApp(t, store)

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/v1/health", nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusNoContent {
		t.Fatalf("status %d", resp.StatusCode)
	}

	id := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	_, err = store.Get(t.Context(), id)
	if err != user.ErrNotFound {
		t.Fatalf("expected no user, got %v", err)
	}
}

func TestGetMeUpsertsIdentity(t *testing.T) {
	t.Parallel()
	store := user.NewMemoryStore()
	svc := user.NewService(store)
	jit := middlewares.NewJIT(svc)
	me := NewMeHandler()
	id := uuid.MustParse("44444444-4444-4444-4444-444444444444")

	app := fiber.New()
	app.Use(func(c fiber.Ctx) error {
		c.Locals(authn.LocalsIdentity, authn.Identity{
			ID: id,
			Profile: user.Profile{
				Email:     "ada@example.com",
				FirstName: "Ada",
				LastName:  "Lovelace",
			},
		})
		return c.Next()
	})
	app.Get("/v1/users/me", jit.Handle, me.GetMe)

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/v1/users/me", nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}

	got, err := store.Get(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Email != "ada@example.com" {
		t.Fatalf("got %+v", got)
	}
}

type recMail struct {
	n int
}

func (r *recMail) Welcome(_ context.Context, _ user.User) {
	r.n++
}

func TestGetMeWelcomeOnlyOnCreate(t *testing.T) {
	t.Parallel()
	store := user.NewMemoryStore()
	svc := user.NewService(store)
	rec := &recMail{}
	jit := middlewares.NewJIT(svc, rec)
	me := NewMeHandler()
	id := uuid.MustParse("55555555-5555-5555-5555-555555555555")

	app := fiber.New()
	app.Use(func(c fiber.Ctx) error {
		c.Locals(authn.LocalsIdentity, authn.Identity{
			ID:      id,
			Profile: user.Profile{Email: "ada@example.com", FirstName: "Ada", LastName: "Lovelace"},
		})
		return c.Next()
	})
	app.Get("/v1/users/me", jit.Handle, me.GetMe)

	for i := 0; i < 2; i++ {
		resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/v1/users/me", nil))
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != fiber.StatusOK {
			t.Fatalf("status %d", resp.StatusCode)
		}
	}
	if rec.n != 1 {
		t.Fatalf("welcome count %d", rec.n)
	}
}
