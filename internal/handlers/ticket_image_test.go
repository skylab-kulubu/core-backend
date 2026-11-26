package handlers

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/ticketimage"
)

// oneEvent holds one Event; no Media is used elsewhere.
type oneEvent struct{ ev event.Event }

func (o oneEvent) Get(_ context.Context, id uuid.UUID) (event.Event, error) {
	if id != o.ev.ID {
		return event.Event{}, event.ErrNotFound
	}
	return o.ev, nil
}

func (oneEvent) TeamsUsingMedia(context.Context, uuid.UUID, uuid.UUID) ([]string, error) {
	return nil, nil
}

// The route answers the choice; a missing Event is 404 event_not_found (a
// core without the route answers 404 without a code) and a bad region 400
// ticket_image_invalid.
func TestTicketImageRoutes(t *testing.T) {
	cover := uuid.New()
	ev := event.Event{ID: uuid.New(), Name: "Gecekodu", OwnerTeam: "WEBLAB", CoverImageID: &cover}
	svc := &ticketimage.Service{Store: &ticketimage.MemoryStore{}, Events: oneEvent{ev: ev}, Authz: authz.NewAuthorizer(authz.DefaultPolicy())}
	h := NewTicketImageHandler(svc)
	app := fiber.New(fiber.Config{ErrorHandler: ErrorHandler})
	app.Use(func(c fiber.Ctx) error {
		c.Locals(authn.LocalsIdentity, authn.Identity{ID: uuid.New(), Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}})
		return c.Next()
	})
	app.Get("/v1/events/:id/ticket-image", h.Get)
	app.Put("/v1/events/:id/ticket-image", h.Put)
	do := func(method, path, body string) (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := app.Test(req, fiber.TestConfig{Timeout: 0})
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	if status, body := do(fiber.MethodGet, "/v1/events/"+ev.ID.String()+"/ticket-image", ""); status != 200 || body["source"] != "auto" {
		t.Fatalf("get: %d %v", status, body)
	}
	if status, body := do(fiber.MethodGet, "/v1/events/"+uuid.NewString()+"/ticket-image", ""); status != 404 || body["code"] != "event_not_found" {
		t.Fatalf("missing Event: %d %v", status, body)
	}
	if status, body := do(fiber.MethodPut, "/v1/events/"+ev.ID.String()+"/ticket-image", `{"source":"cover","crop":{"x":0.5,"y":0,"w":0.6,"h":0.1}}`); status != 400 || body["code"] != "ticket_image_invalid" {
		t.Fatalf("bad region: %d %v", status, body)
	}
	if status, body := do(fiber.MethodPut, "/v1/events/"+ev.ID.String()+"/ticket-image", `{"source":"cover","crop":{"x":0,"y":0.1,"w":1,"h":0.1}}`); status != 200 || body["source"] != "cover" {
		t.Fatalf("cover region: %d %v", status, body)
	}
}
