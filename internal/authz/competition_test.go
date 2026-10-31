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

// membersManage is the Teams whose setting "Yarışmaları kim yönetir" says
// all members (TeamAttrCompetitionManage=true); the competitor setting
// keeps its default.
type membersManage map[string]bool

func (membersManage) MembersManageCompetitors(string) bool { return true }

func (m membersManage) TeamCompetitionManage(team string) bool { return m[team] }

// With WEBLAB's setting on (Yusuf, 2026-10-12 and 2026-10-31; KARAR §2.4,
// A4): WEBLAB's members, its Sub-teams' members too, manage its
// Competitions but do not archive or restore them; a member of the Team
// above, a sibling Team's member or Leader, and anybody on a Team whose
// setting is off still manage nothing. Leaders and Privileged keep
// everything.
func TestAuthorizer_CompetitionMemberSetting(t *testing.T) {
	t.Parallel()
	policy := DefaultPolicy()
	policy.TeamDelegation = true
	auth := NewAuthorizer(policy, WithTeamTree(realmTree()), WithTeamSettings(membersManage{"WEBLAB": true}))
	weblab := Resource{Type: TypeCompetition, OwnerTeam: "WEBLAB"}
	mobil := Resource{Type: TypeCompetition, OwnerTeam: "MOBIL"}
	gamelab := Resource{Type: TypeCompetition, OwnerTeam: "GAMELAB"}
	noOwner := Resource{Type: TypeCompetition}
	member := Principal{ID: "u1", Groups: []string{"/UYELER/ARGE/WEBLAB"}}
	subMember := Principal{ID: "u2", Groups: []string{"/UYELER/ARGE/WEBLAB/MOBIL"}}
	leader := Principal{ID: "u3", Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}
	gamelabMember := Principal{ID: "u4", Groups: []string{"/UYELER/ARGE/GAMELAB"}}
	gamelabLeader := Principal{ID: "u5", Groups: []string{"/UYELER/ARGE/GAMELAB/LIDERLER"}}
	yk := Principal{ID: "u6", Groups: []string{"/UYELER/YK"}}
	dk := Principal{ID: "u7", Groups: []string{"/DK"}}

	type want struct {
		read, write, archive bool
		basis                CompetitionBasis
	}
	cases := []struct {
		name string
		p    Principal
		r    Resource
		want want
	}{
		{"a member, setting on", member, weblab, want{true, true, false, BasisMemberSetting}},
		{"a Sub-team's member, setting on (A4)", subMember, weblab, want{true, true, false, BasisMemberSetting}},
		{"the Team's Leader", leader, weblab, want{true, true, true, BasisLeader}},
		{"a Leader above, on the Sub-team", leader, mobil, want{true, true, true, BasisLeader}},
		{"a member above, on the Sub-team whose setting is off", member, mobil, want{false, false, false, ""}},
		{"a sibling Team's member", gamelabMember, weblab, want{false, false, false, ""}},
		{"a sibling Team's Leader", gamelabLeader, weblab, want{false, false, false, ""}},
		{"a member, setting off", gamelabMember, gamelab, want{false, false, false, ""}},
		{"a Leader, setting off", gamelabLeader, gamelab, want{true, true, true, BasisLeader}},
		{"YK", yk, weblab, want{true, true, true, BasisPrivileged}},
		{"DK reads and writes nothing", dk, weblab, want{true, false, false, ""}},
		{"a member, an Event without Owner team", member, noOwner, want{false, false, false, ""}},
		{"a Leader, an Event without Owner team", leader, noOwner, want{false, false, false, ""}},
		{"YK, an Event without Owner team", yk, noOwner, want{true, true, true, BasisPrivileged}},
		{"anonymous", Principal{}, weblab, want{false, false, false, ""}},
	}
	for _, tt := range cases {
		got := want{
			read:    auth.Allow(tt.p, tt.r, Read),
			write:   auth.Allow(tt.p, tt.r, Update),
			archive: auth.Allow(tt.p, tt.r, Delete),
			basis:   auth.CompetitionBasis(tt.p, tt.r),
		}
		if create := auth.Allow(tt.p, tt.r, Create); create != got.write {
			t.Errorf("%s: Create %v, Update %v", tt.name, create, got.write)
		}
		if got != tt.want {
			t.Errorf("%s: got %+v, want %+v", tt.name, got, tt.want)
		}
	}
	if b := auth.CompetitionBasis(leader, Resource{Type: TypeCompetitor, OwnerTeam: "WEBLAB"}); b != "" {
		t.Errorf("CompetitionBasis of a competitor: %q", b)
	}

	// The capabilities follow the decisions: the member gets the
	// Competition tab but not the archive.
	caps := auth.Capabilities(member)
	if can := scopeFor(caps, "WEBLAB"); !can["competition.manage"] || can["competition.archive"] {
		t.Errorf("member, setting on: %v", can)
	}
	if can := scopeFor(auth.Capabilities(leader), "WEBLAB"); !can["competition.manage"] || !can["competition.archive"] {
		t.Errorf("Leader: %v", can)
	}
	if can := scopeFor(auth.Capabilities(gamelabMember), "GAMELAB"); can["competition.manage"] || can["competition.archive"] {
		t.Errorf("member, setting off: %v", can)
	}
}

// Without the Team settings (WithTeamSettings unset) every Team has the
// default: the members manage nothing.
func TestAuthorizer_CompetitionMemberSettingDefaultsOff(t *testing.T) {
	t.Parallel()
	auth := NewAuthorizer(DefaultPolicy())
	member := Principal{ID: "u1", Groups: []string{"/UYELER/ARGE/WEBLAB"}}
	weblab := Resource{Type: TypeCompetition, OwnerTeam: "WEBLAB"}
	if auth.Allow(member, weblab, Read) || auth.Allow(member, weblab, Update) || auth.CompetitionBasis(member, weblab) != "" {
		t.Error("a member manages a competition without the setting")
	}
}
