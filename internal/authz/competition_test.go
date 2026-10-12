package authz

import "testing"

// competitionAllowCases are the Competition decisions under today's Group
// paths (the groups mode): an organizer is a Leader of the Event's Owner
// team or Privileged; team membership alone manages nothing (spec §8, S6).
func competitionAllowCases() []allowCase {
	agc := Resource{Type: TypeCompetition, OwnerTeam: "AGC"}
	noOwner := Resource{Type: TypeCompetition}
	agcLeader := Principal{ID: "u1", Groups: []string{"/UYELER/ARGE/ALGOLAB/AGC/LIDERLER"}}
	var out []allowCase
	for _, a := range []Action{Read, Create, Update, Delete} {
		out = append(out,
			allowCase{name: "the Owner team's Leader manages its competition " + string(a), p: agcLeader, r: agc, a: a, want: true},
			allowCase{name: "the Owner team's coordinator manages its competition " + string(a),
				p: Principal{ID: "u1", Groups: []string{"/UYELER/ARGE/ALGOLAB/AGC/KOORDINATORLER"}}, r: agc, a: a, want: true},
			allowCase{name: "a plain member of the Owner team does not manage " + string(a),
				p: Principal{ID: "u1", Groups: []string{"/UYELER/ARGE/ALGOLAB/AGC"}}, r: agc, a: a, want: false},
			allowCase{name: "another team's Leader does not manage " + string(a),
				p: Principal{ID: "u1", Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}, r: agc, a: a, want: false},
			// Today's ownerLevels: a Leader of the team above is no Leader of
			// AGC. The team authority handover (K3, ADR-0065) changes this
			// in one place for every resource, competitions included.
			allowCase{name: "the parent team's Leader does not manage yet " + string(a),
				p: Principal{ID: "u1", Groups: []string{"/UYELER/ARGE/ALGOLAB/LIDERLER"}}, r: agc, a: a, want: false},
			allowCase{name: "YK manages any competition " + string(a),
				p: Principal{ID: "u1", Groups: []string{"/UYELER/YK"}}, r: agc, a: a, want: true},
			allowCase{name: "YK manages a competition of an Event without Owner team " + string(a),
				p: Principal{ID: "u1", Groups: []string{"/UYELER/YK"}}, r: noOwner, a: a, want: true},
			allowCase{name: "a Leader does not manage a competition of an Event without Owner team " + string(a),
				p: agcLeader, r: noOwner, a: a, want: false},
			allowCase{name: "anonymous does not manage " + string(a), r: agc, a: a, want: false},
		)
	}
	for _, a := range []Action{ReadMe, Assign, Issue, Validate} {
		out = append(out, allowCase{name: "no other competition action " + string(a),
			p: Principal{ID: "u1", Groups: []string{"/UYELER/YK"}}, r: agc, a: a, want: false})
	}
	return out
}

func TestAuthorizer_AllowCompetition(t *testing.T) {
	t.Parallel()
	for _, mode := range []RoleMode{RoleModeGroups, RoleModeBoth, RoleModeRoles} {
		auth := NewAuthorizer(policyIn(mode))
		for _, tt := range competitionAllowCases() {
			p := tt.p
			if mode == RoleModeRoles {
				p = asRoleHolder(p)
			}
			if got := auth.Allow(p, tt.r, tt.a); got != tt.want {
				t.Errorf("%s: %s: Allow() = %v, want %v", mode, tt.name, got, tt.want)
			}
		}
	}
}

// competition:manage replaces competitor:manage. While Keycloak renames it
// (e-skylab-keycloak), either name grants what both stand for: the new
// competitions and the old /v1/competitors routes.
func TestAuthorizer_CompetitionRoleAcceptsTheOldName(t *testing.T) {
	t.Parallel()
	competition := Resource{Type: TypeCompetition, OwnerTeam: "AGC"}
	competitor := Resource{Type: TypeCompetitor, OwnerTeam: "AGC"}
	for _, mode := range []RoleMode{RoleModeBoth, RoleModeRoles} {
		auth := NewAuthorizer(policyIn(mode))
		for _, role := range []string{RoleCompetitionManage, RoleCompetitorManage} {
			holder := Principal{ID: "u1", Roles: []string{role}}
			if !auth.Allow(holder, competition, Update) {
				t.Errorf("%s: %s holder refused a competition", mode, role)
			}
			if !auth.Allow(holder, competitor, Update) {
				t.Errorf("%s: %s holder refused a competitor", mode, role)
			}
			if !auth.Permitted(holder, RoleCompetitionManage) || !auth.Permitted(holder, RoleCompetitorManage) {
				t.Errorf("%s: %s holder not permitted both names", mode, role)
			}
			if auth.Allow(Principal{ID: "svc", ServiceAccount: true, Roles: []string{role}}, competition, Update) {
				t.Errorf("%s: service account with %s manages a competition", mode, role)
			}
		}
		if auth.Allow(Principal{ID: "u1", Roles: []string{RoleEventManage}}, competition, Update) {
			t.Errorf("%s: event:manage manages a competition", mode)
		}
	}
	// The groups mode reads the Privileged Groups alone, as for every role.
	groups := NewAuthorizer(policyIn(RoleModeGroups))
	if groups.Allow(Principal{ID: "u1", Roles: []string{RoleCompetitionManage}}, competition, Update) {
		t.Error("groups mode: a role holder outside the Privileged Groups manages a competition")
	}
}

func TestCapabilities_CompetitionManage(t *testing.T) {
	t.Parallel()
	auth := NewAuthorizer(DefaultPolicy())
	leader := auth.Capabilities(Principal{ID: "u1", Groups: []string{"/UYELER/ARGE/ALGOLAB/AGC/LIDERLER"}})
	if !scopeFor(leader, "AGC")["competition.manage"] {
		t.Error("AGC Leader: competition.manage false for AGC")
	}
	if leader.OtherTeams.Can["competition.manage"] || leader.NoOwnerTeam.Can["competition.manage"] {
		t.Errorf("AGC Leader: competition.manage outside AGC: %+v", leader)
	}
	member := auth.Capabilities(Principal{ID: "u2", Groups: []string{"/UYELER/ARGE/ALGOLAB/AGC"}})
	if scopeFor(member, "AGC")["competition.manage"] {
		t.Error("plain AGC member: competition.manage true")
	}
	yk := auth.Capabilities(Principal{ID: "u3", Groups: []string{"/UYELER/YK"}})
	if !yk.OtherTeams.Can["competition.manage"] || !yk.NoOwnerTeam.Can["competition.manage"] {
		t.Errorf("YK: %+v", yk)
	}
}
