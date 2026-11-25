package handlers

import (
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/certificate"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// GET/POST /v1/events/{id}/certificate-template and GET
// /v1/certificate-templates?eventId= (docs/certificate-event-templates.md):
// a GECEKODU member creates the Event's template (201), a second one is
// 409 template_event_exists with the current one's templateId, another
// team's member is 403; binding it elsewhere is 400 template_event_scoped.
func TestCertificateEventTemplateHTTP(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	az := authz.NewAuthorizer(authz.DefaultPolicy())
	events := event.NewMemoryStore()
	store := certificate.NewMemoryStore()
	certs := certificate.NewServiceWithOptions(store, ticket.NewMemoryStore(), events, user.NewMemoryStore(), az, staticPDF("%PDF-1.4"), &recCertMail{}, certificate.Options{
		PublicAPIOrigin: "https://api.example.test", VerifyOrigin: "https://skyl.app/c", Templates: store, Jobs: store,
	})
	ev, err := events.Create(ctx, event.Event{Name: "Gece Kodu 2026", Location: "YTÜ", OwnerTeam: "GECEKODU"})
	if err != nil {
		t.Fatal(err)
	}
	member := authn.Identity{ID: uuid.New(), Groups: []string{"/UYELER/ORGANIZASYON/GECEKODU"}}
	other := authn.Identity{ID: uuid.New(), Groups: []string{"/UYELER/ARGE/ARTLAB/LIDERLER"}}
	call := func(who authn.Identity, method, path, body string) (int, map[string]any) {
		t.Helper()
		ch := NewCertificateHandler(certs)
		app := fiber.New()
		app.Use(func(c fiber.Ctx) error {
			c.Locals(authn.LocalsIdentity, who)
			return c.Next()
		})
		app.Get("/v1/events/:eventId/certificate-template", ch.EventTemplate)
		app.Post("/v1/events/:eventId/certificate-template", ch.CreateEventTemplate)
		app.Get("/v1/certificate-templates", ch.ListTemplates)
		app.Put("/v1/certificate-template-bindings", ch.SetBinding)
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := app.Test(req)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		var out map[string]any
		_ = json.Unmarshal(raw, &out)
		return resp.StatusCode, out
	}
	path := "/v1/events/" + ev.ID.String() + "/certificate-template"

	if status, _ := call(other, fiber.MethodPost, path, `{"from":"blank"}`); status != fiber.StatusForbidden {
		t.Fatalf("other team: %d", status)
	}
	status, created := call(member, fiber.MethodPost, path, `{"from":"blank"}`)
	if status != fiber.StatusCreated || created["eventId"] != ev.ID.String() || created["ownerTeam"] != "GECEKODU" {
		t.Fatalf("create: %d %v", status, created)
	}
	status, conflict := call(member, fiber.MethodPost, path, `{"from":"current"}`)
	if status != fiber.StatusConflict || conflict["code"] != certificate.CodeTemplateEventExists || conflict["templateId"] != created["id"] {
		t.Fatalf("second: %d %v", status, conflict)
	}
	status, state := call(member, fiber.MethodGet, path, "")
	own, _ := state["eventTemplate"].(map[string]any)
	if status != fiber.StatusOK || own["id"] != created["id"] || state["resolution"] != nil {
		t.Fatalf("state: %d %v", status, state)
	}
	if choices, ok := state["choices"].([]any); !ok || len(choices) != 0 {
		t.Fatalf("choices: %v", state["choices"])
	}
	if status, _ := call(member, fiber.MethodGet, "/v1/certificate-templates?eventId=nope", ""); status != fiber.StatusBadRequest {
		t.Fatalf("bad eventId: %d", status)
	}
	if status, _ := call(member, fiber.MethodGet, "/v1/certificate-templates?eventId="+ev.ID.String(), ""); status != fiber.StatusOK {
		t.Fatalf("list: %d", status)
	}
	status, refused := call(member, fiber.MethodPut, "/v1/certificate-template-bindings",
		`{"scope":"ownerTeam","scopeKey":"GECEKODU","templateId":"`+created["id"].(string)+`"}`)
	if status != fiber.StatusBadRequest || refused["code"] != certificate.CodeTemplateEventScoped {
		t.Fatalf("scoped: %d %v", status, refused)
	}
}
