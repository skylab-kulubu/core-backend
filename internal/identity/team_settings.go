package identity

import (
	"context"
	"maps"
	"strconv"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/teamaudit"
)

// TeamSettings are a Team's own settings, kept as its Group's attributes
// (docs/team-delegation.md "Team settings").
type TeamSettings struct {
	// MembersManageCompetitors is whether the Team's members add, edit and
	// remove its Events' competitors (authz.TeamAttrMembersManageCompetitors;
	// Yusuf, 2026-10-25). Unset it is true, today's rule; false keeps them to
	// the Leaders.
	MembersManageCompetitors bool `json:"membersManageCompetitors"`
	// CompetitionManagers is who manages the Team's Events' Competitions
	// ("Yarışmaları kim yönetir", authz.TeamAttrCompetitionManage; Yusuf,
	// 2026-10-12 and 2026-10-31): CompetitionManagersLeaders, the default,
	// or CompetitionManagersMembers.
	CompetitionManagers string `json:"competitionManagers"`
}

// The values of TeamSettings.CompetitionManagers.
const (
	CompetitionManagersLeaders = "leaders"
	CompetitionManagersMembers = "members"
)

// TeamSettingsChange is what a settings write changes: a nil field keeps
// the stored value.
type TeamSettingsChange struct {
	MembersManageCompetitors *bool
	CompetitionManagers      *string
}

// TeamSettingsOf are the settings g's attributes hold.
func TeamSettingsOf(g Group) TeamSettings {
	managers := CompetitionManagersLeaders
	if authz.CompetitionManageValue(g.Attributes[authz.TeamAttrCompetitionManage]) {
		managers = CompetitionManagersMembers
	}
	return TeamSettings{
		MembersManageCompetitors: authz.MembersManageCompetitorsValue(g.Attributes[authz.TeamAttrMembersManageCompetitors]),
		CompetitionManagers:      managers,
	}
}

// valid reports whether the change sets something, and only known values.
func (c TeamSettingsChange) valid() bool {
	if c.MembersManageCompetitors == nil && c.CompetitionManagers == nil {
		return false
	}
	return c.CompetitionManagers == nil ||
		*c.CompetitionManagers == CompetitionManagersLeaders || *c.CompetitionManagers == CompetitionManagersMembers
}

// writeTo sets the changed settings in attrs; the other attributes stay.
func (c TeamSettingsChange) writeTo(attrs map[string]string) {
	if c.MembersManageCompetitors != nil {
		attrs[authz.TeamAttrMembersManageCompetitors] = strconv.FormatBool(*c.MembersManageCompetitors)
	}
	if c.CompetitionManagers != nil {
		attrs[authz.TeamAttrCompetitionManage] = strconv.FormatBool(*c.CompetitionManagers == CompetitionManagersMembers)
	}
}

// SetTeamSettings writes a Team's settings: Privileged on any Team, a
// Leader (team delegation on) on a Team they manage (authz.Policy.Manages:
// their own and those below it), never on a protected Group; the change is
// a group.attributes row of the Team audit record, a refusal a denied row.
// The competition setting widens what the members may do, and still is the
// manager's (ADR-0065's exception for it, Yusuf 2026-10-31). Only a Team
// takes settings (not a Leader group, an area or a root), and a change
// sets something known: 400 (ErrInvalid) otherwise.
// The kept Owner team tree is forgotten, so the decisions follow at once in
// this process and, told through Postgres, at the next decision in the
// others (within a minute if they did not hear it).
func (s *service) SetTeamSettings(ctx context.Context, p authz.Principal, groupRef string, change TeamSettingsChange) (TeamSettings, error) {
	if !change.valid() {
		return TeamSettings{}, ErrInvalid
	}
	apply := func(ctx context.Context, g Group) (TeamSettings, error) {
		if !s.isTeam(g.Path) {
			return TeamSettings{}, ErrInvalid
		}
		defer s.forgetTeamTree()
		attrs := maps.Clone(g.Attributes)
		if attrs == nil {
			attrs = map[string]string{}
		}
		change.writeTo(attrs)
		g.Attributes = attrs
		updated, err := s.dir.UpdateGroup(ctx, g)
		if err != nil {
			return TeamSettings{}, err
		}
		return TeamSettingsOf(updated), nil
	}
	if err := s.allow(p, authz.TypeGroup, authz.Update); err == nil {
		// As a Privileged attribute write (UpdateGroup): no lock.
		g, err := s.dir.GetGroup(ctx, groupRef)
		if err != nil {
			return TeamSettings{}, err
		}
		return apply(ctx, g)
	}
	var out TeamSettings
	err := s.delegatedWrite(ctx, p, teamaudit.ActionGroupAttributes, groupRef, uuid.Nil, authz.Policy.Manages,
		func(ctx context.Context, m authz.TeamManager, target authz.ManagedGroup, g Group) error {
			d := s.delegation.policy.Manages(m, target)
			if !d.Allowed {
				s.deny(ctx, teamaudit.ActionGroupAttributes, target.Group, uuid.Nil, d, d.Refusal)
				return decisionError(d)
			}
			var err error
			out, err = apply(asLeader(ctx, d), g)
			return err
		})
	return out, err
}

// isTeam reports whether path is a Team's own Group (authz.TeamOf).
func (s *service) isTeam(path string) bool {
	team, role, ok := authz.TeamOf(path, s.privilegedRoots, authz.DefaultPolicy().LeaderSubgroups)
	return ok && role == "" && team == path
}

// forgetTeamTree forgets the kept tree here and tells the other core
// processes to forget theirs.
func (s *service) forgetTeamTree() {
	if s.teamTree != nil {
		s.teamTree.ForgetAll()
	}
	s.forgetAllElsewhere()
}
