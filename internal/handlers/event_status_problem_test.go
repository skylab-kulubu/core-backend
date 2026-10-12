package handlers

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/skylab-kulubu/core-backend/internal/certificate"
	"github.com/skylab-kulubu/core-backend/internal/competitor"
	"github.com/skylab-kulubu/core-backend/internal/event"
)

// What a draft or a cancelled Event refuses keeps its HTTP status and names
// why in the problem's code (event_not_published, event_cancelled), which
// the panel turns into its own text.
func TestEventStatusRefusalsCarryTheirProblemCode(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		answer func(c fiber.Ctx) error
		status int
		code   string
	}{
		{"finalize on a draft", func(c fiber.Ctx) error {
			return certError(c, fmt.Errorf("%w: %w", certificate.ErrConflict, event.ErrNotPublished))
		}, fiber.StatusConflict, "event_not_published"},
		{"finalize on a cancelled event", func(c fiber.Ctx) error {
			return certError(c, fmt.Errorf("%w: %w", certificate.ErrConflict, event.ErrCancelled))
		}, fiber.StatusConflict, "event_cancelled"},
		{"finalize while running", func(c fiber.Ctx) error {
			return certError(c, certificate.ErrConflict)
		}, fiber.StatusConflict, ""},
		{"competitor on a draft", func(c fiber.Ctx) error {
			return competitorError(c, fmt.Errorf("%w: %w", competitor.ErrInvalid, event.ErrNotPublished))
		}, fiber.StatusBadRequest, "event_not_published"},
		{"competitor reinstated on a cancelled event", func(c fiber.Ctx) error {
			return competitorError(c, fmt.Errorf("%w: %w", competitor.ErrConflict, event.ErrCancelled))
		}, fiber.StatusConflict, "event_cancelled"},
	}
	for _, tc := range cases {
		app := fiber.New()
		app.Get("/", tc.answer)
		resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/", nil))
		if err != nil {
			t.Fatal(err)
		}
		var body map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		code, _ := body["code"].(string)
		if resp.StatusCode != tc.status || code != tc.code {
			t.Fatalf("%s: %d %v, want %d %q", tc.name, resp.StatusCode, body, tc.status, tc.code)
		}
	}
}
