// Package teammail sends the team membership mail (SkyMail template
// club.team-membership): a person added to a team, or removed from one,
// through core's membership routes is told so by mail. The change is queued
// in the database with the membership write and sent by a worker, so a
// SkyMail that is down delays the mail and never the write. See
// docs/team-membership-mail.md.
package teammail

import (
	"strings"

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

// teamName is how the mail names the team of group (Yusuf, 2026-10-09):
// its code (the team Group's name) and its Turkish display name
// (display_name_tr, as the public team list shows it), "WEBLAB · Web
// Geliştirme", for the team and its leader subgroups alike: the role
// travels in the Role variable. Without a Turkish name, or one that is the
// code itself, the code alone.
func teamName(group identity.Group) string {
	code := strings.TrimSpace(group.Name)
	if code == "" {
		code = group.Path[strings.LastIndex(group.Path, "/")+1:]
	}
	if turkish := strings.TrimSpace(group.Attributes["display_name_tr"]); turkish != "" && turkish != code {
		return code + " · " + turkish
	}
	return code
}
