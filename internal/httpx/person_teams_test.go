package httpx_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/commpref"
	"github.com/skylab-kulubu/core-backend/internal/httpx"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/testauth"
)

func personTeamsPath(sub string) string { return "/internal/v1/users/" + sub + "/teams" }

// serviceToken is a client-credentials token of client holding core's roles.
func (e *delegationEnv) serviceToken(t *testing.T, client string, roles ...string) string {
	t.Helper()
	return e.keys.Token(t, jwt.MapClaims{
		"sub": uuid.NewString(), "azp": client, "client_id": client,
		"resource_access": map[string]any{"core": map[string]any{"roles": roles}},
	})
}

type personTeamRow struct {
	ID          string            `json:"id"`
	Path        string            `json:"path"`
	Code        string            `json:"code"`
	DisplayName map[string]string `json:"displayName"`
	Role        string            `json:"role"`
}

func renderTeamRows(rows []personTeamRow) string {
	parts := make([]string, 0, len(rows))
	for _, row := range rows {
		parts = append(parts, strings.TrimPrefix(row.Path, "/UYELER/ARGE/")+"="+row.Role)
	}
	return strings.Join(parts, " ")
}

// SkyMail (audience:read) and Forms (users:read) ask for a person's Teams
// with TEAM_DELEGATION off, as in production, and on, and get the same
// answer: a Leader's or a coordinator's Team with every Sub-team (a
// protected one too: the answer writes nothing), a Sub-team Leader's own
// subtree with the Team above as a member, a member's Team and the Teams
// above, nothing for a Privileged root's member or an unknown person. No
// address, no count.
func TestPersonTeamsRoute(t *testing.T) {
	for _, on := range []bool{false, true} {
		env := newDelegationEnv(t, delegationSetup{on: on})
		web := "WEBLAB=leader WEBLAB/FRONT=leader WEBLAB/SECURE=leader"
		for _, token := range []string{env.serviceToken(t, "skymail", commpref.AudienceRole), env.serviceToken(t, "forms", "users:read", "ticket:forms")} {
			for name, want := range map[string]string{
				"webLeader":   web,
				"webCoord":    web,
				"frontLeader": "WEBLAB=member WEBLAB/FRONT=leader",
				"mobLeader":   "MOBILAB=leader",
				"member":      "WEBLAB=member",
				"frontMember": "WEBLAB=member WEBLAB/FRONT=member",
				"yk":          "",
				"outsider":    "",
			} {
				got := audienceCall(t, env.app, token, fiber.MethodGet, personTeamsPath(env.people[name].String()), "")
				if got.status != fiber.StatusOK || got.header.Get(fiber.HeaderCacheControl) != "no-store" {
					t.Fatalf("on=%v %s: %d %s", on, name, got.status, got.raw)
				}
				var answer struct {
					Teams []personTeamRow `json:"teams"`
				}
				if err := json.Unmarshal(got.raw, &answer); err != nil || answer.Teams == nil {
					t.Fatalf("on=%v %s: %v %s", on, name, err, got.raw)
				}
				if rendered := renderTeamRows(answer.Teams); rendered != want {
					t.Fatalf("on=%v %s: %q, want %q (%s)", on, name, rendered, want, got.raw)
				}
				for _, field := range []string{"@", "Count", "email"} {
					if strings.Contains(string(got.raw), field) {
						t.Fatalf("on=%v %s: %q in %s", on, name, field, got.raw)
					}
				}
			}
		}
		got := audienceCall(t, env.app, env.serviceToken(t, "skymail", commpref.AudienceRole), fiber.MethodGet, personTeamsPath(env.people["webLeader"].String()), "")
		var answer struct {
			Teams []personTeamRow `json:"teams"`
		}
		if err := json.Unmarshal(got.raw, &answer); err != nil || len(answer.Teams) != 3 {
			t.Fatalf("on=%v: %v %s", on, err, got.raw)
		}
		if team := answer.Teams[0]; team.ID != env.ids["/UYELER/ARGE/WEBLAB"] || team.Code != "WEBLAB" || team.DisplayName["tr"] != "WEBLAB" || team.Path != "/UYELER/ARGE/WEBLAB" {
			t.Fatalf("on=%v: %+v", on, team)
		}
		unknown := audienceCall(t, env.app, env.serviceToken(t, "skymail", commpref.AudienceRole), fiber.MethodGet, personTeamsPath(uuid.NewString()), "")
		if unknown.status != fiber.StatusOK || string(unknown.raw) != `{"teams":[]}` {
			t.Fatalf("on=%v unknown: %d %s", on, unknown.status, unknown.raw)
		}
	}
}

// Only a service account with audience:read, or Forms' with users:read; a
// person's token is refused even with a role, another role is not enough,
// another product's users:read is not either, no token is 401 and a sub
// that is no id 400. With no Team reads the route is 503.
func TestPersonTeamsAuthorization(t *testing.T) {
	env := newDelegationEnv(t, delegationSetup{})
	leader := env.people["webLeader"].String()
	for name, c := range map[string]struct {
		token, code string
	}{
		"person": {groupToken(t, env.keys, leader, "/UYELER/ARGE/WEBLAB/LIDERLER"), "person_teams_service_only"},
		"person with role": {env.keys.Token(t, jwt.MapClaims{
			"sub": leader, "azp": "admin", "groups": []string{"/UYELER/ARGE/WEBLAB/LIDERLER"},
			"resource_access": map[string]any{"core": map[string]any{"roles": []string{commpref.AudienceRole, "users:read"}}},
		}), "person_teams_service_only"},
		"service tracking": {env.serviceToken(t, "skymail", commpref.TrackingResolveRole), "person_teams_role_missing"},
		"service other":    {env.serviceToken(t, "forms", "ticket:forms", "url:forms"), "person_teams_role_missing"},
		// users:read is shared: only Forms' service account may use it here.
		"CMS with users:read":     {env.serviceToken(t, "cms-service", "users:read"), "person_teams_role_missing"},
		"another with users:read": {env.serviceToken(t, "profile-sync", "users:read"), "person_teams_role_missing"},
		"service no role":         {env.serviceToken(t, "skymail"), "person_teams_role_missing"},
	} {
		got := audienceCall(t, env.app, c.token, fiber.MethodGet, personTeamsPath(leader), "")
		if got.status != fiber.StatusForbidden || !strings.Contains(string(got.raw), c.code) || strings.Contains(string(got.raw), "WEBLAB") {
			t.Fatalf("%s: %d %s", name, got.status, got.raw)
		}
	}
	if got := audienceCall(t, env.app, "", fiber.MethodGet, personTeamsPath(leader), ""); got.status != fiber.StatusUnauthorized {
		t.Fatalf("anonymous: %d %s", got.status, got.raw)
	}
	token := env.serviceToken(t, "skymail", commpref.AudienceRole)
	if got := audienceCall(t, env.app, token, fiber.MethodGet, personTeamsPath("not-an-id"), ""); got.status != fiber.StatusBadRequest || !strings.Contains(string(got.raw), "person_teams_sub_invalid") {
		t.Fatalf("bad sub: %d %s", got.status, got.raw)
	}
	none := newDelegationEnv(t, delegationSetup{none: true})
	if got := audienceCall(t, none.app, none.serviceToken(t, "skymail", commpref.AudienceRole), fiber.MethodGet, personTeamsPath(leader), ""); got.status != fiber.StatusServiceUnavailable || !strings.Contains(string(got.raw), "person_teams_unavailable") {
		t.Fatalf("no Team reads: %d %s", got.status, got.raw)
	}
}

// Through the public edge (a forwarding header) the route is a bare 404
// before its token is read.
func TestPersonTeamsIsNotOnTheEdge(t *testing.T) {
	env := newDelegationEnv(t, delegationSetup{})
	path := personTeamsPath(env.people["webLeader"].String())
	for _, header := range []string{"X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "Forwarded", "X-Real-Ip"} {
		for _, token := range []string{env.serviceToken(t, "skymail", commpref.AudienceRole), ""} {
			req := httptest.NewRequest(fiber.MethodGet, path, nil)
			if token != "" {
				req.Header.Set("Authorization", "Bearer "+token)
			}
			req.Header.Set(header, "203.0.113.7")
			resp, err := env.app.Test(req, fiber.TestConfig{Timeout: 0})
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != fiber.StatusNotFound || strings.Contains(string(raw), "WEBLAB") {
				t.Fatalf("%s token=%v: %d %s", header, token != "", resp.StatusCode, raw)
			}
		}
	}
}

// subgroupsDown is a directory whose reads of the Groups under a Group
// fail, after the person's Groups and their Leader group were read.
type subgroupsDown struct{ *identity.Memory }

func (subgroupsDown) Subgroups(context.Context, string) ([]identity.Group, error) {
	return nil, errors.New("keycloak: subgroups read failed with status 502")
}

// A Keycloak read failing halfway is 503 with Retry-After and no Team, for
// a Leader whose tree cannot be walked.
func TestPersonTeamsDirectoryDown(t *testing.T) {
	keys := testauth.New(t)
	dir := identity.NewMemory()
	for _, g := range []identity.Group{
		{ID: "g-uyeler", Name: "UYELER", Path: "/UYELER"},
		{ID: "g-arge", Name: "ARGE", Path: "/UYELER/ARGE"},
		{ID: "g-weblab", Name: "WEBLAB", Path: "/UYELER/ARGE/WEBLAB"},
		{ID: "g-weblab-l", Name: "LIDERLER", Path: "/UYELER/ARGE/WEBLAB/LIDERLER"},
	} {
		dir.PutGroup(g)
	}
	leader := uuid.New()
	dir.PutUser(identity.Person{ID: leader, Email: "lider@example.test"})
	if err := dir.AddMember(context.Background(), "g-weblab-l", leader); err != nil {
		t.Fatal(err)
	}
	deps := memoryDepsOver(dir, identity.Options{})
	deps.ParseToken = keys.Parse()
	down := subgroupsDown{dir}
	deps.ManagedTeams = identity.NewTeamDelegation(authz.DefaultPolicy(), down, down, identity.TeamDelegationOptions{})
	app := httpx.New(deps)
	token := keys.Token(t, jwt.MapClaims{
		"sub": uuid.NewString(), "azp": "skymail", "client_id": "skymail",
		"resource_access": map[string]any{"core": map[string]any{"roles": []string{commpref.AudienceRole}}},
	})
	got := audienceCall(t, app, token, fiber.MethodGet, personTeamsPath(leader.String()), "")
	if got.status != fiber.StatusServiceUnavailable || got.header.Get(fiber.HeaderRetryAfter) != "30" ||
		got.header.Get(fiber.HeaderCacheControl) != "no-store" ||
		!strings.Contains(string(got.raw), "person_teams_directory_unavailable") || strings.Contains(string(got.raw), "WEBLAB") {
		t.Fatalf("%d %v %s", got.status, got.header, got.raw)
	}
}
