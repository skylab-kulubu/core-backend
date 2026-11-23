// Package organizer names who organizes an Event wherever it is shown
// outside the panel: the site, the Ticket page and its PDF, the mails and
// the certificates (Yusuf, 2026-11-23). An Event of the club's boards (YK,
// DK) or of ADMIN, or of no Owner team, is organized by the club itself and
// says the club's name; any other Owner team says its code and Turkish name,
// "WEBLAB · Web Geliştirme", as core's mails name a Team (its code alone
// while the name is unknown). The Owner team (ownerTeam) stays what it is:
// permissions and the panel read that, and the panel still says YK.
package organizer

import "strings"

// ClubName is how core names the club, everywhere (Yusuf, 2026-11-22): the
// Ticket PDF's page head and the organizer of a club Event.
const ClubName = "SKY LAB: Bilgisayar Bilimleri Kulübü"

// clubTeams are the Owner teams that organize as the club: the last
// segments of the Privileged roots (authz.DefaultPrivilegedRoots), which
// teamcode also keeps from ever naming a Team.
var clubTeams = []string{"YK", "DK", "ADMIN"}

// Club reports whether an Event of ownerTeam is the club's own: no Owner
// team, or one of clubTeams (any case, blanks around ignored).
func Club(ownerTeam string) bool {
	team := strings.TrimSpace(ownerTeam)
	if team == "" {
		return true
	}
	for _, club := range clubTeams {
		if strings.EqualFold(team, club) {
			return true
		}
	}
	return false
}

// Name is the organizer of an Event of ownerTeam as shown outside the
// panel: ClubName for the club's own (Club), else the Owner team's code and
// its Turkish name (UseNames) when it has one other than the code.
func Name(ownerTeam string) string {
	if Club(ownerTeam) {
		return ClubName
	}
	code := strings.TrimSpace(ownerTeam)
	if n := names.Load(); n != nil {
		if turkish := n.Turkish(code); turkish != "" && turkish != code {
			return code + " · " + turkish
		}
	}
	return code
}
