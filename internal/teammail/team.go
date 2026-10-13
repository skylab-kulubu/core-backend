// Package teammail sends the membership mail: a person added to a Group, or
// removed from one, through core's membership routes is told so by mail. A
// team's change gets club.team-membership, a Privileged Group's (YK, DK,
// ADMIN) club.role-membership and any other Group's club.group-membership
// (KindOf). The change is queued in the database with the membership write
// and sent by a worker, so a SkyMail that is down delays the mail and never
// the write. See docs/team-membership-mail.md.
package teammail

import (
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/teamcode"
)

// roleValues are the template's Role variable: what the membership is in
// the team. The template words its text by it (a member gets no
// management rights; only leaders and coordinators do).
var roleValues = map[string]string{
	"":               "member",
	"LIDERLER":       "leader",
	"KOORDINATORLER": "coordinator",
}

// Team is the team a membership Group belongs to.
type Team struct {
	// Path is the team's own Group, e.g. /UYELER/ARGE/WEBLAB.
	Path string
	// Role is the leader subgroup the membership is in (LIDERLER,
	// KOORDINATORLER), empty for the team itself.
	Role string
}

// TeamOf reports whether a membership of the Group at path is a team
// membership, and of which team (teamcode.TeamOf): a Group at least three
// deep under /UYELER (/UYELER/<area>/<team>, or a sub-team below one), or
// one of the policy's leader subgroups directly under such a team. A
// Privileged root of privilegedRoots and everything under it is not a team;
// nor are the club root, the areas or Groups outside /UYELER. A Group named
// ADMIN, YK or DK elsewhere is a team like any other.
func TeamOf(path string, privilegedRoots []string) (Team, bool) {
	team, role, ok := teamcode.TeamOf(path, privilegedRoots, authz.DefaultPolicy().LeaderSubgroups)
	if !ok {
		return Team{}, false
	}
	return Team{Path: team, Role: role}, true
}

// teamName is how the team mail names the team of group: groupName, for the
// team and its leader subgroups alike (the role travels in the Role
// variable).
func teamName(group identity.Group) string { return groupName(group) }
