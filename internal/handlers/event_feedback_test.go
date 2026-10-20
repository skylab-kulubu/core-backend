package handlers

import (
	"log"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/requestid"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// The Event's feedback form (etkinlik geri bildirim formu, ticket 02).

// A create without the feedback fields gets the defaults: mode event, no
// form, mailed when the Event ends. A form in a second place is 409
// form_in_use.
func TestEventFeedbackFormOnCreate(t *testing.T) {
	t.Parallel()
	app := eventApp(t, yk(), event.NewMemoryStore())
	status, created := sendEventJSON(t, app, fiber.MethodPost, "/v1/events", `{"name":"Hack","location":"YTÜ","ownerTeam":"WEBLAB"}`)
	if status != fiber.StatusCreated || created["feedbackMode"] != "event" || created["feedbackAutoSend"] != true || created["feedbackFormUrl"] != nil {
		t.Fatalf("defaults %d %v", status, created)
	}
	form := "https://forms.yildizskylab.com/" + uuid.NewString()
	status, withForm := sendEventJSON(t, app, fiber.MethodPost, "/v1/events",
		`{"name":"Hack 2","location":"YTÜ","ownerTeam":"WEBLAB","feedbackFormUrl":"`+form+`","feedbackFormAlias":"hack-gb","feedbackAutoSend":false}`)
	if status != fiber.StatusCreated || withForm["feedbackFormUrl"] != form || withForm["feedbackFormAlias"] != "hack-gb" || withForm["feedbackAutoSend"] != false {
		t.Fatalf("with form %d %v", status, withForm)
	}
	status, refused := sendEventJSON(t, app, fiber.MethodPost, "/v1/events", `{"name":"Hack 3","location":"YTÜ","ownerTeam":"WEBLAB","formUrl":"`+form+`"}`)
	if status != fiber.StatusConflict || refused["code"] != "form_in_use" {
		t.Fatalf("second place %d %v", status, refused)
	}
	status, refused = sendEventJSON(t, app, fiber.MethodPatch, "/v1/events/"+created["id"].(string), `{"feedbackFormUrl":"`+form+`"}`)
	if status != fiber.StatusConflict || refused["code"] != "form_in_use" {
		t.Fatalf("second place on update %d %v", status, refused)
	}
	if status, bad := sendEventJSON(t, app, fiber.MethodPost, "/v1/events", `{"name":"x","location":"YTÜ","ownerTeam":"WEBLAB","feedbackMode":"herkes"}`); status != fiber.StatusBadRequest {
		t.Fatalf("unknown mode %d %v", status, bad)
	}
}

// A feedback answer is 204, writes no Ticket, and logs outcome feedback.
func TestFormResponseToAFeedbackForm(t *testing.T) {
	t.Parallel()
	events := event.NewMemoryStore()
	tickets := ticket.NewMemoryStore()
	feedback := uuid.New()
	ev, err := events.Create(t.Context(), event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB", FeedbackFormURL: "https://forms.yildizskylab.com/" + feedback.String()})
	if err != nil {
		t.Fatal(err)
	}
	var logs lockedBuffer
	h := NewTicketHandler(ticket.NewService(tickets, events, authz.NewAuthorizer(authz.DefaultPolicy()), user.NewMemoryStore()))
	h.logger = log.New(&logs, "", 0)
	app := fiber.New()
	app.Use(requestid.New())
	app.Use(func(c fiber.Ctx) error {
		c.Locals(authn.LocalsIdentity, authn.Identity{
			ID: uuid.New(), Client: "forms", ServiceAccount: true, Product: authz.ProductForms, Roles: []string{"ticket:forms"},
		})
		return c.Next()
	})
	app.Post("/v1/forms/:formId/responses", h.RecordFormResponse)
	req := httptest.NewRequest(fiber.MethodPost, "/v1/forms/"+feedback.String()+"/responses",
		strings.NewReader(`{"responseId":"`+uuid.NewString()+`","status":"accepted","guest":{"firstName":"Ada","lastName":"Lovelace","email":"ada@example.com"}}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusNoContent {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if listed, _ := tickets.ListByEvent(t.Context(), ev.ID); len(listed) != 0 {
		t.Fatalf("tickets %v", listed)
	}
	if !strings.Contains(logs.String(), `"outcome":"feedback"`) {
		t.Fatalf("log %s", logs.String())
	}
}
