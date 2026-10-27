package membershipterm

import (
	"slices"
	"testing"
	"time"
)

func TestScopeOfKeepsADMINAndOrdinaryTeamsOut(t *testing.T) {
	t.Parallel()
	r := DefaultRules()
	for path, want := range map[string]Scope{
		"/YK":                                   ScopePrivileged,
		"/UYELER/YK":                            ScopePrivileged,
		"/UYELER/YK/BASKAN":                     ScopePrivileged,
		"/UYELER/DK":                            ScopePrivileged,
		"/UYELER/ARGE/WEBLAB/LIDERLER":          ScopeLeader,
		"/UYELER/ARGE/WEBLAB/KOORDINATORLER":    ScopeLeader,
		"/UYELER/ARGE/WEBLAB/AI/LIDERLER":       ScopeLeader,
		"/ADMIN":                                "",
		"/UYELER/ADMIN":                         "",
		"/UYELER/ADMIN/KONSOL":                  "",
		"/UYELER":                               "",
		"/UYELER/ARGE":                          "",
		"/UYELER/ARGE/LIDERLER":                 "", // an area's, no Team's
		"/UYELER/ARGE/WEBLAB":                   "",
		"/UYELER/ARGE/WEBLAB/AI":                "",
		"/UYELER/ARGE/WEBLAB/LIDERLER-ESKI":     "",
		"/UYELER/ARGE/WEBLAB/ADMIN":             "",
		"/YKX":                                  "",
		"/UYELER/ARGE/WEBLAB/LIDERLER/ALT":      "",
		"/UYELER/ORGANIZASYON/SKYDAYS/LIDERLER": ScopeLeader,
	} {
		got, ok := r.ScopeOf(path)
		if got != want || ok != (want != "") {
			t.Errorf("%s: %q %v, want %q", path, got, ok, want)
		}
	}
}

func TestOverseersAreADMINAndTheRootOrTheTeamAboveAndYK(t *testing.T) {
	t.Parallel()
	r := DefaultRules()
	for path, want := range map[string][]string{
		"/UYELER/YK/BASKAN":               {"/ADMIN", "/UYELER/ADMIN", "/UYELER/YK"},
		"/DK":                             {"/ADMIN", "/UYELER/ADMIN", "/DK"},
		"/UYELER/ARGE/WEBLAB/LIDERLER":    {"/YK", "/UYELER/YK"},
		"/UYELER/ARGE/WEBLAB/AI/LIDERLER": {"/UYELER/ARGE/WEBLAB/LIDERLER", "/UYELER/ARGE/WEBLAB/KOORDINATORLER", "/YK", "/UYELER/YK"},
	} {
		if got := r.overseers(path); !slices.Equal(got, want) {
			t.Errorf("%s: %v, want %v", path, got, want)
		}
	}
	for path, want := range map[string]string{
		"/UYELER/YK": RoleBoard, "/DK/X": RoleAudit, "/UYELER/ARGE/WEBLAB/LIDERLER": RoleTeamLeaders,
	} {
		if got := r.Role(path); got != want {
			t.Errorf("role %s: %s", path, got)
		}
	}
}

func TestATermEndsAtTheEndOfItsIstanbulDay(t *testing.T) {
	t.Parallel()
	// A season ending on 31 May 2027, stored at midnight UTC.
	end := EndOfDay(time.Date(2027, 5, 31, 0, 0, 0, 0, time.UTC))
	if want := time.Date(2027, 5, 31, 20, 59, 59, 0, time.UTC); !end.Equal(want) {
		t.Fatalf("end %s, want %s", end, want)
	}
	parsed, err := ParseDay("2027-05-31")
	if err != nil || !parsed.Equal(end) {
		t.Fatalf("parsed %s %v", parsed, err)
	}
	for _, bad := range []string{"", "31.05.2027", "2027-05-31T00:00:00Z", "2027-02-30"} {
		if _, err := ParseDay(bad); err != ErrInvalid {
			t.Errorf("%q: %v", bad, err)
		}
	}
	term := Term{EndsAt: &end}
	for at, want := range map[time.Time]Status{
		end.Add(-WarnEarly - time.Second): StatusActive,
		end.Add(-WarnEarly):               StatusExpiring,
		end.Add(-time.Second):             StatusExpiring,
		end:                               StatusExpired,
	} {
		if got := term.Status(at); got != want {
			t.Errorf("at %s: %s, want %s", at, got, want)
		}
	}
	if (Term{}).Status(end) != StatusActive {
		t.Fatal("an undated term expired")
	}
}

func TestModeAndWeeklyFromEnv(t *testing.T) {
	t.Parallel()
	env := func(v string) func(string) string { return func(string) string { return v } }
	for raw, want := range map[string]Mode{"": ModeOff, "off": ModeOff, " dry-run ": ModeDryRun, "apply": ModeApply} {
		if got, err := ModeFromEnv(env(raw)); err != nil || got != want {
			t.Errorf("%q: %q %v", raw, got, err)
		}
	}
	if _, err := ModeFromEnv(env("on")); err == nil {
		t.Fatal("on is a mode")
	}
	for raw, want := range map[string]bool{"": true, "on": true, "off": false} {
		if got, err := WeeklySummaryFromEnv(env(raw)); err != nil || got != want {
			t.Errorf("weekly %q: %v %v", raw, got, err)
		}
	}
	if _, err := WeeklySummaryFromEnv(env("false")); err == nil {
		t.Fatal("false is a weekly value")
	}
}
