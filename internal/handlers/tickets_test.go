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
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

func ticketApp(t *testing.T, ident authn.Identity, events event.Store, tickets ticket.Store) *fiber.App {
	t.Helper()
	svc := ticket.NewService(tickets, events, authz.NewAuthorizer(authz.DefaultPolicy()))
	h := NewTicketHandler(svc)
	app := fiber.New()
	app.Use(func(c fiber.Ctx) error {
		if ident.ID != uuid.Nil || len(ident.Groups) > 0 {
			c.Locals(authn.LocalsIdentity, ident)
		}
		return c.Next()
	})
	app.Post("/v1/events/:eventId/applications/me", h.Apply)
	app.Post("/v1/events/:eventId/applications/guest", h.ApplyGuest)
	app.Get("/v1/events/:eventId/tickets", h.ListByEvent)
	app.Get("/v1/tickets/me", h.Mine)
	app.Post("/v1/tickets/:ticketId/event-days/:eventDayId/check-in", h.CheckIn)
	return app
}

func TestApplyAndListMineHTTP(t *testing.T) {
	t.Parallel()
	events := event.NewMemoryStore()
	tickets := ticket.NewMemoryStore()
	ev, err := events.Create(t.Context(), event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB"})
	if err != nil {
		t.Fatal(err)
	}
	ident := authn.Identity{
		ID:      uuid.MustParse("11111111-1111-1111-1111-111111111111"),
		Profile: user.Profile{Email: "ada@example.com"},
	}
	app := ticketApp(t, ident, events, tickets)

	req := httptest.NewRequest(fiber.MethodPost, "/v1/events/"+ev.ID.String()+"/applications/me", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("apply status %d body %s", resp.StatusCode, body)
	}

	resp, err = app.Test(httptest.NewRequest(fiber.MethodGet, "/v1/tickets/me", nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("mine status %d", resp.StatusCode)
	}
	var mine []ticket.Ticket
	if err := json.NewDecoder(resp.Body).Decode(&mine); err != nil {
		t.Fatal(err)
	}
	if len(mine) != 1 || mine[0].EventID != ev.ID {
		t.Fatalf("mine %+v", mine)
	}
}

func TestGuestApplyHTTP(t *testing.T) {
	t.Parallel()
	events := event.NewMemoryStore()
	ev, err := events.Create(t.Context(), event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB"})
	if err != nil {
		t.Fatal(err)
	}
	app := ticketApp(t, authn.Identity{}, events, ticket.NewMemoryStore())
	req := httptest.NewRequest(fiber.MethodPost, "/v1/events/"+ev.ID.String()+"/applications/guest", strings.NewReader(
		`{"firstName":"Ada","lastName":"Lovelace","email":"ada@example.com","phoneNumber":"555"}`,
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
}

func TestCheckInHTTPForbiddenAndOK(t *testing.T) {
	t.Parallel()
	events := event.NewMemoryStore()
	tickets := ticket.NewMemoryStore()
	ev, err := events.Create(t.Context(), event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB"})
	if err != nil {
		t.Fatal(err)
	}
	day, err := events.CreateDay(t.Context(), event.Day{EventID: ev.ID, Name: "Day 1"})
	if err != nil {
		t.Fatal(err)
	}
	owner := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	tk, err := tickets.Create(t.Context(), ticket.Ticket{EventID: ev.ID, TicketType: ticket.Registered, OwnerID: &owner})
	if err != nil {
		t.Fatal(err)
	}

	member := authn.Identity{ID: uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"), Groups: []string{"/UYELER/ARGE/WEBLAB"}}
	app := ticketApp(t, member, events, tickets)
	path := "/v1/tickets/" + tk.ID.String() + "/event-days/" + day.ID.String() + "/check-in"
	resp, err := app.Test(httptest.NewRequest(fiber.MethodPost, path, nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusForbidden {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("member status %d body %s", resp.StatusCode, body)
	}

	leader := authn.Identity{ID: uuid.MustParse("cccccccc-cccc-cccc-cccc-cccccccccccc"), Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}
	app = ticketApp(t, leader, events, tickets)
	resp, err = app.Test(httptest.NewRequest(fiber.MethodPost, path, nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("leader status %d body %s", resp.StatusCode, body)
	}
}

func TestListEventTicketsHTTP(t *testing.T) {
	t.Parallel()
	events := event.NewMemoryStore()
	tickets := ticket.NewMemoryStore()
	ev, err := events.Create(t.Context(), event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB"})
	if err != nil {
		t.Fatal(err)
	}
	applicant := authn.Identity{ID: uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")}
	app := ticketApp(t, applicant, events, tickets)
	applyReq := httptest.NewRequest(fiber.MethodPost, "/v1/events/"+ev.ID.String()+"/applications/me", nil)
	resp, err := app.Test(applyReq)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("apply status %d body %s", resp.StatusCode, body)
	}

	member := authn.Identity{ID: uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"), Groups: []string{"/UYELER/ARGE/WEBLAB"}}
	app = ticketApp(t, member, events, tickets)
	listPath := "/v1/events/" + ev.ID.String() + "/tickets"
	resp, err = app.Test(httptest.NewRequest(fiber.MethodGet, listPath, nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusForbidden {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("member status %d body %s", resp.StatusCode, body)
	}

	leader := authn.Identity{ID: uuid.MustParse("cccccccc-cccc-cccc-cccc-cccccccccccc"), Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}
	app = ticketApp(t, leader, events, tickets)
	resp, err = app.Test(httptest.NewRequest(fiber.MethodGet, listPath, nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("leader status %d body %s", resp.StatusCode, body)
	}
	var listed []ticket.Ticket
	if err := json.NewDecoder(resp.Body).Decode(&listed); err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].EventID != ev.ID {
		t.Fatalf("listed %+v", listed)
	}
}
