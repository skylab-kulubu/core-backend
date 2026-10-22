package handlers

import (
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/season"
)

// A Session's feedback form over HTTP (etkinlik geri bildirim formu, ticket
// 03): created with it (mailed by default), kept by an update that leaves
// it out (as sky-app and today's panel send), cleared by null.
func TestSessionFeedbackFormOverHTTP(t *testing.T) {
	t.Parallel()
	events := event.NewMemoryStore()
	app := scheduleApp(t, yk(), events, season.NewMemoryStore())
	status, ev := sendEventJSON(t, app, fiber.MethodPost, "/v1/events", `{"name":"Hack","location":"YTÜ","ownerTeam":"WEBLAB"}`)
	if status != fiber.StatusCreated {
		t.Fatalf("event %d %v", status, ev)
	}
	status, day := sendEventJSON(t, app, fiber.MethodPost, "/v1/event-days", `{"eventId":"`+ev["id"].(string)+`","name":"Gün"}`)
	if status != fiber.StatusCreated {
		t.Fatalf("day %d %v", status, day)
	}
	form := "https://forms.yildizskylab.com/" + uuid.NewString()
	status, sess := sendEventJSON(t, app, fiber.MethodPost, "/v1/sessions",
		`{"eventDayId":"`+day["id"].(string)+`","title":"Açılış","speakerName":"Ada","sessionType":"talk","feedbackFormUrl":"`+form+`","feedbackFormAlias":"acilis-gb"}`)
	if status != fiber.StatusCreated || sess["feedbackFormUrl"] != form || sess["feedbackFormAlias"] != "acilis-gb" || sess["feedbackAutoSend"] != true {
		t.Fatalf("session %d %v", status, sess)
	}
	path := "/v1/sessions/" + sess["id"].(string)
	status, kept := sendEventJSON(t, app, fiber.MethodPut, path, `{"title":"Açılış","speakerName":"Can","sessionType":"talk","orderIndex":1}`)
	if status != fiber.StatusOK || kept["feedbackFormUrl"] != form || kept["feedbackFormAlias"] != "acilis-gb" || kept["feedbackAutoSend"] != true {
		t.Fatalf("kept %d %v", status, kept)
	}
	status, cleared := sendEventJSON(t, app, fiber.MethodPut, path, `{"title":"Açılış","speakerName":"Can","sessionType":"talk","feedbackFormUrl":null,"feedbackFormAlias":null,"feedbackAutoSend":false}`)
	if status != fiber.StatusOK || cleared["feedbackFormUrl"] != nil || cleared["feedbackFormAlias"] != nil || cleared["feedbackAutoSend"] != false {
		t.Fatalf("cleared %d %v", status, cleared)
	}
	if status, _ := sendEventJSON(t, app, fiber.MethodPut, path, `{"title":"Açılış","speakerName":"Can","sessionType":"talk","feedbackAutoSend":null}`); status != fiber.StatusBadRequest {
		t.Fatalf("null feedbackAutoSend %d", status)
	}
	// A form already another Session's is 409 form_in_use.
	status, _ = sendEventJSON(t, app, fiber.MethodPut, path, `{"title":"Açılış","speakerName":"Can","sessionType":"talk","feedbackFormUrl":"`+form+`"}`)
	if status != fiber.StatusOK {
		t.Fatalf("own form back %d", status)
	}
	status, refused := sendEventJSON(t, app, fiber.MethodPost, "/v1/sessions",
		`{"eventDayId":"`+day["id"].(string)+`","title":"Kapanış","speakerName":"Ada","sessionType":"talk","feedbackFormUrl":"`+form+`"}`)
	if status != fiber.StatusConflict || refused["code"] != "form_in_use" {
		t.Fatalf("second place %d %v", status, refused)
	}
}
