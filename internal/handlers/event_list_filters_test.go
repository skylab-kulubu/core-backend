package handlers

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
)

// GET /v1/events?team= and ?managed=true (docs/co-organizers.md): team is
// the Owner team or a Co-organizer shown outside the panel, anyone may ask;
// managed needs a session (401) and answers what the caller may edit; a
// value that is not true/false, ?team= with ?ownerTeam=, or either with
// the archive is 400 event_list_filter_invalid.
func TestEventListTeamAndManaged(t *testing.T) {
	t.Parallel()
	store := event.NewMemoryStore()
	svc := event.NewServiceWithOptions(store, authz.NewAuthorizer(authz.DefaultPolicy()), event.ServiceOptions{Teams: organizerTree{
		"WEBLAB": "/UYELER/ARGE/WEBLAB", "ARTLAB": "/UYELER/SANAT/ARTLAB", "GECEKODU": "/UYELER/ORGANIZASYON/GECEKODU",
	}, CoOrganizersWrite: true})
	ykApp := organizersApp(t, yk(), svc)
	created := organizersCall(t, ykApp, fiber.MethodPost, "/v1/events", `{"name":"Hack","location":"YTÜ","ownerTeam":"ARTLAB","status":"published","endDate":"2099-12-12T18:00:00Z"}`)
	if created.status != fiber.StatusCreated {
		t.Fatalf("create %d %s", created.status, created.raw)
	}
	id, _ := created.body["id"].(string)
	if got := organizersCall(t, ykApp, fiber.MethodPut, "/v1/events/"+id+"/organizers",
		`{"teams":[{"code":"WEBLAB"},{"code":"GECEKODU","visibility":"helper"}]}`); got.status != fiber.StatusOK {
		t.Fatalf("organizers %d %s", got.status, got.raw)
	}
	count := func(app *fiber.App, query string) (int, int) {
		t.Helper()
		resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/v1/events"+query, nil))
		if err != nil {
			t.Fatal(err)
		}
		var list []map[string]any
		if resp.StatusCode == fiber.StatusOK {
			if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
				t.Fatal(err)
			}
		}
		return resp.StatusCode, len(list)
	}
	public := eventServiceApp(t, authn.Identity{}, svc)
	leader := eventServiceApp(t, weblabLeader(), svc)
	gecekodu := eventServiceApp(t, authn.Identity{ID: uuid.New(), Groups: []string{"/UYELER/ORGANIZASYON/GECEKODU/LIDERLER"}}, svc)
	for name, c := range map[string]struct {
		app          *fiber.App
		query        string
		status, size int
	}{
		"signed out, team=WEBLAB":      {public, "?team=WEBLAB", fiber.StatusOK, 1},
		"signed out, a helper team":    {public, "?team=GECEKODU", fiber.StatusOK, 0},
		"signed out, managed":          {public, "?managed=true", fiber.StatusUnauthorized, 0},
		"WEBLAB's Leader, managed":     {leader, "?managed=true", fiber.StatusOK, 1},
		"GECEKODU's Leader, managed":   {gecekodu, "?managed=true", fiber.StatusOK, 1},
		"GECEKODU's Leader, team":      {gecekodu, "?team=GECEKODU", fiber.StatusOK, 0},
		"ownerTeam=WEBLAB":             {leader, "?ownerTeam=WEBLAB", fiber.StatusOK, 0},
		"managed=false":                {leader, "?managed=false&team=WEBLAB", fiber.StatusOK, 1},
		"managed=yes":                  {leader, "?managed=yes", fiber.StatusBadRequest, 0},
		"team and ownerTeam":           {leader, "?team=WEBLAB&ownerTeam=ARTLAB", fiber.StatusBadRequest, 0},
		"team and the archive":         {leader, "?team=WEBLAB&lifecycle=inactive", fiber.StatusBadRequest, 0},
		"managed and the archive":      {leader, "?managed=true&lifecycle=all", fiber.StatusBadRequest, 0},
		"managed and ownerTeam=ARTLAB": {leader, "?managed=true&ownerTeam=ARTLAB", fiber.StatusOK, 1},
		"managed and ownerTeam=WEBLAB": {leader, "?managed=true&ownerTeam=WEBLAB", fiber.StatusOK, 0},
	} {
		status, size := count(c.app, c.query)
		if status != c.status || size != c.size {
			t.Errorf("%s: %d with %d Events, want %d with %d", name, status, size, c.status, c.size)
		}
	}
	resp, err := leader.Test(httptest.NewRequest(fiber.MethodGet, "/v1/events?managed=yes", nil))
	if err != nil {
		t.Fatal(err)
	}
	if problem := decodeJSONMap(t, resp); problem["code"] != "event_list_filter_invalid" {
		t.Fatalf("problem %v", problem)
	}
}
