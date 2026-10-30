package identity

import (
	"context"
	"errors"
	"net/http"
	"slices"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/teamaudit"
	"github.com/skylab-kulubu/core-backend/internal/teamcode"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// Team Leaders (docs/team-delegation.md "Leaders", ADR 0065 ticket 06):
// whoever manages a Team strictly names and removes its Leaders and
// Coordinators. The Leader group (LIDERLER, KOORDINATORLER) is opened by
// core, right under the Team, when its first Leader is named: the only way
// a Group gets a leader subgroup's name once delegation is on. Nobody
// changes the Leaders of their own Team (S2), and a top Team's only
// Privileged.

// TeamLeader is one Leader or Coordinator of a Team (GET
// /v1/groups/{id}/leaders): the person, which Leader group they are in
// (Kind, a leader subgroup's name) and its id.
type TeamLeader struct {
	Person
	Kind    string `json:"kind"`
	GroupID string `json:"groupId"`
}

// LeaderRemoval is the answer of a removal: LastLeader when the Team has
// no Leader or Coordinator left (the panel warns; the Team stays managed
// from above).
type LeaderRemoval struct {
	LastLeader bool `json:"lastLeader"`
}

// TeamLeaders are the Leaders of the Team groupRef (a Keycloak id), the
// Leader groups in the policy's order (LIDERLER, then KOORDINATORLER), and
// in each by username: Privileged, or a Leader who manages the Team
// (Manages), who sees a person's id, names, e-mail address and profile
// picture (SPEC §6.4).
func (s *service) TeamLeaders(ctx context.Context, p authz.Principal, groupRef string) ([]TeamLeader, error) {
	var team Group
	full := false
	if s.allow(p, authz.TypeGroup, authz.Read) == nil {
		if _, err := uuid.Parse(groupRef); err != nil {
			return nil, ErrNotFound
		}
		var err error
		if team, _, err = s.dir.GroupNode(ctx, groupRef); err != nil {
			return nil, err
		}
		full = true
	} else {
		var err error
		if _, _, team, err = s.delegatedRead(ctx, p, groupRef); err != nil {
			return nil, err
		}
	}
	if !s.isTeam(team.Path) {
		return nil, ErrInvalid
	}
	groups, err := s.leaderGroups(ctx, team)
	if err != nil {
		return nil, err
	}
	out := make([]TeamLeader, 0)
	for _, g := range groups {
		people, err := s.dir.Members(ctx, g.ID)
		if err != nil {
			return nil, err
		}
		slices.SortFunc(people, byUsername)
		for _, person := range projectPeople(people, full) {
			out = append(out, TeamLeader{Person: person, Kind: g.Name, GroupID: g.ID})
		}
	}
	return out, nil
}

// leaderGroups are team's Leader groups, in the policy's order: a Group
// right under it, by its path too, named after a leader subgroup.
func (s *service) leaderGroups(ctx context.Context, team Group) ([]Group, error) {
	children, err := s.dir.Subgroups(ctx, team.ID)
	if err != nil {
		return nil, err
	}
	var out []Group
	for _, kind := range authz.DefaultPolicy().LeaderSubgroups {
		for _, child := range children {
			if child.Name == kind && child.Path == team.Path+"/"+kind {
				out = append(out, child)
			}
		}
	}
	return out, nil
}

// AddTeamLeader makes userID a Leader (kind LIDERLER, the default) or a
// Coordinator (KOORDINATORLER) of the Team groupRef: Privileged on any Team,
// a Leader (delegation on) on a Team they manage strictly (a Sub-team below
// their own). The Leader group is opened under the Team, under the lock,
// when it has none. The person is an active account; a delegated add keeps
// them within the delegated Group limit and never reaches a protected
// Leader group. A leader.add row with the kind; the team membership mail
// goes with Role leader or coordinator.
func (s *service) AddTeamLeader(ctx context.Context, p authz.Principal, groupRef string, userID uuid.UUID, kind string) error {
	if kind == "" {
		kind = authz.DefaultPolicy().LeaderSubgroups[0]
	}
	if !slices.Contains(authz.DefaultPolicy().LeaderSubgroups, kind) {
		return ErrInvalid
	}
	if s.allow(p, authz.TypeGroup, authz.Update) == nil {
		if _, err := uuid.Parse(groupRef); err != nil {
			return ErrNotFound
		}
		return s.codes.Locked(ctx, func(ctx context.Context, records teamcode.Records) error {
			team, _, err := s.dir.GroupNode(ctx, groupRef)
			if err != nil {
				return err
			}
			return s.addTeamLeader(ctx, p, records, team, userID, kind, nil)
		})
	}
	return s.delegatedRecordsWrite(ctx, p, teamaudit.ActionLeaderAdd, groupRef, userID, authz.Policy.ManagesStrict,
		func(ctx context.Context, records teamcode.Records, m authz.TeamManager, target authz.ManagedGroup, team Group) error {
			d := s.delegation.policy.ManagesStrict(m, target)
			if !d.Allowed {
				s.deny(ctx, teamaudit.ActionLeaderAdd, target.Group, userID, d, d.Refusal)
				return decisionError(d)
			}
			return s.addTeamLeader(asLeader(ctx, d), p, records, team, userID, kind, &delegatedLeaderWrite{d: d, target: target})
		})
}

// delegatedLeaderWrite is a Leader's decision on a Leader write and the
// Team it was made on: the checks only a delegated write makes.
type delegatedLeaderWrite struct {
	d      authz.ManageDecision
	target authz.ManagedGroup
}

func (s *service) addTeamLeader(ctx context.Context, p authz.Principal, records teamcode.Records, team Group, userID uuid.UUID, kind string, delegated *delegatedLeaderWrite) error {
	if !s.isTeam(team.Path) {
		return ErrInvalid
	}
	refuse := func(status int, code string) error {
		if delegated != nil {
			s.deny(ctx, teamaudit.ActionLeaderAdd, delegated.target.Group, userID, delegated.d, code)
		}
		return refusedWith(status, code)
	}
	if delegated != nil {
		clash, err := s.delegation.nameClash(ctx, delegated.target)
		if err != nil {
			return err
		}
		if clash != "" {
			return refuse(http.StatusConflict, CodeTeamCodeNotUnique)
		}
	}
	shadow, err := s.users.Get(ctx, userID)
	if errors.Is(err, user.ErrNotFound) || (err == nil && (shadow.AccountState != user.AccountActive || userID == user.DeletedSubject)) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	groups, err := s.leaderGroups(ctx, team)
	if err != nil {
		return err
	}
	var leaders Group
	for _, g := range groups {
		if g.Name == kind {
			leaders = g
		}
	}
	if leaders.ID != "" {
		if delegated != nil {
			protected, err := s.delegation.protected(ctx, []string{leaders.ID}, true)
			if err != nil {
				return err
			}
			if protected {
				return refuse(http.StatusForbidden, authz.RefusalProtectedGroup)
			}
		} else if err := s.notKeycloakAdminGroup(ctx, leaders.ID); err != nil {
			return err
		}
	}
	current, err := s.dir.GroupsForUser(ctx, userID)
	if err != nil {
		return err
	}
	if leaders.ID != "" && slices.ContainsFunc(current, func(g Group) bool { return g.ID == leaders.ID }) {
		// Already a Leader of this kind: nothing changes, nothing is told.
		return nil
	}
	if delegated != nil && len(current)+1 > s.delegation.options.GroupLimit {
		return refuse(http.StatusConflict, CodeGroupLimit)
	}
	if leaders.ID == "" {
		// The Team's first Leader of this kind: its Leader group, right
		// under it, with nothing under it (teamcode: a leader subgroup's
		// name at its one place).
		scope, err := s.names.Create(team.Path, kind)
		if err != nil {
			return err
		}
		if err := s.nameFree(ctx, records, scope, team.Path+"/"+kind, kind, ""); err != nil {
			return err
		}
		if leaders, err = s.dir.CreateSubgroup(ctx, team.ID, kind, nil); err != nil {
			return err
		}
		// The managed trees count the Team's Leaders from its Leader groups.
		s.forgetAllGroups()
	}
	defer s.forgetGroups(userID)
	notify := s.membership != nil && s.membership.Notifies(leaders)
	if err := s.leaderWrite(ctx, teamaudit.ActionLeaderAdd, team, userID, kind, func(dir Directory) error {
		return dir.AddMember(ctx, leaders.ID, userID)
	}); err != nil {
		return err
	}
	if notify {
		s.membership.MembershipChanged(ctx, MembershipChange{UserID: userID, Group: leaders, Added: true, ActorID: p.ID})
	}
	return nil
}

// RemoveTeamLeader takes userID out of every Leader group of the Team
// groupRef they are directly in (a leader.remove row each): Privileged, or
// a Leader who manages the Team strictly. Nobody leaves the Leaders of
// their own Team this way. LastLeader says the Team has none left.
func (s *service) RemoveTeamLeader(ctx context.Context, p authz.Principal, groupRef string, userID uuid.UUID) (LeaderRemoval, error) {
	var out LeaderRemoval
	if s.allow(p, authz.TypeGroup, authz.Update) == nil {
		if _, err := uuid.Parse(groupRef); err != nil {
			return out, ErrNotFound
		}
		err := s.codes.Locked(ctx, func(ctx context.Context, _ teamcode.Records) error {
			team, _, err := s.dir.GroupNode(ctx, groupRef)
			if err != nil {
				return err
			}
			out, err = s.removeTeamLeader(ctx, p, team, userID, nil)
			return err
		})
		return out, err
	}
	err := s.delegatedWrite(ctx, p, teamaudit.ActionLeaderRemove, groupRef, userID, authz.Policy.ManagesStrict,
		func(ctx context.Context, m authz.TeamManager, target authz.ManagedGroup, team Group) error {
			d := s.delegation.policy.ManagesStrict(m, target)
			if !d.Allowed {
				s.deny(ctx, teamaudit.ActionLeaderRemove, target.Group, userID, d, d.Refusal)
				return decisionError(d)
			}
			var err error
			out, err = s.removeTeamLeader(asLeader(ctx, d), p, team, userID, &delegatedLeaderWrite{d: d, target: target})
			return err
		})
	return out, err
}

func (s *service) removeTeamLeader(ctx context.Context, p authz.Principal, team Group, userID uuid.UUID, delegated *delegatedLeaderWrite) (LeaderRemoval, error) {
	if !s.isTeam(team.Path) {
		return LeaderRemoval{}, ErrInvalid
	}
	groups, err := s.leaderGroups(ctx, team)
	if err != nil {
		return LeaderRemoval{}, err
	}
	var in []Group
	left := 0
	for _, g := range groups {
		people, err := s.dir.Members(ctx, g.ID)
		if err != nil {
			return LeaderRemoval{}, err
		}
		for _, person := range people {
			if person.ID == userID {
				in = append(in, g)
			} else {
				left++
			}
		}
	}
	if len(in) == 0 {
		return LeaderRemoval{}, ErrNotFound
	}
	for _, g := range in {
		if delegated != nil {
			protected, err := s.delegation.protected(ctx, []string{g.ID}, true)
			if err != nil {
				return LeaderRemoval{}, err
			}
			if protected {
				s.deny(ctx, teamaudit.ActionLeaderRemove, delegated.target.Group, userID, delegated.d, authz.RefusalProtectedGroup)
				return LeaderRemoval{}, refusedWith(http.StatusForbidden, authz.RefusalProtectedGroup)
			}
		} else if err := s.notKeycloakAdminGroup(ctx, g.ID); err != nil {
			return LeaderRemoval{}, err
		}
	}
	defer s.forgetGroups(userID)
	for _, g := range in {
		if err := s.leaderWrite(ctx, teamaudit.ActionLeaderRemove, team, userID, g.Name, func(dir Directory) error {
			return dir.RemoveMember(ctx, g.ID, userID)
		}); err != nil {
			return LeaderRemoval{}, err
		}
		if s.membership != nil && s.membership.Notifies(g) {
			s.membership.MembershipChanged(ctx, MembershipChange{UserID: userID, Group: g, Added: false, ActorID: p.ID})
		}
	}
	return LeaderRemoval{LastLeader: left == 0}, nil
}

// leaderWrite records a Leader write as action on the Team (its id and
// path, so the Team's history shows it) with the kind, around do on the
// directory beneath the audit (no second member.add or member.remove row).
// Without an audited directory it only does it.
func (s *service) leaderWrite(ctx context.Context, action teamaudit.Action, team Group, userID uuid.UUID, kind string, do func(Directory) error) error {
	audited, ok := s.dir.(*auditedDirectory)
	if !ok {
		return do(s.dir)
	}
	state := &teamaudit.State{ID: team.ID, Path: team.Path, Kind: kind}
	r := teamaudit.Record{Action: action, GroupID: team.ID, GroupPath: team.Path, TargetUserID: userID}
	if action == teamaudit.ActionLeaderAdd {
		r.After = state
	} else {
		r.Before = state
	}
	return audited.write(ctx, r, func() (*teamaudit.State, error) { return nil, do(audited.Directory) })
}

// leaderGroupsByLeadersRoute refuses a leader subgroup's name on the
// Privileged Group routes (POST /v1/groups, a rename through PATCH) once
// team delegation is on: a Leader group is then made only by naming a
// Leader (AddTeamLeader), Privileged's too (ticket 06, the exception of
// ticket 01 ends). Off, they make one as before.
func (s *service) leaderGroupsByLeadersRoute(name string) error {
	if !s.delegation.On() || !slices.Contains(authz.DefaultPolicy().LeaderSubgroups, name) {
		return nil
	}
	return &teamcode.Error{Code: teamcode.ReservedName,
		Detail: "A Leader group (" + name + ") is made by naming a Leader of the Team: POST /v1/groups/{id}/leaders."}
}

// nothingUnderLeaderGroups refuses, once team delegation is on, a Group
// under a Leader group (SPEC §4.1: nothing sits under one), Privileged's
// POST /v1/groups too. Off, as before.
func (s *service) nothingUnderLeaderGroups(parentPath string) error {
	if !s.delegation.On() {
		return nil
	}
	if _, role, ok := authz.TeamOf(parentPath, s.privilegedRoots, authz.DefaultPolicy().LeaderSubgroups); ok && role != "" {
		return ErrInvalid
	}
	return nil
}
