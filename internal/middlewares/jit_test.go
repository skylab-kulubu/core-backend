package middlewares_test

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/middlewares"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

func TestJITRejectsOldTokenForDeletionPendingAccount(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := user.NewMemoryStore()
	users := user.NewService(store)
	subjectID := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	if _, _, err := users.Ensure(ctx, subjectID, user.Profile{Email: "before@example.com", FirstName: "Before"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RequestDeletion(ctx, subjectID, nil); err != nil {
		t.Fatal(err)
	}

	app := fiber.New()
	app.Use(func(c fiber.Ctx) error {
		c.Locals(authn.LocalsIdentity, authn.Identity{
			ID:      subjectID,
			Profile: user.Profile{Email: "old-token@example.com", FirstName: "Old Token"},
		})
		return c.Next()
	})
	app.Use(middlewares.NewJIT(users).Handle)
	app.Get("/private", func(c fiber.Ctx) error { return c.SendStatus(fiber.StatusNoContent) })

	response, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/private", nil))
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != fiber.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", response.StatusCode, fiber.StatusUnauthorized)
	}
	blocked, err := store.Get(ctx, subjectID)
	if err != nil {
		t.Fatal(err)
	}
	if blocked.Email != "before@example.com" || blocked.FirstName != "Before" {
		t.Fatalf("old token repopulated blocked identity: %+v", blocked)
	}
}
