package httpx_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/editablesites"
	"github.com/skylab-kulubu/core-backend/internal/httpx"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/testauth"
)

// cmsAccess is Keycloak's answer of who holds cms:access on which Site client.
type cmsAccess map[uuid.UUID][]string

func (c cmsAccess) EffectiveClientRoles(_ context.Context, userID uuid.UUID, clientID string) ([]string, error) {
	if slices.Contains(c[userID], clientID) {
		return []string{"cms:access", "content:read", "content:write"}, nil
	}
	return []string{}, nil
}

func editableSitesOf(t *testing.T, app *fiber.App, token string) []editablesites.Site {
	t.Helper()
	req := httptest.NewRequest(fiber.MethodGet, "/v1/users/me/capabilities", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := app.Test(req, fiber.TestConfig{Timeout: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("capabilities %d %s", resp.StatusCode, raw)
	}
	var body struct {
		authz.Capabilities
		EditableSites []editablesites.Site `json:"editableSites"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	if body.EditableSites == nil || body.Can == nil {
		t.Fatalf("answer %s, want editableSites beside the capabilities", raw)
	}
	return body.EditableSites
}

// The assembled app answers editableSites from Deps.EditableSites, by the
// token's sub; without it the list is empty, never missing.
func TestCapabilitiesRouteAnswersEditableSites(t *testing.T) {
	t.Parallel()

	keys := testauth.New(t)
	leader, member := uuid.New(), uuid.New()
	token := func(sub uuid.UUID, groups ...string) string {
		return keys.Token(t, jwt.MapClaims{"sub": sub.String(), "email": sub.String() + "@example.test", "azp": "admin", "given_name": "A", "family_name": "B", "groups": groups})
	}
	sites := []editablesites.Site{
		{ClientID: "frontend-main", Name: "Ana site", URL: "https://yildizskylab.com"},
		{ClientID: "frontend-artlab", Name: "ARTLAB", URL: "https://artlab.yildizskylab.com"},
	}
	az := authz.NewAuthorizer(authz.DefaultPolicy())
	deps := memoryDepsDecidedBy(identity.NewMemory(), identity.Options{}, az)
	deps.ParseToken = keys.Parse()
	deps.EditableSites = editablesites.New(sites, cmsAccess{leader: {"frontend-artlab"}}, editablesites.Options{})
	app := httpx.New(deps)

	if got := editableSitesOf(t, app, token(leader, "/UYELER/ARGE/AIRLAB/LIDERLER")); len(got) != 1 || got[0] != sites[1] {
		t.Fatalf("leader: %+v", got)
	}
	if got := editableSitesOf(t, app, token(member, "/UYELER/ARGE/AIRLAB")); len(got) != 0 {
		t.Fatalf("member: %+v", got)
	}

	deps.EditableSites = nil
	if got := editableSitesOf(t, httpx.New(deps), token(leader, "/UYELER/ARGE/AIRLAB/LIDERLER")); len(got) != 0 {
		t.Fatalf("without sites: %+v", got)
	}
}
