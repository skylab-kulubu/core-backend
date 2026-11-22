package handlers

import (
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
)

// The panel's two actions answer: 202 with the resend's number, 429 past
// three an hour, 409 with a code when no mail can go; the renewal 200 with
// the Ticket (its code, never its key) and whether the mail was queued.
func TestTicketResendRoutes(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	events := event.NewPostgresStore(pool)
	store := ticket.NewPostgresStore(pool)
	store.NotifyCreated(true)
	svc := ticket.NewService(store, events, authz.NewAuthorizer(authz.DefaultPolicy()))
	end := time.Now().Add(72 * time.Hour)
	ev, err := events.Create(ctx, event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB", EndDate: &end})
	if err != nil {
		t.Fatal(err)
	}
	lead := authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}
	applied, err := svc.ApplyGuest(ctx, lead, ev.ID, ticket.GuestInfo{FirstName: "Ada", LastName: "Lovelace", Email: "ada@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	h := NewTicketHandler(svc)
	app := fiber.New(fiber.Config{ErrorHandler: ErrorHandler})
	app.Use(func(c fiber.Ctx) error {
		c.Locals(authn.LocalsIdentity, authn.Identity{ID: uuid.MustParse(lead.ID), Groups: lead.Groups})
		return c.Next()
	})
	app.Post("/v1/tickets/:id/notification", h.ResendTicket)
	app.Post("/v1/tickets/:id/access-key/rotate", h.RenewTicketLink)
	post := func(path, key string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(fiber.MethodPost, path, nil)
		if key != "" {
			req.Header.Set("Idempotency-Key", key)
		}
		resp, err := app.Test(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}
	base := "/v1/tickets/" + applied.Ticket.ID.String()

	status, body := post(base+"/notification", "panel-1")
	if status != fiber.StatusAccepted || body != `{"resend":1,"replayed":false,"queued":true}` {
		t.Fatalf("resend: %d %s", status, body)
	}
	if status, body := post(base+"/notification", "panel-1"); status != fiber.StatusAccepted || !strings.Contains(body, `"replayed":true`) {
		t.Fatalf("replay: %d %s", status, body)
	}
	status, body = post(base+"/access-key/rotate", "")
	var renewed map[string]any
	if err := json.Unmarshal([]byte(body), &renewed); err != nil || status != fiber.StatusOK || renewed["notificationQueued"] != true ||
		renewed["code"] != applied.Ticket.Code || renewed["resend"] != float64(2) {
		t.Fatalf("renew: %d %s", status, body)
	}
	got, _ := store.Get(ctx, applied.Ticket.ID)
	if strings.Contains(body, got.AccessKey) || strings.Contains(body, applied.Ticket.AccessKey) || strings.Contains(strings.ToLower(body), "accesskey") {
		t.Fatalf("the renewal answers a key: %s", body)
	}
	if status, _ := post(base+"/notification", ""); status != fiber.StatusAccepted {
		t.Fatalf("third: %d", status)
	}
	if status, body := post(base+"/notification", ""); status != fiber.StatusTooManyRequests || !strings.Contains(body, "ticket_resend_rate_limited") {
		t.Fatalf("fourth: %d %s", status, body)
	}
	store.NotifyCreated(false)
	if status, body := post(base+"/access-key/rotate", ""); status != fiber.StatusConflict || !strings.Contains(body, "ticket_notification_off") {
		t.Fatalf("off: %d %s", status, body)
	}
	if status, body := post(base+"/access-key/rotate?notify=false", ""); status != fiber.StatusOK || !strings.Contains(body, `"notificationQueued":false`) {
		t.Fatalf("notify=false: %d %s", status, body)
	}
	if status, _ := post(base+"/access-key/rotate?notify=belki", ""); status != fiber.StatusBadRequest {
		t.Fatalf("notify=belki: %d", status)
	}
	if status, _ := post("/v1/tickets/"+uuid.NewString()+"/notification", ""); status != fiber.StatusNotFound {
		t.Fatalf("unknown: %d", status)
	}
}
