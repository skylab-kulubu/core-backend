package authz

import (
	"bytes"
	"log"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestPrivilegedRootsFromEnv(t *testing.T) {
	t.Parallel()
	env := func(value string) func(string) string {
		return func(name string) string {
			if name != PrivilegedRootsEnv {
				t.Fatalf("read %s", name)
			}
			return value
		}
	}
	for _, tc := range []struct {
		raw  string
		want []string
	}{
		{"", DefaultPrivilegedRoots()},
		{"   ", DefaultPrivilegedRoots()},
		{"/ADMIN", []string{"/ADMIN"}},
		{" /ADMIN , /UYELER/YK ", []string{"/ADMIN", "/UYELER/YK"}},
	} {
		got, err := PrivilegedRootsFromEnv(env(tc.raw))
		if err != nil || !slices.Equal(got, tc.want) {
			t.Errorf("%q: %v, %v; want %v", tc.raw, got, err, tc.want)
		}
	}
	for _, raw := range []string{
		"ADMIN",          // no leading slash
		"/ADMIN/",        // trailing slash
		"/",              // the realm itself
		"/ADMIN,,/YK",    // an empty entry
		"/ADMIN,",        // a trailing comma
		"/UYELER//YK",    // an empty segment
		"/ADMIN,/ADMIN",  // named twice
		"/YK, /DK , /YK", // named twice, blanks around
	} {
		got, err := PrivilegedRootsFromEnv(env(raw))
		if err == nil {
			t.Errorf("%q: %v, want an error", raw, got)
			continue
		}
		if !strings.Contains(err.Error(), PrivilegedRootsEnv) {
			t.Errorf("%q: %v does not name %s", raw, err, PrivilegedRootsEnv)
		}
	}
}

func TestDefaultPolicyHasTheSixRoots(t *testing.T) {
	t.Parallel()
	want := []string{"/ADMIN", "/YK", "/DK", "/UYELER/ADMIN", "/UYELER/YK", "/UYELER/DK"}
	if got := DefaultPolicy().PrivilegedRoots; !slices.Equal(got, want) {
		t.Fatalf("roots = %v, want %v", got, want)
	}
}

// privilegedPaths are Privileged: the six roots and what is under them.
var privilegedPaths = []string{
	"/ADMIN", "/YK", "/DK", "/UYELER/ADMIN", "/UYELER/YK", "/UYELER/DK",
	"/ADMIN/X", "/UYELER/YK/BASKAN", "/UYELER/YK/BASKAN/YARDIMCI", "/UYELER/DK/LIDERLER",
}

// unprivilegedPaths carry the names ADMIN, YK or DK somewhere other than a
// root, or a root's name only as part of a segment. Under the name match
// that core had before, every one but the plain team was Privileged.
var unprivilegedPaths = []string{
	"/UYELER/ARGE/WEBLAB/ADMIN",
	"/UYELER/ARGE/WEBLAB/ADMIN/X",
	"/UYELER/ARGE/YK",
	"/UYELER/ORGANIZASYON/DK",
	"/UYELER/ARGE/WEBLAB/LIDERLER/ADMIN",
	"/A/ADMIN/B",
	"/X/YK",
	"/YKX",
	"/ADMINX",
	"/XADMIN",
	"/UYELER/YKX",
	"/UYELER/ADMIN-ESKI",
	"/UYELERX/ADMIN",
	"/uyeler/yk",
	"/admin",
	"/UYELER/ARGE/WEBLAB",
}

// The anchored roots in all three modes: a root or a Group under one is
// Privileged where the mode reads Groups; the same name elsewhere never is.
// The roles mode reads roles only.
func TestAuthorizer_PrivilegedOnlyUnderTheRoots(t *testing.T) {
	t.Parallel()
	for _, mode := range []RoleMode{RoleModeGroups, RoleModeBoth, RoleModeRoles} {
		auth := NewAuthorizer(policyIn(mode))
		for _, path := range privilegedPaths {
			p := Principal{ID: "u1", Groups: []string{path}}
			want := mode != RoleModeRoles
			// A DK path reads and changes nothing (grup-yonetimi GY1).
			write := want && PrivilegedRoot(DefaultPrivilegedReaderRoots(), path) == ""
			if got := auth.Permitted(p, RoleGroupsManage); got != write {
				t.Errorf("%s %s: Permitted(groups:manage) = %v, want %v", mode, path, got, write)
			}
			if got := auth.Allow(p, Resource{Type: TypeGroup}, Create); got != write {
				t.Errorf("%s %s: group create = %v, want %v", mode, path, got, write)
			}
			if got := auth.Allow(p, Resource{Type: TypeGroup}, Read); got != want {
				t.Errorf("%s %s: group read = %v, want %v", mode, path, got, want)
			}
			if got := auth.Allow(p, Resource{Type: TypeURL}, Read); got != want {
				t.Errorf("%s %s: short link moderation = %v, want %v", mode, path, got, want)
			}
		}
		for _, path := range unprivilegedPaths {
			p := Principal{ID: "u1", Groups: []string{path}}
			if auth.Permitted(p, RoleGroupsManage) || auth.Allow(p, Resource{Type: TypeGroup}, Create) {
				t.Errorf("%s %s: Privileged", mode, path)
			}
			if auth.Allow(p, Resource{Type: TypeUser}, Update) || auth.Allow(p, Resource{Type: TypeEvent, OwnerTeam: "SKYSEC"}, Delete) {
				t.Errorf("%s %s: Privileged user or Event decision", mode, path)
			}
			if auth.Allow(p, Resource{Type: TypeURL}, Read) || auth.Allow(p, Resource{Type: TypeFormLink}, Update) {
				t.Errorf("%s %s: the Privileged shortcut of short links or form links", mode, path)
			}
			if c := auth.Capabilities(p); len(c.Permissions) != 0 || c.Can["group.write"] || c.Can["user.write"] {
				t.Errorf("%s %s: capabilities %v", mode, path, c.Permissions)
			}
		}
		// With the role, everyone is Permitted where the mode reads roles.
		holder := Principal{ID: "u1", Groups: []string{"/UYELER/ARGE/WEBLAB/ADMIN"}, Roles: []string{RoleGroupsManage}}
		if got, want := auth.Permitted(holder, RoleGroupsManage), mode != RoleModeGroups; got != want {
			t.Errorf("%s: role holder Permitted = %v, want %v", mode, got, want)
		}
	}
}

// A root of one's own (AUTHZ_PRIVILEGED_ROOTS) replaces the six: only it
// and what is under it count.
func TestAuthorizer_ConfiguredRoots(t *testing.T) {
	t.Parallel()
	policy := DefaultPolicy()
	policy.PrivilegedRoots = []string{"/STAFF", "/UYELER/YONETIM"}
	auth := NewAuthorizer(policy)
	for path, want := range map[string]bool{
		"/STAFF":                  true,
		"/STAFF/ADMIN":            true,
		"/UYELER/YONETIM/BASKAN":  true,
		"/ADMIN":                  false,
		"/UYELER/YK":              false,
		"/STAFFX":                 false,
		"/UYELER/ARGE/STAFF":      false,
		"/UYELER/ARGE/YONETIM":    false,
		"/UYELER/YONETIM-ESKI/YK": false,
	} {
		if got := auth.Permitted(Principal{ID: "u1", Groups: []string{path}}, RoleUsersManage); got != want {
			t.Errorf("%s: Permitted = %v, want %v", path, got, want)
		}
	}
	// No roots: nobody is Privileged through a Group.
	policy.PrivilegedRoots = nil
	if NewAuthorizer(policy).Permitted(Principal{ID: "u1", Groups: []string{"/ADMIN"}}, RoleUsersManage) {
		t.Error("Privileged without a root")
	}
}

// The both mode's log still names the root by its last segment (ADMIN, YK
// or DK), never the path.
func TestAuthorizer_BothModeLogNamesTheRoot(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	metrics := NewRoleMetrics(log.New(&out, "", 0), func() time.Time { return time.Unix(0, 0) })
	auth := NewAuthorizer(policyIn(RoleModeBoth), WithRoleMetrics(metrics))
	if !auth.Permitted(Principal{ID: "u1", Groups: []string{"/UYELER/YK/BASKAN"}, Client: "admin"}, RoleUsersManage) {
		t.Fatal("a YK subgroup member is not Privileged in the both mode")
	}
	line := out.String()
	if !strings.Contains(line, `"group":"YK"`) || strings.Contains(line, "BASKAN") || strings.Contains(line, "/UYELER") {
		t.Fatalf("log = %s", line)
	}
	if !strings.Contains(metrics.Prometheus(), `skylab_authz_role_disagreements_total{permission="users:manage",granted_by="group",client="admin"} 1`) {
		t.Fatalf("metrics = %s", metrics.Prometheus())
	}
	// A Group named YK elsewhere is not Privileged: the role alone allows,
	// and the log names no group.
	out.Reset()
	if !auth.Permitted(Principal{ID: "u2", Groups: []string{"/UYELER/ARGE/YK"}, Roles: []string{RoleUsersManage}, Client: "admin"}, RoleUsersManage) {
		t.Fatal("role holder refused")
	}
	if line := out.String(); !strings.Contains(line, `"granted_by":"role"`) || strings.Contains(line, `"group"`) {
		t.Fatalf("log = %s", line)
	}
}

// Leader and Owner team by whole segments.
func TestAuthorizer_OwnerLevelsByWholeSegments(t *testing.T) {
	t.Parallel()
	auth := NewAuthorizer(DefaultPolicy()).(*authorizer)
	for _, tc := range []struct {
		path  string
		owner string
		want  []Level
	}{
		{"/UYELER/ARGE/WEBLAB/LIDERLER", "WEBLAB", []Level{LevelLeader, LevelMember}},
		{"/UYELER/ORGANIZASYON/ARTLAB/KOORDINATORLER", "ARTLAB", []Level{LevelLeader, LevelMember}},
		{"/UYELER/ARGE/WEBLAB", "WEBLAB", []Level{LevelMember}},
		{"/UYELER/ARGE/ALGOLAB/AGC/LIDERLER", "AGC", []Level{LevelLeader, LevelMember}},
		{"/UYELER/ARGE/ALGOLAB/AGC/LIDERLER", "ALGOLAB", []Level{LevelMember}},
		// A Group whose name only starts with a leader subgroup's.
		{"/UYELER/ARGE/WEBLAB/LIDERLER-ESKI", "WEBLAB", []Level{LevelMember}},
		{"/UYELER/ARGE/WEBLAB/LIDERLERX", "WEBLAB", []Level{LevelMember}},
		// Nothing sits under a Leader group: a Group there leads nothing.
		{"/UYELER/ARGE/WEBLAB/LIDERLER/X", "WEBLAB", []Level{LevelMember}},
		// A team whose name only starts with the Owner team's.
		{"/UYELER/ARGE/WEBLAB2", "WEBLAB", nil},
		{"/UYELER/ARGE/WEBLAB2/LIDERLER", "WEBLAB", nil},
		{"/UYELER/ARGE/XWEBLAB/LIDERLER", "WEBLAB", nil},
		{"/UYELER/ARGE/WEBLAB", "WEB", nil},
		// Case counts, as it always has.
		{"/UYELER/ARGE/weblab/LIDERLER", "WEBLAB", nil},
		{"/UYELER/ARGE/WEBLAB/liderler", "WEBLAB", []Level{LevelMember}},
		// A Privileged root names no Owner team.
		{"/UYELER/YK", "YK", nil},
		{"/UYELER/YK", "UYELER", nil},
		{"/UYELER/YK/BASKAN", "BASKAN", nil},
		{"/UYELER/DK/LIDERLER", "DK", nil},
		// The same names elsewhere are plain teams.
		{"/UYELER/ARGE/WEBLAB/ADMIN", "ADMIN", []Level{LevelMember}},
		{"/UYELER/ARGE/WEBLAB/ADMIN", "WEBLAB", []Level{LevelMember}},
	} {
		got := auth.ownerLevels(Principal{ID: "u1", Groups: []string{tc.path}}, tc.owner)
		if !slices.Equal(got, tc.want) {
			t.Errorf("ownerLevels(%s, %s) = %v, want %v", tc.path, tc.owner, got, tc.want)
		}
	}
}

// A Group named like a Leader group in name only leads no Event, and makes
// no Leader of anyone for the club default certificate template.
func TestAuthorizer_LeaderByWholeSegment(t *testing.T) {
	t.Parallel()
	auth := NewAuthorizer(DefaultPolicy())
	for _, path := range []string{
		"/UYELER/ARGE/WEBLAB/LIDERLER-ESKI",
		"/UYELER/ARGE/WEBLAB/LIDERLER/X",
		"/UYELER/ARGE/WEBLAB/liderler",
		"/LIDERLER",
	} {
		p := Principal{ID: "u1", Groups: []string{path}}
		if auth.Allow(p, Resource{Type: TypeEvent, OwnerTeam: "WEBLAB"}, Delete) {
			t.Errorf("%s deletes a WEBLAB Event", path)
		}
		if auth.Allow(p, Resource{Type: TypeTicket, OwnerTeam: "WEBLAB"}, Read) {
			t.Errorf("%s reads WEBLAB's applicants", path)
		}
		if auth.Allow(p, Resource{Type: TypeCertificateTemplate}, Read) {
			t.Errorf("%s reads the club default certificate template", path)
		}
	}
	leader := Principal{ID: "u1", Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}
	if !auth.Allow(leader, Resource{Type: TypeCertificateTemplate}, Read) || !auth.Allow(leader, Resource{Type: TypeEvent, OwnerTeam: "WEBLAB"}, Delete) {
		t.Error("a real Leader lost a decision")
	}
	// The club default template for a member of the members' tree with a
	// template role, anchored at /UYELER.
	withRole := func(path string) Principal {
		return Principal{ID: "u1", Groups: []string{path}, Roles: []string{"certificate:template:manage"}}
	}
	if !auth.Allow(withRole("/UYELER/ARGE/WEBLAB"), Resource{Type: TypeCertificateTemplate}, Read) {
		t.Error("a team member with the template role lost the club default template")
	}
	for _, path := range []string{"/X/UYELER/WEBLAB", "/uyeler/arge/weblab", "/UYELER"} {
		if auth.Allow(withRole(path), Resource{Type: TypeCertificateTemplate}, Read) {
			t.Errorf("%s reads the club default template", path)
		}
	}
}

// The capabilities' teams leave out Privileged roots and what is under
// them: none of them is an Owner team.
func TestCapabilities_NoTeamsFromPrivilegedRoots(t *testing.T) {
	t.Parallel()
	for _, mode := range []RoleMode{RoleModeGroups, RoleModeRoles} {
		c := NewAuthorizer(policyIn(mode)).Capabilities(Principal{ID: "u1", Groups: []string{"/UYELER/YK/BASKAN", "/ADMIN"}})
		if len(c.Teams) != 0 {
			t.Errorf("%s: teams = %+v", mode, c.Teams)
		}
	}
	c := NewAuthorizer(DefaultPolicy()).Capabilities(Principal{ID: "u1", Groups: []string{"/UYELER/ARGE/WEBLAB/ADMIN"}})
	var names []string
	for _, team := range c.Teams {
		names = append(names, team.Team)
	}
	if want := []string{"ADMIN", "ARGE", "UYELER", "WEBLAB"}; !slices.Equal(names, want) {
		t.Errorf("teams = %v, want %v", names, want)
	}
}
