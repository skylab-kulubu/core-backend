package httpx_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
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
	"github.com/skylab-kulubu/core-backend/internal/user"
)

type audienceEnv struct {
	app    *fiber.App
	keys   *testauth.Bundle
	person uuid.UUID
}

// newAudienceEnv is the app with a WEBLAB member; enabled false leaves the
// preferences (and so the audience) off.
func newAudienceEnv(t *testing.T, enabled bool) audienceEnv {
	t.Helper()
	keys := testauth.New(t)
	dir := identity.NewMemory()
	for _, g := range []identity.Group{
		{ID: "g-uyeler", Name: "UYELER", Path: "/UYELER"},
		{ID: "g-arge", Name: "ARGE", Path: "/UYELER/ARGE"},
		{ID: "g-weblab", Name: "WEBLAB", Path: "/UYELER/ARGE/WEBLAB", Attributes: map[string]string{"public_listing": "true"}},
	} {
		dir.PutGroup(g)
	}
	person := uuid.New()
	dir.PutUser(identity.Person{ID: person, Email: "uye@example.test", FirstName: "Ad", LastName: "Soyad"})
	if err := dir.AddMember(context.Background(), "g-weblab", person); err != nil {
		t.Fatal(err)
	}
	deps := memoryDepsOver(dir, identity.Options{})
	deps.ParseToken = keys.Parse()
	config := commpref.Config{}
	if enabled {
		config = commpref.TestConfig(bytes.Repeat([]byte{7}, 32), "https://api.example.test")
	}
	prefs := commpref.NewService(config, commpref.NewMemoryStore(), dir, authz.DefaultPrivilegedRoots())
	deps.CommunicationPreferences = prefs
	deps.Audience = commpref.NewAudience(prefs, dir, user.NewMemoryStore(), false)
	return audienceEnv{app: httpx.New(deps), keys: keys, person: person}
}

func (e audienceEnv) service(t *testing.T, roles ...string) string {
	t.Helper()
	return e.keys.Token(t, jwt.MapClaims{
		"sub": uuid.NewString(), "azp": "skymail", "client_id": "skymail",
		"resource_access": map[string]any{"core": map[string]any{"roles": roles}},
	})
}

type audienceAnswer struct {
	status int
	header http.Header
	raw    []byte
}

func audienceCall(t *testing.T, app *fiber.App, token, method, path, body string) audienceAnswer {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := app.Test(req, fiber.TestConfig{Timeout: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return audienceAnswer{status: resp.StatusCode, header: resp.Header, raw: raw}
}

const weblabDuyuru = `{"type":"teams","teams":["/UYELER/ARGE/WEBLAB"],"newsKind":"duyuru"}`

// Only a service account holding audience:read may ask; a person's token is
// refused even with the role in it.
func TestAudienceRouteAuthorization(t *testing.T) {
	env := newAudienceEnv(t, true)
	personWithRole := env.keys.Token(t, jwt.MapClaims{
		"sub": env.person.String(), "azp": "admin", "groups": []string{"/YK"},
		"resource_access": map[string]any{"core": map[string]any{"roles": []string{commpref.AudienceRole}}},
	})
	for name, c := range map[string]struct {
		token, code string
	}{
		"person":             {groupToken(t, env.keys, env.person.String(), "/UYELER/ARGE/WEBLAB"), "audience_service_only"},
		"person with role":   {personWithRole, "audience_service_only"},
		"service no role":    {env.service(t, "consent:audience:read"), "audience_role_missing"},
		"service other role": {env.service(t), "audience_role_missing"},
	} {
		got := audienceCall(t, env.app, c.token, fiber.MethodPost, "/v1/audiences/resolve", weblabDuyuru)
		if got.status != fiber.StatusForbidden || !strings.Contains(string(got.raw), c.code) || strings.Contains(string(got.raw), "uye@example.test") {
			t.Fatalf("%s: %d %s", name, got.status, got.raw)
		}
	}

	got := audienceCall(t, env.app, env.service(t, commpref.AudienceRole), fiber.MethodPost, "/v1/audiences/resolve", weblabDuyuru)
	if got.status != fiber.StatusOK || got.header.Get(fiber.HeaderCacheControl) != "no-store" {
		t.Fatalf("skymail: %d %s", got.status, got.raw)
	}
	var answer struct {
		Items   []commpref.Recipient     `json:"items"`
		Next    *string                  `json:"next"`
		Counts  commpref.AudienceCounts  `json:"counts"`
		Skipped commpref.AudienceSkipped `json:"skipped"`
	}
	if err := json.Unmarshal(got.raw, &answer); err != nil {
		t.Fatal(err)
	}
	if len(answer.Items) != 1 || answer.Next != nil || answer.Counts.Total != 1 || answer.Items[0].UserID != env.person ||
		answer.Items[0].Email != "uye@example.test" || answer.Items[0].Name != "Ad Soyad" ||
		!strings.HasPrefix(answer.Items[0].UnsubscribeURL, "https://api.example.test/v1/communication-preferences/unsubscribe?t=") {
		t.Fatalf("answer %s", got.raw)
	}

	count := audienceCall(t, env.app, env.service(t, commpref.AudienceRole), fiber.MethodPost, "/v1/audiences/resolve?countOnly=true", weblabDuyuru)
	if count.status != fiber.StatusOK || strings.Contains(string(count.raw), "items") || strings.Contains(string(count.raw), "uye@") ||
		!strings.Contains(string(count.raw), `"total":1`) {
		t.Fatalf("countOnly: %d %s", count.status, count.raw)
	}
}

func TestAudienceRouteRefusals(t *testing.T) {
	env := newAudienceEnv(t, true)
	skymail := env.service(t, commpref.AudienceRole)
	for _, c := range []struct {
		path, body, code string
		status           int
	}{
		{"/v1/audiences/resolve", `{"type":"teams","teams":["/UYELER/ARGE/YOK"],"newsKind":"duyuru"}`, "audience_team_unknown", fiber.StatusBadRequest},
		{"/v1/audiences/resolve", `{"type":"teams","teams":["g-weblab"],"newsKind":"duyuru","visibility":"uyeler"}`, "audience_visibility_not_enabled", fiber.StatusBadRequest},
		{"/v1/audiences/resolve", `{"type":"teams","teams":["g-weblab"],"newsKind":"duyuru","extra":1}`, "audience_invalid", fiber.StatusBadRequest},
		{"/v1/audiences/resolve", `{"type":"members","newsKind":"bulten"}`, "audience_invalid", fiber.StatusBadRequest},
		{"/v1/audiences/resolve?limit=501", weblabDuyuru, "audience_invalid", fiber.StatusBadRequest},
		{"/v1/audiences/resolve?after=nope", weblabDuyuru, "audience_invalid", fiber.StatusBadRequest},
		{"/v1/audiences/resolve?countOnly=yes", weblabDuyuru, "audience_invalid", fiber.StatusBadRequest},
	} {
		got := audienceCall(t, env.app, skymail, fiber.MethodPost, c.path, c.body)
		if got.status != c.status || !strings.Contains(string(got.raw), c.code) {
			t.Fatalf("%s %s: %d %s", c.path, c.body, got.status, got.raw)
		}
	}
	off := newAudienceEnv(t, false)
	got := audienceCall(t, off.app, off.service(t, commpref.AudienceRole), fiber.MethodPost, "/v1/audiences/resolve", weblabDuyuru)
	if got.status != fiber.StatusServiceUnavailable || !strings.Contains(string(got.raw), "audience_unavailable") {
		t.Fatalf("off: %d %s", got.status, got.raw)
	}
}
