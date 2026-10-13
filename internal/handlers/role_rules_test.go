package handlers

import (
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// The role cleanup's rules in core (sky_lab_genel .scratch/rol-temizligi,
// ticket 06): no direct role on a person (K1), roles mapped only to a
// Privileged root, a top Team's Leader group or a role group (K4, S5), and
// no membership change in a Group holding a Keycloak administration role
// (K3).

type roleRulesEnv struct {
	t   *testing.T
	dir *identity.Memory
	app *fiber.App
}

func newRoleRulesEnv(t *testing.T, options identity.Options) roleRulesEnv {
	t.Helper()
	dir := identity.NewMemory()
	for _, g := range []identity.Group{
		{ID: "g-uyeler", Name: "UYELER", Path: "/UYELER"},
		{ID: "g-arge", Name: "ARGE", Path: "/UYELER/ARGE"},
		{ID: "g-weblab", Name: "WEBLAB", Path: "/UYELER/ARGE/WEBLAB"},
		{ID: "g-weblab-leaders", Name: "LIDERLER", Path: "/UYELER/ARGE/WEBLAB/LIDERLER"},
		{ID: "g-front", Name: "FRONT", Path: "/UYELER/ARGE/WEBLAB/FRONT"},
		{ID: "g-front-leaders", Name: "LIDERLER", Path: "/UYELER/ARGE/WEBLAB/FRONT/LIDERLER"},
		{ID: "g-yk", Name: "YK", Path: "/UYELER/YK"},
		{ID: "g-yk-duty", Name: "ARGE_LIDERI", Path: "/UYELER/YK/ARGE_LIDERI"},
		{ID: "g-roller", Name: "ROLLER", Path: "/ROLLER"},
		{ID: "g-forms-access", Name: "FORMS-ACCESS", Path: "/ROLLER/FORMS-ACCESS"},
		{ID: "g-konsol", Name: "KONSOL", Path: "/KONSOL"},
		{ID: "g-konsol-sub", Name: "YAZICILAR", Path: "/KONSOL/YAZICILAR"},
	} {
		dir.PutGroup(g)
	}
	options.AccountErasureEnabled = true
	options.AccessProjector = handlerDeletionProjector{}
	options.AdminGroups = dir
	svc := identity.NewServiceWithOptions(dir, user.NewMemoryStore(), authz.NewAuthorizer(authz.DefaultPolicy()), options)
	h := NewIdentityHandler(svc)
	app := fiber.New()
	app.Use(func(c fiber.Ctx) error {
		c.Locals(authn.LocalsIdentity, ykIdent())
		return c.Next()
	})
	app.Post("/v1/users/:id/client-roles", h.AddUserExtraRole)
	app.Delete("/v1/users/:id/client-roles", h.RemoveUserExtraRole)
	app.Put("/v1/groups/:groupId/client-roles", h.SetGroupClientRoles)
	app.Post("/v1/groups/:groupId/members", h.AddMember)
	app.Delete("/v1/groups/:groupId/members/:userId", h.RemoveMember)
	return roleRulesEnv{t: t, dir: dir, app: app}
}

type ruleAnswer struct {
	status int
	body   map[string]any
}

func (e roleRulesEnv) send(method, path, body string) ruleAnswer {
	e.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := e.app.Test(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	out := ruleAnswer{status: resp.StatusCode, body: map[string]any{}}
	_ = json.Unmarshal(raw, &out.body)
	return out
}

func (e roleRulesEnv) roles(groupID string) []identity.ClientRole {
	e.t.Helper()
	roles, err := e.dir.GroupClientRoles(context.Background(), groupID)
	if err != nil {
		e.t.Fatal(err)
	}
	return roles
}

func TestDirectRoleOnAPersonIsRefused(t *testing.T) {
	t.Parallel()
	e := newRoleRulesEnv(t, identity.Options{})
	person := uuid.New()
	e.dir.PutUser(identity.Person{ID: person, Email: "ada@example.test"})
	service := uuid.New()
	e.dir.PutUser(identity.Person{ID: service, Username: "service-account-forms", ServiceAccount: true})
	role := `{"clientId":"forms","role":"skyforms:access"}`

	got := e.send(fiber.MethodPost, "/v1/users/"+person.String()+"/client-roles", role)
	if got.status != fiber.StatusForbidden || got.body["code"] != identity.CodeDirectRoleNotAllowed || got.body["detail"] == "" {
		t.Fatalf("person: %d %v", got.status, got.body)
	}
	if card, _ := e.dir.UserExtraRoles(context.Background(), person); len(card) != 0 {
		t.Fatalf("the role reached the person: %v", card)
	}
	if got := e.send(fiber.MethodPost, "/v1/users/"+service.String()+"/client-roles", role); got.status != fiber.StatusNoContent {
		t.Fatalf("service account: %d %v", got.status, got.body)
	}
	// Taking a direct role off stays allowed (F2 migrates them away).
	if err := e.dir.AddUserExtraRole(context.Background(), person, identity.ClientRole{ClientID: "forms", Role: "skyforms:access"}); err != nil {
		t.Fatal(err)
	}
	if got := e.send(fiber.MethodDelete, "/v1/users/"+person.String()+"/client-roles?clientId=forms&role=skyforms:access", ""); got.status != fiber.StatusNoContent {
		t.Fatalf("remove: %d %v", got.status, got.body)
	}
	if card, _ := e.dir.UserExtraRoles(context.Background(), person); len(card) != 0 {
		t.Fatalf("not removed: %v", card)
	}

	// DIRECT_USER_ROLES=on: the way back.
	on := newRoleRulesEnv(t, identity.Options{DirectUserRoles: true})
	on.dir.PutUser(identity.Person{ID: person, Email: "ada@example.test"})
	if got := on.send(fiber.MethodPost, "/v1/users/"+person.String()+"/client-roles", role); got.status != fiber.StatusNoContent {
		t.Fatalf("on: %d %v", got.status, got.body)
	}
}

func TestRolePlacementHTTP(t *testing.T) {
	t.Parallel()
	e := newRoleRulesEnv(t, identity.Options{})
	role := `[{"clientId":"forms","role":"skyforms:access"}]`
	for _, tc := range []struct {
		group  string
		reason string
	}{
		{"g-yk", ""},
		{"g-weblab-leaders", ""},
		{"g-forms-access", ""},
		{"g-uyeler", authz.PlacementMembersRoot},
		{"g-arge", authz.PlacementArea},
		{"g-weblab", authz.PlacementTeam},
		{"g-front", authz.PlacementTeam},
		{"g-front-leaders", authz.PlacementSubTeamLeaderGroup},
		{"g-yk-duty", authz.PlacementPrivilegedSubgroup},
		{"g-roller", authz.PlacementRolesRoot},
		{"g-konsol", authz.PlacementOutside},
	} {
		got := e.send(fiber.MethodPut, "/v1/groups/"+tc.group+"/client-roles", role)
		if tc.reason == "" {
			if got.status != fiber.StatusNoContent || len(e.roles(tc.group)) != 1 {
				t.Errorf("%s: %d %v", tc.group, got.status, got.body)
			}
			continue
		}
		if got.status != fiber.StatusConflict || got.body["code"] != identity.CodeRolePlacement || got.body["reason"] != tc.reason || got.body["detail"] == "" {
			t.Errorf("%s: %d %v, want 409 %s", tc.group, got.status, got.body, tc.reason)
		}
		if len(e.roles(tc.group)) != 0 {
			t.Errorf("%s: a refused mapping was written", tc.group)
		}
	}
	// A role group with a Group under it takes no role.
	e.dir.PutGroup(identity.Group{ID: "g-forms-sub", Name: "ALT", Path: "/ROLLER/FORMS-ACCESS/ALT"})
	e.dir.PutGroup(identity.Group{ID: "g-mail", Name: "SKYMAIL-ACCESS", Path: "/ROLLER/SKYMAIL-ACCESS"})
	if got := e.send(fiber.MethodPut, "/v1/groups/g-forms-access/client-roles", `[{"clientId":"forms","role":"skyforms:access"},{"clientId":"forms","role":"skyforms:*"}]`); got.status != fiber.StatusConflict || got.body["reason"] != authz.PlacementRolesGroupNotLeaf {
		t.Errorf("role group with subgroups: %d %v", got.status, got.body)
	}
	if got := e.send(fiber.MethodPut, "/v1/groups/g-forms-sub/client-roles", role); got.status != fiber.StatusConflict || got.body["reason"] != authz.PlacementRolesNested {
		t.Errorf("nested role group: %d %v", got.status, got.body)
	}

	// A mapping made before the rule: kept as it is, taken off, never added
	// to.
	old := []identity.ClientRole{{ClientID: "forms", Role: "skyforms:access"}, {ClientID: "skymail", Role: "skymail:access"}}
	if err := e.dir.SetGroupClientRoles(context.Background(), "g-weblab", old); err != nil {
		t.Fatal(err)
	}
	if got := e.send(fiber.MethodPut, "/v1/groups/g-weblab/client-roles", `[{"clientId":"skymail","role":"skymail:access"},{"clientId":"forms","role":"skyforms:access"}]`); got.status != fiber.StatusNoContent {
		t.Errorf("same mappings: %d %v", got.status, got.body)
	}
	if got := e.send(fiber.MethodPut, "/v1/groups/g-weblab/client-roles", `[{"clientId":"skymail","role":"skymail:access"},{"clientId":"core","role":"url:access"}]`); got.status != fiber.StatusConflict {
		t.Errorf("swap in a new role: %d %v", got.status, got.body)
	}
	if got := e.send(fiber.MethodPut, "/v1/groups/g-weblab/client-roles", `[{"clientId":"skymail","role":"skymail:access"}]`); got.status != fiber.StatusNoContent {
		t.Errorf("take one off: %d %v", got.status, got.body)
	}
	if got := e.roles("g-weblab"); !slices.Equal(got, []identity.ClientRole{{ClientID: "skymail", Role: "skymail:access"}}) {
		t.Errorf("after taking one off: %v", got)
	}
	if got := e.send(fiber.MethodPut, "/v1/groups/g-weblab/client-roles", `[]`); got.status != fiber.StatusNoContent || len(e.roles("g-weblab")) != 0 {
		t.Errorf("take all off: %d %v", got.status, got.body)
	}
}

func TestNoMembershipChangeInAKeycloakAdminGroup(t *testing.T) {
	t.Parallel()
	e := newRoleRulesEnv(t, identity.Options{})
	ctx := context.Background()
	person := uuid.New()
	e.dir.PutUser(identity.Person{ID: person, Email: "ada@example.test"})
	add := `{"userId":"` + person.String() + `"}`

	// The Group itself maps realm-management's role.
	if err := e.dir.SetGroupClientRoles(ctx, "g-konsol", []identity.ClientRole{{ClientID: identity.KeycloakAdminClient, Role: "manage-users"}}); err != nil {
		t.Fatal(err)
	}
	for _, group := range []string{"g-konsol", "g-konsol-sub"} {
		got := e.send(fiber.MethodPost, "/v1/groups/"+group+"/members", add)
		if got.status != fiber.StatusForbidden || got.body["code"] != identity.CodeKeycloakAdminGroup {
			t.Errorf("add to %s: %d %v", group, got.status, got.body)
		}
	}
	// A member placed there in Keycloak is not taken out by core either.
	if err := e.dir.AddMember(ctx, "g-konsol-sub", person); err != nil {
		t.Fatal(err)
	}
	if got := e.send(fiber.MethodDelete, "/v1/groups/g-konsol-sub/members/"+person.String(), ""); got.status != fiber.StatusForbidden || got.body["code"] != identity.CodeKeycloakAdminGroup {
		t.Errorf("remove: %d %v", got.status, got.body)
	}
	if got := e.send(fiber.MethodDelete, "/v1/groups/g-konsol/members/"+person.String(), ""); got.status != fiber.StatusForbidden || got.body["code"] != identity.CodeKeycloakAdminGroup {
		t.Errorf("remove through the parent roster: %d %v", got.status, got.body)
	}

	// Through a composite: an app role of another client that holds
	// realm-management's role.
	e.dir.PutCompositeRole(identity.ClientRole{ClientID: "skymail", Role: "skymail:access"}, identity.ClientRole{ClientID: identity.KeycloakAdminClient, Role: "view-users"})
	if err := e.dir.SetGroupClientRoles(ctx, "g-forms-access", []identity.ClientRole{{ClientID: "skymail", Role: "skymail:access"}}); err != nil {
		t.Fatal(err)
	}
	if got := e.send(fiber.MethodPost, "/v1/groups/g-forms-access/members", add); got.status != fiber.StatusForbidden || got.body["code"] != identity.CodeKeycloakAdminGroup {
		t.Errorf("composite: %d %v", got.status, got.body)
	}

	// Any other Group: as before.
	if got := e.send(fiber.MethodPost, "/v1/groups/g-weblab/members", add); got.status != fiber.StatusNoContent {
		t.Errorf("weblab: %d %v", got.status, got.body)
	}
}
