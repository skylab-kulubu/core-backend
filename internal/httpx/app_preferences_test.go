package httpx_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/apppref"
	"github.com/skylab-kulubu/core-backend/internal/httpx"
	"github.com/skylab-kulubu/core-backend/internal/testauth"
)

type appPrefEnv struct {
	app    *fiber.App
	keys   *testauth.Bundle
	store  *apppref.MemoryStore
	person string
}

// newAppPrefEnv serves App preferences with clients; "" leaves them off.
func newAppPrefEnv(t *testing.T, clients string) appPrefEnv {
	t.Helper()
	config, err := apppref.ParseClients(clients)
	if err != nil {
		t.Fatal(err)
	}
	keys := testauth.New(t)
	deps := memoryDeps()
	deps.ParseToken = keys.Parse()
	env := appPrefEnv{keys: keys, store: apppref.NewMemoryStore(), person: uuid.NewString()}
	deps.AppPreferences = apppref.NewService(config, env.store)
	env.app = httpx.New(deps)
	return env
}

// token is the person's token issued to client (azp).
func (e appPrefEnv) token(t *testing.T, client string) string {
	t.Helper()
	return e.keys.Token(t, jwt.MapClaims{"sub": e.person, "email": "kisi@example.test", "azp": client})
}

// serviceToken is client's own service account token.
func (e appPrefEnv) serviceToken(t *testing.T, client string) string {
	t.Helper()
	return e.keys.Token(t, jwt.MapClaims{"sub": uuid.NewString(), "azp": client, "client_id": client})
}

type appPrefAnswer struct {
	status int
	header http.Header
	body   map[string]any
}

func (e appPrefEnv) send(t *testing.T, token, method, path, body string, header ...string) appPrefAnswer {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := e.app.Test(req, fiber.TestConfig{Timeout: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	got := map[string]any{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("status %d body %s: %v", resp.StatusCode, raw, err)
		}
	}
	return appPrefAnswer{status: resp.StatusCode, header: resp.Header, body: got}
}

const appPrefRoot = "/v1/users/me/app-preferences"

func TestAppPreferencesOffServeNothing(t *testing.T) {
	e := newAppPrefEnv(t, "")
	for _, tc := range []struct{ method, path string }{
		{fiber.MethodGet, appPrefRoot},
		{fiber.MethodGet, appPrefRoot + "/forms"},
		{fiber.MethodPut, appPrefRoot + "/forms/forms.list"},
		{fiber.MethodDelete, appPrefRoot + "/forms"},
	} {
		if got := e.send(t, e.token(t, "skyforms"), tc.method, tc.path, `{"value":1}`); got.status != fiber.StatusNotFound {
			t.Fatalf("%s %s: %d %v", tc.method, tc.path, got.status, got.body)
		}
	}
}

// Forms writes, reads back and deletes its own keys; the answers are never
// stored on the way; the ETag is the version and If-Match holds it.
func TestAppPreferencesFormsRoundTrip(t *testing.T) {
	e := newAppPrefEnv(t, "forms:skyforms,account:account-center")
	forms := e.token(t, "skyforms")

	got := e.send(t, forms, fiber.MethodGet, appPrefRoot+"/forms", "")
	if got.status != fiber.StatusOK || got.body["app"] != "forms" || len(got.body["items"].(map[string]any)) != 0 || got.header.Get("Cache-Control") != "private, no-store" {
		t.Fatalf("empty: %d %v %v", got.status, got.body, got.header)
	}
	put := e.send(t, forms, fiber.MethodPut, appPrefRoot+"/forms/forms.list", `{"schemaVersion":2,"value":{ "details": false, "sortBy": "updatedAt" }}`)
	if put.status != fiber.StatusOK || put.header.Get("ETag") != `"1"` || put.body["key"] != "forms.list" || put.body["version"] != float64(1) ||
		put.body["schemaVersion"] != float64(2) || put.header.Get("Cache-Control") != "private, no-store" {
		t.Fatalf("put: %d %v %v", put.status, put.body, put.header)
	}
	if e.store.Client(uuid.MustParse(e.person), "forms", "forms.list") != "skyforms" {
		t.Fatal("the writer client was not kept")
	}
	got = e.send(t, forms, fiber.MethodGet, appPrefRoot+"/forms", "")
	item, _ := got.body["items"].(map[string]any)["forms.list"].(map[string]any)
	if value, _ := item["value"].(map[string]any); value["details"] != false || value["sortBy"] != "updatedAt" || item["version"] != float64(1) || item["updatedAt"] == "" {
		t.Fatalf("get: %v", got.body)
	}

	if got := e.send(t, forms, fiber.MethodPut, appPrefRoot+"/forms/forms.list", `{"value":true}`, "If-Match", `"7"`); got.status != fiber.StatusPreconditionFailed || got.body["code"] != "app_preference_conflict" {
		t.Fatalf("stale If-Match: %d %v", got.status, got.body)
	}
	if got := e.send(t, forms, fiber.MethodPut, appPrefRoot+"/forms/forms.list", `{"value":true}`, "If-Match", `W/"x"`); got.status != fiber.StatusPreconditionFailed {
		t.Fatalf("unreadable If-Match: %d %v", got.status, got.body)
	}
	if got := e.send(t, forms, fiber.MethodPut, appPrefRoot+"/forms/forms.list", `{"value":true}`, "If-Match", `"1"`); got.status != fiber.StatusOK || got.header.Get("ETag") != `"2"` || got.body["schemaVersion"] != float64(1) {
		t.Fatalf("matching If-Match: %d %v %v", got.status, got.body, got.header)
	}

	if got := e.send(t, forms, fiber.MethodDelete, appPrefRoot+"/forms/forms.list", ""); got.status != fiber.StatusNoContent || got.header.Get("Cache-Control") != "private, no-store" {
		t.Fatalf("delete: %d %v", got.status, got.body)
	}
	if got := e.send(t, forms, fiber.MethodDelete, appPrefRoot+"/forms/forms.list", ""); got.status != fiber.StatusNoContent {
		t.Fatalf("delete again: %d %v", got.status, got.body)
	}
}

func TestAppPreferencesRefuseOtherClientsAndServiceAccounts(t *testing.T) {
	e := newAppPrefEnv(t, "forms:skyforms,panel:admin,account:account-center")
	forms := e.token(t, "skyforms")
	for _, tc := range []struct {
		name, token, method, path string
		status                    int
		code                      string
	}{
		{"forms reads panel", forms, fiber.MethodGet, appPrefRoot + "/panel", fiber.StatusForbidden, "app_preference_wrong_client"},
		{"forms writes panel", forms, fiber.MethodPut, appPrefRoot + "/panel/density", fiber.StatusForbidden, "app_preference_wrong_client"},
		{"forms resets panel", forms, fiber.MethodDelete, appPrefRoot + "/panel", fiber.StatusForbidden, "app_preference_wrong_client"},
		{"forms reads the summary", forms, fiber.MethodGet, appPrefRoot, fiber.StatusForbidden, "app_preference_wrong_client"},
		{"unknown app", forms, fiber.MethodGet, appPrefRoot + "/skymail", fiber.StatusNotFound, "app_preference_unknown_app"},
		{"the site", e.token(t, "frontend-main"), fiber.MethodGet, appPrefRoot + "/forms", fiber.StatusForbidden, "app_preference_wrong_client"},
		{"account center reads forms", e.token(t, "account-center"), fiber.MethodGet, appPrefRoot + "/forms", fiber.StatusForbidden, "app_preference_wrong_client"},
		{"account center writes forms", e.token(t, "account-center"), fiber.MethodPut, appPrefRoot + "/forms/forms.list", fiber.StatusForbidden, "app_preference_wrong_client"},
		{"forms service account", e.serviceToken(t, "skyforms"), fiber.MethodGet, appPrefRoot + "/forms", fiber.StatusForbidden, "app_preference_person_only"},
		{"account center service account", e.serviceToken(t, "account-center"), fiber.MethodGet, appPrefRoot, fiber.StatusForbidden, "app_preference_person_only"},
	} {
		if got := e.send(t, tc.token, tc.method, tc.path, `{"value":1}`); got.status != tc.status || got.body["code"] != tc.code {
			t.Fatalf("%s: %d %v", tc.name, got.status, got.body)
		}
	}
	if got := e.send(t, "", fiber.MethodGet, appPrefRoot+"/forms", ""); got.status != fiber.StatusUnauthorized {
		t.Fatalf("no token: %d", got.status)
	}
}

func TestAppPreferencesKeepTheLimits(t *testing.T) {
	e := newAppPrefEnv(t, "forms:skyforms")
	forms := e.token(t, "skyforms")
	for _, tc := range []struct {
		name, path, body string
		status           int
		code             string
	}{
		{"body over 8 KB", "/forms/forms.list", `{"value":"` + strings.Repeat("a", apppref.MaxBodyBytes) + `"}`, fiber.StatusRequestEntityTooLarge, "app_preference_body_too_large"},
		{"value over 4 KB", "/forms/forms.list", `{"value":"` + strings.Repeat("a", apppref.MaxValueBytes) + `"}`, fiber.StatusUnprocessableEntity, "app_preference_value_too_large"},
		{"key outside the names", "/forms/Forms.List", `{"value":1}`, fiber.StatusUnprocessableEntity, "app_preference_invalid_key"},
		{"null value", "/forms/forms.list", `{"value":null}`, fiber.StatusBadRequest, "app_preference_invalid"},
		{"no value", "/forms/forms.list", `{"schemaVersion":1}`, fiber.StatusBadRequest, "app_preference_invalid"},
		{"unknown field", "/forms/forms.list", `{"value":1,"owner":"x"}`, fiber.StatusBadRequest, "app_preference_invalid"},
		{"schemaVersion zero", "/forms/forms.list", `{"value":1,"schemaVersion":0}`, fiber.StatusBadRequest, "app_preference_invalid"},
		{"not JSON", "/forms/forms.list", `value=1`, fiber.StatusBadRequest, "app_preference_invalid"},
	} {
		if got := e.send(t, forms, fiber.MethodPut, appPrefRoot+tc.path, tc.body); got.status != tc.status || got.body["code"] != tc.code {
			t.Fatalf("%s: %d %v", tc.name, got.status, got.body)
		}
	}
}

// Account center sees which apps hold a preference, never a value, and
// resets one.
func TestAppPreferencesAccountCenterSummaryAndReset(t *testing.T) {
	e := newAppPrefEnv(t, "forms:skyforms,account:account-center")
	forms, center := e.token(t, "skyforms"), e.token(t, "account-center")
	for _, key := range []string{"forms.list", "share-format"} {
		if got := e.send(t, forms, fiber.MethodPut, appPrefRoot+"/forms/"+key, `{"value":{"gizli":"deger"}}`); got.status != fiber.StatusOK {
			t.Fatalf("put %s: %d %v", key, got.status, got.body)
		}
	}
	got := e.send(t, center, fiber.MethodGet, appPrefRoot, "")
	items, _ := got.body["items"].([]any)
	if got.status != fiber.StatusOK || len(items) != 1 {
		t.Fatalf("summary: %d %v", got.status, got.body)
	}
	row := items[0].(map[string]any)
	if row["app"] != "forms" || row["keys"] != float64(2) || row["updatedAt"] == "" || len(row) != 3 {
		t.Fatalf("summary row: %v", row)
	}
	if got := e.send(t, center, fiber.MethodDelete, appPrefRoot+"/forms", ""); got.status != fiber.StatusNoContent {
		t.Fatalf("reset: %d %v", got.status, got.body)
	}
	if got := e.send(t, forms, fiber.MethodGet, appPrefRoot+"/forms", ""); len(got.body["items"].(map[string]any)) != 0 {
		t.Fatalf("after reset: %v", got.body)
	}
	if got := e.send(t, center, fiber.MethodGet, appPrefRoot, ""); len(got.body["items"].([]any)) != 0 {
		t.Fatalf("summary after reset: %v", got.body)
	}
}

// 60 writes a minute a person; another person has a budget of their own.
func TestAppPreferencesWriteBudget(t *testing.T) {
	e := newAppPrefEnv(t, "forms:skyforms")
	forms := e.token(t, "skyforms")
	for i := range apppref.WritesPerMinute {
		if got := e.send(t, forms, fiber.MethodPut, appPrefRoot+"/forms/forms.list", `{"value":1}`); got.status != fiber.StatusOK {
			t.Fatalf("write %d: %d %v", i, got.status, got.body)
		}
	}
	if got := e.send(t, forms, fiber.MethodDelete, appPrefRoot+"/forms/forms.list", ""); got.status != fiber.StatusTooManyRequests || got.body["code"] != "app_preference_rate_limited" {
		t.Fatalf("61st write: %d %v", got.status, got.body)
	}
	if got := e.send(t, forms, fiber.MethodGet, appPrefRoot+"/forms", ""); got.status != fiber.StatusOK {
		t.Fatalf("a read is not a write: %d", got.status)
	}
	other := e.keys.Token(t, jwt.MapClaims{"sub": uuid.NewString(), "email": "baska@example.test", "azp": "skyforms"})
	if got := e.send(t, other, fiber.MethodPut, appPrefRoot+"/forms/forms.list", `{"value":1}`); got.status != fiber.StatusOK {
		t.Fatalf("another person: %d %v", got.status, got.body)
	}
}

func TestAppPreferencesMetricsOnTheMetricsRoute(t *testing.T) {
	e := newAppPrefEnv(t, "forms:skyforms")
	if got := e.send(t, e.token(t, "skyforms"), fiber.MethodPut, appPrefRoot+"/forms/forms.list", `{"value":1}`); got.status != fiber.StatusOK {
		t.Fatalf("put: %d", got.status)
	}
	resp, err := e.app.Test(httptest.NewRequest(fiber.MethodGet, "/v1/metrics", nil), fiber.TestConfig{Timeout: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(raw), `skylab_app_preferences_requests_total{app="forms",op="put",outcome="ok"} 1`+"\n") || strings.Contains(string(raw), e.person) {
		t.Fatalf("metrics:\n%s", raw)
	}
}
