package handlers

import (
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
)

// doorStatsService answers DoorStaffStats as told.
type doorStatsService struct {
	ticket.Service
	out ticket.DoorStaffStats
	err error
}

func (s *doorStatsService) DoorStaffStats(context.Context, authz.Principal, uuid.UUID) (ticket.DoorStaffStats, error) {
	return s.out, s.err
}

// GET /v1/events/{id}/door-staff/stats answers the statistics, not
// cached; a refusal is the ticket routes' 403, a bad id 400.
func TestDoorStaffStatsRoute(t *testing.T) {
	t.Parallel()
	first := time.Date(2026, 11, 27, 16, 2, 0, 0, time.UTC)
	staff := uuid.New()
	svc := &doorStatsService{out: ticket.DoorStaffStats{Scope: ticket.DoorStatsAll, Staff: []ticket.DoorStaffRow{{
		UserID: staff, Name: "Elif Kaya", Entries: ticket.DoorEntryCounts{Scan: 60, Desk: 4, WalkIn: 12, Total: 76},
		WalkIns: 12, FirstAt: &first, LastAt: &first, Hours: []ticket.DoorHour{{Hour: first.Truncate(time.Hour), Count: 30}},
	}}, Others: &ticket.DoorStatsOthers{SelfEntries: 20}}}
	app := fiber.New(fiber.Config{ErrorHandler: ErrorHandler})
	app.Use(func(c fiber.Ctx) error {
		c.Locals(authn.LocalsIdentity, authn.Identity{ID: uuid.New()})
		return c.Next()
	})
	app.Get("/v1/events/:eventId/door-staff/stats", NewTicketHandler(svc).DoorStaffStats)
	get := func(path string) (int, string, map[string]any) {
		t.Helper()
		resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, path, nil))
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		out := map[string]any{}
		_ = json.Unmarshal(raw, &out)
		return resp.StatusCode, resp.Header.Get(fiber.HeaderCacheControl), out
	}
	status, cache, body := get("/v1/events/" + uuid.NewString() + "/door-staff/stats")
	rows, _ := body["staff"].([]any)
	if status != fiber.StatusOK || cache != "no-store" || body["scope"] != "all" || len(rows) != 1 {
		t.Fatalf("%d %q %v", status, cache, body)
	}
	row, _ := rows[0].(map[string]any)
	entries, _ := row["entries"].(map[string]any)
	if row["name"] != "Elif Kaya" || entries["walkIn"] != float64(12) || row["walkIns"] != float64(12) || row["email"] != nil {
		t.Fatalf("row %v", row)
	}
	svc.err = ticket.ErrForbidden
	if status, _, _ := get("/v1/events/" + uuid.NewString() + "/door-staff/stats"); status != fiber.StatusForbidden {
		t.Fatalf("refused: %d", status)
	}
	if status, _, _ := get("/v1/events/nope/door-staff/stats"); status != fiber.StatusBadRequest {
		t.Fatalf("bad id: %d", status)
	}
}
