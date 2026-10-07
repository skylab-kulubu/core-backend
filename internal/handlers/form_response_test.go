package handlers

import (
	"encoding/json"
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

func TestFormResponseLogsTheReportButNotWhoAnswered(t *testing.T) {
	t.Parallel()
	events := event.NewMemoryStore()
	users := user.NewMemoryStore()
	formID := uuid.New()
	if _, err := events.Create(t.Context(), event.Event{
		Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB", FormURL: "https://forms.yildizskylab.com/" + formID.String(),
	}); err != nil {
		t.Fatal(err)
	}
	member := uuid.MustParse("dddddddd-dddd-dddd-dddd-dddddddddddd")
	if _, _, err := user.NewService(users).Ensure(t.Context(), member, user.Profile{
		Email: "member@example.com", FirstName: "Grace", LastName: "Hopper",
	}); err != nil {
		t.Fatal(err)
	}
	var logs lockedBuffer
	h := NewTicketHandler(ticket.NewService(ticket.NewMemoryStore(), events, authz.NewAuthorizer(authz.DefaultPolicy()), users))
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

	responseID := uuid.New()
	for _, step := range []struct {
		body string
		want int
	}{
		{`{"responseId":"` + responseID.String() + `","status":"accepted","guest":{"firstName":"Ada","lastName":"Lovelace","email":"ada@example.com"}}`, fiber.StatusNoContent},
		{`{"responseId":"` + uuid.NewString() + `","status":"accepted","userId":"` + member.String() + `"}`, fiber.StatusNoContent},
		{`{"responseId":"` + uuid.NewString() + `","status":"pending","guest":{"firstName":"Ada","lastName":"Lovelace","email":"ada@example.com"}}`, fiber.StatusNoContent},
		{`{"responseId":"` + uuid.NewString() + `","status":"ada@example.com","guest":{"firstName":"Ada","lastName":"Lovelace","email":"ada@example.com"}}`, fiber.StatusBadRequest},
	} {
		req := httptest.NewRequest(fiber.MethodPost, "/v1/forms/"+formID.String()+"/responses", strings.NewReader(step.body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := app.Test(req)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != step.want {
			t.Fatalf("%s: status %d, want %d", step.body, resp.StatusCode, step.want)
		}
	}

	out := logs.String()
	for _, personal := range []string{"ada@example.com", "Ada", "Lovelace", member.String(), "member@example.com", "Grace", "Hopper"} {
		if strings.Contains(out, personal) {
			t.Fatalf("log carries %q: %s", personal, out)
		}
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 4 {
		t.Fatalf("log lines %q", lines)
	}
	type line struct {
		Event          string `json:"event"`
		CorrelationID  string `json:"correlation_id"`
		FormID         string `json:"form_id"`
		ResponseID     string `json:"response_id"`
		Status         string `json:"status"`
		Outcome        string `json:"outcome"`
		TicketsWritten int    `json:"tickets_written"`
	}
	want := []line{
		{Status: "accepted", Outcome: "recorded", TicketsWritten: 1},
		{Status: "accepted", Outcome: "recorded", TicketsWritten: 1},
		{Status: "pending", Outcome: "not_accepted"},
		{Status: "unknown", Outcome: "invalid"},
	}
	for i, raw := range lines {
		var got line
		if err := json.Unmarshal([]byte(raw), &got); err != nil {
			t.Fatalf("line %d %q: %v", i, raw, err)
		}
		if got.Event != "form_response" || got.CorrelationID == "" || got.FormID != formID.String() || got.ResponseID == "" ||
			got.Status != want[i].Status || got.Outcome != want[i].Outcome || got.TicketsWritten != want[i].TicketsWritten {
			t.Errorf("line %d %+v, want %+v", i, got, want[i])
		}
	}
	if !strings.Contains(lines[0], responseID.String()) {
		t.Errorf("first line %s lacks the response id", lines[0])
	}
}
