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
}

// TeamSettingsOf are the settings g's attributes hold.
func TeamSettingsOf(g Group) TeamSettings {
	v, err := strconv.ParseBool(g.Attributes[authz.TeamAttrMembersManageCompetitors])
	return TeamSettings{MembersManageCompetitors: err != nil || v}
}

// SetTeamSettings writes a Team's settings: Privileged on any Team, a
// Leader (team delegation on) on a Team they manage (authz.Policy.Manages:
// their own and those below it), never on a protected Group; the change is
// a group.attributes row of the Team audit record, a refusal a denied row.
// Only a Team takes settings (not a Leader group, an area or a root): 400.
// The kept Owner team tree is forgotten, so the decisions follow at once in
// this process (another core within a minute).
func (s *service) SetTeamSettings(ctx context.Context, p authz.Principal, groupRef string, settings TeamSettings) (TeamSettings, error) {
	apply := func(ctx context.Context, g Group) (TeamSettings, error) {
		if !s.isTeam(g.Path) {
			return TeamSettings{}, ErrInvalid
		}
		defer s.forgetTeamTree()
		attrs := maps.Clone(g.Attributes)
		if attrs == nil {
			attrs = map[string]string{}
		}
		attrs[authz.TeamAttrMembersManageCompetitors] = strconv.FormatBool(settings.MembersManageCompetitors)
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

func (s *service) forgetTeamTree() {
	if s.teamTree != nil {
		s.teamTree.ForgetAll()
	}
}
