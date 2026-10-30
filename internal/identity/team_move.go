package identity

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/teamaudit"
	"github.com/skylab-kulubu/core-backend/internal/teamcode"
)

// Moving a Team (docs/team-delegation.md "Moving a Team", ADR 0065 ticket
// 07, K4): a Sub-team goes under another Team when the caller manages
// both, the one it leaves strictly (authz.Policy.ManagesStrict) and the one
// it joins (Manages): their common upper Leader, or Privileged. Privileged
// also moves a top Team, to another area. Never into its own subtree, never
// under a Leader group, never from, to or into a Privileged root. A preview
// says what the move would do first.

// The problem codes of a refused move, beside depth_limit, children_limit,
// privileged_root and protected_group.
const (
	// CodeInvalidMoveSource: the Group moved is no Team (an area, the
	// members' tree, a Leader group, a Group outside the members' tree).
	CodeInvalidMoveSource = "invalid_move_source"
	// CodeInvalidMoveTarget: the new parent is no Team (or, for
	// Privileged, no area).
	CodeInvalidMoveTarget = "invalid_move_target"
	// CodeLeaderGroupTarget: the new parent is a Leader group; nothing
	// sits under one.
	CodeLeaderGroupTarget = "leader_group_target"
	// CodeCycle: the new parent is the Group itself or under it.
	CodeCycle = "cycle"
	// CodeSameParent: the Group is right under the new parent already.
	CodeSameParent = "same_parent"
)

// MovePreview is what moving a Team would do (GET
// /v1/groups/{id}/move-preview): whether the rules allow it and, if not,
// the code that refuses it; the path before and after; how many people are
// in the Team and every Group under it, each once; the client roles that
// the Groups above it give the Team now and would give after the move
// (gained and lost, "client:role"); the new parent's Leaders.
type MovePreview struct {
	Allowed          bool     `json:"allowed"`
	Refusal          string   `json:"refusal,omitempty"`
	OldPath          string   `json:"oldPath"`
	NewPath          string   `json:"newPath"`
	Members          int      `json:"members"`
	RolesGained      []string `json:"rolesGained"`
	RolesLost        []string `json:"rolesLost"`
	NewParentLeaders int      `json:"newParentLeaders"`
}

// moveSides are the two Groups of a move, read by id inside or outside the
// lock: the Group with the Groups above it (by id), and the new parent with
// the Groups above it.
type moveSides struct {
	group, parent           Group
	groupAbove, parentAbove []Group
}

// MoveTeam moves the Team groupRef under parentRef, both Keycloak ids (a
// name or a path is 400): Privileged, or a Leader (delegation on) who
// manages the Team strictly and the new parent. Every rule is checked
// under the Team code lock with both Groups read again by id; the move is
// a team.move row. Every kept Group read is forgotten.
func (s *service) MoveTeam(ctx context.Context, p authz.Principal, groupRef, parentRef string) (Group, error) {
	if _, err := uuid.Parse(parentRef); err != nil {
		return Group{}, ErrInvalid
	}
	var moved Group
	if s.allow(p, authz.TypeGroup, authz.Update) == nil {
		if _, err := uuid.Parse(groupRef); err != nil {
			return Group{}, ErrNotFound
		}
		err := s.codes.Locked(ctx, func(ctx context.Context, records teamcode.Records) error {
			sides, err := s.readMoveSides(ctx, groupRef, parentRef)
			if err != nil {
				return err
			}
			if err := s.checkMove(ctx, records, sides, true); err != nil {
				return err
			}
			moved, err = s.moveGroup(ctx, sides)
			return err
		})
		return moved, err
	}
	err := s.delegatedRecordsWrite(ctx, p, teamaudit.ActionTeamMove, groupRef, uuid.Nil, authz.Policy.ManagesStrict,
		func(ctx context.Context, records teamcode.Records, m authz.TeamManager, target authz.ManagedGroup, _ Group) error {
			d := s.delegation.policy.ManagesStrict(m, target)
			if !d.Allowed {
				s.deny(ctx, teamaudit.ActionTeamMove, target.Group, uuid.Nil, d, d.Refusal)
				return decisionError(d)
			}
			if err := s.delegatedMoveTarget(ctx, m, target, d, parentRef); err != nil {
				return err
			}
			sides, err := s.readMoveSides(ctx, groupRef, parentRef)
			if err != nil {
				return err
			}
			if err := s.checkMove(ctx, records, sides, false); err != nil {
				if code := structureRefusal(err); code != "" {
					s.deny(ctx, teamaudit.ActionTeamMove, target.Group, uuid.Nil, d, code)
				}
				return err
			}
			moved, err = s.moveGroup(asLeader(ctx, d), sides)
			return err
		})
	return moved, err
}

// delegatedMoveTarget decides whether the Leader m, who manages the moved
// Team strictly (d), manages the new parent too (authz.Policy.Manages,
// read fresh): a protected parent is protected_group. A refusal is a
// denied row on the moved Team.
func (s *service) delegatedMoveTarget(ctx context.Context, m authz.TeamManager, moved authz.ManagedGroup, d authz.ManageDecision, parentRef string) error {
	parent, _, err := s.delegation.target(ctx, parentRef, true)
	var refused *DelegationError
	switch {
	case errors.Is(err, ErrNotFound):
		s.deny(ctx, teamaudit.ActionTeamMove, moved.Group, uuid.Nil, d, "forbidden")
		return ErrForbidden
	case errors.As(err, &refused):
		s.deny(ctx, teamaudit.ActionTeamMove, moved.Group, uuid.Nil, d, refused.Code)
		return err
	case err != nil:
		return err
	}
	dp := s.delegation.policy.Manages(m, parent)
	if !dp.Allowed {
		code := dp.Refusal
		if code == "" {
			code = "forbidden"
		}
		s.deny(ctx, teamaudit.ActionTeamMove, moved.Group, uuid.Nil, d, code)
		return decisionError(dp)
	}
	return nil
}

// MoveTeamPreview is MovePreview of moving groupRef under parentRef, for
// whoever may make the move (MoveTeam's decision); a rule the move would
// break is Allowed false with its code, not an error.
func (s *service) MoveTeamPreview(ctx context.Context, p authz.Principal, groupRef, parentRef string) (MovePreview, error) {
	if _, err := uuid.Parse(parentRef); err != nil {
		return MovePreview{}, ErrInvalid
	}
	// The move's own decision: the write permission, so a Privileged
	// reader (DK) previews as a Leader or not at all.
	privileged := s.allow(p, authz.TypeGroup, authz.Update) == nil
	if privileged {
		if _, err := uuid.Parse(groupRef); err != nil {
			return MovePreview{}, ErrNotFound
		}
	} else {
		userID, err := s.delegatedCaller(p, groupRef)
		if err != nil {
			return MovePreview{}, err
		}
		m, err := s.delegation.manager(ctx, userID, false)
		if err != nil {
			return MovePreview{}, err
		}
		if len(m.LeaderGroups) == 0 {
			return MovePreview{}, ErrForbidden
		}
		target, _, err := s.delegation.target(ctx, groupRef, false)
		if errors.Is(err, ErrNotFound) {
			return MovePreview{}, ErrForbidden
		}
		if err != nil {
			return MovePreview{}, err
		}
		if d := s.delegation.policy.ManagesStrict(m, target); !d.Allowed {
			return MovePreview{}, decisionError(d)
		}
		parent, _, err := s.delegation.target(ctx, parentRef, false)
		if errors.Is(err, ErrNotFound) {
			return MovePreview{}, ErrForbidden
		}
		if err != nil {
			return MovePreview{}, err
		}
		if d := s.delegation.policy.Manages(m, parent); !d.Allowed {
			return MovePreview{}, decisionError(d)
		}
	}
	sides, err := s.readMoveSides(ctx, groupRef, parentRef)
	if err != nil {
		return MovePreview{}, err
	}
	out := MovePreview{Allowed: true, OldPath: sides.group.Path, NewPath: sides.parent.Path + "/" + sides.group.Name, RolesGained: []string{}, RolesLost: []string{}}
	// The preview takes no lock: it reads what the move would see now
	// (checkMove asks the records nothing: the path is checked AtPath).
	err = s.checkMove(ctx, nil, sides, privileged)
	if code := moveRefusal(err); code != "" {
		out.Allowed, out.Refusal = false, code
	} else if err != nil {
		return MovePreview{}, err
	}
	hits, err := s.memberHits(ctx, sides.group)
	if err != nil {
		return MovePreview{}, err
	}
	out.Members = len(hits)
	before, err := s.inheritedRoles(ctx, sides.groupAbove)
	if err != nil {
		return MovePreview{}, err
	}
	after, err := s.inheritedRoles(ctx, append([]Group{sides.parent}, sides.parentAbove...))
	if err != nil {
		return MovePreview{}, err
	}
	for _, role := range after {
		if !slices.Contains(before, role) {
			out.RolesGained = append(out.RolesGained, role)
		}
	}
	for _, role := range before {
		if !slices.Contains(after, role) {
			out.RolesLost = append(out.RolesLost, role)
		}
	}
	if s.isTeam(sides.parent.Path) {
		leaders, err := s.leaders(ctx, sides.parent)
		if err != nil {
			return MovePreview{}, err
		}
		out.NewParentLeaders = len(leaders)
	}
	return out, nil
}

// moveRefusal is the code of a rule a move breaks, "" for any other error.
func moveRefusal(err error) string {
	var refused *DelegationError
	if errors.As(err, &refused) {
		return refused.Code
	}
	return structureRefusal(err)
}

// inheritedRoles are the client roles the Groups above map ("client:role",
// sorted, each once): what they give everyone under them.
func (s *service) inheritedRoles(ctx context.Context, above []Group) ([]string, error) {
	var out []string
	for _, g := range above {
		roles, err := s.dir.GroupClientRoles(ctx, g.ID)
		if err != nil {
			return nil, err
		}
		for _, role := range roleNames(roles) {
			if !slices.Contains(out, role) {
				out = append(out, role)
			}
		}
	}
	slices.Sort(out)
	return out, nil
}

// readMoveSides reads the Group groupID and the new parent parentID, each
// with the Groups above it, by id. A chain whose ids and paths disagree is
// group_tree_mismatch.
func (s *service) readMoveSides(ctx context.Context, groupID, parentID string) (moveSides, error) {
	group, groupAbove, err := s.groupWithAbove(ctx, groupID)
	if err != nil {
		return moveSides{}, err
	}
	parent, parentAbove, err := s.groupWithAbove(ctx, parentID)
	if err != nil {
		return moveSides{}, err
	}
	return moveSides{group: group, parent: parent, groupAbove: groupAbove, parentAbove: parentAbove}, nil
}

// groupWithAbove is the Group id and every Group above it, parent first,
// each read by the id its child names, the paths agreeing.
func (s *service) groupWithAbove(ctx context.Context, id string) (Group, []Group, error) {
	g, next, err := s.dir.GroupNode(ctx, id)
	if err != nil {
		return Group{}, nil, err
	}
	var above []Group
	node := g
	for next != "" {
		if len(above) == maxGroupDepth {
			return Group{}, nil, refusedWith(http.StatusConflict, CodeGroupTreeMismatch)
		}
		parent, parentOfParent, err := s.dir.GroupNode(ctx, next)
		if errors.Is(err, ErrNotFound) {
			return Group{}, nil, refusedWith(http.StatusConflict, CodeGroupTreeMismatch)
		}
		if err != nil {
			return Group{}, nil, err
		}
		if strings.Contains(node.Name, "/") || node.Path != parent.Path+"/"+node.Name {
			return Group{}, nil, refusedWith(http.StatusConflict, CodeGroupTreeMismatch)
		}
		above = append(above, parent)
		node, next = parent, parentOfParent
	}
	if strings.Contains(node.Name, "/") || node.Path != "/"+node.Name {
		return Group{}, nil, refusedWith(http.StatusConflict, CodeGroupTreeMismatch)
	}
	return g, above, nil
}

// checkMove checks the rules of moving sides.group under sides.parent
// (SPEC §7; review of #227, Y4), for Privileged when privileged.
func (s *service) checkMove(ctx context.Context, records teamcode.Records, sides moveSides, privileged bool) error {
	g, parent := sides.group, sides.parent
	below, err := s.groupsBelow(ctx, g)
	if err != nil {
		return err
	}
	newPath := parent.Path + "/" + g.Name
	// No caller moves a Privileged root, a Group above or under one, nor
	// makes one (a Group named ADMIN moved under /UYELER), wherever it is.
	paths := []string{g.Path, newPath}
	for _, under := range below {
		paths = append(paths, under.Path, newPath+strings.TrimPrefix(under.Path, g.Path))
	}
	for _, path := range paths {
		if s.touchesPrivilegedRoot(path) {
			return moveRefused(teamcode.PrivilegedRoot)
		}
	}
	// Nothing moves under a root (a Group above one, /UYELER, only takes
	// what makes no root, checked above).
	if authz.PrivilegedRoot(upperAll(s.privilegedRoots), strings.ToUpper(parent.Path)) != "" {
		return moveRefused(teamcode.PrivilegedRoot)
	}
	if !s.isTeam(g.Path) {
		return moveRefused(CodeInvalidMoveSource)
	}
	if parent.ID == g.ID || parent.Path == g.Path || strings.HasPrefix(parent.Path, g.Path+"/") ||
		slices.ContainsFunc(sides.parentAbove, func(a Group) bool { return a.ID == g.ID }) {
		return moveRefused(CodeCycle)
	}
	if _, role, ok := authz.TeamOf(parent.Path, s.privilegedRoots, authz.DefaultPolicy().LeaderSubgroups); ok && role != "" {
		return moveRefused(CodeLeaderGroupTarget)
	}
	area := strings.HasPrefix(parent.Path, authz.MembersRoot+"/") && strings.Count(parent.Path, "/") == 2
	if !s.isTeam(parent.Path) && !(privileged && area) {
		return moveRefused(CodeInvalidMoveTarget)
	}
	if !privileged && !s.isTeam(g.Path[:strings.LastIndex(g.Path, "/")]) {
		// A Leader moves Sub-teams only; ManagesStrict says so already.
		return moveRefused(CodeInvalidMoveSource)
	}
	if len(sides.groupAbove) > 0 && sides.groupAbove[0].ID == parent.ID {
		return moveRefused(CodeSameParent)
	}
	if err := s.withinStructureLimits(newPath); err != nil {
		return err
	}
	for _, under := range below {
		if err := s.withinStructureLimits(newPath + strings.TrimPrefix(under.Path, g.Path)); err != nil {
			return err
		}
	}
	if err := s.moveKeepsPlacement(ctx, g, below, newPath); err != nil {
		return err
	}
	children, err := s.dir.Subgroups(ctx, parent.ID)
	if err != nil {
		return err
	}
	_, _, limit := s.structureLimits()
	subTeams := 0
	for _, child := range children {
		if !s.names.Structural(child.Name) {
			subTeams++
		}
	}
	if subTeams >= limit {
		return &TeamStructureError{Status: http.StatusConflict, Code: CodeChildrenLimit, Detail: "The new parent has as many Sub-teams as a Team may."}
	}
	// The code is unique, so nothing else sits at the new path; check it
	// all the same, as Keycloak would refuse it.
	return s.nameFree(ctx, records, teamcode.AtPath, newPath, g.Name, g.ID)
}

// moveKeepsPlacement refuses (role_placement) a move after which g or a
// Group under it would map a client role where the placement rule
// (authz.RolePlacementOf) maps none: a top Team's app roles under another
// Team, as renameKeepsPlacement refuses a rename. A Group mapped against
// the rule already is left to be cleaned.
func (s *service) moveKeepsPlacement(ctx context.Context, g Group, below []Group, newPath string) error {
	for _, group := range append([]Group{g}, below...) {
		roles, err := s.dir.GroupClientRoles(ctx, group.ID)
		if err != nil {
			return err
		}
		if len(roles) == 0 {
			continue
		}
		hasSubgroups := slices.ContainsFunc(below, func(under Group) bool { return strings.HasPrefix(under.Path, group.Path+"/") })
		if s.placementOf(group.Path, hasSubgroups, roles) != "" {
			continue
		}
		reason := s.placementOf(newPath+strings.TrimPrefix(group.Path, g.Path), hasSubgroups, roles)
		if reason == "" {
			continue
		}
		return &RoleRuleError{Status: http.StatusConflict, Code: CodeRolePlacement, Reason: reason,
			Detail: "A Group moved maps roles; at the new path they would sit where no role is mapped. " + authz.PlacementDetail(reason)}
	}
	return nil
}

// touchesPrivilegedRoot reports whether path is a Privileged root, under
// one or above one, compared without case.
func (s *service) touchesPrivilegedRoot(path string) bool {
	path = strings.ToUpper(path)
	for _, root := range s.privilegedRoots {
		root = strings.ToUpper(root)
		if path == root || strings.HasPrefix(path, root+"/") || strings.HasPrefix(root, path+"/") {
			return true
		}
	}
	return false
}

// moveGroup makes the move, forgetting every kept Group read (even when
// it fails: it may have reached Keycloak).
func (s *service) moveGroup(ctx context.Context, sides moveSides) (Group, error) {
	defer s.forgetAllGroups()
	return s.dir.MoveGroup(ctx, sides.group.ID, sides.parent.ID)
}

func moveRefused(code string) error {
	status := http.StatusConflict
	detail := map[string]string{
		CodeInvalidMoveSource:   "Only a Team moves (a Leader moves Sub-teams only).",
		CodeInvalidMoveTarget:   "A Team moves under a Team (Privileged: or an area).",
		CodeLeaderGroupTarget:   "Nothing sits under a Leader group.",
		CodeCycle:               "A Team does not move under itself or a Group under it.",
		CodeSameParent:          "The Team is right under that Group already.",
		teamcode.PrivilegedRoot: "A Privileged root, a Group above or under one, is never moved, nor a Group moved to make one.",
	}[code]
	return &TeamStructureError{Status: status, Code: code, Detail: detail}
}

func upperAll(paths []string) []string {
	out := make([]string, len(paths))
	for i, p := range paths {
		out[i] = strings.ToUpper(p)
	}
	return out
}
