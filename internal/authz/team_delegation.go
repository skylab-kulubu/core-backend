package authz

import (
	"fmt"
	"slices"
	"strings"
)

// Team delegation (docs/team-delegation.md, ADR 0065): a Leader manages
// their Team and every Sub-team below it through core. The decision is made
// here, from what core read from Keycloak with its own service account: the
// target Group and the Groups above it, by id, and the Leader groups the
// caller is directly in. Nothing of it comes from the caller's token.

// TeamDelegationEnv switches team delegation: off (the default) or on.
const TeamDelegationEnv = "TEAM_DELEGATION"

// ParseTeamDelegation reads TeamDelegationEnv: empty or "off" is off, "on"
// is on, anything else is an error that names the variable, so a typo
// cannot quietly hand Leaders the management of their Teams.
func ParseTeamDelegation(raw string) (bool, error) {
	switch strings.TrimSpace(raw) {
	case "", "off":
		return false, nil
	case "on":
		return true, nil
	default:
		return false, fmt.Errorf("%s: %q is not %q or %q", TeamDelegationEnv, raw, "off", "on")
	}
}

// The problem codes (RFC 7807 code member) of a delegated refusal.
const (
	// RefusalProtectedGroup: the Group, or one above it, maps a protected
	// role (ProtectedRole); only Privileged manages it.
	RefusalProtectedGroup = "protected_group"
	// RefusalLeaderGroupNeedsParent: a Leader group's membership is
	// changed only by whoever manages the Team above its Team
	// (ManagesStrict): nobody makes themselves a Leader or leaves their own
	// Team without one.
	RefusalLeaderGroupNeedsParent = "leader_group_needs_parent"
)

// RoleAuditRead reads the Team audit record. It is read as the role alone,
// in every role mode: no Privileged Group stands for it (S11: DK does not
// read the record).
const RoleAuditRead = "audit:read"

// protectedCoreRoles are the roles of core's client whose holders a Leader
// must not make: the contract's (PermissionRoles) and the audit reader's.
var protectedCoreRoles = append(PermissionRoles(), RoleAuditRead)

// ProtectedRole reports whether a Group that maps role of clientID (a
// Keycloak client id) is a Protected group: managed only by Privileged,
// because a member added to it, or to a Group under it, would hold the
// role. They are core's permission roles and audit:read (whatever client
// carries a role of that name), any client's client:admin, and every role
// of realm-management.
func ProtectedRole(clientID, role string) bool {
	return slices.Contains(protectedCoreRoles, role) || role == "client:admin" || clientID == "realm-management"
}

// GroupNode is one Group as Keycloak answers it by id: its name, its path
// and the id of the Group above it ("" at the realm's top).
type GroupNode struct {
	ID       string
	Name     string
	Path     string
	ParentID string
}

// ManagedGroup is the target of a delegated decision.
type ManagedGroup struct {
	Group GroupNode
	// Ancestors are the Groups above Group, its parent first and the
	// realm's top Group last, each read by the id its child named.
	Ancestors []GroupNode
	// Protected is whether Group or one of Ancestors maps a protected role
	// (ProtectedRole).
	Protected bool
}

// LeaderGroup is a Leader group the caller is directly in, as Keycloak
// answers the caller's Groups: its id, name and path, and the id of the
// Group above it.
type LeaderGroup struct {
	ID     string
	Name   string
	Path   string
	TeamID string
}

// TeamManager is the caller of a delegated decision.
type TeamManager struct {
	// Privileged is the groups:manage decision (Allow on TypeGroup), as
	// the role mode reads it.
	Privileged bool
	// LeaderGroups are the Leader groups the caller is directly in, read
	// from Keycloak's current membership.
	LeaderGroups []LeaderGroup
}

// ManageBasis is why a decision allows: the caller is Privileged, or a
// Leader of a Team the target sits in or under.
type ManageBasis string

const (
	ManageBasisPrivileged ManageBasis = "privileged"
	ManageBasisLeader     ManageBasis = "leader"
)

// ManageDecision is the answer of Manages, ManagesStrict and
// ManagesMembership.
type ManageDecision struct {
	Allowed bool
	Basis   ManageBasis
	// ViaTeamID is, for a Leader, the Team whose Leader group grants the
	// decision: the nearest one above or at the target.
	ViaTeamID string
	// Refusal is the problem code of a refusal, "" for a plain one (403).
	// A refusal of a Leader (protected, needs the parent) keeps Basis and
	// ViaTeamID: the denied row names the Team they acted for.
	Refusal string
}

func privilegedDecision() ManageDecision {
	return ManageDecision{Allowed: true, Basis: ManageBasisPrivileged}
}

// Manages reports whether m manages g (SPEC §4.2): m is Privileged, or team
// delegation is on, g is a Team or a Team's Leader group, and m is directly
// in a Leader group (LIDERLER or KOORDINATORLER) of g itself or of a Team
// above it. A Protected group is Privileged's alone. Areas, the members'
// tree, Privileged roots and everything under them are no Team.
func (p Policy) Manages(m TeamManager, g ManagedGroup) ManageDecision {
	if m.Privileged {
		return privilegedDecision()
	}
	if !p.TeamDelegation || !p.chainHolds(g) {
		return ManageDecision{}
	}
	team, _, ok := TeamOf(g.Group.Path, p.PrivilegedRoots, p.LeaderSubgroups)
	if !ok {
		return ManageDecision{}
	}
	// The chain from g up: g itself, then its ancestors. The Teams in it
	// are g's own Team (g, or for a Leader group its parent) and the Teams
	// above that one.
	chain := append([]GroupNode{g.Group}, g.Ancestors...)
	for _, node := range chain {
		if !strings.HasPrefix(team+"/", node.Path+"/") {
			// A Leader group itself; its Team comes next.
			continue
		}
		if !p.isTeamNode(node) {
			// An area or the members' tree: no Team above it.
			break
		}
		if via := p.ledVia(m, node); via != "" {
			if g.Protected {
				return ManageDecision{Basis: ManageBasisLeader, ViaTeamID: via, Refusal: RefusalProtectedGroup}
			}
			return ManageDecision{Allowed: true, Basis: ManageBasisLeader, ViaTeamID: via}
		}
	}
	return ManageDecision{}
}

// ManagesStrict reports whether m manages g strictly (SPEC §4.2): g is a
// Sub-team and m manages the Team above it. Leadership, deletion, the source
// of a move and a code change need it: a Leader makes them only in the Teams
// below their own. Privileged manages every Group strictly.
func (p Policy) ManagesStrict(m TeamManager, g ManagedGroup) ManageDecision {
	if m.Privileged {
		return privilegedDecision()
	}
	if !p.TeamDelegation || !p.chainHolds(g) || !p.isTeamNode(g.Group) || len(g.Ancestors) == 0 || !p.isTeamNode(g.Ancestors[0]) {
		return ManageDecision{}
	}
	parent := ManagedGroup{Group: g.Ancestors[0], Ancestors: g.Ancestors[1:], Protected: g.Protected}
	return p.Manages(m, parent)
}

// ManagesMembership is the decision on adding a member to g or removing
// one from it, g being the Group the membership is in (SPEC §4.3): Manages
// for a Team, and for a Leader group ManagesStrict of its Team. A Leader
// group's membership refused only for that reason is
// RefusalLeaderGroupNeedsParent.
func (p Policy) ManagesMembership(m TeamManager, g ManagedGroup) ManageDecision {
	if m.Privileged {
		return privilegedDecision()
	}
	_, role, ok := TeamOf(g.Group.Path, p.PrivilegedRoots, p.LeaderSubgroups)
	if !ok || role == "" {
		return p.Manages(m, g)
	}
	if !p.chainHolds(g) || len(g.Ancestors) == 0 {
		return ManageDecision{}
	}
	team := ManagedGroup{Group: g.Ancestors[0], Ancestors: g.Ancestors[1:], Protected: g.Protected}
	if strict := p.ManagesStrict(m, team); strict.Allowed || strict.Refusal != "" {
		return strict
	}
	plain := p.Manages(m, team)
	switch {
	case plain.Refusal != "":
		return plain
	case plain.Allowed:
		return ManageDecision{Basis: ManageBasisLeader, ViaTeamID: plain.ViaTeamID, Refusal: RefusalLeaderGroupNeedsParent}
	}
	return ManageDecision{}
}

// isTeamNode reports whether node is a Team's own Group (not a Leader
// group).
func (p Policy) isTeamNode(node GroupNode) bool {
	team, role, ok := TeamOf(node.Path, p.PrivilegedRoots, p.LeaderSubgroups)
	return ok && role == "" && team == node.Path
}

// ledVia is team's id when m is directly in one of its Leader groups: a
// Group named after a leader subgroup whose parent, by id, is team and
// whose path is right under team's. "" otherwise.
func (p Policy) ledVia(m TeamManager, team GroupNode) string {
	for _, leader := range m.LeaderGroups {
		if leader.TeamID == "" || leader.TeamID != team.ID {
			continue
		}
		if !slices.Contains(p.LeaderSubgroups, leader.Name) || strings.Contains(leader.Name, "/") {
			continue
		}
		if leader.Path != team.Path+"/"+leader.Name {
			continue
		}
		return team.ID
	}
	return ""
}

// chainHolds reports whether g's ancestors are the chain its path says:
// each node's parent is the next one by id, each path is its parent's path
// and its own name, and no name holds a "/" (Keycloak allows one, and its
// path would then read as more Groups than there are). A chain that does
// not hold decides nothing.
func (p Policy) chainHolds(g ManagedGroup) bool {
	return ChainHolds(g.Group, g.Ancestors)
}

// ChainHolds reports whether ancestors are the Groups above node, parent
// first, as their ids and paths both say (see Policy.Manages).
func ChainHolds(node GroupNode, ancestors []GroupNode) bool {
	chain := append([]GroupNode{node}, ancestors...)
	for i, n := range chain {
		if n.ID == "" || n.Name == "" || strings.Contains(n.Name, "/") {
			return false
		}
		if i == len(chain)-1 {
			return n.ParentID == "" && n.Path == "/"+n.Name
		}
		parent := chain[i+1]
		if n.ParentID != parent.ID || n.Path != parent.Path+"/"+n.Name {
			return false
		}
	}
	return false
}

// TeamOf reports whether the Group at path is a Team or one of its Leader
// groups, and which: the Team's path and, for a Leader group, its name
// (role). A Team is a Group at least three deep in the members' tree
// (/UYELER/<area>/<team>, or a Sub-team below one); a Leader group is one of
// leaders right under a Team, with nothing under it. A Group under a
// Privileged root of roots is neither, nor are the members' tree itself,
// its areas, or Groups outside it.
func TeamOf(path string, roots, leaders []string) (team, role string, ok bool) {
	if !strings.HasPrefix(path, MembersRoot+"/") || PrivilegedRoot(roots, path) != "" {
		return "", "", false
	}
	segments := strings.Split(strings.TrimPrefix(path, "/"), "/")
	for i, segment := range segments {
		if segment == "" {
			return "", "", false
		}
		// A leader subgroup ends a path: nothing sits under one.
		if slices.Contains(leaders, segment) && i != len(segments)-1 {
			return "", "", false
		}
	}
	team = path
	if last := segments[len(segments)-1]; slices.Contains(leaders, last) {
		segments = segments[:len(segments)-1]
		team, role = "/"+strings.Join(segments, "/"), last
	}
	if len(segments) < minTeamDepth {
		return "", "", false
	}
	return team, role, true
}

// minTeamDepth is the depth of a Team's own Group: /UYELER/<area>/<team>.
// /UYELER itself is the club and /UYELER/<area> an area (ARGE,
// ORGANIZASYON, …) or a technical Group, none of them a Team.
const minTeamDepth = 3
