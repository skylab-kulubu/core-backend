package handlers

import (
	"encoding/json"
	"io"
	"net/http"
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

func ticketSearchApp(t *testing.T, ident authn.Identity) *fiber.App {
	t.Helper()
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
	h := NewTicketHandler(ticket.NewService(tickets, events, authz.NewAuthorizer(authz.DefaultPolicy()), users))
	app := fiber.New()
	app.Use(func(c fiber.Ctx) error {
		c.Locals(authn.LocalsIdentity, ident)
		return c.Next()
	})
	app.Get("/v1/tickets", h.List)
	app.Post(TicketSearchPath, h.Search)
	return app
}

func ticketSearchCall(t *testing.T, app *fiber.App, method, path, body string) (*http.Response, []byte) {
	t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
	}
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, raw
}

func TestTicketSearchCarriesTheEmailInTheBody(t *testing.T) {
	t.Parallel()
	owner := authn.Identity{ID: uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"), Profile: user.Profile{Email: "ada@example.com"}}
	app := ticketSearchApp(t, owner)

	for _, test := range []struct {
		body string
		want int
	}{
		{body: `{"email":"ada@example.com"}`, want: 2},
		{body: `{"userId":"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"}`, want: 1},
	} {
		resp, raw := ticketSearchCall(t, app, fiber.MethodPost, TicketSearchPath, test.body)
		if resp.StatusCode != fiber.StatusOK {
			t.Fatalf("%s: %d %s", test.body, resp.StatusCode, raw)
		}
		var got []ticket.Ticket
		if err := json.Unmarshal(raw, &got); err != nil || len(got) != test.want {
			t.Fatalf("%s: %d tickets, %v", test.body, len(got), err)
		}
		if resp.Header.Get("Deprecation") != "" {
			t.Fatalf("%s: search is deprecated", test.body)
		}
	}

	for _, body := range []string{`{}`, `{"userId":"not-a-uuid"}`, `{"email":`} {
		resp, raw := ticketSearchCall(t, app, fiber.MethodPost, TicketSearchPath, body)
		if resp.StatusCode != fiber.StatusBadRequest {
			t.Fatalf("%s: %d %s", body, resp.StatusCode, raw)
		}
	}

	stranger := ticketSearchApp(t, authn.Identity{ID: uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")})
	resp, raw := ticketSearchCall(t, stranger, fiber.MethodPost, TicketSearchPath, `{"userId":"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"}`)
	if resp.StatusCode != fiber.StatusForbidden {
		t.Fatalf("stranger %d %s", resp.StatusCode, raw)
	}
}

// The old GET ?email= still answers, and says what replaces it.
func TestTicketListByEmailQueryIsDeprecated(t *testing.T) {
	t.Parallel()
	owner := authn.Identity{ID: uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"), Profile: user.Profile{Email: "ada@example.com"}}
	app := ticketSearchApp(t, owner)
	resp, raw := ticketSearchCall(t, app, fiber.MethodGet, "/v1/tickets?email=ada@example.com", "")
	var got []ticket.Ticket
	if resp.StatusCode != fiber.StatusOK || json.Unmarshal(raw, &got) != nil || len(got) != 2 {
		t.Fatalf("%d %s", resp.StatusCode, raw)
	}
	if resp.Header.Get("Deprecation") != "@1795478400" || resp.Header.Get(fiber.HeaderLink) != `</v1/tickets/search>; rel="successor-version"` {
		t.Fatalf("headers %v", resp.Header)
	}
	resp, _ = ticketSearchCall(t, app, fiber.MethodGet, "/v1/tickets?userId=aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", "")
	if resp.StatusCode != fiber.StatusOK || resp.Header.Get("Deprecation") != "" {
		t.Fatalf("by user: %d %v", resp.StatusCode, resp.Header)
	}
}

// A problem's instance is the path: the query string, which can carry an
// e-mail, is not handed back.
func TestProblemInstanceLeavesTheQueryOut(t *testing.T) {
	t.Parallel()
	app := ticketSearchApp(t, authn.Identity{ID: uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")})
	resp, raw := ticketSearchCall(t, app, fiber.MethodGet, "/v1/tickets?email=ada@example.com&userId=nope", "")
	if resp.StatusCode != fiber.StatusBadRequest {
		t.Fatalf("%d %s", resp.StatusCode, raw)
	}
	var problem map[string]any
	if err := json.Unmarshal(raw, &problem); err != nil {
		t.Fatal(err)
	}
	if problem["instance"] != "/v1/tickets" || strings.Contains(string(raw), "example.com") {
		t.Fatalf("problem %s", raw)
	}
}
