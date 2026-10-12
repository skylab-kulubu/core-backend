package handlers

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/skylab-kulubu/core-backend/internal/event"
)

func sendEventJSON(t *testing.T, app *fiber.App, method, path, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("%s %s: %v %s", method, path, err, raw)
		}
	}
	return resp.StatusCode, out
}

// A client that knows nothing of the status (sky-app, today's panel) creates
// a published Event, and the answer still carries active as a boolean.
func TestEventCreateWithoutStatusIsPublished(t *testing.T) {
	t.Parallel()
	app := eventApp(t, weblabLeader(), event.NewMemoryStore())
	status, created := sendEventJSON(t, app, fiber.MethodPost, "/v1/events",
		`{"name":"Hack","location":"YTÜ","ownerTeam":"WEBLAB","active":false}`)
	if status != fiber.StatusCreated {
		t.Fatalf("create %d %v", status, created)
	}
	if created["status"] != "published" || created["active"] != true {
		t.Fatalf("created %v", created)
	}
	if _, ok := created["publishedAt"].(string); !ok {
		t.Fatalf("publishedAt %v", created["publishedAt"])
	}
	for _, absent := range []string{"cancelledAt", "cancelReason", "copiedFromEventId", "publishedBy", "cancelledBy"} {
		if _, ok := created[absent]; ok {
			t.Fatalf("%s answered: %v", absent, created)
		}
	}
}

func TestEventCreateAsDraft(t *testing.T) {
	t.Parallel()
	app := eventApp(t, weblabLeader(), event.NewMemoryStore())
	status, created := sendEventJSON(t, app, fiber.MethodPost, "/v1/events",
		`{"name":"Hack","location":"YTÜ","ownerTeam":"WEBLAB","status":"draft","active":true}`)
	if status != fiber.StatusCreated {
		t.Fatalf("create %d %v", status, created)
	}
	if created["status"] != "draft" || created["active"] != false {
		t.Fatalf("created %v", created)
	}
	if _, ok := created["publishedAt"]; ok {
		t.Fatalf("a draft has no publishedAt: %v", created)
	}
}

func TestEventCreateRefusesCancelledAndUnknownStatus(t *testing.T) {
	t.Parallel()
	app := eventApp(t, weblabLeader(), event.NewMemoryStore())
	for _, value := range []string{"cancelled", "live", "Draft"} {
		status, body := sendEventJSON(t, app, fiber.MethodPost, "/v1/events",
			`{"name":"Hack","location":"YTÜ","ownerTeam":"WEBLAB","status":"`+value+`"}`)
		if status != fiber.StatusBadRequest {
			t.Fatalf("status %q: %d %v", value, status, body)
		}
	}
}

// PUT and PATCH write the whole Event but neither its status nor active: an
// old body (active false, or a status) leaves a published Event published and
// a draft a draft.
func TestEventUpdateIgnoresActiveAndStatus(t *testing.T) {
	t.Parallel()
	app := eventApp(t, weblabLeader(), event.NewMemoryStore())
	_, published := sendEventJSON(t, app, fiber.MethodPost, "/v1/events",
		`{"name":"Hack","location":"YTÜ","ownerTeam":"WEBLAB"}`)
	_, draft := sendEventJSON(t, app, fiber.MethodPost, "/v1/events",
		`{"name":"Taslak","location":"YTÜ","ownerTeam":"WEBLAB","status":"draft"}`)
	for _, method := range []string{fiber.MethodPut, fiber.MethodPatch} {
		status, got := sendEventJSON(t, app, method, "/v1/events/"+published["id"].(string),
			`{"name":"Hack 2","location":"YTÜ","ownerTeam":"WEBLAB","active":false,"status":"draft"}`)
		if status != fiber.StatusOK || got["status"] != "published" || got["active"] != true || got["name"] != "Hack 2" {
			t.Fatalf("%s published: %d %v", method, status, got)
		}
		if got["publishedAt"] != published["publishedAt"] {
			t.Fatalf("%s publishedAt %v, was %v", method, got["publishedAt"], published["publishedAt"])
		}
		status, got = sendEventJSON(t, app, method, "/v1/events/"+draft["id"].(string),
			`{"name":"Taslak","location":"YTÜ","ownerTeam":"WEBLAB","active":true,"status":"published"}`)
		if status != fiber.StatusOK || got["status"] != "draft" || got["active"] != false {
			t.Fatalf("%s draft: %d %v", method, status, got)
		}
	}
}
