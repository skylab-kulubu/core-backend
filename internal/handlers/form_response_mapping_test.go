package handlers

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/formsfeed"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
)

type stubFormMapper struct {
	reports []ticket.FormResponse
	result  ticket.FormResponseResult
	err     error
}

func (s *stubFormMapper) RecordReport(_ context.Context, _ formsfeed.RecordFunc, r ticket.FormResponse) (ticket.FormResponseResult, error) {
	s.reports = append(s.reports, r)
	return s.result, s.err
}

// With the mappings on, the forms report goes through them; a failed read
// of the answer answers 5xx so that Forms sends the report again.
func TestFormResponseGoesThroughTheMapper(t *testing.T) {
	events := event.NewMemoryStore()
	h := NewTicketHandler(ticket.NewService(ticket.NewMemoryStore(), events, authz.NewAuthorizer(authz.DefaultPolicy())))
	h.logger = nil
	mapper := &stubFormMapper{result: ticket.FormResponseResult{Outcome: ticket.FormResponseMappedGuest, TicketsWritten: 1}}
	h.MapFormResponsesWith(mapper)
	app := fiber.New()
	app.Use(func(c fiber.Ctx) error {
		c.Locals(authn.LocalsIdentity, authn.Identity{ID: uuid.New(), Client: "forms", ServiceAccount: true, Product: authz.ProductForms, Roles: []string{"ticket:forms"}})
		return c.Next()
	})
	app.Post("/v1/forms/:formId/responses", h.RecordFormResponse)
	send := func() int {
		req := httptest.NewRequest("POST", "/v1/forms/"+uuid.NewString()+"/responses", strings.NewReader(`{"responseId":"`+uuid.NewString()+`","status":"accepted","userId":null,"guest":null}`))
		req.Header.Set("Content-Type", "application/json")
		resp, err := app.Test(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if got := send(); got != fiber.StatusNoContent || len(mapper.reports) != 1 || mapper.reports[0].Status != ticket.FormResponseAccepted {
		t.Fatalf("%d %+v", got, mapper.reports)
	}
	mapper.err = errors.New("formsfeed: Forms answered 503")
	if got := send(); got != fiber.StatusInternalServerError {
		t.Fatalf("a failed read: %d", got)
	}
}
