package teammail

import (
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/identity"
)

func TestTeamOf(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		path     string
		team     string
		role     string
		isTeamOK bool
	}{
		{path: "/UYELER/ARGE/WEBLAB", team: "/UYELER/ARGE/WEBLAB", isTeamOK: true},
		{path: "/UYELER/ARGE/WEBLAB/LIDERLER", team: "/UYELER/ARGE/WEBLAB", role: "LIDERLER", isTeamOK: true},
		{path: "/UYELER/ORGANIZASYON/ARTLAB/KOORDINATORLER", team: "/UYELER/ORGANIZASYON/ARTLAB", role: "KOORDINATORLER", isTeamOK: true},
		{path: "/UYELER/ARGE/ALGOLAB/AGC", team: "/UYELER/ARGE/ALGOLAB/AGC", isTeamOK: true},
		{path: "/UYELER/ARGE/ALGOLAB/AGC/LIDERLER", team: "/UYELER/ARGE/ALGOLAB/AGC", role: "LIDERLER", isTeamOK: true},
		// Not a team: the club itself, an area, an area's own leaders.
		{path: "/UYELER"},
		{path: "/UYELER/ARGE"},
		{path: "/UYELER/ARGE/LIDERLER"},
		{path: "/UYELER/ESKI-EDITORLER"},
		// Privileged Groups and anything under them.
		{path: "/ADMIN"},
		{path: "/UYELER/ADMIN"},
		{path: "/UYELER/YK"},
		{path: "/UYELER/YK/BASKAN"},
		{path: "/UYELER/YK/BASKAN/YARDIMCI"},
		{path: "/UYELER/DK/LIDERLER"},
		// Outside the members' tree, and malformed paths.
		{path: "/SERVIS/ARGE/WEBLAB"},
		{path: ""},
		{path: "UYELER/ARGE/WEBLAB"},
		{path: "/UYELER/ARGE/WEBLAB/LIDERLER/X"},
		{path: "/UYELER//WEBLAB"},
	} {
		team, ok := TeamOf(tc.path)
		if ok != tc.isTeamOK || team.Path != tc.team || team.Role != tc.role {
			t.Errorf("TeamOf(%q) = %+v, %v; want %q %q %v", tc.path, team, ok, tc.team, tc.role, tc.isTeamOK)
		}
	}
}

func TestTeamName(t *testing.T) {
	t.Parallel()
	artlab := identity.Group{Name: "ARTLAB", Path: "/UYELER/ORGANIZASYON/ARTLAB"}
	named := identity.Group{Name: "WEBLAB", Path: "/UYELER/ARGE/WEBLAB", Attributes: map[string]string{"display_name_tr": "Web Lab"}}
	for _, tc := range []struct {
		group identity.Group
		role  string
		want  string
	}{
		{artlab, "", "ARTLAB"},
		{artlab, "LIDERLER", "ARTLAB · Liderler"},
		{artlab, "KOORDINATORLER", "ARTLAB · Koordinatörler"},
		{named, "", "Web Lab"},
		{identity.Group{Path: "/UYELER/ARGE/GAMELAB"}, "", "GAMELAB"},
	} {
		if got := teamName(tc.group, tc.role); got != tc.want {
			t.Errorf("teamName(%v, %q) = %q, want %q", tc.group, tc.role, got, tc.want)
		}
	}
}
