package authz

import (
	"strings"
	"testing"
)

// delegationTree is a realm like production's, with a Sub-team below
// WEBLAB and one below that. Ids are the paths' last letters, short for the
// tables.
var delegationTree = map[string]GroupNode{
	"u":   {ID: "u", Name: "UYELER", Path: "/UYELER"},
	"a":   {ID: "a", Name: "ARGE", Path: "/UYELER/ARGE", ParentID: "u"},
	"al":  {ID: "al", Name: "LIDERLER", Path: "/UYELER/ARGE/LIDERLER", ParentID: "a"},
	"w":   {ID: "w", Name: "WEBLAB", Path: "/UYELER/ARGE/WEBLAB", ParentID: "a"},
	"wl":  {ID: "wl", Name: "LIDERLER", Path: "/UYELER/ARGE/WEBLAB/LIDERLER", ParentID: "w"},
	"wk":  {ID: "wk", Name: "KOORDINATORLER", Path: "/UYELER/ARGE/WEBLAB/KOORDINATORLER", ParentID: "w"},
	"wx":  {ID: "wx", Name: "LIDERLER-ESKI", Path: "/UYELER/ARGE/WEBLAB/LIDERLER-ESKI", ParentID: "w"},
	"wf":  {ID: "wf", Name: "FRONT", Path: "/UYELER/ARGE/WEBLAB/FRONT", ParentID: "w"},
	"wfl": {ID: "wfl", Name: "LIDERLER", Path: "/UYELER/ARGE/WEBLAB/FRONT/LIDERLER", ParentID: "wf"},
	"wfr": {ID: "wfr", Name: "REACT", Path: "/UYELER/ARGE/WEBLAB/FRONT/REACT", ParentID: "wf"},
	"m":   {ID: "m", Name: "MOBILAB", Path: "/UYELER/ARGE/MOBILAB", ParentID: "a"},
	"ml":  {ID: "ml", Name: "LIDERLER", Path: "/UYELER/ARGE/MOBILAB/LIDERLER", ParentID: "m"},
	"yk":  {ID: "yk", Name: "YK", Path: "/UYELER/YK", ParentID: "u"},
	"ykb": {ID: "ykb", Name: "BASKAN", Path: "/UYELER/YK/BASKAN", ParentID: "yk"},
	"ykl": {ID: "ykl", Name: "LIDERLER", Path: "/UYELER/YK/BASKAN/LIDERLER", ParentID: "ykb"},
}

// target is the Group id as a delegated decision reads it: the Group and
// every Group above it by id.
func target(id string, protected bool) ManagedGroup {
	g := ManagedGroup{Group: delegationTree[id], Protected: protected}
	for parent := g.Group.ParentID; parent != ""; parent = delegationTree[parent].ParentID {
		g.Ancestors = append(g.Ancestors, delegationTree[parent])
	}
	return g
}

// leaderIn is a caller directly in the Groups ids, as Keycloak answers
// their Groups.
func leaderIn(ids ...string) TeamManager {
	var m TeamManager
	for _, id := range ids {
		g := delegationTree[id]
		m.LeaderGroups = append(m.LeaderGroups, LeaderGroup{ID: g.ID, Name: g.Name, Path: g.Path, TeamID: g.ParentID})
	}
	return m
}

func delegationOn() Policy {
	p := DefaultPolicy()
	p.TeamDelegation = true
	return p
}

func TestParseTeamDelegation(t *testing.T) {
	t.Parallel()
	for raw, want := range map[string]bool{"": false, "off": false, " off ": false, "on": true, " on": true} {
		if got, err := ParseTeamDelegation(raw); err != nil || got != want {
			t.Errorf("%q: %v, %v; want %v", raw, got, err, want)
		}
	}
	for _, raw := range []string{"ON", "true", "1", "yes", "of", "on,off"} {
		if _, err := ParseTeamDelegation(raw); err == nil || !strings.Contains(err.Error(), TeamDelegationEnv) {
			t.Errorf("%q: %v; want an error naming %s", raw, err, TeamDelegationEnv)
		}
	}
}

func TestManages(t *testing.T) {
	t.Parallel()
	p := delegationOn()
	type want struct {
		allowed bool
		via     string
		refusal string
	}
	for _, tc := range []struct {
		name   string
		m      TeamManager
		target ManagedGroup
		want   want
	}{
		{"WEBLAB's Leader: WEBLAB", leaderIn("wl"), target("w", false), want{true, "w", ""}},
		{"WEBLAB's Leader: a Sub-team", leaderIn("wl"), target("wf", false), want{true, "w", ""}},
		{"WEBLAB's Leader: a Sub-team's Sub-team", leaderIn("wl"), target("wfr", false), want{true, "w", ""}},
		{"WEBLAB's Leader: WEBLAB's Leader group", leaderIn("wl"), target("wl", false), want{true, "w", ""}},
		{"WEBLAB's Leader: a Sub-team's Leader group", leaderIn("wl"), target("wfl", false), want{true, "w", ""}},
		{"KOORDINATORLER is a Leader group (S3)", leaderIn("wk"), target("wf", false), want{true, "w", ""}},
		{"both levels: the nearest Team grants", leaderIn("wl", "wfl"), target("wfr", false), want{true, "wf", ""}},
		{"a Sub-team's Leader: their Sub-team", leaderIn("wfl"), target("wf", false), want{true, "wf", ""}},
		{"a Sub-team's Leader: below it", leaderIn("wfl"), target("wfr", false), want{true, "wf", ""}},
		{"a Sub-team's Leader: not the Team above", leaderIn("wfl"), target("w", false), want{}},
		{"a sibling Team's Leader", leaderIn("ml"), target("w", false), want{}},
		{"a sibling Team's Leader: a Sub-team", leaderIn("ml"), target("wf", false), want{}},
		{"a member is no Leader", leaderIn("w"), target("w", false), want{}},
		{"LIDERLER-ESKI is no Leader group", leaderIn("wx"), target("w", false), want{}},
		{"an area is no Team", leaderIn("wl"), target("a", false), want{}},
		{"an area's LIDERLER makes no Leader", leaderIn("al"), target("w", false), want{}},
		{"the members' tree is no Team", leaderIn("wl"), target("u", false), want{}},
		{"a Privileged root is no Team", leaderIn("ykl"), target("ykb", false), want{}},
		{"under a Privileged root", leaderIn("ykl"), target("ykl", false), want{}},
		{"a protected Group", leaderIn("wl"), target("wf", true), want{false, "w", RefusalProtectedGroup}},
		{"a protected Group nobody leads stays a plain refusal", leaderIn("ml"), target("wf", true), want{}},
		{"nobody", TeamManager{}, target("w", false), want{}},
	} {
		got := p.Manages(tc.m, tc.target)
		if got.Allowed != tc.want.allowed || got.ViaTeamID != tc.want.via || got.Refusal != tc.want.refusal {
			t.Errorf("%s: %+v; want %+v", tc.name, got, tc.want)
		}
		if (got.Allowed || got.Refusal != "") && got.Basis != ManageBasisLeader {
			t.Errorf("%s: basis %s", tc.name, got.Basis)
		}
	}
}

// Privileged manages every Group, delegation on or off, protected or not;
// with delegation off nobody else manages any.
func TestManagesPrivilegedAndOff(t *testing.T) {
	t.Parallel()
	for _, on := range []bool{false, true} {
		p := DefaultPolicy()
		p.TeamDelegation = on
		for id := range delegationTree {
			for _, protected := range []bool{false, true} {
				g := target(id, protected)
				privileged := TeamManager{Privileged: true}
				for name, decide := range map[string]func(TeamManager, ManagedGroup) ManageDecision{
					"Manages": p.Manages, "ManagesStrict": p.ManagesStrict, "ManagesMembership": p.ManagesMembership,
				} {
					if got := decide(privileged, g); !got.Allowed || got.Basis != ManageBasisPrivileged || got.ViaTeamID != "" {
						t.Errorf("on=%v %s %s: Privileged %+v", on, name, id, got)
					}
					if on {
						continue
					}
					if got := decide(leaderIn("wl", "wk", "wfl", "ml", "al", "ykl"), g); got != (ManageDecision{}) {
						t.Errorf("off %s %s: a Leader %+v", name, id, got)
					}
				}
			}
		}
	}
}

func TestManagesStrict(t *testing.T) {
	t.Parallel()
	p := delegationOn()
	for _, tc := range []struct {
		name   string
		m      TeamManager
		target string
		want   bool
	}{
		{"WEBLAB's Leader: their own Team", leaderIn("wl"), "w", false},
		{"WEBLAB's Leader: a Sub-team", leaderIn("wl"), "wf", true},
		{"WEBLAB's Leader: a Sub-team's Sub-team", leaderIn("wl"), "wfr", true},
		{"a Sub-team's Leader: their own Sub-team", leaderIn("wfl"), "wf", false},
		{"a Sub-team's Leader: below it", leaderIn("wfl"), "wfr", true},
		{"a Leader group is not strictly managed", leaderIn("wl"), "wfl", false},
		{"a sibling Team's Leader", leaderIn("ml"), "wf", false},
		{"an area's Leader group: a top Team", leaderIn("al"), "w", false},
	} {
		if got := p.ManagesStrict(tc.m, target(tc.target, false)); got.Allowed != tc.want {
			t.Errorf("%s: %+v; want %v", tc.name, got, tc.want)
		}
	}
	if got := p.ManagesStrict(leaderIn("wl"), target("wf", true)); got.Allowed || got.Refusal != RefusalProtectedGroup {
		t.Errorf("a protected Sub-team: %+v", got)
	}
}

// A code change is Privileged's unless LeaderTeamCodes is on: a Leader who
// manages the Sub-team strictly is refused with its own code (and the
// decision's basis and Team, for the denied row); every other answer is
// ManagesStrict's.
func TestChangesTeamCode(t *testing.T) {
	t.Parallel()
	p := delegationOn()
	if got := p.ChangesTeamCode(TeamManager{Privileged: true}, target("wf", false)); !got.Allowed {
		t.Errorf("Privileged: %+v", got)
	}
	got := p.ChangesTeamCode(leaderIn("wl"), target("wf", false))
	if got.Allowed || got.Refusal != RefusalTeamCodePrivilegedOnly || got.Basis != ManageBasisLeader || got.ViaTeamID == "" {
		t.Errorf("a Leader of the Team above, off: %+v", got)
	}
	if got := p.ChangesTeamCode(leaderIn("wl"), target("w", false)); got.Allowed || got.Refusal != "" {
		t.Errorf("their own Team: %+v", got)
	}
	if got := p.ChangesTeamCode(leaderIn("wl"), target("wf", true)); got.Allowed || got.Refusal != RefusalProtectedGroup {
		t.Errorf("a protected Sub-team: %+v", got)
	}
	p.LeaderTeamCodes = true
	if got := p.ChangesTeamCode(leaderIn("wl"), target("wf", false)); !got.Allowed || got.Basis != ManageBasisLeader {
		t.Errorf("a Leader of the Team above, on: %+v", got)
	}
	if got := p.ChangesTeamCode(leaderIn("wl"), target("w", false)); got.Allowed {
		t.Errorf("their own Team, on: %+v", got)
	}
}

func TestParseLeaderTeamCodes(t *testing.T) {
	t.Parallel()
	for raw, want := range map[string]bool{"": false, "off": false, " on ": true} {
		if got, err := ParseLeaderTeamCodes(raw); err != nil || got != want {
			t.Errorf("%q: %v %v", raw, got, err)
		}
	}
	for _, raw := range []string{"true", "ON", "1"} {
		if _, err := ParseLeaderTeamCodes(raw); err == nil || !strings.Contains(err.Error(), LeaderTeamCodesEnv) {
			t.Errorf("%q: %v", raw, err)
		}
	}
}

// The membership rule: a Team's by Manages, a Leader group's by
// ManagesStrict of its Team. Nobody makes themselves a Leader, or leaves
// their Team without one, through the members routes.
func TestManagesMembership(t *testing.T) {
	t.Parallel()
	p := delegationOn()
	for _, tc := range []struct {
		name    string
		m       TeamManager
		target  string
		allowed bool
		refusal string
	}{
		{"WEBLAB's Leader: WEBLAB", leaderIn("wl"), "w", true, ""},
		{"WEBLAB's Leader: their own Leader group", leaderIn("wl"), "wl", false, RefusalLeaderGroupNeedsParent},
		{"WEBLAB's Coordinator: WEBLAB's LIDERLER", leaderIn("wk"), "wl", false, RefusalLeaderGroupNeedsParent},
		{"WEBLAB's Leader: a Sub-team's Leader group", leaderIn("wl"), "wfl", true, ""},
		{"a Sub-team's Leader: their own Leader group", leaderIn("wfl"), "wfl", false, RefusalLeaderGroupNeedsParent},
		{"a sibling's Leader: WEBLAB's Leader group", leaderIn("ml"), "wl", false, ""},
		{"a member: WEBLAB's Leader group", leaderIn("w"), "wl", false, ""},
		{"an area's Leader group", leaderIn("wl"), "al", false, ""},
	} {
		got := p.ManagesMembership(tc.m, target(tc.target, false))
		if got.Allowed != tc.allowed || got.Refusal != tc.refusal {
			t.Errorf("%s: %+v; want %v %q", tc.name, got, tc.allowed, tc.refusal)
		}
	}
	if got := p.ManagesMembership(leaderIn("wl"), target("wfl", true)); got.Allowed || got.Refusal != RefusalProtectedGroup {
		t.Errorf("a protected Sub-team's Leader group: %+v", got)
	}
	if got := p.ManagesMembership(leaderIn("wl"), target("wl", true)); got.Allowed || got.Refusal != RefusalProtectedGroup {
		t.Errorf("a protected own Leader group: %+v", got)
	}
}

// The chain is the ids' and the paths' together: a chain that does not
// hold decides nothing, for anyone but Privileged.
func TestManagesChainMustHold(t *testing.T) {
	t.Parallel()
	p := delegationOn()
	leader := leaderIn("wl")
	for name, change := range map[string]func(*ManagedGroup){
		"a parent id that is not the next Group": func(g *ManagedGroup) { g.Group.ParentID = "m" },
		"a path that is not the parent's":        func(g *ManagedGroup) { g.Group.Path = "/UYELER/ARGE/MOBILAB/FRONT" },
		"an ancestor missing":                    func(g *ManagedGroup) { g.Ancestors = g.Ancestors[1:] },
		"no ancestors":                           func(g *ManagedGroup) { g.Ancestors = nil },
		"a top Group with a parent":              func(g *ManagedGroup) { g.Ancestors[len(g.Ancestors)-1].ParentID = "x" },
		// Keycloak takes a "/" in a name: WEB/FRONT under ARGE reads as
		// /UYELER/ARGE/WEB/FRONT, a Group that is not there.
		"a name with a slash": func(g *ManagedGroup) {
			g.Group.Name, g.Group.Path, g.Group.ParentID = "WEBLAB/FRONT", "/UYELER/ARGE/WEBLAB/FRONT", "a"
			g.Ancestors = g.Ancestors[1:]
		},
	} {
		g := target("wf", false)
		change(&g)
		for decide, got := range map[string]ManageDecision{
			"Manages": p.Manages(leader, g), "ManagesStrict": p.ManagesStrict(leader, g), "ManagesMembership": p.ManagesMembership(leader, g),
		} {
			if got.Allowed {
				t.Errorf("%s: %s allowed %+v", name, decide, got)
			}
		}
		if ChainHolds(g.Group, g.Ancestors) {
			t.Errorf("%s: the chain holds", name)
		}
	}
	if g := target("wfr", false); !ChainHolds(g.Group, g.Ancestors) {
		t.Errorf("a real chain does not hold")
	}
}

// A Leader group answers for its Team only when its id, name and path
// agree: a Group the caller is in whose parent is the Team but whose path
// or name says otherwise grants nothing.
func TestManagesLeaderGroupMustAgree(t *testing.T) {
	t.Parallel()
	p := delegationOn()
	for name, leader := range map[string]LeaderGroup{
		"another parent":     {ID: "wl", Name: "LIDERLER", Path: "/UYELER/ARGE/WEBLAB/LIDERLER", TeamID: "m"},
		"no parent":          {ID: "wl", Name: "LIDERLER", Path: "/UYELER/ARGE/WEBLAB/LIDERLER"},
		"a path elsewhere":   {ID: "wl", Name: "LIDERLER", Path: "/UYELER/ARGE/MOBILAB/LIDERLER", TeamID: "w"},
		"not a leader name":  {ID: "wx", Name: "LIDERLER-ESKI", Path: "/UYELER/ARGE/WEBLAB/LIDERLER-ESKI", TeamID: "w"},
		"a name with slash":  {ID: "wz", Name: "X/LIDERLER", Path: "/UYELER/ARGE/WEBLAB/X/LIDERLER", TeamID: "w"},
		"lower-case leaders": {ID: "wy", Name: "liderler", Path: "/UYELER/ARGE/WEBLAB/liderler", TeamID: "w"},
	} {
		if got := p.Manages(TeamManager{LeaderGroups: []LeaderGroup{leader}}, target("w", false)); got.Allowed {
			t.Errorf("%s: %+v", name, got)
		}
	}
}

// A client role is delegable only when it is an app role, named by its
// (client, role) pair; a known admin role and a role nobody has classified
// protect (ADR 0065 decision 10, addendum of 2026-10-12).
func TestClassifyRole(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		client, role string
		want         RoleClass
	}{
		{"core", RoleEventManage, RoleClassAdmin},
		{"core", RoleGroupsManage, RoleClassAdmin},
		{"core", RoleUsersManage, RoleClassAdmin},
		{"core", RoleURLModerator, RoleClassAdmin},
		{"core", RoleAuditRead, RoleClassAdmin},
		{"core", "users:read", RoleClassAdmin},
		{"core", "media:attach", RoleClassAdmin},
		{"skymail", "client:admin", RoleClassAdmin},
		{"forms", "client:admin", RoleClassAdmin},
		{"realm-management", "view-users", RoleClassAdmin},
		{"realm-management", "anything", RoleClassAdmin},
		// A Site editor, by its own name and by its write, on any client.
		{"frontend-main", "cms:access", RoleClassAdmin},
		{"frontend-arge", "cms:access", RoleClassAdmin},
		{"skyapp", "cms:access", RoleClassAdmin},
		{"frontend-main", "content:write", RoleClassAdmin},
		{"frontend-artlab", "content:write", RoleClassAdmin},
		{"admin", "content:write", RoleClassAdmin},
		{"frontend-main", "schema:sync", RoleClassAdmin},
		{"forms", "skyforms:*", RoleClassAdmin},
		{"forms", "skyforms:feedback:*", RoleClassAdmin},
		// DK's read-only Forms role (grup-yonetimi 08): every form and answer,
		// club-wide personal data, so an admin role (not unclassified).
		{"forms", "skyforms:read-all", RoleClassAdmin},
		{"skymail", "skymail:templates:write", RoleClassAdmin},
		{"skymail", "skymail:lists:read", RoleClassAdmin},
		{"skymail", "skymail:mails:approve", RoleClassAdmin},
		{"skymail", "skymail:system-mails:read", RoleClassAdmin},
		{"place", "place:admin", RoleClassAdmin},
		{"place", "place:moderator", RoleClassAdmin},
		{"core", RoleCompetitionManage, RoleClassAdmin},
		// The app roles.
		{"core", "certificate:issue", RoleClassApp},
		{"core", "certificate:revoke", RoleClassApp},
		{"core", "certificate:template:manage", RoleClassApp},
		{"core", "certificate:binding:manage", RoleClassApp},
		{"core", "url:create", RoleClassApp},
		{"core", "url:get", RoleClassApp},
		{"core", "url:update", RoleClassApp},
		{"core", "url:delete", RoleClassApp},
		{"core", RoleURLAccess, RoleClassApp},
		{"forms", "skyforms:access", RoleClassApp},
		{"forms", "skyforms:form:manage", RoleClassApp},
		{"forms", "skyforms:response:manage", RoleClassApp},
		{"forms", "skyforms:feedback:manage", RoleClassApp},
		{"broker", "read-token", RoleClassUnclassified},
		{"skycloud", "skycloud:access", RoleClassUnclassified},
		{"skymail", "skymail:access", RoleClassApp},
		{"skymail", "skymail:templates:read", RoleClassApp},
		{"frontend-main", "content:read", RoleClassApp},
		{"frontend-arge", "content:read", RoleClassApp},
		{"frontend-artlab", "content:read", RoleClassApp},
		{"frontend-yildizjam", "content:read", RoleClassApp},
		{"frontend-skydays", "content:read", RoleClassApp},
		// The pair, not the name: an app role's name on another client is
		// unclassified, so protected.
		{"skymail", "certificate:issue", RoleClassUnclassified},
		{"dotnet", "skyforms:access", RoleClassUnclassified},
		{"admin", "content:read", RoleClassUnclassified},
		{"skyapp", "content:read", RoleClassUnclassified},
		{"frontend-newsite", "content:read", RoleClassUnclassified},
		{"forms", "forms:editor", RoleClassUnclassified},
		{"core", "news:read", RoleClassUnclassified},
		{"core", "", RoleClassUnclassified},
	} {
		if got := ClassifyRole(tc.client, tc.role); got != tc.want {
			t.Errorf("%s %s: %s, want %s", tc.client, tc.role, got, tc.want)
		}
		if got := DelegableRole(tc.client, tc.role); got != (tc.want == RoleClassApp) {
			t.Errorf("%s %s: delegable %v", tc.client, tc.role, got)
		}
	}
}

// No app role is also an admin role: the admin list would win and quietly
// take it off the delegable list.
func TestRoleListsAreDisjoint(t *testing.T) {
	t.Parallel()
	for _, app := range AppRoles() {
		if app.Client == "" || app.AnyRole {
			t.Errorf("app role %+v names more than one (client, role) pair", app)
		}
		if got := ClassifyRole(app.Client, app.Role); got != RoleClassApp {
			t.Errorf("app role %s:%s classifies as %s", app.Client, app.Role, got)
		}
	}
}

// Every realm role is an admin role: none is classified (fail-closed).
func TestClassifyRealmRoleIsAlwaysAdmin(t *testing.T) {
	t.Parallel()
	for _, role := range []string{"USER", "WEBLAB", "WEBLAB_LEADER", "ADMIN", "offline_access", ""} {
		if got := ClassifyRealmRole(role); got != RoleClassAdmin {
			t.Errorf("%q: %s", role, got)
		}
	}
}

// ProtectedRole, the old name, answers "a known admin role": an app role
// and a role nobody has classified are not.
func TestProtectedRoleIsAKnownAdminRole(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		client, role string
		want         bool
	}{
		{"core", RoleGroupsManage, true},
		{"frontend-main", "cms:access", true},
		{"skymail", "client:admin", true},
		{"realm-management", "view-users", true},
		{"core", "certificate:issue", false},
		{"forms", "viewer", false},
	} {
		if got := ProtectedRole(tc.client, tc.role); got != tc.want {
			t.Errorf("%s %s: %v", tc.client, tc.role, got)
		}
	}
}

func TestTeamOf(t *testing.T) {
	t.Parallel()
	roots, leaders := DefaultPrivilegedRoots(), DefaultPolicy().LeaderSubgroups
	for _, tc := range []struct {
		path, team, role string
		ok               bool
	}{
		{"/UYELER/ARGE/WEBLAB", "/UYELER/ARGE/WEBLAB", "", true},
		{"/UYELER/ARGE/WEBLAB/FRONT", "/UYELER/ARGE/WEBLAB/FRONT", "", true},
		{"/UYELER/ARGE/WEBLAB/LIDERLER", "/UYELER/ARGE/WEBLAB", "LIDERLER", true},
		{"/UYELER/ARGE/WEBLAB/KOORDINATORLER", "/UYELER/ARGE/WEBLAB", "KOORDINATORLER", true},
		{"/UYELER/ARGE/WEBLAB/LIDERLER/X", "", "", false},
		{"/UYELER/ARGE", "", "", false},
		{"/UYELER/ARGE/LIDERLER", "", "", false},
		{"/UYELER", "", "", false},
		{"/UYELER/YK/BASKAN", "", "", false},
		{"/ADMIN/X/Y", "", "", false},
		{"/OTHER/A/B", "", "", false},
		{"/UYELER//WEBLAB", "", "", false},
	} {
		team, role, ok := TeamOf(tc.path, roots, leaders)
		if team != tc.team || role != tc.role || ok != tc.ok {
			t.Errorf("%s: %q %q %v", tc.path, team, role, ok)
		}
	}
}

// A Group whose name only looks like a leader subgroup's (LIDERLER-ESKI,
// "LIDERLER " with a blank, liderler) is neither a Leader group nor a Team
// a Leader manages: paths read by substring elsewhere could take its
// members for Leaders (review of #235, D1). Only Privileged manages it.
func TestManagesRefusesLeaderLookAlikes(t *testing.T) {
	t.Parallel()
	p := delegationOn()
	for _, name := range []string{"LIDERLER-ESKI", "LIDERLER ", " LIDERLER", "liderler", "KOORDINATORLER2", "Liderler-2026",
		"LİDERLER", "LİDERLER-ESKİ", "KOORDİNATORLER", "lıderler"} {
		g := target("wf", false)
		g.Group.Name, g.Group.Path = name, "/UYELER/ARGE/WEBLAB/"+name
		g.Group.ParentID = "w"
		for decide, got := range map[string]ManageDecision{
			"Manages": p.Manages(leaderIn("wl"), g), "ManagesMembership": p.ManagesMembership(leaderIn("wl"), g), "ManagesStrict": p.ManagesStrict(leaderIn("wl"), g),
		} {
			if got.Allowed {
				t.Errorf("%q: %s allowed", name, decide)
			}
		}
		if got := p.Manages(TeamManager{Privileged: true}, g); !got.Allowed {
			t.Errorf("%q: Privileged refused", name)
		}
		// Nor anything below one.
		below := ManagedGroup{Group: GroupNode{ID: "x", Name: "X", Path: g.Group.Path + "/X", ParentID: g.Group.ID}, Ancestors: append([]GroupNode{g.Group}, g.Ancestors...)}
		if got := p.Manages(leaderIn("wl"), below); got.Allowed {
			t.Errorf("under %q: allowed", name)
		}
	}
	// A real Sub-team whose name starts with neither stays managed.
	if got := p.Manages(leaderIn("wl"), target("wf", false)); !got.Allowed {
		t.Fatal("FRONT refused")
	}
}
