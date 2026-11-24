package handlers

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
)

// The operator's cancel and restore over HTTP (docs/ticket-cancellation.md):
// the reason in the body, the Ticket's status in the answer, the door's
// 409 ticket_cancelled, the lists' ?status and ?include, and 409
// ticket_checked_in.
func TestCancelAndRestoreHTTP(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	events := event.NewMemoryStore()
	store := ticket.NewMemoryStore()
	ev, err := events.Create(ctx, event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB"})
	if err != nil {
		t.Fatal(err)
	}
	day, err := events.CreateDay(ctx, event.Day{EventID: ev.ID, Name: "Gün 1"})
	if err != nil {
		t.Fatal(err)
	}
	sess, err := events.CreateSession(ctx, event.Session{EventDayID: day.ID, Title: "Açılış", SpeakerName: "Ada", SessionType: "PRESENTATION"})
	if err != nil {
		t.Fatal(err)
	}
	holder := uuid.New()
	mine, err := store.Create(ctx, ticket.Ticket{EventID: ev.ID, TicketType: ticket.Registered, OwnerID: &holder})
	if err != nil {
		t.Fatal(err)
	}
	other, err := store.Create(ctx, ticket.Ticket{EventID: ev.ID, TicketType: ticket.Guest, GuestFirstName: "Grace", GuestLastName: "Hopper", GuestEmail: "grace@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	leader := authn.Identity{ID: uuid.New(), Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}
	app := ticketApp(t, leader, events, store)
	holderApp := ticketApp(t, authn.Identity{ID: holder}, events, store)

	call := func(app *fiber.App, method, path, body string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
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

	status, body := call(app, fiber.MethodPost, "/v1/tickets/"+mine.ID.String()+"/cancel", `{"reason":"Gelemeyecek"}`)
	var got ticket.Ticket
	if err := json.Unmarshal([]byte(body), &got); status != fiber.StatusOK || err != nil ||
		got.Status != ticket.StatusCancelled || got.CancelledBy != ticket.CancelledByOperator || got.CancelReason != "Gelemeyecek" {
		t.Fatalf("cancel %d %s", status, body)
	}
	if status, body := call(app, fiber.MethodPost, "/v1/tickets/"+mine.ID.String()+"/cancel", ""); status != fiber.StatusOK || !strings.Contains(body, `"status":"cancelled"`) {
		t.Fatalf("cancel again %d %s", status, body)
	}
	// Someone else's Ticket is no Ticket to a member (kontenjan-iptal-hatirlatma 03).
	if status, body := call(holderApp, fiber.MethodPost, "/v1/tickets/"+other.ID.String()+"/cancel", ""); status != fiber.StatusNotFound {
		t.Fatalf("a member %d %s", status, body)
	}
	if status, body := call(app, fiber.MethodPost, "/v1/tickets/"+mine.ID.String()+"/sessions/"+sess.ID.String()+"/check-in", ""); status != fiber.StatusConflict || !strings.Contains(body, "ticket_cancelled") {
		t.Fatalf("door %d %s", status, body)
	}

	for path, want := range map[string]int{
		"/v1/events/" + ev.ID.String() + "/tickets":                  1,
		"/v1/events/" + ev.ID.String() + "/tickets?status=active":    1,
		"/v1/events/" + ev.ID.String() + "/tickets?status=cancelled": 1,
		"/v1/events/" + ev.ID.String() + "/tickets?status=all":       2,
	} {
		status, body := call(app, fiber.MethodGet, path, "")
		var listed []ticket.Ticket
		if err := json.Unmarshal([]byte(body), &listed); status != fiber.StatusOK || err != nil || len(listed) != want {
			t.Errorf("%s: %d %s", path, status, body)
		}
	}
	if status, _ := call(app, fiber.MethodGet, "/v1/events/"+ev.ID.String()+"/tickets?status=deleted", ""); status != fiber.StatusBadRequest {
		t.Errorf("unknown status: %d", status)
	}
	for path, want := range map[string]int{
		"/v1/tickets/me": 0,
		"/v1/tickets/me?include=waitlisted,cancelled": 1,
	} {
		status, body := call(holderApp, fiber.MethodGet, path, "")
		var listed []ticket.Ticket
		if err := json.Unmarshal([]byte(body), &listed); status != fiber.StatusOK || err != nil || len(listed) != want {
			t.Errorf("%s: %d %s", path, status, body)
		}
	}
	if status, _ := call(holderApp, fiber.MethodGet, "/v1/tickets/me?include=deleted", ""); status != fiber.StatusBadRequest {
		t.Errorf("unknown include: %d", status)
	}

	if status, body := call(app, fiber.MethodPost, "/v1/tickets/"+mine.ID.String()+"/restore", ""); status != fiber.StatusOK || !strings.Contains(body, `"status":"active"`) {
		t.Fatalf("restore %d %s", status, body)
	}
	if status, body := call(app, fiber.MethodPost, "/v1/tickets/"+other.ID.String()+"/sessions/"+sess.ID.String()+"/check-in", ""); status != fiber.StatusCreated {
		t.Fatalf("check-in %d %s", status, body)
	}
	if status, body := call(app, fiber.MethodPost, "/v1/tickets/"+other.ID.String()+"/cancel", ""); status != fiber.StatusConflict || !strings.Contains(body, "ticket_checked_in") {
		t.Fatalf("checked in %d %s", status, body)
	}

	// The holder cancels their own Ticket on the same route; the reason
	// they send is not kept.
	status, body = call(holderApp, fiber.MethodPost, "/v1/tickets/"+mine.ID.String()+"/cancel", `{"reason":"benim","notify":false}`)
	var own ticket.Ticket
	if err := json.Unmarshal([]byte(body), &own); status != fiber.StatusOK || err != nil ||
		own.Status != ticket.StatusCancelled || own.CancelledBy != ticket.CancelledByHolder || own.CancelReason != "" {
		t.Fatalf("holder cancel %d %s", status, body)
	}
	// Once the Event has started: 409 ticket_cancel_closed.
	start, end := time.Now().Add(-time.Minute), time.Now().Add(time.Hour)
	started, err := events.Create(ctx, event.Event{Name: "Başladı", Location: "YTÜ", OwnerTeam: "WEBLAB", StartDate: &start, EndDate: &end})
	if err != nil {
		t.Fatal(err)
	}
	late, err := store.Create(ctx, ticket.Ticket{EventID: started.ID, TicketType: ticket.Registered, OwnerID: &holder})
	if err != nil {
		t.Fatal(err)
	}
	if status, body := call(holderApp, fiber.MethodPost, "/v1/tickets/"+late.ID.String()+"/cancel", ""); status != fiber.StatusConflict || !strings.Contains(body, "ticket_cancel_closed") {
		t.Fatalf("started %d %s", status, body)
	}
}
