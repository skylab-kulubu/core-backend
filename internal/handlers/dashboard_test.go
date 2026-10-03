package handlers

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/dashboard"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

func dashboardApp(t *testing.T, ident *authn.Identity, events event.Store, tickets ticket.Store) *fiber.App {
	t.Helper()
	svc, err := dashboard.NewService(dashboard.Options{
		Events: events, Store: dashboard.NewMemoryStore(tickets, user.NewMemoryStore()),
		Authz: authz.NewAuthorizer(authz.DefaultPolicy()), Directory: identity.NewMemory(),
	})
	if err != nil {
		t.Fatal(err)
	}
	app := fiber.New(fiber.Config{ErrorHandler: ErrorHandler})
	app.Use(func(c fiber.Ctx) error {
		if ident != nil {
			c.Locals(authn.LocalsIdentity, *ident)
		}
		return c.Next()
	})
	app.Get("/v1/dashboard/summary", NewDashboardHandler(svc).Summary)
	return app
}

func TestDashboardSummaryHTTP(t *testing.T) {
	t.Parallel()
	events := event.NewMemoryStore()
	tickets := ticket.NewMemoryStore()
	start := time.Now().Add(48 * time.Hour)
	mine, err := events.Create(t.Context(), event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB", StartDate: &start, Capacity: 10})
	if err != nil {
		t.Fatal(err)
	}
	theirs, err := events.Create(t.Context(), event.Event{Name: "Gece", Location: "YTÜ", OwnerTeam: "GECEKODU", StartDate: &start})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range []event.Event{mine, mine, theirs} {
		if _, err := tickets.Create(t.Context(), ticket.Ticket{EventID: e.ID, TicketType: ticket.Guest}); err != nil {
			t.Fatal(err)
		}
	}

	resp, err := dashboardApp(t, nil, events, tickets).Test(httptest.NewRequest(fiber.MethodGet, "/v1/dashboard/summary", nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusUnauthorized {
		t.Fatalf("anonymous: %d", resp.StatusCode)
	}

	leader := &authn.Identity{ID: uuid.New(), Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}
	resp, err = dashboardApp(t, leader, events, tickets).Test(httptest.NewRequest(fiber.MethodGet, "/v1/dashboard/summary", nil))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}
	if got := resp.Header.Get(fiber.HeaderCacheControl); got != "no-store" {
		t.Fatalf("cache-control %q", got)
	}
	var got dashboard.Summary
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.Events.Total != 1 || got.Applications.Total != 2 || got.Applications.Guests != 2 || len(got.EventStats) != 1 || got.EventStats[0].ID != mine.ID {
		t.Fatalf("summary %s", body)
	}
	if got.Applications.Daily[len(got.Applications.Daily)-1].Count != 2 || got.Members != nil {
		t.Fatalf("summary %s", body)
	}
}
