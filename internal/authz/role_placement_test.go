package authz

import "testing"

// rol-temizligi S5 (ticket 06): a role is mapped only to a Privileged root,
// a top Team's Leader group or a role group (a leaf under /ROLLER).
func TestRolePlacement(t *testing.T) {
	t.Parallel()
	roots, leaders := DefaultPrivilegedRoots(), DefaultPolicy().LeaderSubgroups
	for _, tc := range []struct {
		path      string
		subgroups bool
		want      string
	}{
		{"/ADMIN", true, ""},
		{"/UYELER/YK", true, ""},
		{"/UYELER/DK", false, ""},
		{"/UYELER/YK/ARGE_LIDERI", false, PlacementPrivilegedSubgroup},
		{"/ADMIN/X", false, PlacementPrivilegedSubgroup},
		{"/UYELER/ARGE/WEBLAB/LIDERLER", false, ""},
		{"/UYELER/ORGANIZASYON/ETKINLIK/KOORDINATORLER", false, ""},
		{"/ROLLER/FORMS-ACCESS", false, ""},
		{"/ROLLER/CF-ZERO-TRUST-ACCESS", false, ""},
		{"/ROLLER/FORMS-ACCESS", true, PlacementRolesGroupNotLeaf},
		{"/ROLLER", true, PlacementRolesRoot},
		{"/ROLLER/FORMS/ACCESS", false, PlacementRolesNested},
		{"/UYELER", true, PlacementMembersRoot},
		{"/UYELER/ARGE", true, PlacementArea},
		{"/UYELER/ARGE/WEBLAB", true, PlacementTeam},
		{"/UYELER/ARGE/WEBLAB/FRONT", false, PlacementTeam},
		{"/UYELER/ARGE/WEBLAB/FRONT/LIDERLER", false, PlacementSubTeamLeaderGroup},
		{"/UYELER/ARGE/LIDERLER", false, PlacementOutside},
		{"/UYELER/ARGE/WEBLAB/LIDERLER-ESKI", false, PlacementTeam},
		{"/KONSOL", false, PlacementOutside},
		{"/ROLLERX/A", false, PlacementOutside},
		{"/UYELER/ARGE/YK", true, PlacementTeam}, // a Team named YK, no root
	} {
		if got := RolePlacement(tc.path, roots, leaders, tc.subgroups); got != tc.want {
			t.Errorf("%s (subgroups %v): %q, want %q", tc.path, tc.subgroups, got, tc.want)
		}
		if tc.want != "" && PlacementDetail(tc.want) == "" {
			t.Errorf("%s: no detail for %q", tc.path, tc.want)
		}
	}
}

// ekip-yetkileri ticket 04: a top Team's own Group takes app roles only;
// an admin role or an unclassified one stays refused there, and a Sub-team
// takes none. Every other place answers as RolePlacement.
func TestRolePlacementOf(t *testing.T) {
	t.Parallel()
	roots, leaders := DefaultPrivilegedRoots(), DefaultPolicy().LeaderSubgroups
	for _, tc := range []struct {
		path, client, role string
		want               string
	}{
		{"/UYELER/ARGE/WEBLAB", "forms", "skyforms:access", ""},
		{"/UYELER/ARGE/WEBLAB", "skymail", "skymail:access", ""},
		{"/UYELER/ARGE/WEBLAB", "skymail", "skymail:templates:read", ""},
		{"/UYELER/ARGE/WEBLAB", "frontend-main", "cms:access", PlacementTopTeamAdminRole},
		{"/UYELER/ARGE/WEBLAB", "skymail", "skymail:mails:write", PlacementTopTeamAdminRole},
		{"/UYELER/ARGE/WEBLAB", "forms", "skyforms:*", PlacementTopTeamAdminRole},
		{"/UYELER/ARGE/WEBLAB", "core", "users:read", PlacementTopTeamAdminRole},
		{"/UYELER/ARGE/WEBLAB", "forms", "skyforms:never-classified", PlacementTopTeamAdminRole},
		{"/UYELER/ARGE/WEBLAB/FRONT", "forms", "skyforms:access", PlacementTeam},
		{"/UYELER/ARGE/WEBLAB/FRONT/LIDERLER", "forms", "skyforms:access", PlacementSubTeamLeaderGroup},
		{"/UYELER/ARGE/WEBLAB/LIDERLER", "frontend-main", "cms:access", ""},
		{"/UYELER/ARGE", "forms", "skyforms:access", PlacementArea},
		{"/UYELER", "forms", "skyforms:access", PlacementMembersRoot},
		{"/UYELER/YK", "skymail", "skymail:mails:approve", ""},
		{"/UYELER/YK/ARGE_LIDERI", "forms", "skyforms:access", PlacementPrivilegedSubgroup},
		{"/ROLLER/FORMS-ACCESS", "forms", "skyforms:access", ""},
		{"/UYELER/ARGE/YK", "frontend-main", "cms:access", PlacementTopTeamAdminRole}, // a Team named YK, no root
	} {
		if got := RolePlacementOf(tc.path, roots, leaders, false, tc.client, tc.role); got != tc.want {
			t.Errorf("%s %s:%s: %q, want %q", tc.path, tc.client, tc.role, got, tc.want)
		}
		if tc.want != "" && PlacementDetail(tc.want) == "" {
			t.Errorf("%s: no detail for %q", tc.path, tc.want)
		}
	}
}
