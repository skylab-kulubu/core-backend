package httpx_test

import (
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
)

// The summary is behind the bearer, and its scope is the caller's team
// decisions as core makes them for every other route: a Group overage token
// gets its Groups from Keycloak first, so a leader with too many Groups for a
// token still sees their own team's Events and no other team's.
func TestDashboardSummaryFollowsTheCallersTeamDecisions(t *testing.T) {
	t.Parallel()
	e := newOverageEnv(t)
	admin := e.adminToken(t)
	for _, team := range []string{"WEBLAB", "GAMELAB"} {
		if got := e.createEvent(t, admin, team); got.status != fiber.StatusCreated {
			t.Fatalf("create %s: %d %v", team, got.status, got.body)
		}
	}

	resp, err := e.app.Test(httptest.NewRequest(fiber.MethodGet, "/v1/dashboard/summary", nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusUnauthorized {
		t.Fatalf("anonymous: %d", resp.StatusCode)
	}

	leader := sendJSON(t, e.app, e.markedToken(t), fiber.MethodGet, "/v1/dashboard/summary", "")
	if leader.status != fiber.StatusOK {
		t.Fatalf("leader: %d %v", leader.status, leader.body)
	}
	teams, _ := leader.body["ownerTeams"].([]any)
	if len(teams) != 1 || teams[0] != "WEBLAB" {
		t.Fatalf("leader's teams %v", leader.body["ownerTeams"])
	}
	if events, _ := leader.body["events"].(map[string]any); events["total"] != float64(1) {
		t.Fatalf("leader's events %v", leader.body["events"])
	}
	if leader.body["members"] != nil {
		t.Fatalf("a leader reads no people: %v", leader.body["members"])
	}

	all := sendJSON(t, e.app, admin, fiber.MethodGet, "/v1/dashboard/summary", "")
	if teams, _ := all.body["ownerTeams"].([]any); all.status != fiber.StatusOK || len(teams) != 2 {
		t.Fatalf("admin: %d %v", all.status, all.body["ownerTeams"])
	}
}
