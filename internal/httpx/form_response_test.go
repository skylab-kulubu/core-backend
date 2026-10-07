package httpx_test

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/httpx"
	"github.com/skylab-kulubu/core-backend/internal/testauth"
)

// Only the forms service's account with ticket:forms reports an answer, and
// a report sent twice leaves one Ticket.
func TestFormResponseReportNeedsTheFormsServiceRoleAndWritesOneTicket(t *testing.T) {
	t.Parallel()
	keys := testauth.New(t)
	deps := memoryDeps()
	deps.ParseToken = keys.Parse()
	app := httpx.New(deps)
	yk := groupToken(t, keys, ykSub, "/UYELER/YK")
	formID := uuid.NewString()
	created := sendJSON(t, app, yk, fiber.MethodPost, "/v1/events",
		`{"name":"Hack","location":"YTÜ","ownerTeam":"WEBLAB","formUrl":"https://forms.yildizskylab.com/`+formID+`"}`)
	if created.status != fiber.StatusCreated {
		t.Fatalf("create event %d %v", created.status, created.body)
	}
	eventID := created.body["id"].(string)
	path := "/v1/forms/" + formID + "/responses"
	report := `{"responseId":"` + uuid.NewString() + `","status":"accepted","guest":{"firstName":"Ada","lastName":"Lovelace","email":"ada@example.com"}}`

	for name, token := range map[string]string{
		"person":                         yk,
		"person through forms with role": personToken(t, keys, uuid.NewString(), "forms", "ticket:forms"),
		"forms no role":                  serviceToken(t, keys, "forms"),
		"other product":                  serviceToken(t, keys, cmsClient, "ticket:forms"),
		"unmapped client":                serviceToken(t, keys, "unknown-app", "ticket:forms"),
	} {
		if got := sendJSON(t, app, token, fiber.MethodPost, path, report); got.status != fiber.StatusForbidden {
			t.Errorf("%s: %d %v", name, got.status, got.body)
		}
	}

	forms := serviceToken(t, keys, "forms", "ticket:forms")
	for range 2 {
		if got := sendJSON(t, app, forms, fiber.MethodPost, path, report); got.status != fiber.StatusNoContent {
			t.Fatalf("report %d %v", got.status, got.body)
		}
	}
	if got := sendJSON(t, app, forms, fiber.MethodPost, path, `{"status":"approved"}`); got.status != fiber.StatusBadRequest {
		t.Fatalf("unknown status %d %v", got.status, got.body)
	}

	tickets := eventTickets(t, app, yk, eventID)
	if len(tickets) != 1 || tickets[0]["guestEmail"] != "ada@example.com" || tickets[0]["ticketType"] != "GUEST" {
		t.Fatalf("tickets %v", tickets)
	}
}

func eventTickets(t *testing.T, app *fiber.App, token, eventID string) []map[string]any {
	t.Helper()
	req := httptest.NewRequest(fiber.MethodGet, "/v1/events/"+eventID+"/tickets", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := app.Test(req, fiber.TestConfig{Timeout: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("tickets %d %s", resp.StatusCode, raw)
	}
	var tickets []map[string]any
	if err := json.Unmarshal(raw, &tickets); err != nil {
		t.Fatal(err)
	}
	return tickets
}
