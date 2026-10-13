package httpx_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/identity"
)

// grup-yonetimi GY1 (ticket 08): DK reads everything and the Team audit
// record, and changes nothing; ADMIN and YK change DK's membership.

// withBoards adds /ADMIN and /UYELER/DK to the delegation realm, with an
// ADMIN member and a DK member.
func (e *delegationEnv) withBoards() {
	e.t.Helper()
	for _, g := range []identity.Group{{Name: "ADMIN", Path: "/ADMIN"}, {Name: "DK", Path: "/UYELER/DK"}} {
		g.ID = uuid.NewString()
		e.ids[g.Path] = g.ID
		e.dir.PutGroup(g)
	}
	e.person("admin", "/ADMIN")
	e.person("dk", "/UYELER/DK")
}

// boardToken is name's token as the mode reads it: the groups claim from
// the directory, and from the both mode on the roles Keycloak maps to the
// person's board (DK: global:read and audit:read; ADMIN and YK: the
// contract's roles and audit:read).
func (e *delegationEnv) boardToken(name string, mode authz.RoleMode) string {
	e.t.Helper()
	mine, err := e.dir.GroupsForUser(context.Background(), e.people[name])
	if err != nil {
		e.t.Fatal(err)
	}
	claims := jwt.MapClaims{
		"sub": e.people[name].String(), "email": name + "@example.test", "azp": "admin",
		"given_name": name, "family_name": "K", "groups": identity.GroupPaths(mine),
	}
	roles := []string{authz.RoleAuditRead}
	switch {
	case mode == authz.RoleModeGroups:
	case name == "dk":
		roles = append(roles, authz.RoleGlobalRead)
	default:
		roles = append(roles, authz.PermissionRoles()...)
	}
	claims["resource_access"] = map[string]any{"core": map[string]any{"roles": roles}}
	return e.keys.Token(e.t, claims)
}

func TestDKReadsEverythingChangesNothing(t *testing.T) {
	t.Parallel()
	for _, mode := range []authz.RoleMode{authz.RoleModeGroups, authz.RoleModeBoth, authz.RoleModeRoles} {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			e := newDelegationEnv(t, delegationSetup{on: true, mode: mode})
			e.withBoards()
			dk := e.boardToken("dk", mode)
			web := e.group("/UYELER/ARGE/WEBLAB")
			member := e.people["member"].String()

			for _, path := range []string{
				"/v1/groups", web, web + "/members", web + "/client-roles",
				e.group("/UYELER/DK") + "/members",
				"/v1/users", "/v1/users/" + member,
				"/v1/events", "/v1/seasons",
			} {
				wantStatus(t, "DK GET "+path, e.send(dk, fiber.MethodGet, path, ""), fiber.StatusOK, "")
			}
			// The full card: Groups and roles, not the name-only one.
			card := map[string]any{}
			if err := json.Unmarshal(e.send(dk, fiber.MethodGet, "/v1/users/"+member, "").body, &card); err != nil {
				t.Fatal(err)
			}
			if _, ok := card["groups"]; !ok {
				t.Errorf("DK user card %v: no groups (name-only card)", card)
			}

			newcomer := `{"userId":"` + e.people["newcomer"].String() + `"}`
			for _, w := range []struct{ method, path, body string }{
				{fiber.MethodPost, "/v1/groups", `{"name":"YENI","parentId":"` + e.ids["/UYELER/ARGE"] + `"}`},
				{fiber.MethodPatch, web, `{"attributes":{"display_name_tr":"X"}}`},
				{fiber.MethodPost, web + "/members", newcomer},
				{fiber.MethodDelete, web + "/members/" + member, ""},
				{fiber.MethodPut, web + "/client-roles", `[{"clientId":"core","role":"url:access"}]`},
				// DK changes not even DK.
				{fiber.MethodPost, e.group("/UYELER/DK") + "/members", newcomer},
				{fiber.MethodDelete, e.group("/UYELER/DK") + "/members/" + e.people["dk"].String(), ""},
				{fiber.MethodPost, "/v1/users", `{"email":"yeni@example.test","firstName":"Y","lastName":"Z"}`},
				{fiber.MethodPatch, "/v1/users/" + member, `{"firstName":"X"}`},
				{fiber.MethodDelete, "/v1/users/" + member, ""},
				{fiber.MethodPost, "/v1/users/" + member + "/logout", ""},
				{fiber.MethodPost, "/v1/users/" + member + "/client-roles", `{"clientId":"core","role":"url:access"}`},
				{fiber.MethodPost, "/v1/seasons", `{"name":"2027"}`},
			} {
				got := e.send(dk, w.method, w.path, w.body)
				if got.status != fiber.StatusForbidden {
					t.Errorf("DK %s %s: status %d (%s), want 403", w.method, w.path, got.status, got.body)
				}
			}
			if !e.directlyIn("member", "/UYELER/ARGE/WEBLAB") || e.directlyIn("newcomer", "/UYELER/DK") || !e.directlyIn("dk", "/UYELER/DK") {
				t.Fatal("a DK write reached the directory")
			}

			var caps authz.Capabilities
			if err := json.Unmarshal(e.send(dk, fiber.MethodGet, "/v1/users/me/capabilities", "").body, &caps); err != nil {
				t.Fatal(err)
			}
			for _, key := range []string{"season.write", "group.write", "user.write", "user.create", "user.delete", "media.delete", "url.create", "formLink.write"} {
				if caps.Can[key] {
					t.Errorf("DK capabilities %s = true", key)
				}
			}
			for _, key := range []string{"group.read", "user.read", "user.readFull", "media.list", "githubActivity.read", "audit.read"} {
				if !caps.Can[key] {
					t.Errorf("DK capabilities %s = false", key)
				}
			}
			for _, key := range []string{"event.create", "event.update", "ticket.assign", "certificate.issue", "competitor.update"} {
				if caps.OtherTeams.Can[key] || caps.NoOwnerTeam.Can[key] {
					t.Errorf("DK capabilities team %s = true", key)
				}
			}

			// The managed tree: every Team to read, none to change.
			var tree []identity.ManagedNode
			if err := json.Unmarshal(e.send(dk, fiber.MethodGet, "/v1/users/me/managed-groups", "").body, &tree); err != nil {
				t.Fatal(err)
			}
			if len(tree) == 0 {
				t.Fatal("DK managed tree is empty")
			}
			var walk func(nodes []identity.ManagedNode)
			walk = func(nodes []identity.ManagedNode) {
				for _, n := range nodes {
					if !n.Can["group.read"] || n.Can["members.write"] || n.Can["leaders.write"] {
						t.Errorf("DK tree %s: can %v", n.Path, n.Can)
					}
					walk(n.Children)
				}
			}
			walk(tree)

			// ADMIN and YK change DK's membership (Yusuf, 2026-10-12 22:15).
			for _, as := range []string{"admin", "yk"} {
				token := e.boardToken(as, mode)
				if got := e.send(token, fiber.MethodPost, e.group("/UYELER/DK")+"/members", newcomer); got.status/100 != 2 {
					t.Errorf("%s adds to DK: %d (%s)", as, got.status, got.body)
				}
				if !e.directlyIn("newcomer", "/UYELER/DK") {
					t.Errorf("%s: newcomer not in DK", as)
				}
				if got := e.send(token, fiber.MethodDelete, e.group("/UYELER/DK")+"/members/"+e.people["newcomer"].String(), ""); got.status/100 != 2 {
					t.Errorf("%s removes from DK: %d (%s)", as, got.status, got.body)
				}
				if e.directlyIn("newcomer", "/UYELER/DK") {
					t.Errorf("%s: newcomer still in DK", as)
				}
			}
		})
	}
}

// A DK member who leads a Team keeps that Team's Leader decisions, and the
// managed tree says so; DK alone adds them nothing.
func TestDKLeaderKeepsTheirTeam(t *testing.T) {
	t.Parallel()
	e := newDelegationEnv(t, delegationSetup{on: true})
	e.withBoards()
	if err := e.dir.AddMember(context.Background(), e.ids["/UYELER/ARGE/MOBILAB/LIDERLER"], e.people["dk"]); err != nil {
		t.Fatal(err)
	}
	dk := e.boardToken("dk", authz.RoleModeGroups)
	newcomer := `{"userId":"` + e.people["newcomer"].String() + `"}`
	if got := e.send(dk, fiber.MethodPost, e.group("/UYELER/ARGE/MOBILAB")+"/members", newcomer); got.status/100 != 2 {
		t.Fatalf("DK Leader adds to their Team: %d (%s)", got.status, got.body)
	}
	if got := e.send(dk, fiber.MethodPost, e.group("/UYELER/ARGE/WEBLAB")+"/members", newcomer); got.status != fiber.StatusForbidden {
		t.Fatalf("DK Leader adds to another Team: %d (%s)", got.status, got.body)
	}
	var tree []identity.ManagedNode
	if err := json.Unmarshal(e.send(dk, fiber.MethodGet, "/v1/users/me/managed-groups", "").body, &tree); err != nil {
		t.Fatal(err)
	}
	seen := map[string]map[string]bool{}
	var walk func(nodes []identity.ManagedNode)
	walk = func(nodes []identity.ManagedNode) {
		for _, n := range nodes {
			seen[n.Path] = n.Can
			walk(n.Children)
		}
	}
	walk(tree)
	if can := seen["/UYELER/ARGE/MOBILAB"]; !can["group.read"] || !can["members.write"] {
		t.Errorf("MOBILAB: %v", can)
	}
	if can := seen["/UYELER/ARGE/WEBLAB"]; !can["group.read"] || can["members.write"] {
		t.Errorf("WEBLAB: %v", can)
	}
}
