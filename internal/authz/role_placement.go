package authz

import (
	"slices"
	"strings"
)

// Role placement (rol-temizligi K4, S5; ticket 06; Microsoft Entra's
// model: access through groups, never a role on a person). A client role is
// mapped to three kinds of Group only:
//
//   - a Privileged root itself (/ADMIN, /UYELER/YK, …): the board's own
//     roles; not a Group under one (a YK duty group);
//   - a top Team's Leader group (/UYELER/<area>/<team>/LIDERLER or
//     KOORDINATORLER): what every Leader of the Team gets, Forms' seat;
//   - a role group, a leaf right under RolesRoot (/ROLLER/<APP>-<ROLE>,
//     S4): one app role, handed out by adding people.
//
// Anywhere else (the members' tree, an area, a Team or Sub-team, a
// Sub-team's Leader group, /ROLLER itself or a role group with Groups
// under it, a Group outside these trees) a new mapping is refused; taking
// an existing one off is always allowed.

// RolesRoot is the root of the role groups (S4: /ROLLER/<APP>-<ROLE>).
const RolesRoot = "/ROLLER"

// The reasons a placement is refused (RolePlacement).
const (
	PlacementPrivilegedSubgroup = "privileged_subgroup"
	PlacementMembersRoot        = "members_root"
	PlacementArea               = "area"
	PlacementTeam               = "team"
	PlacementSubTeamLeaderGroup = "sub_team_leader_group"
	PlacementRolesRoot          = "roles_root"
	PlacementRolesGroupNotLeaf  = "roles_group_not_leaf"
	PlacementRolesNested        = "roles_nested"
	PlacementOutside            = "outside_role_places"
)

// placementDetails say each refusal's reason, for the problem's detail.
var placementDetails = map[string]string{
	PlacementPrivilegedSubgroup: "A Group under a Privileged root (a duty group) takes no role: map it to the root itself.",
	PlacementMembersRoot:        "/UYELER takes no role: every member would hold it. Use a role group under /ROLLER.",
	PlacementArea:               "An area takes no role: every Team under it would hold it. Use a role group under /ROLLER.",
	PlacementTeam:               "A Team or Sub-team takes no role: map it to the top Team's Leader group, or use a role group under /ROLLER.",
	PlacementSubTeamLeaderGroup: "Only a top Team's Leader group takes roles, not a Sub-team's.",
	PlacementRolesRoot:          "/ROLLER itself takes no role: map it to a role group /ROLLER/<APP>-<ROLE>.",
	PlacementRolesGroupNotLeaf:  "A role group with Groups under it takes no role: its roles would flow down to them.",
	PlacementRolesNested:        "A role group sits right under /ROLLER: /ROLLER/<APP>-<ROLE>.",
	PlacementOutside:            "Roles are mapped only to a Privileged root, a top Team's Leader group or a role group under /ROLLER.",
}

// PlacementDetail is the sentence that explains a refusal's reason.
func PlacementDetail(reason string) string { return placementDetails[reason] }

// RolePlacement is "" when a client role may be mapped to the Group at path
// (S5), else the reason it may not. hasSubgroups is whether the Group has a
// Group under it.
func RolePlacement(path string, roots, leaders []string, hasSubgroups bool) string {
	if root := PrivilegedRoot(roots, path); root != "" {
		if root == path {
			return ""
		}
		return PlacementPrivilegedSubgroup
	}
	switch {
	case path == RolesRoot:
		return PlacementRolesRoot
	case strings.HasPrefix(path, RolesRoot+"/"):
		if strings.Contains(strings.TrimPrefix(path, RolesRoot+"/"), "/") {
			return PlacementRolesNested
		}
		if hasSubgroups {
			return PlacementRolesGroupNotLeaf
		}
		return ""
	case path == MembersRoot:
		return PlacementMembersRoot
	}
	team, role, ok := TeamOf(path, roots, leaders)
	switch {
	case ok && role == "":
		return PlacementTeam
	case ok && len(pathSegments(team)) == minTeamDepth:
		return ""
	case ok:
		return PlacementSubTeamLeaderGroup
	}
	if strings.HasPrefix(path, MembersRoot+"/") && len(pathSegments(path)) == 2 && !slices.Contains(leaders, pathSegments(path)[1]) {
		return PlacementArea
	}
	return PlacementOutside
}
