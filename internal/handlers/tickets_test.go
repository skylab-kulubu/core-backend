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

func ticketApp(t *testing.T, ident authn.Identity, events event.Store, tickets ticket.Store, users ...user.Store) *fiber.App {
	t.Helper()
	svc := ticket.NewService(tickets, events, authz.NewAuthorizer(authz.DefaultPolicy()), users...)
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
	app.Get("/v1/tickets/user/:userId/event/:eventId", h.ByUserEvent)
	app.Get("/v1/tickets/:id", h.Get)
	app.Get("/v1/tickets", h.List)
	app.Post("/v1/tickets/:ticketId/sessions/:sessionId/check-in", h.CheckIn)
	app.Post("/v1/sessions/:sessionId/check-in/me", h.CheckInMe)
	app.Post("/v1/sessions/:sessionId/check-in/guest", h.CheckInGuest)
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
	if mine[0].Event == nil || mine[0].Event.Name != "Hack" || mine[0].Event.Location != "YTÜ" {
		t.Fatalf("embedded event %+v", mine[0].Event)
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
	sess, err := events.CreateSession(t.Context(), event.Session{
		EventDayID: day.ID, Title: "Opening", SpeakerName: "Ada", SessionType: "PRESENTATION",
	})
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
	path := "/v1/tickets/" + tk.ID.String() + "/sessions/" + sess.ID.String() + "/check-in"
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
	var created ticket.CheckIn
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	if created.SessionID != sess.ID || created.TicketID != tk.ID {
		t.Fatalf("created %+v", created)
	}

	resp, err = app.Test(httptest.NewRequest(fiber.MethodPost, path, nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusConflict {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("dup status %d body %s", resp.StatusCode, body)
	}
}

func TestCheckInMeAndGuestHTTP(t *testing.T) {
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
	sess, err := events.CreateSession(t.Context(), event.Session{
		EventDayID: day.ID, Title: "Opening", SpeakerName: "Ada", SessionType: "PRESENTATION",
	})
	if err != nil {
		t.Fatal(err)
	}
	owner := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	if _, err := tickets.Create(t.Context(), ticket.Ticket{EventID: ev.ID, TicketType: ticket.Registered, OwnerID: &owner}); err != nil {
		t.Fatal(err)
	}
	if _, err := tickets.Create(t.Context(), ticket.Ticket{
		EventID: ev.ID, TicketType: ticket.Guest, GuestEmail: "ada@example.com", GuestFirstName: "Ada", GuestLastName: "Lovelace",
	}); err != nil {
		t.Fatal(err)
	}

	ownerApp := ticketApp(t, authn.Identity{ID: owner}, events, tickets)
	mePath := "/v1/sessions/" + sess.ID.String() + "/check-in/me"
	resp, err := ownerApp.Test(httptest.NewRequest(fiber.MethodPost, mePath, nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("me status %d body %s", resp.StatusCode, body)
	}

	guestApp := ticketApp(t, authn.Identity{}, events, tickets)
	req := httptest.NewRequest(fiber.MethodPost, "/v1/sessions/"+sess.ID.String()+"/check-in/guest", strings.NewReader(`{"email":"ada@example.com"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err = guestApp.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("guest status %d body %s", resp.StatusCode, body)
	}
	dup := httptest.NewRequest(fiber.MethodPost, "/v1/sessions/"+sess.ID.String()+"/check-in/guest", strings.NewReader(`{"email":"ada@example.com"}`))
	dup.Header.Set("Content-Type", "application/json")
	resp, err = guestApp.Test(dup)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusConflict {
		t.Fatalf("dup guest %d", resp.StatusCode)
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

func TestGetTicketByIDOwnerAndForbidden(t *testing.T) {
	t.Parallel()
	events := event.NewMemoryStore()
	tickets := ticket.NewMemoryStore()
	ev, err := events.Create(t.Context(), event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB"})
	if err != nil {
		t.Fatal(err)
	}
	owner := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	tk, err := tickets.Create(t.Context(), ticket.Ticket{EventID: ev.ID, TicketType: ticket.Registered, OwnerID: &owner})
	if err != nil {
		t.Fatal(err)
	}
	path := "/v1/tickets/" + tk.ID.String()

	ownerApp := ticketApp(t, authn.Identity{ID: owner}, events, tickets)
	resp, err := ownerApp.Test(httptest.NewRequest(fiber.MethodGet, path, nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("owner status %d body %s", resp.StatusCode, body)
	}
	var got ticket.Ticket
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.ID != tk.ID || got.Event == nil || got.Event.Name != "Hack" {
		t.Fatalf("got %+v", got)
	}

	stranger := ticketApp(t, authn.Identity{ID: uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")}, events, tickets)
	resp, err = stranger.Test(httptest.NewRequest(fiber.MethodGet, path, nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusForbidden {
		t.Fatalf("stranger %d", resp.StatusCode)
	}

	leader := ticketApp(t, authn.Identity{ID: uuid.MustParse("cccccccc-cccc-cccc-cccc-cccccccccccc"), Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}, events, tickets)
	resp, err = leader.Test(httptest.NewRequest(fiber.MethodGet, path, nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("leader %d", resp.StatusCode)
	}
}

func TestGetTicketByUserAndEventHTTP(t *testing.T) {
	t.Parallel()
	events := event.NewMemoryStore()
	tickets := ticket.NewMemoryStore()
	ev, err := events.Create(t.Context(), event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB"})
	if err != nil {
		t.Fatal(err)
	}
	owner := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	if _, err := tickets.Create(t.Context(), ticket.Ticket{EventID: ev.ID, TicketType: ticket.Registered, OwnerID: &owner}); err != nil {
		t.Fatal(err)
	}
	path := "/v1/tickets/user/" + owner.String() + "/event/" + ev.ID.String()
	app := ticketApp(t, authn.Identity{ID: owner}, events, tickets)
	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, path, nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}
}

func TestListTicketsQueryHTTP(t *testing.T) {
	t.Parallel()
	events := event.NewMemoryStore()
	tickets := ticket.NewMemoryStore()
	users := user.NewMemoryStore()
	ev, err := events.Create(t.Context(), event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB"})
	if err != nil {
		t.Fatal(err)
	}
	owner := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	if _, _, err := user.NewService(users).Ensure(t.Context(), owner, user.Profile{Email: "ada@example.com", FirstName: "Ada", LastName: "Lovelace"}); err != nil {
		t.Fatal(err)
	}
	if _, err := tickets.Create(t.Context(), ticket.Ticket{EventID: ev.ID, TicketType: ticket.Registered, OwnerID: &owner}); err != nil {
		t.Fatal(err)
	}
	if _, err := tickets.Create(t.Context(), ticket.Ticket{
		EventID: ev.ID, TicketType: ticket.Guest, GuestEmail: "ada@example.com", GuestFirstName: "Ada", GuestLastName: "Guest",
	}); err != nil {
		t.Fatal(err)
	}

	ownerApp := ticketApp(t, authn.Identity{ID: owner, Profile: user.Profile{Email: "ada@example.com"}}, events, tickets, users)
	resp, err := ownerApp.Test(httptest.NewRequest(fiber.MethodGet, "/v1/tickets?userId="+owner.String(), nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("own list %d %s", resp.StatusCode, body)
	}
	var own []ticket.Ticket
	if err := json.NewDecoder(resp.Body).Decode(&own); err != nil {
		t.Fatal(err)
	}
	if len(own) != 1 {
		t.Fatalf("own %+v", own)
	}

	resp, err = ownerApp.Test(httptest.NewRequest(fiber.MethodGet, "/v1/tickets?email=ada@example.com", nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("email %d", resp.StatusCode)
	}
	var byEmail []ticket.Ticket
	if err := json.NewDecoder(resp.Body).Decode(&byEmail); err != nil {
		t.Fatal(err)
	}
	if len(byEmail) != 2 {
		t.Fatalf("email %+v", byEmail)
	}

	stranger := ticketApp(t, authn.Identity{ID: uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")}, events, tickets, users)
	resp, err = stranger.Test(httptest.NewRequest(fiber.MethodGet, "/v1/tickets?userId="+owner.String(), nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusForbidden {
		t.Fatalf("stranger %d", resp.StatusCode)
	}

	resp, err = ownerApp.Test(httptest.NewRequest(fiber.MethodGet, "/v1/tickets", nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusBadRequest {
		t.Fatalf("empty query %d", resp.StatusCode)
	}
}

func TestListTicketsQuerySelfUserAndVictimEmailIsIntersection(t *testing.T) {
	t.Parallel()
	events := event.NewMemoryStore()
	tickets := ticket.NewMemoryStore()
	users := user.NewMemoryStore()
	ev, err := events.Create(t.Context(), event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB"})
	if err != nil {
		t.Fatal(err)
	}
	self := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	victim := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	if _, _, err := user.NewService(users).Ensure(t.Context(), self, user.Profile{Email: "self@example.com"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := user.NewService(users).Ensure(t.Context(), victim, user.Profile{Email: "victim@example.com"}); err != nil {
		t.Fatal(err)
	}
	ownTk, err := tickets.Create(t.Context(), ticket.Ticket{EventID: ev.ID, TicketType: ticket.Registered, OwnerID: &self})
	if err != nil {
		t.Fatal(err)
	}
	victimTk, err := tickets.Create(t.Context(), ticket.Ticket{EventID: ev.ID, TicketType: ticket.Registered, OwnerID: &victim})
	if err != nil {
		t.Fatal(err)
	}

	member := ticketApp(t, authn.Identity{
		ID:      self,
		Profile: user.Profile{Email: "self@example.com"},
		Groups:  []string{"/UYELER/ARGE/WEBLAB"},
	}, events, tickets, users)
	path := "/v1/tickets?userId=" + self.String() + "&email=victim@example.com"
	resp, err := member.Test(httptest.NewRequest(fiber.MethodGet, path, nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}
	var listed []ticket.Ticket
	if err := json.NewDecoder(resp.Body).Decode(&listed); err != nil {
		t.Fatal(err)
	}
	for _, tk := range listed {
		if tk.ID == victimTk.ID || (tk.OwnerID != nil && *tk.OwnerID == victim) {
			t.Fatalf("leaked victim ticket %+v", listed)
		}
	}
	if len(listed) != 0 {
		t.Fatalf("expected empty intersection, got %+v", listed)
	}

	bothOwn := "/v1/tickets?userId=" + self.String() + "&email=self@example.com"
	resp, err = member.Test(httptest.NewRequest(fiber.MethodGet, bothOwn, nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("own both %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&listed); err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].ID != ownTk.ID {
		t.Fatalf("own intersection %+v", listed)
	}
}
