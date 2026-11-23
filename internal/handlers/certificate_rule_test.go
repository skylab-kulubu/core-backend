package handlers

import (
	"fmt"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/season"
)

// GET and PUT /v1/events/{id}/certificate-rule and the Session marks
// (certificate × session ticket 01; docs/certificate-rule.md).
func TestCertificateRuleHTTP(t *testing.T) {
	t.Parallel()
	events := event.NewMemoryStore()
	seasons := season.NewMemoryStore()
	app := scheduleApp(t, weblabLeader(), events, seasons)

	status, ev := sendEventJSON(t, app, fiber.MethodPost, "/v1/events", `{"name":"ARTLAB","location":"YTÜ","ownerTeam":"WEBLAB","attendanceRule":"count"}`)
	if status != fiber.StatusCreated {
		t.Fatalf("event %d %v", status, ev)
	}
	eventID := ev["id"].(string)
	status, day := sendEventJSON(t, app, fiber.MethodPost, "/v1/event-days", `{"eventId":"`+eventID+`","name":"Cumartesi"}`)
	if status != fiber.StatusCreated {
		t.Fatalf("day %d %v", status, day)
	}
	session := func(body string) map[string]any {
		t.Helper()
		status, sess := sendEventJSON(t, app, fiber.MethodPost, "/v1/sessions", `{"eventDayId":"`+day["id"].(string)+`","speakerName":"Ada",`+body+`}`)
		if status != fiber.StatusCreated {
			t.Fatalf("session %s: %d %v", body, status, sess)
		}
		return sess
	}
	talk := session(`"title":"Açılış","sessionType":"TALK"`)
	networking := session(`"title":"Tanışma","sessionType":"NETWORKING"`)
	final := session(`"title":"Final","sessionType":"TALK","certificateRequired":true`)
	if talk["certificateCounted"] != true || talk["certificateRequired"] != false ||
		networking["certificateCounted"] != false || final["certificateCounted"] != true || final["certificateRequired"] != true {
		t.Fatalf("marks %v %v %v", talk, networking, final)
	}
	if status, out := sendEventJSON(t, app, fiber.MethodPost, "/v1/sessions", `{"eventDayId":"`+day["id"].(string)+`","speakerName":"Ada","title":"X","sessionType":"TALK","certificateCounted":false,"certificateRequired":true}`); status != fiber.StatusBadRequest {
		t.Fatalf("required, not counted: %d %v", status, out)
	}
	// A Session update that leaves the marks out keeps them.
	status, kept := sendEventJSON(t, app, fiber.MethodPut, "/v1/sessions/"+final["id"].(string), `{"title":"Final","speakerName":"Can","sessionType":"TALK"}`)
	if status != fiber.StatusOK || kept["certificateRequired"] != true {
		t.Fatalf("kept %d %v", status, kept)
	}

	path := "/v1/events/" + eventID + "/certificate-rule"
	status, rule := sendEventJSON(t, app, fiber.MethodGet, path, "")
	if status != fiber.StatusOK || rule["rule"] != "count" || rule["minCount"] != nil || rule["countedTotal"] != float64(2) || rule["effectiveMin"] != float64(2) {
		t.Fatalf("read %d %v", status, rule)
	}
	if _, present := rule["minCount"]; !present {
		t.Fatalf("minCount left out: %v", rule)
	}
	if sessions := rule["sessions"].([]any); len(sessions) != 3 {
		t.Fatalf("sessions %v", sessions)
	}

	body := fmt.Sprintf(`{"rule":"count","minCount":3,"sessions":[{"id":%q,"counted":true,"required":false},{"id":%q,"counted":true,"required":false}]}`,
		networking["id"], final["id"])
	status, rule = sendEventJSON(t, app, fiber.MethodPut, path, body)
	if status != fiber.StatusOK || rule["minCount"] != float64(3) || rule["countedTotal"] != float64(3) || rule["effectiveMin"] != float64(3) {
		t.Fatalf("write %d %v", status, rule)
	}
	_, read := sendEventJSON(t, app, fiber.MethodGet, "/v1/events/"+eventID, "")
	if read["attendanceRule"] != "count" || read["attendanceMinCount"] != float64(3) {
		t.Fatalf("event %v", read)
	}

	for name, refused := range map[string]string{
		"ratio":                 `{"rule":"ratio","minCount":null,"sessions":[]}`,
		"threshold 0":           `{"rule":"count","minCount":0,"sessions":[]}`,
		"required, not counted": fmt.Sprintf(`{"rule":"count","sessions":[{"id":%q,"counted":false,"required":true}]}`, talk["id"]),
		"a mark left out":       fmt.Sprintf(`{"rule":"count","sessions":[{"id":%q,"counted":false}]}`, talk["id"]),
		"another Event's":       `{"rule":"count","sessions":[{"id":"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa","counted":true,"required":false}]}`,
	} {
		if status, out := sendEventJSON(t, app, fiber.MethodPut, path, refused); status != fiber.StatusBadRequest {
			t.Fatalf("%s: %d %v", name, status, out)
		}
	}
	// A refused write wrote nothing.
	if _, after := sendEventJSON(t, app, fiber.MethodGet, path, ""); after["minCount"] != float64(3) || after["countedTotal"] != float64(3) {
		t.Fatalf("after refusals %v", after)
	}

	// The ratio rule is refused on PATCH; another team's leader may not
	// read or write the rule.
	if status, out := sendEventJSON(t, app, fiber.MethodPatch, "/v1/events/"+eventID, `{"attendanceRule":"ratio","attendanceRatio":0.75}`); status != fiber.StatusBadRequest {
		t.Fatalf("patch ratio %d %v", status, out)
	}
	other := scheduleApp(t, authn.Identity{ID: uuid.MustParse("33333333-3333-3333-3333-333333333333"), Groups: []string{"/UYELER/ARGE/AIRLAB/LIDERLER"}}, events, seasons)
	if status, out := sendEventJSON(t, other, fiber.MethodPut, path, `{"rule":"once","sessions":[]}`); status != fiber.StatusForbidden {
		t.Fatalf("another team writes %d %v", status, out)
	}
	if status, out := sendEventJSON(t, other, fiber.MethodGet, path, ""); status != fiber.StatusForbidden {
		t.Fatalf("another team reads %d %v", status, out)
	}
}
