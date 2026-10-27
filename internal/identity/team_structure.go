package identity

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/teamaudit"
	"github.com/skylab-kulubu/core-backend/internal/teamcode"
)

// Team structure (docs/team-delegation.md "Team structure", ADR 0065
// ticket 05): a Team's manager opens a Sub-team under it, and whoever
// manages a Sub-team strictly changes its code or deletes it. Privileged
// does the same on every Team. Every check and the Keycloak write run under
// the Team code lock, with the Groups read again by id inside it.

// The problem codes of a Team structure write, beside the name rule's
// (teamcode.InvalidName, …) and delegation's.
const (
	// CodeDepthLimit: the Sub-team would sit deeper below its top Team, or
	// have a longer path, than the Team structure limits allow.
	CodeDepthLimit = "depth_limit"
	// CodeChildrenLimit: the Team has as many Sub-teams as a Team may.
	CodeChildrenLimit = "children_limit"
	// CodeCodeInUse: core's records still name the code (Records says
	// which kinds), so it is not changed or given up.
	CodeCodeInUse = "code_in_use"
)

// TeamStructureError is a Team structure write core refused, with the
// status it is answered with. Records are, for CodeCodeInUse, the kinds of
// record that name the code (teamcode.RecordEvent, …), never a record.
type TeamStructureError struct {
	Status  int
	Code    string
	Detail  string
	Records []string
}

func (e *TeamStructureError) Error() string {
	return fmt.Sprintf("identity: team structure refused: %s (%d)", e.Code, e.Status)
}

// NewSubTeam is a Sub-team to open (POST /v1/groups/{id}/children): its
// code and its display names, Turkish required.
type NewSubTeam struct {
	Code          string
	DisplayNameTr string
	DisplayNameEn string
}

// maxDisplayName is the most characters a display name a Sub-team is
// opened with may have.
const maxDisplayName = 80

// CreateSubTeam opens a Sub-team under the Team parentRef (a Keycloak id):
// Privileged under any Team, a Leader (delegation on) under a Team they
// manage (authz.Policy.Manages). The code passes the name rule and is free
// everywhere; a leader subgroup's name, or one that only looks like one, is
// refused (a Leader group is made by naming a Leader, ticket 06). The
// Team structure limits hold for every caller. The Sub-team is created with
// its display names in one Keycloak write, a team.create row.
func (s *service) CreateSubTeam(ctx context.Context, p authz.Principal, parentRef string, in NewSubTeam) (Group, error) {
	var created Group
	if s.allow(p, authz.TypeGroup, authz.Create) == nil {
		if _, err := uuid.Parse(parentRef); err != nil {
			return Group{}, ErrNotFound
		}
		err := s.codes.Locked(ctx, func(ctx context.Context, records teamcode.Records) error {
			parent, _, err := s.dir.GroupNode(ctx, parentRef)
			if err != nil {
				return err
			}
			created, err = s.createSubTeam(ctx, records, parent, in)
			return err
		})
		return created, err
	}
	err := s.delegatedRecordsWrite(ctx, p, teamaudit.ActionTeamCreate, parentRef, uuid.Nil, authz.Policy.Manages,
		func(ctx context.Context, records teamcode.Records, m authz.TeamManager, target authz.ManagedGroup, parent Group) error {
			d := s.delegation.policy.Manages(m, target)
			if !d.Allowed {
				s.deny(ctx, teamaudit.ActionTeamCreate, target.Group, uuid.Nil, d, d.Refusal)
				return decisionError(d)
			}
			var err error
			created, err = s.createSubTeam(asLeader(ctx, d), records, parent, in)
			if code := structureRefusal(err); code != "" {
				s.deny(ctx, teamaudit.ActionTeamCreate, target.Group, uuid.Nil, d, code)
			}
			return err
		})
	return created, err
}

// createSubTeam opens in under parent, read inside the lock.
func (s *service) createSubTeam(ctx context.Context, records teamcode.Records, parent Group, in NewSubTeam) (Group, error) {
	code := strings.TrimSpace(in.Code)
	tr, en := strings.TrimSpace(in.DisplayNameTr), strings.TrimSpace(in.DisplayNameEn)
	if code == "" || tr == "" || utf8.RuneCountInString(tr) > maxDisplayName || utf8.RuneCountInString(en) > maxDisplayName {
		return Group{}, ErrInvalid
	}
	// Only a Team takes Sub-teams: not an area, a root, a Leader group or a
	// Group outside the members' tree.
	if !s.isTeam(parent.Path) {
		return Group{}, ErrInvalid
	}
	if err := s.subTeamCode(parent.Path, code); err != nil {
		return Group{}, err
	}
	path := parent.Path + "/" + code
	if err := s.withinStructureLimits(path); err != nil {
		return Group{}, err
	}
	children, err := s.dir.Subgroups(ctx, parent.ID)
	if err != nil {
		return Group{}, err
	}
	_, _, limit := s.structureLimits()
	subTeams := 0
	for _, child := range children {
		if !s.names.Structural(child.Name) {
			subTeams++
		}
	}
	if subTeams >= limit {
		return Group{}, &TeamStructureError{Status: http.StatusConflict, Code: CodeChildrenLimit,
			Detail: fmt.Sprintf("A Team has at most %d Sub-teams.", limit)}
	}
	if err := s.nameFree(ctx, records, teamcode.Everywhere, path, code, ""); err != nil {
		return Group{}, err
	}
	attributes := map[string]string{"display_name_tr": tr}
	if en != "" {
		attributes["display_name_en"] = en
	}
	// The managed trees and the realm's names change with it.
	defer s.forgetAllGroups()
	return s.dir.CreateSubgroup(ctx, parent.ID, code, attributes)
}

// ChangeTeamCode changes the code (Keycloak name) of the Team groupRef (a
// Keycloak id): Privileged on any Team, a Leader (delegation on) on a
// Sub-team they manage strictly (authz.Policy.ManagesStrict: not their own
// Team). The new code passes the name rule and is free everywhere, as a
// create's; the old one is retired with the write, and refused with
// code_in_use while core's records still name it (S9). Every kept Group
// read is forgotten: the Team's path, and every path under it, changed.
func (s *service) ChangeTeamCode(ctx context.Context, p authz.Principal, groupRef, code string) (Group, error) {
	var updated Group
	if s.allow(p, authz.TypeGroup, authz.Update) == nil {
		if _, err := uuid.Parse(groupRef); err != nil {
			return Group{}, ErrNotFound
		}
		err := s.codes.Locked(ctx, func(ctx context.Context, records teamcode.Records) error {
			g, _, err := s.dir.GroupNode(ctx, groupRef)
			if err != nil {
				return err
			}
			updated, err = s.changeTeamCode(ctx, records, g, code)
			return err
		})
		return updated, err
	}
	err := s.delegatedRecordsWrite(ctx, p, teamaudit.ActionTeamCode, groupRef, uuid.Nil, authz.Policy.ManagesStrict,
		func(ctx context.Context, records teamcode.Records, m authz.TeamManager, target authz.ManagedGroup, g Group) error {
			d := s.delegation.policy.ManagesStrict(m, target)
			if !d.Allowed {
				s.deny(ctx, teamaudit.ActionTeamCode, target.Group, uuid.Nil, d, d.Refusal)
				return decisionError(d)
			}
			var err error
			updated, err = s.changeTeamCode(asLeader(ctx, d), records, g, code)
			if code := structureRefusal(err); code != "" {
				s.deny(ctx, teamaudit.ActionTeamCode, target.Group, uuid.Nil, d, code)
			}
			return err
		})
	return updated, err
}

// changeTeamCode renames the Team g, read inside the lock, to code.
func (s *service) changeTeamCode(ctx context.Context, records teamcode.Records, g Group, code string) (Group, error) {
	code = strings.TrimSpace(code)
	if code == "" || !s.isTeam(g.Path) {
		return Group{}, ErrInvalid
	}
	if code == g.Name {
		return g, nil
	}
	below, err := s.groupsBelow(ctx, g)
	if err != nil {
		return Group{}, err
	}
	if err := s.subTeamCode(g.Path[:strings.LastIndex(g.Path, "/")], code); err != nil {
		return Group{}, err
	}
	if _, err := s.names.Rename(g.Path, code, len(below) == 0); err != nil {
		return Group{}, err
	}
	if err := s.renameKeepsPlacement(ctx, g, code, len(below) > 0); err != nil {
		return Group{}, err
	}
	path := g.Path[:strings.LastIndex(g.Path, "/")+1] + code
	if err := s.withinStructureLimits(path); err != nil {
		return Group{}, err
	}
	for _, under := range below {
		if err := s.withinStructureLimits(path + strings.TrimPrefix(under.Path, g.Path)); err != nil {
			return Group{}, err
		}
	}
	// A change of case keeps the code: the records that hold it are this
	// Team's own, and nothing is retired.
	sameCode := strings.EqualFold(strings.TrimSpace(g.Name), code)
	scope := teamcode.Everywhere
	if sameCode {
		scope = teamcode.AtPath
	} else {
		kinds, err := records.References(ctx, g.Name)
		if err != nil {
			return Group{}, err
		}
		if len(kinds) > 0 {
			return Group{}, &TeamStructureError{Status: http.StatusConflict, Code: CodeCodeInUse, Records: kinds,
				Detail: "Core's records (" + strings.Join(kinds, ", ") + ") still name the Team's code; it is not changed while they do."}
		}
	}
	if err := s.nameFree(ctx, records, scope, path, code, g.ID); err != nil {
		return Group{}, err
	}
	if !sameCode && !s.names.Reserved(g.Name) {
		if err := records.Retire(ctx, g.Name, g.ID, teamcode.ReasonRenamed); err != nil {
			return Group{}, err
		}
	}
	// Forgotten even when the write fails: it may have reached Keycloak.
	defer s.forgetAllGroups()
	g.Name = code
	return s.dir.UpdateGroup(ctx, g)
}

// subTeamCode refuses, beside the name rule, the names a Team structure
// write never gives: a leader subgroup's (a Leader group is made only by
// naming a Leader, ticket 06; #227 review Y1) and one that only looks like
// one (LIDERLER-ESKI: a reader of paths by substring, such as the CMS's
// Teams rule, could take its members for Leaders).
func (s *service) subTeamCode(parentPath, code string) error {
	scope, err := s.names.Create(parentPath, code)
	if err != nil {
		return err
	}
	if scope != teamcode.Everywhere {
		return reservedForLeaders(code)
	}
	folded := strings.ToUpper(code)
	for _, leader := range authz.DefaultPolicy().LeaderSubgroups {
		if strings.HasPrefix(folded, strings.ToUpper(leader)) {
			return reservedForLeaders(code)
		}
	}
	return nil
}

func reservedForLeaders(code string) error {
	return &teamcode.Error{Code: teamcode.ReservedName,
		Detail: "The name " + code + " is a Leader group's, or looks like one: a Leader group is made only by naming a Leader of the Team."}
}

// structureLimits are the Team structure limits: how many levels of
// Sub-teams below a top Team, how long a path, how many Sub-teams a Team.
func (s *service) structureLimits() (depth, pathLen, subTeams int) {
	if s.delegation != nil {
		return structureLimits(s.delegation.options)
	}
	return structureLimits(TeamDelegationOptions{})
}

// withinStructureLimits refuses a Team at path deeper below its top Team,
// or with a longer path, than the limits allow (depth_limit).
func (s *service) withinStructureLimits(path string) error {
	depth, pathLen, _ := s.structureLimits()
	if strings.Count(path, "/") > teamTopDepth+depth {
		return &TeamStructureError{Status: http.StatusConflict, Code: CodeDepthLimit,
			Detail: fmt.Sprintf("A Team has at most %d levels of Sub-teams below it.", depth)}
	}
	if len(path) > pathLen {
		return &TeamStructureError{Status: http.StatusConflict, Code: CodeDepthLimit,
			Detail: fmt.Sprintf("A Team's path is at most %d characters long.", pathLen)}
	}
	return nil
}

// teamTopDepth is how many Groups deep a top Team is: /UYELER/<area>/<team>.
const teamTopDepth = 3

// groupsBelow are every Group under g, read by walking down from it by id,
// each with a path that is its parent's and its own name. A tree that does
// not hold, or one far larger than a Team's, is group_tree_mismatch.
func (s *service) groupsBelow(ctx context.Context, g Group) ([]Group, error) {
	var out []Group
	type level struct {
		group Group
		depth int
	}
	queue := []level{{g, 0}}
	for len(queue) > 0 {
		next := queue[0]
		queue = queue[1:]
		children, err := s.dir.Subgroups(ctx, next.group.ID)
		if err != nil {
			return nil, err
		}
		for _, child := range children {
			if next.depth+1 >= maxGroupDepth || len(out) >= maxGroupsBelow || child.ID == "" ||
				strings.Contains(child.Name, "/") || child.Path != next.group.Path+"/"+child.Name {
				return nil, refusedWith(http.StatusConflict, CodeGroupTreeMismatch)
			}
			out = append(out, child)
			queue = append(queue, level{child, next.depth + 1})
		}
	}
	return out, nil
}

// maxGroupsBelow bounds a walk down a Team: a Team has at most a few
// dozen Groups under it (SPEC §4.5).
const maxGroupsBelow = 512

// structureRefusal is the problem code of a Team structure refusal (the
// name rule, the structure limits, a code in use), "" for any other error:
// a delegated refusal is a denied row.
func structureRefusal(err error) string {
	switch e := err.(type) {
	case *teamcode.Error:
		return e.Code
	case *TeamStructureError:
		return e.Code
	case *RoleRuleError:
		return e.Code
	}
	return ""
}
