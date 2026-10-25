package authz

import (
	"slices"
	"strings"
)

// Role placement (rol-temizligi K4, S5; ticket 06; Microsoft Entra's
// model: access through groups, never a role on a person). A client role is
// mapped to four kinds of Group only:
//
//   - a Privileged root itself (/ADMIN, /UYELER/YK, …): the board's own
//     roles; not a Group under one (a YK duty group);
//   - a top Team's Leader group (/UYELER/<area>/<team>/LIDERLER or
//     KOORDINATORLER): what every Leader of the Team gets;
//   - a top Team itself (/UYELER/<area>/<team>), an app role only
//     (ClassifyRole; ekip-yetkileri ticket 04, ADR 0065 addendum): what
//     every member of the Team and its Sub-teams gets, Forms' seat and
//     SkyMail's access. An admin role, or one nobody has classified, would
//     make the Team a Protected group, so it is refused there
//     (RolePlacementOf);
//   - a role group, a leaf right under RolesRoot (/ROLLER/<APP>-<ROLE>,
//     S4): one app role, handed out by adding people.
//
// Anywhere else (the members' tree, an area, a Sub-team, a Sub-team's
// Leader group, /ROLLER itself or a role group with Groups under it, a
// Group outside these trees) a new mapping is refused; taking an existing
// one off is always allowed.

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
	// PlacementTopTeamAdminRole: a top Team takes app roles only.
	PlacementTopTeamAdminRole = "top_team_admin_role"
)

// placementDetails say each refusal's reason, for the problem's detail.
var placementDetails = map[string]string{
	PlacementPrivilegedSubgroup: "A Group under a Privileged root (a duty group) takes no role: map it to the root itself.",
	PlacementMembersRoot:        "/UYELER takes no role: every member would hold it. Use a role group under /ROLLER.",
	PlacementArea:               "An area takes no role: every Team under it would hold it. Use a role group under /ROLLER.",
	PlacementTeam:               "A Sub-team takes no role: map it to the top Team (an app role) or its Leader group, or use a role group under /ROLLER.",
	PlacementSubTeamLeaderGroup: "Only a top Team's Leader group takes roles, not a Sub-team's.",
	PlacementRolesRoot:          "/ROLLER itself takes no role: map it to a role group /ROLLER/<APP>-<ROLE>.",
	PlacementRolesGroupNotLeaf:  "A role group with Groups under it takes no role: its roles would flow down to them.",
	PlacementRolesNested:        "A role group sits right under /ROLLER: /ROLLER/<APP>-<ROLE>.",
	PlacementOutside:            "Roles are mapped only to a Privileged root, a top Team (an app role), a top Team's Leader group or a role group under /ROLLER.",
	PlacementTopTeamAdminRole:   "A top Team takes app roles only (such as Forms' seat or SkyMail's access): an admin role, or one nobody has classified, would make the Team Protected. Map it to the Team's Leader group or a role group under /ROLLER.",
	// App assignments (app_assignments.go).
	PlacementAdminRole:      "Only an app role is assigned from the panel; an admin role, or one nobody has classified, is mapped in code (a Privileged root or a role group under /ROLLER).",
	PlacementCodeArea:       "A Privileged root, /ROLLER and the Groups under them are mapped in code (Keycloak's reconciler), not from the panel.",
	PlacementServiceAccount: "A service account's roles are mapped in code.",
	PlacementInactiveUser:   "The account is disabled, erased or being erased.",
	PlacementUseAppAssignments: "With the app assignments on, a Team's, a Sub-team's or a Leader group's app roles are assigned and taken off on the Uygulamalar page " +
		"(POST and DELETE /v1/apps/{clientId}/assignments), each with its audit row; this route changes their other roles only.",
}

// PlacementDetail is the sentence that explains a refusal's reason.
func PlacementDetail(reason string) string { return placementDetails[reason] }

// RolePlacementOf is "" when the client role role of clientID (a Keycloak
// client id) may be mapped to the Group at path, else the reason it may
// not: RolePlacement, and on a top Team an app role is allowed
// (PlacementTopTeamAdminRole for any other role). The class is the role's
// name (ClassifyRole); a composite app role is checked by name too, as the
// protection read checks what it holds.
func RolePlacementOf(path string, roots, leaders []string, hasSubgroups bool, clientID, role string) string {
	reason := RolePlacement(path, roots, leaders, hasSubgroups)
	if reason != PlacementTeam || !isTopTeam(path, roots, leaders) {
		return reason
	}
	if DelegableRole(clientID, role) {
		return ""
	}
	return PlacementTopTeamAdminRole
}

// isTopTeam reports whether the Group at path is a top Team's own Group
// (/UYELER/<area>/<team>), not a Sub-team or a Leader group.
func isTopTeam(path string, roots, leaders []string) bool {
	team, role, ok := TeamOf(path, roots, leaders)
	return ok && role == "" && len(pathSegments(team)) == minTeamDepth
}

// RolePlacement is "" when a client role may be mapped to the Group at path
// (S5) whatever its class, else the reason it may not; a top Team's own
// Group answers PlacementTeam here, as it takes app roles only
// (RolePlacementOf). hasSubgroups is whether the Group has a Group under
// it.
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
