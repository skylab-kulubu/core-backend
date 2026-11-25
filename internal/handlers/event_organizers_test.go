package handlers

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
)

// organizerTree answers the Groups the Co-organizer checks look up.
type organizerTree map[string]string

func (t organizerTree) TeamPath(team string) (string, bool) {
	path, ok := t[strings.ToUpper(strings.TrimSpace(team))]
	return path, ok
}

func (organizerTree) SubTeams(string) []string { return nil }

func organizersApp(t *testing.T, ident authn.Identity, svc event.Service) *fiber.App {
	t.Helper()
	app := eventServiceApp(t, ident, svc)
	h := NewEventHandler(svc)
	app.Get("/v1/events/:id/organizers", h.Organizers)
	app.Put("/v1/events/:id/organizers", h.SetOrganizers)
	return app
}

type organizersAnswer struct {
	status int
	body   map[string]any
	raw    string
}

func organizersCall(t *testing.T, app *fiber.App, method, path, body string) organizersAnswer {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	out := organizersAnswer{status: resp.StatusCode, raw: string(raw)}
	_ = json.Unmarshal(raw, &out.body)
	return out
}

// The routes: YK writes a list and reads it back, with the defaults; each
// refusal is 400 event_organizers_invalid naming the field, item and
// reason; a body that is not the list's (a person with choices, an unknown
// field, a bad id) is a plain 400; the Owner team's Leader reads but may not
// write (403); another team's Leader neither; the public Event JSON names
// the co_organizer team alone and the editor's the whole lists.
func TestEventOrganizersRoutes(t *testing.T) {
	t.Parallel()
	store := event.NewMemoryStore()
	svc := event.NewServiceWithOptions(store, authz.NewAuthorizer(authz.DefaultPolicy()), event.ServiceOptions{Teams: organizerTree{
		"WEBLAB": "/UYELER/ARGE/WEBLAB", "ARTLAB": "/UYELER/SANAT/ARTLAB", "GECEKODU": "/UYELER/ORGANIZASYON/GECEKODU",
	}})
	ykApp := organizersApp(t, yk(), svc)
	created := organizersCall(t, ykApp, fiber.MethodPost, "/v1/events", `{"name":"Hack","location":"YTÜ","ownerTeam":"WEBLAB","endDate":"2026-12-12T18:00:00Z"}`)
	if created.status != fiber.StatusCreated {
		t.Fatalf("create %d %s", created.status, created.raw)
	}
	id, _ := created.body["id"].(string)
	path := "/v1/events/" + id + "/organizers"
	person := uuid.New().String()

	put := organizersCall(t, ykApp, fiber.MethodPut, path,
		`{"teams":[{"code":"ARTLAB"},{"code":"GECEKODU","management":"members","visibility":"helper"}],"people":["`+person+`"]}`)
	if put.status != fiber.StatusOK {
		t.Fatalf("PUT %d %s", put.status, put.raw)
	}
	for _, want := range []string{
		`"ownerTeam":"WEBLAB"`, `"ownerRule":"leaders"`, `"canManage":true`,
		`{"code":"ARTLAB","name":"ARTLAB","management":"leaders","visibility":"co_organizer"}`,
		`{"code":"GECEKODU","name":"GECEKODU","management":"members","visibility":"helper"}`,
		`"id":"` + person + `"`, `"expiresAt":"2027-01-11T18:00:00Z"`,
	} {
		if !strings.Contains(put.raw, want) {
			t.Errorf("PUT answer lacks %s: %s", want, put.raw)
		}
	}
	if get := organizersCall(t, ykApp, fiber.MethodGet, path, ""); get.status != fiber.StatusOK || get.raw != put.raw {
		t.Fatalf("GET %d %s", get.status, get.raw)
	}

	refusals := []struct {
		body, field, reason string
		index               float64
	}{
		{`{"teams":[{"code":"WEBLAB"}]}`, "teams", "owner_team", 0},
		{`{"teams":[{"code":"ARTLAB"},{"code":"NOPE"}]}`, "teams", "unknown_team", 1},
		{`{"teams":[{"code":"YK"}]}`, "teams", "privileged_team", 0},
		{`{"teams":[{"code":"ARTLAB"},{"code":"artlab"}]}`, "teams", "duplicate", 1},
		{`{"teams":[{"code":"ARTLAB","management":"everyone"}]}`, "teams", "invalid_management", 0},
		{`{"teams":[{"code":"ARTLAB","visibility":"public"}]}`, "teams", "invalid_visibility", 0},
		{`{"people":["` + person + `","` + person + `"]}`, "people", "duplicate", 1},
		{`{"people":["00000000-0000-0000-0000-000000000000"]}`, "people", "invalid_person", 0},
	}
	for _, r := range refusals {
		got := organizersCall(t, ykApp, fiber.MethodPut, path, r.body)
		if got.status != fiber.StatusBadRequest || got.body["code"] != "event_organizers_invalid" ||
			got.body["field"] != r.field || got.body["reason"] != r.reason || got.body["index"] != r.index {
			t.Errorf("%s: %d %s", r.body, got.status, got.raw)
		}
	}
	tooMany := `{"teams":[` + strings.TrimSuffix(strings.Repeat(`{"code":"ARTLAB"},`, 11), ",") + `]}`
	if got := organizersCall(t, ykApp, fiber.MethodPut, path, tooMany); got.status != fiber.StatusBadRequest || got.body["reason"] != "too_many" || got.body["index"] != nil {
		t.Errorf("eleven teams: %d %s", got.status, got.raw)
	}
	for _, body := range []string{
		`{"people":[{"id":"` + person + `","management":"leaders"}]}`,
		`{"people":["not-an-id"]}`,
		`{"teams":[{"code":"ARTLAB","role":"x"}]}`,
		`{"teams":[],"owner":"ARTLAB"}`,
		`{"teams":[]}{}`,
		``,
	} {
		if got := organizersCall(t, ykApp, fiber.MethodPut, path, body); got.status != fiber.StatusBadRequest || got.body["code"] == "event_organizers_invalid" {
			t.Errorf("%q: %d %s", body, got.status, got.raw)
		}
	}
	if got := organizersCall(t, ykApp, fiber.MethodGet, path, ""); got.raw != put.raw {
		t.Fatalf("a refused PUT changed the list: %s", got.raw)
	}

	leaderApp := organizersApp(t, weblabLeader(), svc)
	if got := organizersCall(t, leaderApp, fiber.MethodPut, path, `{"teams":[]}`); got.status != fiber.StatusForbidden {
		t.Errorf("the Owner team's Leader PUT: %d %s", got.status, got.raw)
	}
	if got := organizersCall(t, leaderApp, fiber.MethodGet, path, ""); got.status != fiber.StatusOK || !strings.Contains(got.raw, `"canManage":false`) {
		t.Errorf("the Owner team's Leader GET: %d %s", got.status, got.raw)
	}
	other := authn.Identity{ID: uuid.New(), Groups: []string{"/UYELER/SANAT/ARTLAB/LIDERLER"}}
	otherApp := organizersApp(t, other, svc)
	for _, method := range []string{fiber.MethodGet, fiber.MethodPut} {
		if got := organizersCall(t, otherApp, method, path, `{"teams":[]}`); got.status != fiber.StatusForbidden {
			t.Errorf("another team's Leader %s: %d %s", method, got.status, got.raw)
		}
	}
	if got := organizersCall(t, organizersApp(t, authn.Identity{}, svc), fiber.MethodGet, path, ""); got.status != fiber.StatusUnauthorized {
		t.Errorf("signed out: %d", got.status)
	}
	if got := organizersCall(t, ykApp, fiber.MethodGet, "/v1/events/"+uuid.NewString()+"/organizers", ""); got.status != fiber.StatusNotFound {
		t.Errorf("no Event: %d", got.status)
	}

	// The Event's JSON: the public sees ARTLAB alone; the editor the lists.
	public := organizersCall(t, organizersApp(t, authn.Identity{}, svc), fiber.MethodGet, "/v1/events/"+id, "")
	if public.status != fiber.StatusOK || !strings.Contains(public.raw, `"coOrganizerTeams":["ARTLAB"]`) ||
		strings.Contains(public.raw, "GECEKODU") || strings.Contains(public.raw, person) || strings.Contains(public.raw, "coOrganizers\"") {
		t.Errorf("public Event JSON: %s", public.raw)
	}
	list := organizersCall(t, organizersApp(t, authn.Identity{}, svc), fiber.MethodGet, "/v1/events", "")
	if strings.Contains(list.raw, "GECEKODU") || strings.Contains(list.raw, person) {
		t.Errorf("public list: %s", list.raw)
	}
	editor := organizersCall(t, ykApp, fiber.MethodGet, "/v1/events/"+id, "")
	if !strings.Contains(editor.raw, `"coOrganizers":[`) || !strings.Contains(editor.raw, "GECEKODU") || !strings.Contains(editor.raw, `"coOrganizerIds":["`+person+`"]`) {
		t.Errorf("editor Event JSON: %s", editor.raw)
	}

	// A PUT of the Event that sends the lists changes nothing of them.
	update := organizersCall(t, ykApp, fiber.MethodPatch, "/v1/events/"+id, `{"name":"Hack 2","coOrganizers":[],"coOrganizerIds":[],"coOrganizerTeams":[]}`)
	if update.status != fiber.StatusOK {
		t.Fatalf("PATCH %d %s", update.status, update.raw)
	}
	if got := organizersCall(t, ykApp, fiber.MethodGet, path, ""); got.raw != put.raw {
		t.Fatalf("an Event PATCH changed the list: %s", got.raw)
	}
}
