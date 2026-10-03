package httpx_test

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"io"
	"log"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/githubactivity"
	"github.com/skylab-kulubu/core-backend/internal/httpx"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/testauth"
)

type roleModeEnv struct {
	app     *fiber.App
	keys    *testauth.Bundle
	metrics *authz.RoleMetrics
	logs    *bytes.Buffer
}

func newRoleModeEnv(t *testing.T, mode authz.RoleMode) roleModeEnv {
	t.Helper()
	keys := testauth.New(t)
	var logs bytes.Buffer
	metrics := authz.NewRoleMetrics(log.New(&logs, "", 0), time.Now)
	policy := authz.DefaultPolicy()
	policy.RoleMode = mode
	az := authz.NewAuthorizer(policy, authz.WithRoleMetrics(metrics))
	dir := identity.NewMemory()
	dir.PutGroup(identity.Group{ID: "g-yk", Name: "YK", Path: "/UYELER/YK"})
	deps := memoryDepsDecidedBy(dir, identity.Options{}, az)
	deps.ParseToken = keys.Parse()
	deps.AuthzRoleMetrics = metrics
	gh := quietGitHub(t)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	deps.GithubActivity = githubactivity.New(githubactivity.Config{Org: "skylab-kulubu", AppID: 1, InstallationID: 9, PrivateKey: key, WindowDays: 30},
		az, githubactivity.Options{APIURL: gh.URL, HTTP: gh.Client()})
	return roleModeEnv{app: httpx.New(deps), keys: keys, metrics: metrics, logs: &logs}
}

// token is a person's access token for the admin client with groups and
// core roles.
func (e roleModeEnv) token(t *testing.T, groups []string, roles ...string) string {
	t.Helper()
	sub := uuid.NewString()
	claims := jwt.MapClaims{"sub": sub, "email": sub + "@example.test", "azp": "admin", "given_name": "A", "family_name": "B"}
	if groups != nil {
		claims["groups"] = groups
	}
	if roles != nil {
		claims["resource_access"] = map[string]any{"core": map[string]any{"roles": roles}}
	}
	return e.keys.Token(t, claims)
}

// status is the status of a request whose body the test does not read.
func (e roleModeEnv) status(t *testing.T, token, method, path, body string) int {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := e.app.Test(req, fiber.TestConfig{Timeout: 0})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

// serviceToken is a client's service account token (client credentials)
// with core roles.
func (e roleModeEnv) serviceToken(t *testing.T, roles ...string) string {
	t.Helper()
	return e.keys.Token(t, jwt.MapClaims{
		"sub": uuid.NewString(), "azp": "some-service", "client_id": "some-service",
		"resource_access": map[string]any{"core": map[string]any{"roles": roles}},
	})
}

func (e roleModeEnv) capabilities(t *testing.T, token string) authz.Capabilities {
	t.Helper()
	req := httptest.NewRequest(fiber.MethodGet, "/v1/users/me/capabilities", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := e.app.Test(req, fiber.TestConfig{Timeout: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != fiber.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("capabilities %d %s", resp.StatusCode, raw)
	}
	if got := resp.Header.Get(fiber.HeaderCacheControl); got != "no-store" {
		t.Fatalf("Cache-Control %q", got)
	}
	var out authz.Capabilities
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func (e roleModeEnv) metricsText(t *testing.T) string {
	t.Helper()
	resp, err := e.app.Test(httptest.NewRequest(fiber.MethodGet, "/v1/metrics", nil))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return string(raw)
}

// privilegedRequest is one request per Privileged area, with the role that
// lets it through and the status that means it was let through.
type privilegedRequest struct {
	role, method, path, body string
	allowed                  int
}

func privilegedRequests(eventID string) []privilegedRequest {
	return []privilegedRequest{
		{authz.RoleSeasonManage, fiber.MethodPost, "/v1/seasons", `{"name":"2026"}`, fiber.StatusCreated},
		{authz.RoleEventManage, fiber.MethodPost, "/v1/events", `{"name":"Hack","location":"YTÜ","ownerTeam":"WEBLAB"}`, fiber.StatusCreated},
		{authz.RoleTicketManage, fiber.MethodGet, "/v1/events/" + eventID + "/tickets", "", fiber.StatusOK},
		{authz.RoleGroupsManage, fiber.MethodGet, "/v1/groups", "", fiber.StatusOK},
		{authz.RoleUsersManage, fiber.MethodGet, "/v1/users", "", fiber.StatusOK},
		// The admin erasure request: let through, it stops at the erasure
		// switch, off here.
		{authz.RoleUsersManage, fiber.MethodDelete, "/v1/users/" + uuid.NewString(), "", fiber.StatusServiceUnavailable},
		{authz.RoleMediaManage, fiber.MethodGet, "/v1/media", "", fiber.StatusOK},
		{authz.RoleGithubActivityRead, fiber.MethodGet, "/v1/dashboard/github-activity", "", fiber.StatusOK},
		{authz.RoleCompetitorManage, fiber.MethodGet, "/v1/competitors", "", fiber.StatusOK},
		{authz.RoleCertificateManage, fiber.MethodGet, "/v1/events/" + eventID + "/certificates", "", fiber.StatusOK},
	}
}

func (e roleModeEnv) newEvent(t *testing.T) string {
	t.Helper()
	created := sendJSON(t, e.app, e.token(t, []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}), fiber.MethodPost, "/v1/events",
		`{"name":"Hack","location":"YTÜ","ownerTeam":"WEBLAB"}`)
	if created.status != fiber.StatusCreated {
		t.Fatalf("create event %d %v", created.status, created.body)
	}
	return created.body["id"].(string)
}

// In the roles mode the role lets a person through and the Privileged Group
// alone does not; the capabilities answer says the same.
func TestRolesModeDecidesFromTheRole(t *testing.T) {
	t.Parallel()
	e := newRoleModeEnv(t, authz.RoleModeRoles)
	eventID := e.newEvent(t)
	for _, r := range privilegedRequests(eventID) {
		if got := e.status(t, e.token(t, nil, r.role), r.method, r.path, r.body); got != r.allowed {
			t.Errorf("%s %s with %s: %d", r.method, r.path, r.role, got)
		}
		if got := e.status(t, e.token(t, []string{"/UYELER/YK"}), r.method, r.path, r.body); got != fiber.StatusForbidden {
			t.Errorf("%s %s as YK without roles: %d, want 403", r.method, r.path, got)
		}
		if got := e.status(t, e.serviceToken(t, r.role), r.method, r.path, r.body); got != fiber.StatusForbidden {
			t.Errorf("%s %s as a service account with %s: %d, want 403", r.method, r.path, r.role, got)
		}
	}

	// Guest apply trusts an operator of the Event: the role's holder gets the
	// Ticket back, the YK member without roles the anonymous answer.
	guest := `{"firstName":"Ada","lastName":"Lovelace","email":"ada@example.test"}`
	trusted := sendJSON(t, e.app, e.token(t, nil, authz.RoleEventManage), fiber.MethodPost, "/v1/events/"+eventID+"/applications/guest", guest)
	if trusted.status != fiber.StatusCreated || trusted.body["id"] == nil {
		t.Errorf("guest apply as event:manage: %d %v", trusted.status, trusted.body)
	}
	untrusted := sendJSON(t, e.app, e.token(t, []string{"/UYELER/YK"}), fiber.MethodPost, "/v1/events/"+eventID+"/applications/guest", guest)
	if untrusted.status != fiber.StatusCreated || untrusted.body["id"] != nil || untrusted.body["status"] != "applied" {
		t.Errorf("guest apply as YK without roles: %d %v", untrusted.status, untrusted.body)
	}

	yk := e.capabilities(t, e.token(t, []string{"/UYELER/YK"}))
	if len(yk.Permissions) != 0 || yk.Can["season.write"] || yk.OtherTeams.Can["event.create"] {
		t.Errorf("YK without roles: %+v", yk)
	}
	holder := e.capabilities(t, e.token(t, nil, authz.RoleSeasonManage, authz.RoleEventManage))
	if !slices.Equal(holder.Permissions, []string{authz.RoleEventManage, authz.RoleSeasonManage}) ||
		!holder.Can["season.write"] || !holder.OtherTeams.Can["event.create"] || holder.Can["group.read"] {
		t.Errorf("role holder: %+v", holder)
	}
}

// The default mode is today's: the Group decides, a role alone does not.
func TestGroupsModeIsTodays(t *testing.T) {
	t.Parallel()
	e := newRoleModeEnv(t, authz.RoleModeGroups)
	eventID := e.newEvent(t)
	for _, r := range privilegedRequests(eventID) {
		if got := e.status(t, e.token(t, []string{"/UYELER/YK"}), r.method, r.path, r.body); got != r.allowed {
			t.Errorf("%s %s as YK: %d", r.method, r.path, got)
		}
		if got := e.status(t, e.token(t, nil, r.role), r.method, r.path, r.body); got != fiber.StatusForbidden {
			t.Errorf("%s %s with %s alone: %d, want 403", r.method, r.path, r.role, got)
		}
	}
	if text := e.metricsText(t); !strings.Contains(text, `skylab_authz_role_mode{mode="groups"} 1`) ||
		strings.Contains(text, "skylab_authz_role_disagreements_total{") {
		t.Errorf("metrics:\n%s", text)
	}
}

// The both mode lets either through and counts where they disagree.
func TestBothModeAllowsEitherAndCountsDisagreements(t *testing.T) {
	t.Parallel()
	e := newRoleModeEnv(t, authz.RoleModeBoth)
	eventID := e.newEvent(t)
	for _, r := range privilegedRequests(eventID) {
		for _, token := range []string{e.token(t, []string{"/UYELER/YK"}), e.token(t, nil, r.role)} {
			if got := e.status(t, token, r.method, r.path, r.body); got != r.allowed {
				t.Errorf("%s %s: %d", r.method, r.path, got)
			}
		}
	}
	text := e.metricsText(t)
	for _, want := range []string{
		`skylab_authz_role_mode{mode="both"} 1`,
		`skylab_authz_role_disagreements_total{permission="season:manage",granted_by="group",client="admin"}`,
		`skylab_authz_role_disagreements_total{permission="season:manage",granted_by="role",client="admin"}`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("metrics lack %s:\n%s", want, text)
		}
	}
	if !strings.Contains(e.logs.String(), `"event":"authz_role_disagreement"`) || strings.Contains(e.logs.String(), "@example.test") {
		t.Errorf("logs:\n%s", e.logs.String())
	}
}

// A Privileged member, a Leader, an Owner team member and an ordinary
// member get the answers their real requests get.
func TestCapabilitiesRoute(t *testing.T) {
	t.Parallel()
	e := newRoleModeEnv(t, authz.RoleModeGroups)
	eventID := e.newEvent(t)

	resp, err := e.app.Test(httptest.NewRequest(fiber.MethodGet, "/v1/users/me/capabilities", nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusUnauthorized {
		t.Fatalf("anonymous %d", resp.StatusCode)
	}

	people := map[string]string{
		"privileged": e.token(t, []string{"/UYELER/YK"}),
		"leader":     e.token(t, []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}),
		"member":     e.token(t, []string{"/UYELER/ARGE/WEBLAB"}, "certificate:issue"),
		"ordinary":   e.token(t, []string{"/UYELER"}),
	}
	for name, token := range people {
		c := e.capabilities(t, token)
		weblab := c.OtherTeams.Can
		for _, team := range c.Teams {
			if team.Team == "WEBLAB" {
				weblab = team.Can
			}
		}
		checks := []struct {
			can     bool
			method  string
			path    string
			body    string
			allowed int
		}{
			{c.Can["season.write"], fiber.MethodPost, "/v1/seasons", `{"name":"2026"}`, fiber.StatusCreated},
			{c.Can["group.read"], fiber.MethodGet, "/v1/groups", "", fiber.StatusOK},
			{c.Can["user.read"], fiber.MethodGet, "/v1/users", "", fiber.StatusOK},
			{c.Can["githubActivity.read"], fiber.MethodGet, "/v1/dashboard/github-activity", "", fiber.StatusOK},
			{weblab["ticket.read"], fiber.MethodGet, "/v1/events/" + eventID + "/tickets", "", fiber.StatusOK},
			{weblab["event.update"], fiber.MethodPut, "/v1/events/" + eventID, `{"name":"Hack","location":"Davutpaşa","ownerTeam":"WEBLAB"}`, fiber.StatusOK},
			{weblab["certificate.read"], fiber.MethodGet, "/v1/events/" + eventID + "/certificates", "", fiber.StatusOK},
			{c.OtherTeams.Can["event.create"], fiber.MethodPost, "/v1/events", `{"name":"Jam","location":"YTÜ","ownerTeam":"GAMELAB"}`, fiber.StatusCreated},
		}
		for _, check := range checks {
			got := e.status(t, token, check.method, check.path, check.body)
			if (got == check.allowed) != check.can {
				t.Errorf("%s: %s %s answered %d, capabilities said %v", name, check.method, check.path, got, check.can)
			}
		}
		switch name {
		case "privileged":
			if !slices.Equal(c.Permissions, authz.PermissionRoles()) {
				t.Errorf("privileged permissions %v", c.Permissions)
			}
		case "leader":
			if len(c.Permissions) != 0 || !weblab["event.update"] || weblab["event.assignDoorStaff"] {
				t.Errorf("leader %+v", c)
			}
		case "member":
			if weblab["event.update"] || !weblab["ticket.read"] || !weblab["certificate.issue"] {
				t.Errorf("member WEBLAB %v", weblab)
			}
		case "ordinary":
			for key, v := range c.Can {
				if v {
					t.Errorf("ordinary can %s", key)
				}
			}
		}
	}
}
