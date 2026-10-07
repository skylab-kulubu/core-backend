package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/editablesites"
	"github.com/skylab-kulubu/core-backend/internal/identity"
)

var capabilitySites = []editablesites.Site{
	{ClientID: "frontend-main", Name: "Ana site", URL: "https://yildizskylab.com"},
	{ClientID: "frontend-arge", Name: "Ar-Ge", URL: "https://arge.yildizskylab.com"},
	{ClientID: "frontend-artlab", Name: "ARTLAB", URL: "https://artlab.yildizskylab.com"},
	// Not in the realm yet: nobody edits it.
	{ClientID: "frontend-skydays", Name: "SkyDays", URL: "https://skydays.yildizskylab.com"},
}

// siteRoles is Keycloak's effective client roles by person and client, or a
// Keycloak that is down.
type siteRoles struct {
	held  map[uuid.UUID][]string // person → clients with cms:access
	down  bool
	calls atomic.Int64
}

func (s *siteRoles) EffectiveClientRoles(_ context.Context, userID uuid.UUID, clientID string) ([]string, error) {
	s.calls.Add(1)
	if s.down {
		return nil, errors.New("identity: keycloak effective client roles request failed: connection refused")
	}
	if clientID == "frontend-skydays" {
		return nil, identity.ErrNotFound
	}
	if slices.Contains(s.held[userID], clientID) {
		return []string{"cms:access", "content:read", "content:write"}, nil
	}
	return []string{}, nil
}

func capabilitiesApp(t *testing.T, ident authn.Identity, sites EditableSitesSource, logs *bytes.Buffer) *fiber.App {
	t.Helper()
	h := NewCapabilitiesHandler(authz.NewAuthorizer(authz.DefaultPolicy()), sites)
	h.logger = log.New(logs, "", 0)
	app := fiber.New(fiber.Config{ErrorHandler: ErrorHandler})
	app.Use(func(c fiber.Ctx) error {
		c.Locals(authn.LocalsIdentity, ident)
		return c.Next()
	})
	app.Get("/v1/users/me/capabilities", h.Get)
	return app
}

// getCapabilities answers the raw JSON object of the route.
func getCapabilities(t *testing.T, app *fiber.App) map[string]json.RawMessage {
	t.Helper()
	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/v1/users/me/capabilities", nil), fiber.TestConfig{Timeout: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status %d %s", resp.StatusCode, raw)
	}
	var out map[string]json.RawMessage
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func editableClientIDs(t *testing.T, body map[string]json.RawMessage) []string {
	t.Helper()
	raw, ok := body["editableSites"]
	if !ok {
		t.Fatalf("no editableSites in %v", body)
	}
	var sites []editablesites.Site
	if err := json.Unmarshal(raw, &sites); err != nil || sites == nil {
		t.Fatalf("editableSites = %s, want a list", raw)
	}
	out := []string{}
	for _, s := range sites {
		out = append(out, s.ClientID)
	}
	return out
}

// editableSites sits beside the authorizer's answer, which keeps every key
// it had: a Privileged member, a site's Leader and a plain member each get
// the sites Keycloak says they hold cms:access on.
func TestCapabilitiesListEditableSites(t *testing.T) {
	t.Parallel()

	admin, leader, member := uuid.New(), uuid.New(), uuid.New()
	roles := &siteRoles{held: map[uuid.UUID][]string{
		admin:  {"frontend-main", "frontend-arge", "frontend-artlab", "frontend-skydays"},
		leader: {"frontend-artlab"},
	}}
	sites := editablesites.New(capabilitySites, roles, editablesites.Options{})
	for _, tc := range []struct {
		name  string
		ident authn.Identity
		want  []string
	}{
		{"privileged", authn.Identity{ID: admin, Groups: []string{"/ADMIN"}}, []string{"frontend-main", "frontend-arge", "frontend-artlab"}},
		{"site leader", authn.Identity{ID: leader, Groups: []string{"/UYELER/ARGE/AIRLAB/LIDERLER"}}, []string{"frontend-artlab"}},
		{"member", authn.Identity{ID: member, Groups: []string{"/UYELER/ARGE/AIRLAB"}}, []string{}},
	} {
		var logs bytes.Buffer
		body := getCapabilities(t, capabilitiesApp(t, tc.ident, sites, &logs))
		if got := editableClientIDs(t, body); !slices.Equal(got, tc.want) {
			t.Fatalf("%s: editableSites = %v, want %v", tc.name, got, tc.want)
		}
		for _, key := range []string{"permissions", "can", "teams", "otherTeams", "noOwnerTeam"} {
			if _, ok := body[key]; !ok {
				t.Fatalf("%s: %s missing from %v", tc.name, key, body)
			}
		}
		line := logs.String()
		if !strings.Contains(line, `"event":"editable_sites"`) || !strings.Contains(line, `"outcome":"fetched"`) || strings.Contains(line, tc.ident.ID.String()) {
			t.Fatalf("%s: log = %q", tc.name, line)
		}
	}
	var site []editablesites.Site
	body := getCapabilities(t, capabilitiesApp(t, authn.Identity{ID: leader}, sites, &bytes.Buffer{}))
	_ = json.Unmarshal(body["editableSites"], &site)
	if len(site) != 1 || site[0] != capabilitySites[2] {
		t.Fatalf("site = %+v", site)
	}
}

// Keycloak down never fails the capabilities answer: the list is empty and
// one log line says why, naming nobody.
func TestCapabilitiesAnswerWhenKeycloakIsDown(t *testing.T) {
	t.Parallel()

	person := uuid.New()
	var logs bytes.Buffer
	app := capabilitiesApp(t, authn.Identity{ID: person, Groups: []string{"/ADMIN"}}, editablesites.New(capabilitySites, &siteRoles{down: true}, editablesites.Options{}), &logs)
	body := getCapabilities(t, app)
	if got := editableClientIDs(t, body); len(got) != 0 {
		t.Fatalf("editableSites = %v", got)
	}
	var permissions []string
	if err := json.Unmarshal(body["permissions"], &permissions); err != nil || len(permissions) == 0 {
		t.Fatalf("permissions = %s, want the Privileged member's", body["permissions"])
	}
	line := logs.String()
	if !strings.Contains(line, `"outcome":"unavailable"`) || !strings.Contains(line, "connection refused") || strings.Contains(line, person.String()) {
		t.Fatalf("log = %q", line)
	}
}

// A service account edits no site and causes no Keycloak call; without the
// sites wired the list is empty, never missing.
func TestCapabilitiesEditableSitesOfServiceAccountsAndWithoutSites(t *testing.T) {
	t.Parallel()

	roles := &siteRoles{held: map[uuid.UUID][]string{}}
	service := authn.Identity{ID: uuid.New(), Client: "forms", ServiceAccount: true}
	body := getCapabilities(t, capabilitiesApp(t, service, editablesites.New(capabilitySites, roles, editablesites.Options{}), &bytes.Buffer{}))
	if got := editableClientIDs(t, body); len(got) != 0 || roles.calls.Load() != 0 {
		t.Fatalf("service account: %v, calls %d", got, roles.calls.Load())
	}
	body = getCapabilities(t, capabilitiesApp(t, authn.Identity{ID: uuid.New(), Groups: []string{"/ADMIN"}}, nil, &bytes.Buffer{}))
	if got := editableClientIDs(t, body); len(got) != 0 {
		t.Fatalf("without sites: %v", got)
	}
}
