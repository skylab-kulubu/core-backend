package authz

import (
	"slices"
	"strings"
)

// TeamTree reads the realm's Teams by their Team code for the Owner team
// decisions (ADR-0065 decision 2, ekip-yetkileri ticket 12): a Leader of a
// Team above an Event's Owner team is that team's Leader too. The token
// names the caller's Groups but not where the Owner team sits, so core
// reads the tree with its own service account and keeps it a minute
// (identity.TeamTree).
type TeamTree interface {
	// TeamPath is the path of the Group whose name is team: ok only when
	// the tree could be read and exactly one Group of the realm has that
	// name, compared without case (a Team code is unique, docs/team-codes.md).
	TeamPath(team string) (path string, ok bool)
	// SubTeams are the names of the Groups below the Group at path.
	SubTeams(path string) []string
}

// TeamAttrMembersManageCompetitors is the Team attribute (Keycloak group
// attribute) by which the Team's Leader decides whether its members add,
// edit and remove its Events' competitors (Yusuf, 2026-10-25): "false" keeps
// them to the Leaders; anything else, or none, is today's rule.
const TeamAttrMembersManageCompetitors = "members_manage_competitors"

// MembersManageCompetitorsValue reads TeamAttrMembersManageCompetitors'
// value: false only for "false" (case and blanks aside), true for anything
// else or none. The decision and the settings route's answer both read it.
func MembersManageCompetitorsValue(v string) bool {
	return !strings.EqualFold(strings.TrimSpace(v), "false")
}

// TeamSettings reads a Team's own settings by its Team code
// (identity.TeamTree, kept a minute).
type TeamSettings interface {
	// MembersManageCompetitors is false only when the one Group named team
	// carries TeamAttrMembersManageCompetitors=false; a Group that cannot be
	// read or a name that is not one Group's is the default, true.
	MembersManageCompetitors(team string) bool
}

// WithTeamSettings lets the Teams' settings narrow the members' decisions.
// Without it every Team has the defaults.
func WithTeamSettings(s TeamSettings) Option {
	return func(a *authorizer) { a.settings = s }
}

// WithTeamTree lets a Leader of a Team above an Owner team lead it, with
// team delegation on (Policy.TeamDelegation, the switch of ADR-0065). Without
// it, or with delegation off, a Leader leads only their own Team's records.
func WithTeamTree(t TeamTree) Option {
	return func(a *authorizer) { a.teams = t }
}

// inheritsFromAbove reports whether p is directly in a Leader group of a
// Team strictly above the Team owner (ADR-0065 decision 2). Both are Teams
// as delegation reads them (TeamOf): an area, the members' tree and a
// Privileged root lead nothing below them. The tree is read only for a
// caller in some Team's Leader group.
func (a *authorizer) inheritsFromAbove(p Principal, owner string) bool {
	if a.teams == nil || !a.policy.TeamDelegation || owner == "" || strings.ContainsAny(owner, "/\x00") {
		return false
	}
	led := a.ledTeams(p)
	if len(led) == 0 {
		return false
	}
	path, ok := a.teams.TeamPath(owner)
	if !ok || !a.isTeamPath(path) || path[strings.LastIndex(path, "/")+1:] != owner {
		return false
	}
	for _, team := range led {
		if strings.HasPrefix(path, team+"/") {
			return true
		}
	}
	return false
}

// ledTeams are the paths of the Teams p is directly in a Leader group of.
func (a *authorizer) ledTeams(p Principal) []string {
	var out []string
	for _, group := range p.Groups {
		team, role, ok := TeamOf(group, a.policy.PrivilegedRoots, a.policy.LeaderSubgroups)
		if ok && role != "" && !slices.Contains(out, team) {
			out = append(out, team)
		}
	}
	return out
}

// isTeamPath reports whether path is a Team's own Group, not a Leader group.
func (a *authorizer) isTeamPath(path string) bool {
	team, role, ok := TeamOf(path, a.policy.PrivilegedRoots, a.policy.LeaderSubgroups)
	return ok && role == "" && team == path
}

// subTeamCandidates are the Teams below the Teams p leads, by name, that p
// leads from above: the capabilities list them with their decisions.
func (a *authorizer) subTeamCandidates(p Principal) []string {
	if a.teams == nil || !a.policy.TeamDelegation {
		return nil
	}
	var out []string
	for _, team := range a.ledTeams(p) {
		for _, name := range a.teams.SubTeams(team) {
			if slices.Contains(out, name) || slices.Contains(a.policy.LeaderSubgroups, name) {
				continue
			}
			if a.inheritsFromAbove(p, name) {
				out = append(out, name)
			}
		}
	}
	return out
}
