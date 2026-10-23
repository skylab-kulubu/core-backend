package httpx_test

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/httpx"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/teamaudit"
	"github.com/skylab-kulubu/core-backend/internal/teamcode"
	"github.com/skylab-kulubu/core-backend/internal/testauth"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// The panel's app assignments (docs/app-assignments.md, ekip-yetkileri
// ticket 15): Entra's Enterprise apps → "Users and groups".

var appGroups = []identity.Group{
	{Name: "ADMIN", Path: "/ADMIN"},
	{Name: "DK", Path: "/UYELER/DK"},
	{Name: "BASKAN", Path: "/UYELER/YK/BASKAN"},
	{Name: "ROLLER", Path: "/ROLLER"},
	{Name: "FORMS-ACCESS", Path: "/ROLLER/FORMS-ACCESS"},
}

// newAppsEnv is the delegation realm with ADMIN, DK, a YK duty group and a
// role group, an ADMIN, a DK and a MOBILAB member, a service account and a
// disabled person, and the app assignment routes on or off.
func newAppsEnv(t *testing.T, on bool) *delegationEnv {
	t.Helper()
	e := &delegationEnv{
		t: t, keys: testauth.New(t), dir: identity.NewMemory(), users: user.NewMemoryStore(), audit: teamaudit.NewMemory(), heard: &heardChanges{},
		ids: map[string]string{}, people: map[string]uuid.UUID{},
	}
	for _, g := range append(slices.Clone(delegationGroups), appGroups...) {
		g.ID = uuid.NewString()
		e.ids[g.Path] = g.ID
		e.dir.PutGroup(g)
	}
	for name, path := range map[string]string{
		"admin": "/ADMIN", "yk": "/UYELER/YK", "dk": "/UYELER/DK",
		"webLeader": "/UYELER/ARGE/WEBLAB/LIDERLER", "member": "/UYELER/ARGE/WEBLAB",
		"frontMember": "/UYELER/ARGE/WEBLAB/FRONT", "mobMember": "/UYELER/ARGE/MOBILAB", "outsider": "",
		"disabled": "/UYELER/ARGE/MOBILAB",
	} {
		e.person(name, path)
	}
	e.people["service"] = uuid.New()
	e.dir.PutUser(identity.Person{ID: e.people["service"], Username: "service-account-forms", ServiceAccount: true})
	if err := e.dir.DisableUser(context.Background(), e.people["disabled"]); err != nil {
		t.Fatal(err)
	}
	policy := authz.DefaultPolicy()
	policy.AppAssignments = on
	az := authz.NewAuthorizer(policy)
	options := identity.Options{TeamCodes: teamcode.NewMemoryLedger(), TeamAudit: e.audit, AppAudit: e.audit, AdminGroups: e.dir, AppAssignments: on}
	deps := memoryDepsDecidedBy(e.dir, options, az)
	deps.Identity = identity.NewServiceWithOptions(identity.AuditedDirectory(e.dir, e.audit, nil), e.users, az, options)
	deps.ParseToken = e.keys.Parse()
	deps.AppAssignments = on
	e.app = httpx.New(deps)
	return e
}

// appToken is name's token: their Groups and, for the boards, audit:read
// (Keycloak maps it to ADMIN, YK and DK).
func (e *delegationEnv) appToken(name string) string {
	e.t.Helper()
	mine, err := e.dir.GroupsForUser(context.Background(), e.people[name])
	if err != nil {
		e.t.Fatal(err)
	}
	claims := jwt.MapClaims{
		"sub": e.people[name].String(), "email": name + "@example.test", "azp": "admin",
		"given_name": name, "family_name": "K", "groups": identity.GroupPaths(mine),
	}
	if name == "admin" || name == "yk" || name == "dk" {
		claims["resource_access"] = map[string]any{"core": map[string]any{"roles": []string{authz.RoleAuditRead}}}
	}
	return e.keys.Token(e.t, claims)
}

func groupTarget(e *delegationEnv, path, role string) string {
	return `{"type":"group","id":"` + e.ids[path] + `","role":"` + role + `"}`
}

func userTarget(e *delegationEnv, who, role string) string {
	return `{"type":"user","id":"` + e.people[who].String() + `","role":"` + role + `"}`
}

func (e *delegationEnv) appRows() []teamaudit.Row {
	out := []teamaudit.Row{}
	for _, r := range e.audit.Rows() {
		if r.Action.IsApp() {
			out = append(out, r)
		}
	}
	return out
}

func (e *delegationEnv) groupMaps(path string, role identity.ClientRole) bool {
	roles, err := e.dir.GroupClientRoles(context.Background(), e.ids[path])
	if err != nil {
		e.t.Fatal(err)
	}
	return slices.Contains(roles, role)
}

func (e *delegationEnv) userMaps(who string, role identity.ClientRole) bool {
	roles, err := e.dir.UserExtraRoles(context.Background(), e.people[who])
	if err != nil {
		e.t.Fatal(err)
	}
	return slices.Contains(roles, role)
}

var formsSeat = identity.ClientRole{ClientID: "forms", Role: "skyforms:access"}

// Off (the default), nothing is served and the capabilities say no: the
// release changes nothing.
func TestAppAssignmentsOffAreNotServed(t *testing.T) {
	t.Parallel()
	e := newAppsEnv(t, false)
	admin := e.appToken("admin")
	for _, path := range []string{"/v1/apps", "/v1/apps/forms/assignments", "/v1/apps/forms/audit", "/v1/apps/forms/targets?type=group"} {
		wantStatus(t, "GET "+path, e.send(admin, fiber.MethodGet, path, ""), fiber.StatusNotFound, "")
	}
	wantStatus(t, "POST", e.send(admin, fiber.MethodPost, "/v1/apps/forms/assignments", groupTarget(e, "/UYELER/ARGE/WEBLAB", "skyforms:access")), fiber.StatusNotFound, "")
	var caps authz.Capabilities
	if err := json.Unmarshal(e.send(admin, fiber.MethodGet, "/v1/users/me/capabilities", "").body, &caps); err != nil {
		t.Fatal(err)
	}
	if caps.Can["apps.read"] || caps.Can["apps.assign"] {
		t.Errorf("capabilities %v", caps.Can)
	}
	if e.groupMaps("/UYELER/ARGE/WEBLAB", formsSeat) || len(e.appRows()) != 0 {
		t.Fatal("an off route changed something")
	}
}

// YK and ADMIN give an app role to a Team, a Sub-team, a Leader group and a
// person and take it off; each change is one app row, a repeat none.
func TestAppAssignmentsYKAndADMINAssignAndRemove(t *testing.T) {
	t.Parallel()
	e := newAppsEnv(t, true)
	yk, admin := e.appToken("yk"), e.appToken("admin")

	// An older panel's note is ignored: the change keeps no free text.
	withNote := `{"type":"group","id":"` + e.ids["/UYELER/ARGE/WEBLAB"] + `","role":"skyforms:access","note":"ekip talebi"}`
	got := e.send(yk, fiber.MethodPost, "/v1/apps/forms/assignments", withNote)
	wantStatus(t, "YK assigns WEBLAB", got, fiber.StatusCreated, "")
	var row identity.AppAssignment
	if err := json.Unmarshal(got.body, &row); err != nil {
		t.Fatal(err)
	}
	// WEBLAB: LIDERLER, KOORDINATORLER, FRONT, FRONT/LIDERLER and SECURE
	// below; webLeader, member and frontMember in it.
	if row.Type != "group" || row.Path != "/UYELER/ARGE/WEBLAB" || row.Area != "panel" || row.InheritsTo != 5 || row.Members != 3 ||
		row.AssignedBy == nil || row.AssignedBy.ID != e.people["yk"] || row.AssignedAt == nil {
		t.Errorf("row %+v", row)
	}
	if !e.groupMaps("/UYELER/ARGE/WEBLAB", formsSeat) {
		t.Fatal("not mapped")
	}
	rows := e.appRows()
	if len(rows) != 1 || rows[0].Action != teamaudit.ActionAppAssign || rows[0].Outcome != teamaudit.OutcomeOK ||
		rows[0].Actor.Basis != teamaudit.BasisPrivileged || rows[0].GroupID != e.ids["/UYELER/ARGE/WEBLAB"] ||
		rows[0].After.Client != "forms" || rows[0].After.Role != "skyforms:access" {
		t.Fatalf("rows %+v", rows)
	}

	wantStatus(t, "again", e.send(yk, fiber.MethodPost, "/v1/apps/forms/assignments", groupTarget(e, "/UYELER/ARGE/WEBLAB", "skyforms:access")), fiber.StatusOK, "")
	if len(e.appRows()) != 1 {
		t.Fatal("a repeat wrote a row")
	}

	for _, path := range []string{"/UYELER/ARGE/WEBLAB/FRONT", "/UYELER/ARGE/WEBLAB/FRONT/LIDERLER", "/UYELER/ARGE/MOBILAB/LIDERLER"} {
		wantStatus(t, "YK assigns "+path, e.send(yk, fiber.MethodPost, "/v1/apps/skymail/assignments", groupTarget(e, path, "skymail:access")), fiber.StatusCreated, "")
	}
	got = e.send(admin, fiber.MethodPost, "/v1/apps/forms/assignments", userTarget(e, "mobMember", "skyforms:access"))
	wantStatus(t, "ADMIN assigns a person", got, fiber.StatusCreated, "")
	if err := json.Unmarshal(got.body, &row); err != nil {
		t.Fatal(err)
	}
	if row.Type != "user" || row.ID != e.people["mobMember"].String() || row.Area != "panel" || row.Name != "mobMember Kişi" ||
		row.Email != "mobmember@example.test" || row.AssignedBy == nil || row.AssignedBy.Name != "admin Kişi" {
		t.Errorf("person row %+v", row)
	}
	if !e.userMaps("mobMember", formsSeat) {
		t.Fatal("person not mapped")
	}

	var list identity.AppAssignments
	got = e.send(e.appToken("dk"), fiber.MethodGet, "/v1/apps/forms/assignments", "")
	wantStatus(t, "DK lists", got, fiber.StatusOK, "")
	if err := json.Unmarshal(got.body, &list); err != nil {
		t.Fatal(err)
	}
	if !list.AuditVisible || list.App.ClientID != "forms" || !list.App.AssignmentRequired || len(list.Assignments) != 2 ||
		list.Assignments[0].Type != "group" || list.Assignments[1].Type != "user" || list.App.Groups != 1 || list.App.Users != 1 {
		t.Fatalf("list %+v", list)
	}
	got = e.send(yk, fiber.MethodGet, "/v1/apps/forms/assignments?type=user", "")
	if err := json.Unmarshal(got.body, &list); err != nil || len(list.Assignments) != 1 || list.Assignments[0].Type != "user" {
		t.Fatalf("people only %s %v", got.body, err)
	}
	var apps []identity.AppSummary
	if err := json.Unmarshal(e.send(yk, fiber.MethodGet, "/v1/apps", "").body, &apps); err != nil {
		t.Fatal(err)
	}
	if len(apps) == 0 || apps[0].ClientID != "forms" || apps[0].Groups != 1 || apps[0].Users != 1 || apps[1].ClientID != "skymail" || apps[1].Groups != 3 {
		t.Fatalf("apps %+v", apps)
	}

	wantStatus(t, "YK removes WEBLAB", e.send(yk, fiber.MethodDelete, "/v1/apps/forms/assignments", groupTarget(e, "/UYELER/ARGE/WEBLAB", "skyforms:access")), fiber.StatusNoContent, "")
	wantStatus(t, "ADMIN removes the person", e.send(admin, fiber.MethodDelete, "/v1/apps/forms/assignments", userTarget(e, "mobMember", "skyforms:access")), fiber.StatusNoContent, "")
	if e.groupMaps("/UYELER/ARGE/WEBLAB", formsSeat) || e.userMaps("mobMember", formsSeat) {
		t.Fatal("still mapped")
	}
	rows = e.appRows()
	if len(rows) != 7 || rows[5].Action != teamaudit.ActionAppUnassign || rows[6].TargetUserID != e.people["mobMember"] || rows[6].GroupPath != "" {
		t.Fatalf("rows %+v", rows)
	}
	wantStatus(t, "remove again", e.send(yk, fiber.MethodDelete, "/v1/apps/forms/assignments", groupTarget(e, "/UYELER/ARGE/WEBLAB", "skyforms:access")), fiber.StatusNotFound, "not_assigned")
	if len(e.appRows()) != 7 {
		t.Fatal("a removal of nothing wrote a row")
	}

	var audit []identity.AppAuditEntry
	got = e.send(yk, fiber.MethodGet, "/v1/apps/forms/audit", "")
	if err := json.Unmarshal(got.body, &audit); err != nil {
		t.Fatal(err)
	}
	if len(audit) != 4 || audit[0].Action != "app.unassign" || audit[0].Type != "user" || audit[0].Name != "mobMember Kişi" || audit[0].Actor.Name != "admin Kişi" ||
		audit[3].Role != "skyforms:access" || audit[3].Path != "/UYELER/ARGE/WEBLAB" {
		t.Fatalf("audit %s", got.body)
	}
	// No free text is kept or shown (T2): a client's old note field is
	// ignored.
	if strings.Contains(string(got.body), `"note"`) || strings.Contains(string(got.body), "ekip talebi") {
		t.Errorf("a note reached the audit record: %s", got.body)
	}
}

// DK reads and is refused a change with a denied row; a Leader, a member
// and a service account read nothing; the rules refuse an admin role, a
// Keycloak administration composite, the code's places, a service account
// and a disabled person, each with a denied row and nothing written.
func TestAppAssignmentsRefusals(t *testing.T) {
	t.Parallel()
	e := newAppsEnv(t, true)
	yk, dk := e.appToken("yk"), e.appToken("dk")
	weblab := groupTarget(e, "/UYELER/ARGE/WEBLAB", "skyforms:access")

	wantStatus(t, "DK reads", e.send(dk, fiber.MethodGet, "/v1/apps/forms/assignments", ""), fiber.StatusOK, "")
	wantStatus(t, "DK assigns", e.send(dk, fiber.MethodPost, "/v1/apps/forms/assignments", weblab), fiber.StatusForbidden, "")
	wantStatus(t, "DK removes", e.send(dk, fiber.MethodDelete, "/v1/apps/forms/assignments", weblab), fiber.StatusForbidden, "")
	wantStatus(t, "DK searches", e.send(dk, fiber.MethodGet, "/v1/apps/forms/targets?type=group", ""), fiber.StatusForbidden, "")
	rows := e.appRows()
	if len(rows) != 2 || rows[0].Outcome != teamaudit.OutcomeDenied || rows[0].Actor.Basis != teamaudit.BasisNone || rows[0].ErrorCode != "forbidden" {
		t.Fatalf("DK rows %+v", rows)
	}
	for _, who := range []string{"webLeader", "member", "outsider"} {
		token := e.appToken(who)
		wantStatus(t, who+" lists apps", e.send(token, fiber.MethodGet, "/v1/apps", ""), fiber.StatusForbidden, "")
		wantStatus(t, who+" lists", e.send(token, fiber.MethodGet, "/v1/apps/forms/assignments", ""), fiber.StatusForbidden, "")
		wantStatus(t, who+" assigns", e.send(token, fiber.MethodPost, "/v1/apps/forms/assignments", weblab), fiber.StatusForbidden, "")
	}
	if len(e.appRows()) != 2 {
		t.Fatal("a caller who reads nothing wrote rows")
	}

	// skymail:templates:read holds Keycloak administration.
	e.dir.PutCompositeRole(identity.ClientRole{ClientID: "skymail", Role: "skymail:templates:read"}, identity.ClientRole{ClientID: "realm-management", Role: "manage-users"})
	for _, tc := range []struct {
		name, client, body string
		status             int
		code, reason       string
	}{
		{"admin role", "forms", groupTarget(e, "/UYELER/ARGE/WEBLAB", "skyforms:*"), fiber.StatusConflict, "role_placement", authz.PlacementAdminRole},
		{"another app's role", "forms", groupTarget(e, "/UYELER/ARGE/WEBLAB", "skymail:access"), fiber.StatusConflict, "role_placement", authz.PlacementAdminRole},
		{"unknown role", "forms", groupTarget(e, "/UYELER/ARGE/WEBLAB", "skyforms:nothing"), fiber.StatusConflict, "role_placement", authz.PlacementAdminRole},
		{"keycloak composite", "skymail", groupTarget(e, "/UYELER/ARGE/WEBLAB", "skymail:templates:read"), fiber.StatusForbidden, "keycloak_admin_role", ""},
		{"YK root", "forms", groupTarget(e, "/UYELER/YK", "skyforms:access"), fiber.StatusConflict, "role_placement", authz.PlacementCodeArea},
		{"YK duty group", "forms", groupTarget(e, "/UYELER/YK/BASKAN", "skyforms:access"), fiber.StatusConflict, "role_placement", authz.PlacementCodeArea},
		{"ADMIN", "forms", groupTarget(e, "/ADMIN", "skyforms:access"), fiber.StatusConflict, "role_placement", authz.PlacementCodeArea},
		{"role group", "forms", groupTarget(e, "/ROLLER/FORMS-ACCESS", "skyforms:access"), fiber.StatusConflict, "role_placement", authz.PlacementCodeArea},
		{"ROLLER", "forms", groupTarget(e, "/ROLLER", "skyforms:access"), fiber.StatusConflict, "role_placement", authz.PlacementCodeArea},
		{"every member", "forms", groupTarget(e, "/UYELER", "skyforms:access"), fiber.StatusConflict, "role_placement", authz.PlacementMembersRoot},
		{"an area", "forms", groupTarget(e, "/UYELER/ARGE", "skyforms:access"), fiber.StatusConflict, "role_placement", authz.PlacementArea},
		{"a service account", "forms", userTarget(e, "service", "skyforms:access"), fiber.StatusConflict, "role_placement", authz.PlacementServiceAccount},
		{"a disabled person", "forms", userTarget(e, "disabled", "skyforms:access"), fiber.StatusConflict, "role_placement", authz.PlacementInactiveUser},
	} {
		got := e.send(yk, fiber.MethodPost, "/v1/apps/"+tc.client+"/assignments", tc.body)
		wantStatus(t, tc.name, got, tc.status, tc.code)
		if reason, _ := got.problem()["reason"].(string); reason != tc.reason {
			t.Errorf("%s: reason %q (%s)", tc.name, reason, got.body)
		}
		last := e.appRows()[len(e.appRows())-1]
		if last.Outcome != teamaudit.OutcomeDenied || last.Actor.Basis != teamaudit.BasisPrivileged || last.ErrorCode != tc.code {
			t.Errorf("%s: row %+v", tc.name, last)
		}
	}
	for _, path := range []string{"/UYELER/ARGE/WEBLAB", "/UYELER/YK", "/UYELER/YK/BASKAN", "/ADMIN", "/ROLLER/FORMS-ACCESS", "/UYELER", "/UYELER/ARGE"} {
		roles, _ := e.dir.GroupClientRoles(context.Background(), e.ids[path])
		if len(roles) != 0 && path != "/UYELER/YK" {
			t.Errorf("%s maps %v", path, roles)
		}
	}
	if e.userMaps("service", formsSeat) || e.userMaps("disabled", formsSeat) {
		t.Fatal("a refused person was mapped")
	}

	for _, tc := range []struct {
		name, client, body string
		status             int
	}{
		{"unknown app", "broker", groupTarget(e, "/UYELER/ARGE/WEBLAB", "read-token"), fiber.StatusNotFound},
		{"unknown Group", "forms", `{"type":"group","id":"` + uuid.NewString() + `","role":"skyforms:access"}`, fiber.StatusNotFound},
		{"unknown kind", "forms", `{"type":"team","id":"` + e.ids["/UYELER/ARGE/WEBLAB"] + `","role":"skyforms:access"}`, fiber.StatusBadRequest},
		{"no role", "forms", `{"type":"group","id":"` + e.ids["/UYELER/ARGE/WEBLAB"] + `"}`, fiber.StatusBadRequest},
		{"id not an id", "forms", `{"type":"group","id":"WEBLAB","role":"skyforms:access"}`, fiber.StatusBadRequest},
		{"no body", "forms", ``, fiber.StatusBadRequest},
	} {
		wantStatus(t, tc.name, e.send(yk, fiber.MethodPost, "/v1/apps/"+tc.client+"/assignments", tc.body), tc.status, "")
	}

	// The old way stays shut: no role on a person directly (K1).
	wantStatus(t, "old direct role", e.send(yk, fiber.MethodPost, "/v1/users/"+e.people["mobMember"].String()+"/client-roles", `{"clientId":"forms","role":"skyforms:access"}`),
		fiber.StatusForbidden, identity.CodeDirectRoleNotAllowed)
	if e.userMaps("mobMember", formsSeat) {
		t.Fatal("the old route gave a role to a person")
	}
}

// The code's mappings of an app role are listed, locked, and the panel
// cannot take them off.
func TestAppAssignmentsCodeRowsAreLocked(t *testing.T) {
	t.Parallel()
	e := newAppsEnv(t, true)
	ctx := context.Background()
	for _, path := range []string{"/UYELER/YK", "/ROLLER/FORMS-ACCESS"} {
		if err := e.dir.AddGroupClientRole(ctx, e.ids[path], formsSeat); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.dir.AddUserExtraRole(ctx, e.people["service"], formsSeat); err != nil {
		t.Fatal(err)
	}
	var list identity.AppAssignments
	if err := json.Unmarshal(e.send(e.appToken("yk"), fiber.MethodGet, "/v1/apps/forms/assignments", "").body, &list); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"/ROLLER/FORMS-ACCESS": authz.PlacementCodeArea, "/UYELER/YK": authz.PlacementCodeArea, "": authz.PlacementServiceAccount}
	if len(list.Assignments) != 3 {
		t.Fatalf("list %+v", list.Assignments)
	}
	for _, row := range list.Assignments {
		if row.Area != "code" || row.Reason != want[row.Path] || row.AssignedBy != nil || row.AssignedAt != nil {
			t.Errorf("row %+v", row)
		}
	}
	yk := e.appToken("yk")
	wantStatus(t, "remove YK's", e.send(yk, fiber.MethodDelete, "/v1/apps/forms/assignments", groupTarget(e, "/UYELER/YK", "skyforms:access")), fiber.StatusConflict, "role_placement")
	wantStatus(t, "remove a role group's", e.send(yk, fiber.MethodDelete, "/v1/apps/forms/assignments", groupTarget(e, "/ROLLER/FORMS-ACCESS", "skyforms:access")), fiber.StatusConflict, "role_placement")
	wantStatus(t, "remove a service account's", e.send(yk, fiber.MethodDelete, "/v1/apps/forms/assignments", userTarget(e, "service", "skyforms:access")), fiber.StatusConflict, "role_placement")
	if !e.groupMaps("/UYELER/YK", formsSeat) || !e.userMaps("service", formsSeat) {
		t.Fatal("a code mapping was taken off")
	}
}

// A person given Forms' seat from the panel is found by Forms' people
// picker at once: the write drops the seat search's minute (ticket 04).
func TestAppAssignmentDropsTheSeatSearchCache(t *testing.T) {
	t.Parallel()
	e := newAppsEnv(t, true)
	if err := e.dir.AddGroupClientRole(context.Background(), e.ids["/UYELER/ARGE/WEBLAB"], formsSeat); err != nil {
		t.Fatal(err)
	}
	picker := e.appToken("member")
	search := func() []string {
		t.Helper()
		got := e.send(picker, fiber.MethodGet, "/v1/users?role=skyforms:access&q=Kişi", "")
		var people []identity.Person
		if err := json.Unmarshal(got.body, &people); err != nil {
			t.Fatalf("%d %s", got.status, got.body)
		}
		out := []string{}
		for _, p := range people {
			out = append(out, p.FirstName)
		}
		slices.Sort(out)
		return out
	}
	if got := search(); slices.Contains(got, "mobMember") {
		t.Fatalf("before: %v", got)
	}
	wantStatus(t, "assign", e.send(e.appToken("yk"), fiber.MethodPost, "/v1/apps/forms/assignments", userTarget(e, "mobMember", "skyforms:access")), fiber.StatusCreated, "")
	if got := search(); !slices.Contains(got, "mobMember") {
		t.Fatalf("after: %v (the seat search kept its old answer)", got)
	}
	wantStatus(t, "remove", e.send(e.appToken("yk"), fiber.MethodDelete, "/v1/apps/forms/assignments", userTarget(e, "mobMember", "skyforms:access")), fiber.StatusNoContent, "")
	if got := search(); slices.Contains(got, "mobMember") {
		t.Fatalf("after the removal: %v", got)
	}
}

// The add drawer's searches: the Groups an app role may go to, with their
// counts and what they hold; people by the member-candidates rule.
func TestAppAssignmentTargets(t *testing.T) {
	t.Parallel()
	e := newAppsEnv(t, true)
	yk := e.appToken("yk")
	wantStatus(t, "assign", e.send(yk, fiber.MethodPost, "/v1/apps/forms/assignments", groupTarget(e, "/UYELER/ARGE/WEBLAB/FRONT", "skyforms:access")), fiber.StatusCreated, "")
	var targets []identity.AppTarget
	got := e.send(yk, fiber.MethodGet, "/v1/apps/forms/targets?type=group&q=weblab", "")
	if err := json.Unmarshal(got.body, &targets); err != nil {
		t.Fatalf("%d %s", got.status, got.body)
	}
	paths := []string{}
	for _, target := range targets {
		paths = append(paths, target.Path)
		if target.Path == "/UYELER/ARGE/WEBLAB/FRONT" && (!slices.Equal(target.AssignedRoles, []string{"skyforms:access"}) || target.InheritsTo != 1 || target.Members != 1) {
			t.Errorf("FRONT %+v", target)
		}
	}
	if !slices.Equal(paths, []string{"/UYELER/ARGE/WEBLAB", "/UYELER/ARGE/WEBLAB/FRONT", "/UYELER/ARGE/WEBLAB/FRONT/LIDERLER",
		"/UYELER/ARGE/WEBLAB/KOORDINATORLER", "/UYELER/ARGE/WEBLAB/LIDERLER", "/UYELER/ARGE/WEBLAB/SECURE"}) {
		t.Errorf("paths %v", paths)
	}
	got = e.send(yk, fiber.MethodGet, "/v1/apps/forms/targets?type=group&q=yk", "")
	if err := json.Unmarshal(got.body, &targets); err != nil || len(targets) != 0 {
		t.Errorf("YK is a target: %s", got.body)
	}
	got = e.send(yk, fiber.MethodGet, "/v1/apps/forms/targets?type=user&q=mobmem", "")
	if err := json.Unmarshal(got.body, &targets); err != nil || len(targets) != 1 || targets[0].Email != "m***@example.test" || targets[0].Name != "mobMember Kişi" {
		t.Errorf("people %s", got.body)
	}
	wantStatus(t, "two letters", e.send(yk, fiber.MethodGet, "/v1/apps/forms/targets?type=user&q=mo", ""), fiber.StatusBadRequest, "")
	wantStatus(t, "no kind", e.send(yk, fiber.MethodGet, "/v1/apps/forms/targets?q=web", ""), fiber.StatusBadRequest, "")
}

// No row, no change: with the audit record down a change is 503 and
// Keycloak is not asked.
func TestAppAssignmentsAuditUnavailable(t *testing.T) {
	t.Parallel()
	e := newAppsEnv(t, true)
	e.audit.SetUnavailable(true)
	wantStatus(t, "audit down", e.send(e.appToken("yk"), fiber.MethodPost, "/v1/apps/forms/assignments", groupTarget(e, "/UYELER/ARGE/WEBLAB", "skyforms:access")),
		fiber.StatusServiceUnavailable, "audit_unavailable")
	if e.groupMaps("/UYELER/ARGE/WEBLAB", formsSeat) || slices.Contains(e.dir.Ops, "AddGroupClientRole") {
		t.Fatal("mapped without an audit row")
	}
}

// An app role that holds, as a composite at any depth, an admin role (its
// own app's or another's) or a role nobody has classified is refused with
// a denied row, to a Group and to a person (K1); one that holds app roles
// only is assigned (ticket 15, T1).
func TestAppAssignmentsRefuseACompositeAdminRole(t *testing.T) {
	t.Parallel()
	e := newAppsEnv(t, true)
	yk := e.appToken("yk")
	role := func(client, name string) identity.ClientRole { return identity.ClientRole{ClientID: client, Role: name} }
	// url:access holds url:get, which holds Forms' skyforms:*.
	e.dir.PutCompositeRole(role("core", "url:access"), role("core", "url:get"))
	e.dir.PutCompositeRole(role("core", "url:get"), role("forms", "skyforms:*"))
	e.dir.PutCompositeRole(role("skymail", "skymail:access"), role("skymail", "skymail:mails:send"))
	e.dir.PutCompositeRole(role("core", "url:create"), role("forms", "forms:editor"))
	e.dir.PutCompositeRole(role("core", "certificate:issue"), role("core", "certificate:revoke"), role("frontend-main", "content:read"))
	for _, tc := range []struct{ name, client, body string }{
		{"another app's admin role, two deep", "core", groupTarget(e, "/UYELER/ARGE/WEBLAB", "url:access")},
		{"its own app's admin role", "skymail", groupTarget(e, "/UYELER/ARGE/WEBLAB", "skymail:access")},
		{"an unclassified role", "core", groupTarget(e, "/UYELER/ARGE/WEBLAB", "url:create")},
		{"to a person", "core", userTarget(e, "mobMember", "url:access")},
	} {
		got := e.send(yk, fiber.MethodPost, "/v1/apps/"+tc.client+"/assignments", tc.body)
		wantStatus(t, tc.name, got, fiber.StatusConflict, "role_placement")
		if reason, _ := got.problem()["reason"].(string); reason != authz.PlacementAdminRole {
			t.Errorf("%s: reason %q (%s)", tc.name, reason, got.body)
		}
		last := e.appRows()[len(e.appRows())-1]
		if last.Outcome != teamaudit.OutcomeDenied || last.ErrorCode != "role_placement" {
			t.Errorf("%s: row %+v", tc.name, last)
		}
	}
	roles, _ := e.dir.GroupClientRoles(context.Background(), e.ids["/UYELER/ARGE/WEBLAB"])
	if len(roles) != 0 || e.userMaps("mobMember", role("core", "url:access")) {
		t.Fatalf("a composite admin role was mapped: %v", roles)
	}
	wantStatus(t, "app roles only", e.send(yk, fiber.MethodPost, "/v1/apps/core/assignments", groupTarget(e, "/UYELER/ARGE/WEBLAB", "certificate:issue")), fiber.StatusCreated, "")
	// Taking one off is never refused for what it holds.
	if err := e.dir.AddGroupClientRole(context.Background(), e.ids["/UYELER/ARGE/WEBLAB"], role("skymail", "skymail:access")); err != nil {
		t.Fatal(err)
	}
	wantStatus(t, "remove", e.send(yk, fiber.MethodDelete, "/v1/apps/skymail/assignments", groupTarget(e, "/UYELER/ARGE/WEBLAB", "skymail:access")), fiber.StatusNoContent, "")
}

// A person whose account core is erasing (Keycloak still enabled) is given
// no app role; one they hold is taken off.
func TestAppAssignmentsAPersonBeingErased(t *testing.T) {
	t.Parallel()
	e := newAppsEnv(t, true)
	ctx := context.Background()
	e.person("erasing", "/UYELER/ARGE/MOBILAB")
	if err := e.dir.AddUserExtraRole(ctx, e.people["erasing"], formsSeat); err != nil {
		t.Fatal(err)
	}
	if _, err := e.users.RequestDeletion(ctx, e.people["erasing"], nil); err != nil {
		t.Fatal(err)
	}
	yk := e.appToken("yk")
	got := e.send(yk, fiber.MethodPost, "/v1/apps/skymail/assignments", userTarget(e, "erasing", "skymail:access"))
	wantStatus(t, "assign", got, fiber.StatusConflict, "role_placement")
	if reason, _ := got.problem()["reason"].(string); reason != authz.PlacementInactiveUser {
		t.Errorf("reason %q", reason)
	}
	if e.userMaps("erasing", identity.ClientRole{ClientID: "skymail", Role: "skymail:access"}) {
		t.Fatal("mapped")
	}
	wantStatus(t, "remove", e.send(yk, fiber.MethodDelete, "/v1/apps/forms/assignments", userTarget(e, "erasing", "skyforms:access")), fiber.StatusNoContent, "")
	if e.userMaps("erasing", formsSeat) {
		t.Fatal("still mapped")
	}
}

// With the app assignments on, the old Group role mapping write changes no
// app role of a Team, a Sub-team or a Leader group (409 role_placement
// use_app_assignments): adding one or taking one off. Their other roles and
// the code's places stay on it. Off, it writes them as before (T3).
func TestAppAssignmentsOldGroupRolesRoute(t *testing.T) {
	t.Parallel()
	put := func(e *delegationEnv, path, body string) rawResponse {
		return e.send(e.appToken("admin"), fiber.MethodPut, "/v1/groups/"+e.ids[path]+"/client-roles", body)
	}
	const seat = `{"clientId":"forms","role":"skyforms:access"}`
	const cms = `{"clientId":"frontend-main","role":"cms:access"}`
	const mail = `{"clientId":"skymail","role":"skymail:access"}`

	off := newAppsEnv(t, false)
	wantStatus(t, "off: a Team's app role", put(off, "/UYELER/ARGE/WEBLAB", `[`+seat+`]`), fiber.StatusNoContent, "")
	if !off.groupMaps("/UYELER/ARGE/WEBLAB", formsSeat) {
		t.Fatal("off: not mapped")
	}

	e := newAppsEnv(t, true)
	wantReason := func(name string, got rawResponse) {
		t.Helper()
		wantStatus(t, name, got, fiber.StatusConflict, "role_placement")
		if reason, _ := got.problem()["reason"].(string); reason != authz.PlacementUseAppAssignments {
			t.Errorf("%s: reason %q (%s)", name, reason, got.body)
		}
	}
	wantReason("a Team's app role", put(e, "/UYELER/ARGE/WEBLAB", `[`+seat+`]`))
	wantReason("a Sub-team's app role", put(e, "/UYELER/ARGE/WEBLAB/FRONT", `[`+seat+`]`))
	if e.groupMaps("/UYELER/ARGE/WEBLAB", formsSeat) || e.groupMaps("/UYELER/ARGE/WEBLAB/FRONT", formsSeat) {
		t.Fatal("mapped")
	}
	yk := e.appToken("yk")
	wantStatus(t, "panel", e.send(yk, fiber.MethodPost, "/v1/apps/forms/assignments", groupTarget(e, "/UYELER/ARGE/WEBLAB", "skyforms:access")), fiber.StatusCreated, "")
	wantReason("taking it off", put(e, "/UYELER/ARGE/WEBLAB", `[]`))
	if !e.groupMaps("/UYELER/ARGE/WEBLAB", formsSeat) {
		t.Fatal("the old route took a panel assignment off")
	}
	wantStatus(t, "unchanged", put(e, "/UYELER/ARGE/WEBLAB", `[`+seat+`]`), fiber.StatusNoContent, "")

	wantStatus(t, "a Leader group's admin role", put(e, "/UYELER/ARGE/WEBLAB/LIDERLER", `[`+cms+`]`), fiber.StatusNoContent, "")
	wantStatus(t, "panel, Leader group", e.send(yk, fiber.MethodPost, "/v1/apps/skymail/assignments", groupTarget(e, "/UYELER/ARGE/WEBLAB/LIDERLER", "skymail:access")), fiber.StatusCreated, "")
	wantStatus(t, "its admin role off, the app role kept", put(e, "/UYELER/ARGE/WEBLAB/LIDERLER", `[`+mail+`]`), fiber.StatusNoContent, "")
	wantReason("its app role off", put(e, "/UYELER/ARGE/WEBLAB/LIDERLER", `[`+cms+`]`))
	if !e.groupMaps("/UYELER/ARGE/WEBLAB/LIDERLER", identity.ClientRole{ClientID: "skymail", Role: "skymail:access"}) ||
		e.groupMaps("/UYELER/ARGE/WEBLAB/LIDERLER", identity.ClientRole{ClientID: "frontend-main", Role: "cms:access"}) {
		t.Fatal("Leader group's mappings")
	}

	wantStatus(t, "a role group", put(e, "/ROLLER/FORMS-ACCESS", `[`+seat+`]`), fiber.StatusNoContent, "")
	wantStatus(t, "a Privileged root", put(e, "/UYELER/YK", `[`+seat+`]`), fiber.StatusNoContent, "")
}

// The list keeps a Group's counts for a minute: a second opening walks no
// subtree; an assignment drops them (T4).
func TestAppAssignmentListKeepsTheCounts(t *testing.T) {
	t.Parallel()
	e := newAppsEnv(t, true)
	yk := e.appToken("yk")
	wantStatus(t, "assign", e.send(yk, fiber.MethodPost, "/v1/apps/forms/assignments", groupTarget(e, "/UYELER/ARGE/WEBLAB", "skyforms:access")), fiber.StatusCreated, "")
	walks := func() int {
		t.Helper()
		from := len(e.dir.Ops)
		var list identity.AppAssignments
		got := e.send(yk, fiber.MethodGet, "/v1/apps/forms/assignments", "")
		if err := json.Unmarshal(got.body, &list); err != nil || len(list.Assignments) == 0 || list.Assignments[0].Members == 0 {
			t.Fatalf("%d %s", got.status, got.body)
		}
		n := 0
		for _, op := range e.dir.Ops[from:] {
			if op == "Members" || op == "Subgroups" {
				n++
			}
		}
		return n
	}
	walks()
	if n := walks(); n != 0 {
		t.Errorf("the second opening walked the tree again: %d reads", n)
	}
	wantStatus(t, "assign FRONT", e.send(yk, fiber.MethodPost, "/v1/apps/forms/assignments", groupTarget(e, "/UYELER/ARGE/WEBLAB/FRONT", "skyforms:access")), fiber.StatusCreated, "")
	if n := walks(); n == 0 {
		t.Error("an assignment did not drop the counts")
	}
}
