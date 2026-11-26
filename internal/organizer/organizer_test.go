package organizer

import "testing"

func TestName(t *testing.T) {
	for _, tc := range []struct{ team, want string }{
		{"YK", ClubName},
		{"ADMIN", ClubName},
		{"DK", ClubName},
		{"yk", ClubName},
		{" YK ", ClubName},
		{"", ClubName},
		{"   ", ClubName},
		{"WEBLAB", "WEBLAB"},
		{" GECEKODU ", "GECEKODU"},
		{"YKX", "YKX"},
	} {
		if got := Name(tc.team); got != tc.want {
			t.Errorf("Name(%q) = %q, want %q", tc.team, got, tc.want)
		}
	}
}

func TestClub(t *testing.T) {
	for team, want := range map[string]bool{"YK": true, "ADMIN": true, "DK": true, "": true, "WEBLAB": false, "UYELER": false} {
		if got := Club(team); got != want {
			t.Errorf("Club(%q) = %v, want %v", team, got, want)
		}
	}
}

func TestClubName(t *testing.T) {
	if ClubName != "SKY LAB: Bilgisayar Bilimleri Kulübü" {
		t.Fatalf("ClubName = %q", ClubName)
	}
}

func TestCoNames(t *testing.T) {
	for _, tc := range []struct {
		teams []string
		want  string
	}{
		{nil, ""},
		{[]string{"WEBLAB"}, "WEBLAB"},
		{[]string{"WEBLAB", "ARTLAB"}, "WEBLAB, ARTLAB"},
		{[]string{"", "DK", "GECEKODU"}, "GECEKODU"},
	} {
		if got := CoNames(tc.teams); got != tc.want {
			t.Errorf("CoNames(%q) = %q, want %q", tc.teams, got, tc.want)
		}
	}
}
