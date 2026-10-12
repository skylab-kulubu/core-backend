package httpx_test

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/httpx"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/teamcode"
	"github.com/skylab-kulubu/core-backend/internal/testauth"
)

// groupNamesEnv is the assembled app over a realm like production's, with
// real signed tokens.
type groupNamesEnv struct {
	app    *fiber.App
	keys   *testauth.Bundle
	ledger *teamcode.MemoryLedger
}

func newGroupNamesEnv(t *testing.T) *groupNamesEnv {
	t.Helper()
	keys := testauth.New(t)
	dir := identity.NewMemory()
	for _, g := range []identity.Group{
		{ID: "g-uyeler", Name: "UYELER", Path: "/UYELER"},
		{ID: "g-yk", Name: "YK", Path: "/UYELER/YK"},
		{ID: "g-arge", Name: "ARGE", Path: "/UYELER/ARGE"},
		{ID: "g-weblab", Name: "WEBLAB", Path: "/UYELER/ARGE/WEBLAB"},
		{ID: "g-skysec", Name: "SKYSEC", Path: "/UYELER/ARGE/SKYSEC"},
	} {
		dir.PutGroup(g)
	}
	ledger := teamcode.NewMemoryLedger()
	deps := memoryDepsOver(dir, identity.Options{TeamCodes: ledger})
	deps.ParseToken = keys.Parse()
	return &groupNamesEnv{app: httpx.New(deps), keys: keys, ledger: ledger}
}

func (e *groupNamesEnv) token(t *testing.T, groups ...string) string {
	t.Helper()
	return e.keys.Token(t, jwt.MapClaims{
		"sub": uuid.NewString(), "email": "p@example.test", "given_name": "P", "family_name": "Q", "groups": groups,
	})
}

// statusOf sends a request and answers its status, whatever the body.
func statusOf(t *testing.T, app *fiber.App, token, method, path, body string) int {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := app.Test(req, fiber.TestConfig{Timeout: 0})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

func wantProblem(t *testing.T, got jsonResponse, status int, code string) {
	t.Helper()
	if got.status != status || got.body["code"] != code {
		t.Fatalf("status %d body %v; want %d %s", got.status, got.body, status, code)
	}
	if detail, _ := got.body["detail"].(string); detail == "" {
		t.Fatalf("no detail: %v", got.body)
	}
}

// POST and PATCH /v1/groups answer a refused name with its problem code,
// and write nothing.
func TestGroupNamesHTTP(t *testing.T) {
	t.Parallel()
	e := newGroupNamesEnv(t)
	yk := e.token(t, "/UYELER/YK")
	post := func(body string) jsonResponse {
		return sendJSON(t, e.app, yk, fiber.MethodPost, "/v1/groups", body)
	}
	patch := func(id, body string) jsonResponse {
		return sendJSON(t, e.app, yk, fiber.MethodPatch, "/v1/groups/"+id, body)
	}

	wantProblem(t, post(`{"name":"admin","parentId":"g-weblab"}`), fiber.StatusUnprocessableEntity, teamcode.InvalidName)
	wantProblem(t, post(`{"name":"ADMİN","parentId":"g-weblab"}`), fiber.StatusUnprocessableEntity, teamcode.InvalidName)
	wantProblem(t, post(`{"name":"ADMIN","parentId":"g-weblab"}`), fiber.StatusUnprocessableEntity, teamcode.ReservedName)
	wantProblem(t, post(`{"name":"LIDERLER","parentId":"g-arge"}`), fiber.StatusUnprocessableEntity, teamcode.ReservedName)
	wantProblem(t, post(`{"name":"SKYSEC","parentId":"g-weblab"}`), fiber.StatusConflict, teamcode.NameTaken)
	e.ledger.Hold("AIRLAB")
	wantProblem(t, post(`{"name":"AIRLAB","parentId":"g-arge"}`), fiber.StatusConflict, teamcode.NameTaken)
	if got := post(`{"name":"LIDERLER","parentId":"g-weblab"}`); got.status != fiber.StatusCreated || got.body["path"] != "/UYELER/ARGE/WEBLAB/LIDERLER" {
		t.Fatalf("WEBLAB's Leader group: %d %v", got.status, got.body)
	}
	if got := post(`{"name":"AI-LAB","parentId":"g-arge"}`); got.status != fiber.StatusCreated {
		t.Fatalf("a new team: %d %v", got.status, got.body)
	}

	wantProblem(t, patch("g-yk", `{"name":"YK-ESKI"}`), fiber.StatusConflict, teamcode.PrivilegedRoot)
	wantProblem(t, patch("g-weblab", `{"name":"ADMIN"}`), fiber.StatusUnprocessableEntity, teamcode.ReservedName)
	wantProblem(t, patch("g-weblab", `{"name":"SKYSEC"}`), fiber.StatusConflict, teamcode.NameTaken)
	// The panel sends the name it shows with every save: no rename.
	if got := patch("g-weblab", `{"name":"WEBLAB","attributes":{"public_listing":"true"}}`); got.status != fiber.StatusOK {
		t.Fatalf("attribute save: %d %v", got.status, got.body)
	}
	if got := patch("g-weblab", `{"name":"WEB"}`); got.status != fiber.StatusOK || got.body["name"] != "WEB" {
		t.Fatalf("rename: %d %v", got.status, got.body)
	}
	// The old code is retired: no new Group takes WEBLAB's records.
	wantProblem(t, post(`{"name":"WEBLAB","parentId":"g-arge"}`), fiber.StatusConflict, teamcode.NameTaken)
}

// The attack ticket 01 closes: membership of a Group named ADMIN, YK or DK
// outside the roots grants nothing Privileged, through the whole app.
func TestFakePrivilegedGroupsHTTP(t *testing.T) {
	t.Parallel()
	e := newGroupNamesEnv(t)
	for _, groups := range [][]string{
		{"/UYELER/ARGE/WEBLAB/ADMIN"},
		{"/UYELER/ARGE/YK"},
		{"/UYELER/ARGE/WEBLAB/DK/LIDERLER"},
		{"/YKX"},
		{"/A/ADMIN/B"},
	} {
		token := e.token(t, groups...)
		for _, req := range []struct{ method, path, body string }{
			{fiber.MethodGet, "/v1/groups", ""},
			{fiber.MethodPost, "/v1/groups", `{"name":"NEWLAB","parentId":"g-arge"}`},
			{fiber.MethodGet, "/v1/users", ""},
			{fiber.MethodPost, "/v1/seasons", `{"name":"2026"}`},
		} {
			if got := statusOf(t, e.app, token, req.method, req.path, req.body); got != fiber.StatusForbidden {
				t.Errorf("%v %s %s: status %d", groups, req.method, req.path, got)
			}
		}
		got := sendJSON(t, e.app, token, fiber.MethodGet, "/v1/users/me/capabilities", "")
		if permissions, _ := got.body["permissions"].([]any); got.status != fiber.StatusOK || len(permissions) != 0 {
			t.Errorf("%v: capabilities %d %v", groups, got.status, got.body["permissions"])
		}
	}
	// The real root still is.
	yk := e.token(t, "/UYELER/YK")
	if got := statusOf(t, e.app, yk, fiber.MethodGet, "/v1/groups", ""); got != fiber.StatusOK {
		t.Fatalf("YK lists Groups: %d", got)
	}
	if got := statusOf(t, e.app, yk, fiber.MethodPost, "/v1/groups", `{"name":"NEWLAB","parentId":"g-arge"}`); got != fiber.StatusCreated {
		t.Fatalf("YK creates a Group: %d", got)
	}
}
