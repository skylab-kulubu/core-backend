package authz

import (
	"slices"
	"testing"
)

func scopeFor(c Capabilities, team string) map[string]bool {
	for _, t := range c.Teams {
		if t.Team == team {
			return t.Can
		}
	}
	return c.OtherTeams.Can
}

func trueKeys(m map[string]bool) []string {
	var out []string
	for k, v := range m {
		if v {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out
}

func TestCapabilities_EveryKeyIsPresent(t *testing.T) {
	t.Parallel()
	c := NewAuthorizer(DefaultPolicy()).Capabilities(Principal{ID: "u1", Groups: []string{"/UYELER/ARGE/WEBLAB"}})
	for _, key := range AppAbilities() {
		if _, ok := c.Can[key]; !ok {
			t.Errorf("can lacks %s", key)
		}
	}
	scopes := map[string]map[string]bool{"otherTeams": c.OtherTeams.Can, "noOwnerTeam": c.NoOwnerTeam.Can}
	for _, team := range c.Teams {
		scopes[team.Team] = team.Can
	}
	for name, can := range scopes {
		for _, key := range TeamAbilities() {
			if _, ok := can[key]; !ok {
				t.Errorf("%s lacks %s", name, key)
			}
		}
		if len(can) != len(TeamAbilities()) {
			t.Errorf("%s has %d keys, want %d", name, len(can), len(TeamAbilities()))
		}
	}
}

func TestCapabilities_Privileged(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		mode RoleMode
		p    Principal
	}{
		{RoleModeGroups, Principal{ID: "u1", Groups: []string{"/UYELER/YK"}}},
		{RoleModeRoles, Principal{ID: "u1", Roles: PermissionRoles()}},
	} {
		c := NewAuthorizer(policyIn(tc.mode)).Capabilities(tc.p)
		if !slices.Equal(c.Permissions, PermissionRoles()) {
			t.Errorf("%s: permissions = %v", tc.mode, c.Permissions)
		}
		for _, key := range AppAbilities() {
			if !c.Can[key] {
				t.Errorf("%s: can[%s] = false", tc.mode, key)
			}
		}
		for _, scope := range []map[string]bool{c.OtherTeams.Can, c.NoOwnerTeam.Can} {
			for _, key := range TeamAbilities() {
				if !scope[key] {
					t.Errorf("%s: scope %s = false", tc.mode, key)
				}
			}
		}
	}
}

// A YK member without roles in the roles mode: nothing Privileged is left.
func TestCapabilities_PrivilegedGroupWithoutRolesInTheRolesMode(t *testing.T) {
	t.Parallel()
	c := NewAuthorizer(policyIn(RoleModeRoles)).Capabilities(Principal{ID: "u1", Groups: []string{"/UYELER/YK"}})
	if len(c.Permissions) != 0 {
		t.Errorf("permissions = %v", c.Permissions)
	}
	if got := trueKeys(c.Can); len(got) != 0 {
		t.Errorf("can = %v", got)
	}
	if got := trueKeys(c.OtherTeams.Can); len(got) != 0 {
		t.Errorf("otherTeams = %v", got)
	}
}

func TestCapabilities_Leader(t *testing.T) {
	t.Parallel()
	c := NewAuthorizer(DefaultPolicy()).Capabilities(Principal{ID: "u1", Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}})
	if len(c.Permissions) != 0 {
		t.Errorf("permissions = %v", c.Permissions)
	}
	var weblab *TeamCapabilities
	for i := range c.Teams {
		if c.Teams[i].Team == "WEBLAB" {
			weblab = &c.Teams[i]
		}
	}
	if weblab == nil {
		t.Fatalf("teams = %+v", c.Teams)
	}
	if !slices.Equal(weblab.Levels, []Level{LevelLeader, LevelMember}) {
		t.Errorf("levels = %v", weblab.Levels)
	}
	want := []string{
		"certificate.issue", "certificate.read", "certificate.revoke",
		"certificateTemplate.assign", "certificateTemplate.create", "certificateTemplate.read", "certificateTemplate.update",
		"competitor.create", "competitor.delete", "competitor.read", "competitor.update",
		"door.checkIn", "event.create", "event.delete", "event.update", "ticket.assign", "ticket.read",
	}
	if got := trueKeys(weblab.Can); !slices.Equal(got, want) {
		t.Errorf("WEBLAB = %v\nwant %v", got, want)
	}
	if got := trueKeys(c.OtherTeams.Can); len(got) != 0 {
		t.Errorf("otherTeams = %v", got)
	}
	// The club default template is readable by any Leader.
	if got := trueKeys(c.NoOwnerTeam.Can); !slices.Equal(got, []string{"certificateTemplate.read"}) {
		t.Errorf("noOwnerTeam = %v", got)
	}
	if !c.Can["media.uploadEventMedia"] || !c.Can["media.uploadCertificateTemplateMedia"] || c.Can["media.list"] {
		t.Errorf("can = %v", trueKeys(c.Can))
	}
}

func TestCapabilities_OwnerTeamMember(t *testing.T) {
	t.Parallel()
	c := NewAuthorizer(DefaultPolicy()).Capabilities(Principal{
		ID: "u1", Groups: []string{"/UYELER/ORGANIZASYON/GECEKODU"}, Roles: []string{"certificate:issue"},
	})
	gecekodu := scopeFor(c, "GECEKODU")
	want := []string{
		"certificate.issue", "certificate.read", "competitor.create", "competitor.delete", "competitor.read", "competitor.update",
		"event.create", "event.update", "ticket.read",
	}
	if got := trueKeys(gecekodu); !slices.Equal(got, want) {
		t.Errorf("GECEKODU = %v\nwant %v", got, want)
	}
}

func TestCapabilities_OrdinaryMember(t *testing.T) {
	t.Parallel()
	c := NewAuthorizer(DefaultPolicy()).Capabilities(Principal{ID: "u1", Roles: []string{"url:access"}})
	if len(c.Teams) != 0 || len(c.Permissions) != 1 || c.Permissions[0] != RoleURLAccess {
		t.Errorf("teams = %v permissions = %v", c.Teams, c.Permissions)
	}
	if got := trueKeys(c.Can); !slices.Equal(got, []string{"url.create", "url.listMine"}) {
		t.Errorf("can = %v", got)
	}
}

// Capabilities answers what Allow answers for the same person and record.
func TestCapabilities_MatchAllow(t *testing.T) {
	t.Parallel()
	people := []Principal{
		{ID: "u1", Groups: []string{"/UYELER/YK"}},
		{ID: "u2", Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER", "/UYELER/ORGANIZASYON/GECEKODU"}, Roles: []string{"certificate:binding:manage"}},
		{ID: "u3", Roles: []string{"users:read", "url:moderator"}},
		{ID: "u4", Roles: []string{RoleEventManage, RoleTicketValidate}},
	}
	for _, mode := range []RoleMode{RoleModeGroups, RoleModeBoth, RoleModeRoles} {
		auth := NewAuthorizer(policyIn(mode))
		for _, p := range people {
			c := auth.Capabilities(p)
			for _, ab := range appAbilities {
				if c.Can[ab.key] != auth.Allow(p, ab.r, ab.action) {
					t.Errorf("%s %s: can[%s] differs from Allow", mode, p.ID, ab.key)
				}
			}
			for _, team := range []string{"WEBLAB", "GECEKODU", "SKYSEC", ""} {
				can := scopeFor(c, team)
				if team == "" {
					can = c.NoOwnerTeam.Can
				}
				for _, ab := range teamAbilities {
					r := ab.r
					r.OwnerTeam = team
					if can[ab.key] != auth.Allow(p, r, ab.action) {
						t.Errorf("%s %s %q: %s differs from Allow", mode, p.ID, team, ab.key)
					}
				}
			}
		}
	}
}
