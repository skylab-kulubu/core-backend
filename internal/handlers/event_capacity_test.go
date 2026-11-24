package handlers

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
)

// A full Event over HTTP (docs/event-capacity.md): Member apply without a
// body (sky-app before the list) is 409 event_full with waitlistEnabled and
// writes nothing; with joinWaitlist it is 201 waitlisted; the public Guest
// apply answers "waitlisted"; the panel's add goes past the capacity with
// overCapacity; "Öne al" promotes.
func TestFullEventHTTP(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	events := event.NewMemoryStore()
	store := ticket.NewMemoryStore()
	on := true
	ev, err := events.Create(ctx, event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB", Capacity: 1, WaitlistEnabled: &on})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(ctx, ticket.Ticket{EventID: ev.ID, TicketType: ticket.Guest, GuestFirstName: "İlk", GuestLastName: "Kişi", GuestEmail: "first@example.com"}); err != nil {
		t.Fatal(err)
	}
	member := uuid.New()
	memberApp := ticketApp(t, authn.Identity{ID: member, Groups: []string{"/UYELER/ARGE/GAMELAB"}}, events, store)
	anonApp := ticketApp(t, authn.Identity{}, events, store)
	leaderApp := ticketApp(t, authn.Identity{ID: uuid.New(), Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}, events, store)
	call := func(app *fiber.App, path, body string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(fiber.MethodPost, path, strings.NewReader(body))
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := app.Test(req)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(raw)
	}
	apply := "/v1/events/" + ev.ID.String() + "/applications/me"
	if status, body := call(memberApp, apply, ""); status != fiber.StatusConflict ||
		!strings.Contains(body, `"code":"event_full"`) || !strings.Contains(body, `"waitlistEnabled":true`) {
		t.Fatalf("apply without a body %d %s", status, body)
	}
	if listed, _ := store.ListByEventStatus(ctx, ev.ID, nil); len(listed) != 1 {
		t.Fatalf("a refused apply wrote %d", len(listed))
	}
	if status, body := call(memberApp, apply, `{"joinWaitlist":"yes"}`); status != fiber.StatusBadRequest {
		t.Fatalf("a malformed body %d %s", status, body)
	}
	status, body := call(memberApp, apply, `{"joinWaitlist":true}`)
	if status != fiber.StatusCreated || !strings.Contains(body, `"status":"waitlisted"`) {
		t.Fatalf("join %d %s", status, body)
	}
	// Joining again while waiting: 409 ticket_waitlisted, never a bare 409
	// that an older sky-app shows as "already registered".
	for _, body := range []string{"", `{"joinWaitlist":true}`} {
		if status, got := call(memberApp, apply, body); status != fiber.StatusConflict || !strings.Contains(got, `"code":"ticket_waitlisted"`) {
			t.Fatalf("again %q: %d %s", body, status, got)
		}
	}
	waiting, err := store.GetByOwnerEvent(ctx, member, ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	guest := "/v1/events/" + ev.ID.String() + "/applications/guest"
	if status, body := call(anonApp, guest, `{"firstName":"Ada","lastName":"L","email":"ada@example.com"}`); status != fiber.StatusConflict || !strings.Contains(body, "event_full") {
		t.Fatalf("guest without joinWaitlist %d %s", status, body)
	}
	if status, body := call(anonApp, guest, `{"firstName":"Ada","lastName":"L","email":"ada@example.com","joinWaitlist":true}`); status != fiber.StatusCreated || body != `{"status":"waitlisted"}` {
		t.Fatalf("guest %d %s", status, body)
	}
	if status, body := call(leaderApp, guest, `{"firstName":"Konuşmacı","lastName":"K","email":"speaker@example.com"}`); status != fiber.StatusCreated ||
		!strings.Contains(body, `"status":"active"`) || !strings.Contains(body, `"overCapacity":true`) {
		t.Fatalf("panel add %d %s", status, body)
	}
	if status, body := call(leaderApp, "/v1/tickets/"+waiting.ID.String()+"/promote", ""); status != fiber.StatusOK ||
		!strings.Contains(body, `"status":"active"`) || !strings.Contains(body, `"overCapacity":true`) {
		t.Fatalf("promote %d %s", status, body)
	}
	if status, body := call(memberApp, "/v1/tickets/"+waiting.ID.String()+"/promote", ""); status != fiber.StatusForbidden && status != fiber.StatusNotFound {
		t.Fatalf("a member promotes %d %s", status, body)
	}
}
