package httpx_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/commpref"
)

const trackingResolvePath = "/internal/v1/communication-preferences/tracking:resolve"

type trackingResult struct {
	Sub     string  `json:"sub"`
	Allowed bool    `json:"allowed"`
	Reason  *string `json:"reason"`
}

// SkyMail's question, as its client sends it: the colon is literal, every
// sub answered once in order, and the route works without the link key.
func TestTrackingResolveRoute(t *testing.T) {
	for _, keyed := range []bool{true, false} {
		env := newAudienceEnv(t, keyed)
		stranger := uuid.New()
		body := fmt.Sprintf(`{"subs":[%q,%q]}`, env.person, stranger)
		got := audienceCall(t, env.app, env.service(t, commpref.TrackingResolveRole), fiber.MethodPost, trackingResolvePath, body)
		if got.status != fiber.StatusOK || got.header.Get(fiber.HeaderCacheControl) != "no-store" {
			t.Fatalf("keyed=%v: %d %s", keyed, got.status, got.raw)
		}
		var answer struct {
			Results []trackingResult `json:"results"`
		}
		if err := json.Unmarshal(got.raw, &answer); err != nil {
			t.Fatal(err)
		}
		if len(answer.Results) != 2 || answer.Results[0].Sub != env.person.String() || !answer.Results[0].Allowed || answer.Results[0].Reason != nil ||
			answer.Results[1].Sub != stranger.String() || answer.Results[1].Allowed || answer.Results[1].Reason == nil || *answer.Results[1].Reason != "not_member" ||
			!strings.Contains(string(got.raw), `"reason":null`) {
			t.Fatalf("keyed=%v: %s", keyed, got.raw)
		}
		// The colon is no parameter: another verb is no route.
		if other := audienceCall(t, env.app, env.service(t, commpref.TrackingResolveRole), fiber.MethodPost,
			"/internal/v1/communication-preferences/tracking:other", body); other.status != fiber.StatusNotFound {
			t.Fatalf("keyed=%v other verb: %d %s", keyed, other.status, other.raw)
		}
	}
}

// Only a service account with communication-preferences:read; a person's
// token is refused even with the role, audience:read is not enough, and no
// token is 401.
func TestTrackingResolveAuthorization(t *testing.T) {
	env := newAudienceEnv(t, true)
	body := fmt.Sprintf(`{"subs":[%q]}`, env.person)
	personWithRole := env.keys.Token(t, jwt.MapClaims{
		"sub": env.person.String(), "azp": "admin", "groups": []string{"/YK"},
		"resource_access": map[string]any{"core": map[string]any{"roles": []string{commpref.TrackingResolveRole}}},
	})
	for name, c := range map[string]struct {
		token, code string
	}{
		"person":             {groupToken(t, env.keys, env.person.String(), "/UYELER/ARGE/WEBLAB"), "tracking_resolve_service_only"},
		"person with role":   {personWithRole, "tracking_resolve_service_only"},
		"service audience":   {env.service(t, commpref.AudienceRole), "tracking_resolve_role_missing"},
		"service other role": {env.service(t, "core:communication-preferences:read"), "tracking_resolve_role_missing"},
	} {
		got := audienceCall(t, env.app, c.token, fiber.MethodPost, trackingResolvePath, body)
		if got.status != fiber.StatusForbidden || !strings.Contains(string(got.raw), c.code) {
			t.Fatalf("%s: %d %s", name, got.status, got.raw)
		}
	}
	if got := audienceCall(t, env.app, "", fiber.MethodPost, trackingResolvePath, body); got.status != fiber.StatusUnauthorized {
		t.Fatalf("anonymous: %d %s", got.status, got.raw)
	}
}

// A request that came through the public edge (it carries a forwarding
// header) is 404 before its token is read, whatever it carries.
func TestTrackingResolveIsNotOnTheEdge(t *testing.T) {
	env := newAudienceEnv(t, true)
	body := fmt.Sprintf(`{"subs":[%q]}`, env.person)
	for _, header := range []string{"X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "Forwarded", "X-Real-Ip"} {
		for _, token := range []string{env.service(t, commpref.TrackingResolveRole), ""} {
			req := httptest.NewRequest(fiber.MethodPost, trackingResolvePath, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
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
			if resp.StatusCode != fiber.StatusNotFound || strings.Contains(string(raw), "allowed") {
				t.Fatalf("%s token=%v: %d %s", header, token != "", resp.StatusCode, raw)
			}
		}
	}
	// The guard is the internal prefix's only: the public routes still
	// answer through the edge.
	req := httptest.NewRequest(fiber.MethodGet, "/v1/health", nil)
	req.Header.Set("X-Forwarded-For", "203.0.113.7")
	if resp, err := env.app.Test(req, fiber.TestConfig{Timeout: 0}); err != nil || resp.StatusCode != fiber.StatusNoContent {
		t.Fatalf("health through the edge: %v %v", resp, err)
	}
}

func TestTrackingResolveRefusesAMalformedQuestion(t *testing.T) {
	env := newAudienceEnv(t, true)
	skymail := env.service(t, commpref.TrackingResolveRole)
	id := uuid.New().String()
	many := make([]string, commpref.MaxTrackingSubjects+1)
	for i := range many {
		many[i] = fmt.Sprintf("%q", uuid.New())
	}
	for _, body := range []string{
		``, `[]`, `{}`, `{"subs":null}`, `{"subs":"x"}`, `{"subs":["nope"]}`,
		fmt.Sprintf(`{"subs":[%q]}`, strings.ToUpper(id)),
		fmt.Sprintf(`{"subs":[%q]}`, "{"+id+"}"),
		fmt.Sprintf(`{"subs":[%q,%q]}`, id, id),
		fmt.Sprintf(`{"subs":[%q],"x":1}`, id),
		`{"subs":["00000000-0000-0000-0000-000000000000"]}`,
		`{"subs":[` + strings.Join(many, ",") + `]}`,
	} {
		got := audienceCall(t, env.app, skymail, fiber.MethodPost, trackingResolvePath, body)
		if got.status != fiber.StatusBadRequest || !strings.Contains(string(got.raw), "tracking_resolve_invalid") {
			t.Fatalf("%.80s: %d %s", body, got.status, got.raw)
		}
	}
	if got := audienceCall(t, env.app, skymail, fiber.MethodPost, trackingResolvePath, `{"subs":[]}`); got.status != fiber.StatusOK || !strings.Contains(string(got.raw), `"results":[]`) {
		t.Fatalf("none: %d %s", got.status, got.raw)
	}
}

// The Account center's switch on the preference routes: the screen carries
// it; PUT {"tracking":{"enabled":false}} turns it off and queues the
// withdrawal; it takes true and false only.
func TestTrackingSwitchRoute(t *testing.T) {
	e := newPrefEnv(t, true)
	got := sendJSON(t, e.app, e.token(t), fiber.MethodGet, "/v1/users/me/communication-preferences", "")
	if tracking, _ := got.body["tracking"].(map[string]any); got.status != fiber.StatusOK || tracking["enabled"] != true || tracking["effective"] != true || tracking["mode"] != "opt_out" {
		t.Fatalf("screen %d %v", got.status, got.body)
	}
	put := sendJSON(t, e.app, e.token(t), fiber.MethodPut, "/v1/users/me/communication-preferences", `{"tracking":{"enabled":false}}`)
	if tracking, _ := put.body["tracking"].(map[string]any); put.status != fiber.StatusOK || tracking["enabled"] != false || tracking["effective"] != false {
		t.Fatalf("off %d %v", put.status, put.body)
	}
	if w := e.store.Withdrawals(); len(w) != 1 || w[0].Subject != e.person {
		t.Fatalf("withdrawals %+v", w)
	}
	both := sendJSON(t, e.app, e.token(t), fiber.MethodPut, "/v1/users/me/communication-preferences",
		`{"items":[{"category":"club:haber","channel":"email","enabled":false}],"tracking":{"enabled":true}}`)
	if tracking, _ := both.body["tracking"].(map[string]any); both.status != fiber.StatusOK || tracking["enabled"] != true {
		t.Fatalf("both %d %v", both.status, both.body)
	}
	if rows, _ := e.store.List(context.Background(), e.person); len(rows) != 2 {
		t.Fatalf("rows %+v", rows)
	}
	for _, body := range []string{
		`{}`, `{"tracking":{}}`, `{"tracking":{"enabled":null}}`, `{"tracking":{"enabled":"no"}}`, `{"tracking":null}`,
		`{"tracking":{"enabled":false,"x":1}}`,
		`{"items":[{"category":"tracking","channel":"email","enabled":false}]}`,
	} {
		if got := sendJSON(t, e.app, e.token(t), fiber.MethodPut, "/v1/users/me/communication-preferences", body); got.status != fiber.StatusBadRequest ||
			got.body["code"] != "communication_preference_invalid" {
			t.Fatalf("%s: %d %v", body, got.status, got.body)
		}
	}
}
