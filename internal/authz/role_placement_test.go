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
