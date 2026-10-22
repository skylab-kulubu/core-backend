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
	app.Post("/v1/groups", h.CreateGroup)
	app.Patch("/v1/groups/:groupId", h.UpdateGroup)
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
		{"g-weblab", ""}, // a top Team takes an app role (ekip-yetkileri 04)
		{"g-uyeler", authz.PlacementMembersRoot},
		{"g-arge", authz.PlacementArea},
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
	// A top Team takes app roles only: an admin role is refused there, and
	// nothing of the write is kept, the app role beside it included.
	for _, body := range []string{
		`[{"clientId":"frontend-main","role":"cms:access"}]`,
		`[{"clientId":"skymail","role":"skymail:mails:write"}]`,
		`[{"clientId":"forms","role":"skyforms:access"},{"clientId":"skymail","role":"skymail:mails:write"}]`,
	} {
		got := e.send(fiber.MethodPut, "/v1/groups/g-weblab/client-roles", body)
		if got.status != fiber.StatusConflict || got.body["code"] != identity.CodeRolePlacement || got.body["reason"] != authz.PlacementTopTeamAdminRole || got.body["detail"] == "" {
			t.Errorf("top Team %s: %d %v, want 409 %s", body, got.status, got.body, authz.PlacementTopTeamAdminRole)
		}
	}
	if roles := e.roles("g-weblab"); len(roles) != 1 || roles[0].Role != "skyforms:access" {
		t.Errorf("top Team roles after refused writes = %v, want the seat alone", roles)
	}
	if got := e.send(fiber.MethodPut, "/v1/groups/g-weblab/client-roles", `[{"clientId":"forms","role":"skyforms:access"},{"clientId":"skymail","role":"skymail:access"},{"clientId":"skymail","role":"skymail:templates:read"}]`); got.status != fiber.StatusNoContent || len(e.roles("g-weblab")) != 3 {
		t.Errorf("top Team app roles: %d %v", got.status, got.body)
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

	// A mapping made before the rule (on a Sub-team, which takes none):
	// kept as it is, taken off, never added to.
	old := []identity.ClientRole{{ClientID: "forms", Role: "skyforms:access"}, {ClientID: "skymail", Role: "skymail:access"}}
	if err := e.dir.SetGroupClientRoles(context.Background(), "g-front", old); err != nil {
		t.Fatal(err)
	}
	if got := e.send(fiber.MethodPut, "/v1/groups/g-front/client-roles", `[{"clientId":"skymail","role":"skymail:access"},{"clientId":"forms","role":"skyforms:access"}]`); got.status != fiber.StatusNoContent {
		t.Errorf("same mappings: %d %v", got.status, got.body)
	}
	if got := e.send(fiber.MethodPut, "/v1/groups/g-front/client-roles", `[{"clientId":"skymail","role":"skymail:access"},{"clientId":"core","role":"url:access"}]`); got.status != fiber.StatusConflict {
		t.Errorf("swap in a new role: %d %v", got.status, got.body)
	}
	if got := e.send(fiber.MethodPut, "/v1/groups/g-front/client-roles", `[{"clientId":"skymail","role":"skymail:access"}]`); got.status != fiber.StatusNoContent {
		t.Errorf("take one off: %d %v", got.status, got.body)
	}
	if got := e.roles("g-front"); !slices.Equal(got, []identity.ClientRole{{ClientID: "skymail", Role: "skymail:access"}}) {
		t.Errorf("after taking one off: %v", got)
	}
	if got := e.send(fiber.MethodPut, "/v1/groups/g-front/client-roles", `[]`); got.status != fiber.StatusNoContent || len(e.roles("g-front")) != 0 {
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

// Keycloak administration is never mapped through core (K3; review of #245,
// B1): a realm-management role, or a composite that holds one at any depth
// or holds a realm role core cannot see into, is refused on a Group and on
// a person, a service account included; nothing reaches the directory.
func TestKeycloakAdminRoleIsNeverMapped(t *testing.T) {
	t.Parallel()
	e := newRoleRulesEnv(t, identity.Options{})
	ctx := context.Background()
	manage := identity.ClientRole{ClientID: identity.KeycloakAdminClient, Role: "manage-users"}
	deep := identity.ClientRole{ClientID: "skymail", Role: "skymail:access"}
	middle := identity.ClientRole{ClientID: "forms", Role: "skyforms:access"}
	e.dir.PutCompositeRole(deep, middle)
	e.dir.PutCompositeRole(middle, manage)
	realmHolder := identity.ClientRole{ClientID: "core", Role: "url:access"}
	e.dir.PutCompositeRole(realmHolder, identity.ClientRole{Role: "SOME_REALM_ROLE"})
	service := uuid.New()
	e.dir.PutUser(identity.Person{ID: service, Username: "service-account-x", ServiceAccount: true})

	for _, body := range []string{
		`[{"clientId":"realm-management","role":"manage-users"}]`,
		`[{"clientId":"skymail","role":"skymail:access"}]`,
		`[{"clientId":"forms","role":"skyforms:access"}]`,
		`[{"clientId":"core","role":"url:access"}]`,
	} {
		for _, group := range []string{"g-yk", "g-forms-access", "g-weblab-leaders"} {
			got := e.send(fiber.MethodPut, "/v1/groups/"+group+"/client-roles", body)
			if got.status != fiber.StatusForbidden || got.body["code"] != identity.CodeKeycloakAdminRole {
				t.Errorf("%s %s: %d %v", group, body, got.status, got.body)
			}
			if len(e.roles(group)) != 0 {
				t.Errorf("%s %s: written", group, body)
			}
		}
		one := strings.TrimSuffix(strings.TrimPrefix(body, "["), "]")
		got := e.send(fiber.MethodPost, "/v1/users/"+service.String()+"/client-roles", one)
		if got.status != fiber.StatusForbidden || got.body["code"] != identity.CodeKeycloakAdminRole {
			t.Errorf("service account %s: %d %v", one, got.status, got.body)
		}
		if roles, _ := e.dir.UserExtraRoles(ctx, service); len(roles) != 0 {
			t.Errorf("service account %s: written %v", one, roles)
		}
	}
	// A role that holds no administration still maps.
	if got := e.send(fiber.MethodPut, "/v1/groups/g-yk/client-roles", `[{"clientId":"core","role":"events:manage"}]`); got.status != fiber.StatusNoContent {
		t.Errorf("plain role: %d %v", got.status, got.body)
	}
}

// The placement rule's structural holes (review of #245): no Group under a
// role group, and a role-bearing top Leader group is not renamed into a
// Team.
func TestRolePlacementCreateAndRename(t *testing.T) {
	t.Parallel()
	e := newRoleRulesEnv(t, identity.Options{})
	ctx := context.Background()
	got := e.send(fiber.MethodPost, "/v1/groups", `{"name":"ALT","parentId":"g-forms-access"}`)
	if got.status != fiber.StatusConflict || got.body["code"] != identity.CodeRolePlacement || got.body["reason"] != authz.PlacementRolesGroupNotLeaf {
		t.Errorf("create under a role group: %d %v", got.status, got.body)
	}
	if got := e.send(fiber.MethodPost, "/v1/groups", `{"name":"SKYMAIL-ACCESS","parentId":"g-roller"}`); got.status != fiber.StatusCreated {
		t.Errorf("a new role group: %d %v", got.status, got.body)
	}

	if err := e.dir.SetGroupClientRoles(ctx, "g-weblab-leaders", []identity.ClientRole{{ClientID: "forms", Role: "skyforms:access"}}); err != nil {
		t.Fatal(err)
	}
	got = e.send(fiber.MethodPatch, "/v1/groups/g-weblab-leaders", `{"name":"ESKI-LIDERLER"}`)
	if got.status != fiber.StatusConflict || got.body["reason"] != authz.PlacementTeam {
		t.Errorf("rename a role-bearing top Leader group: %d %v", got.status, got.body)
	}
	// Without roles it renames as before; a Team mapped before the rule too.
	if got := e.send(fiber.MethodPatch, "/v1/groups/g-front-leaders", `{"name":"KOORDINATORLER"}`); got.status != fiber.StatusOK {
		t.Errorf("rename a Leader group without roles: %d %v", got.status, got.body)
	}
	if err := e.dir.SetGroupClientRoles(ctx, "g-front", []identity.ClientRole{{ClientID: "forms", Role: "skyforms:access"}}); err != nil {
		t.Fatal(err)
	}
	if got := e.send(fiber.MethodPatch, "/v1/groups/g-front", `{"name":"FRONTEND"}`); got.status != fiber.StatusOK {
		t.Errorf("rename a Team mapped before the rule: %d %v", got.status, got.body)
	}
}
