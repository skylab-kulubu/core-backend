// Package teammail sends the team membership mail (SkyMail template
// club.team-membership): a person added to a team, or removed from one,
// through core's membership routes is told so by mail. The change is queued
// in the database with the membership write and sent by a worker, so a
// SkyMail that is down delays the mail and never the write. See
// docs/team-membership-mail.md.
package teammail

import (
	"slices"
	"strings"

	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/identity"
)

// membersRoot is the first segment of every team's path: the club's members
// tree (CONTEXT.md "Member").
const membersRoot = "UYELER"

// minTeamDepth is the depth of a team's own Group: /UYELER/<area>/<team>.
// /UYELER itself is the club and /UYELER/<area> an area (ARGE,
// ORGANIZASYON, …) or a technical Group, none of them a team.
const minTeamDepth = 3

// roleNames are the leader subgroups as the mail names them.
var roleNames = map[string]string{
	"LIDERLER":       "Liderler",
	"KOORDINATORLER": "Koordinatörler",
}

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
// membership, and of which team. A team is a Group at least three deep under
// /UYELER (/UYELER/<area>/<team>, or a sub-team below one), or one of the
// policy's leader subgroups directly under such a team. Privileged Groups
// (ADMIN, YK, DK) and everything under them are not teams, wherever they
// sit; nor are the club root, the areas or Groups outside /UYELER.
func TeamOf(path string) (Team, bool) {
	if !strings.HasPrefix(path, "/") {
		return Team{}, false
	}
	segments := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if segments[0] != membersRoot {
		return Team{}, false
	}
	policy := authz.DefaultPolicy()
	for i, segment := range segments {
		if segment == "" || slices.Contains(policy.PrivilegedGroups, segment) {
			return Team{}, false
		}
		// A leader subgroup ends a path: nothing sits under one.
		if slices.Contains(policy.LeaderSubgroups, segment) && i != len(segments)-1 {
			return Team{}, false
		}
	}
	team := Team{Path: path}
	if last := segments[len(segments)-1]; slices.Contains(policy.LeaderSubgroups, last) {
		segments = segments[:len(segments)-1]
		team = Team{Path: "/" + strings.Join(segments, "/"), Role: last}
	}
	if len(segments) < minTeamDepth {
		return Team{}, false
	}
	return team, true
}

// teamName is how the mail names the team of group (Yusuf, 2026-10-09):
// its code (the Group's name) and its Turkish display name
// (display_name_tr, as the public team list shows it), "WEBLAB · Web
// Geliştirme"; for a leader subgroup the code and the role, "WEBLAB ·
// Liderler". Without a Turkish name, or one that is the code itself, the
// code alone.
func teamName(group identity.Group, role string) string {
	code := strings.TrimSpace(group.Name)
	if code == "" {
		code = group.Path[strings.LastIndex(group.Path, "/")+1:]
	}
	if label, ok := roleNames[role]; ok {
		return code + " · " + label
	}
	if turkish := strings.TrimSpace(group.Attributes["display_name_tr"]); turkish != "" && turkish != code {
		return code + " · " + turkish
	}
	return code
}
