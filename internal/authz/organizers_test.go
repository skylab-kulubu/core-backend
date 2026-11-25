package authz

import (
	"testing"
)

// The organizer rule (docs/co-organizers.md, SPEC §5.1, ADR 0092) as a
// table: each action of an Event, for each person. GAMELAB owns the Event
// (the _default rule: its Leaders); MOBIL is a "members" Co-organizer team,
// GECEKODU a "leaders" one; "person" is a Co-organizer person.
func TestOrganizers_Table(t *testing.T) {
	t.Parallel()
	auth := inheritingAuthorizer(realmTree())
	event := Resource{
		OwnerTeam:      "GAMELAB",
		CoOwnerTeams:   []CoTeam{{Code: "MOBIL", MembersManage: true}, {Code: "GECEKODU"}},
		CoOrganizerIDs: []string{"person"},
	}
	type check struct {
		name string
		typ  Type
		a    Action
	}
	checks := []check{
		{"Event.Update", TypeEvent, Update},
		{"Event.Delete", TypeEvent, Delete},
		{"Event.ReadFull", TypeEvent, ReadFull},
		{"Event.ManageOrganizers", TypeEvent, ManageOrganizers},
		{"Event.AssignDoorStaff", TypeEvent, Assign},
		{"Day.Delete", TypeEventDay, Delete},
		{"Ticket.Read", TypeTicket, Read},
		{"Ticket.Assign", TypeTicket, Assign},
		{"Ticket.Validate", TypeTicket, Validate},
		{"Certificate.Issue", TypeCertificate, Issue},
		{"Certificate.Revoke", TypeCertificate, Revoke},
		{"CertificateTemplate.Assign", TypeCertificateTemplate, Assign},
	}
	//                          Upd  Del  RFul Mng  Door DayD TRd  TAsg Val  Iss  Rev  Tpl
	all := []bool{true, true, true, true, true, true, true, true, true, true, true, true}
	none := make([]bool, len(checks))
	coLeader := []bool{true, false, true, false, false, true, true, true, true, true, true, true}
	coMember := []bool{true, false, true, false, false, false, true, false, false, false, false, false}
	people := []struct {
		name string
		p    Principal
		want []bool
	}{
		{"owner Leader", Principal{ID: "ol", Groups: []string{"/UYELER/ARGE/GAMELAB/LIDERLER"}}, all},
		{"owner member (_default)", Principal{ID: "om", Groups: []string{"/UYELER/ARGE/GAMELAB"}}, none},
		{"members co Leader", Principal{ID: "ml", Groups: []string{"/UYELER/ARGE/WEBLAB/MOBIL/LIDERLER"}}, coLeader},
		{"members co member", Principal{ID: "mm", Groups: []string{"/UYELER/ARGE/WEBLAB/MOBIL"}}, coMember},
		{"members co Sub-team member", Principal{ID: "ms", Groups: []string{"/UYELER/ARGE/WEBLAB/MOBIL/IOS"}}, coMember},
		{"leaders co Leader", Principal{ID: "ll", Groups: []string{"/UYELER/ORGANIZASYON/GECEKODU/KOORDINATORLER"}}, coLeader},
		{"leaders co member", Principal{ID: "lm", Groups: []string{"/UYELER/ORGANIZASYON/GECEKODU"}}, none},
		{"Leader above a co team", Principal{ID: "al", Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}, coLeader},
		{"co person", Principal{ID: "person"}, coLeader},
		{"unrelated", Principal{ID: "x", Groups: []string{"/UYELER/ARGE"}}, none},
		{"DK", Principal{ID: "dk", Groups: []string{"/UYELER/DK"}}, []bool{false, false, true, false, false, false, true, false, false, false, false, false}},
		{"YK", Principal{ID: "yk", Groups: []string{"/UYELER/YK"}}, all},
	}
	for _, person := range people {
		for i, c := range checks {
			r := event
			r.Type = c.typ
			if got := auth.Allow(person.p, r, c.a); got != person.want[i] {
				t.Errorf("%s: %s = %v, want %v", person.name, c.name, got, person.want[i])
			}
		}
	}
}

// With no Co-organizer, every decision is the Owner team's as before: the
// same answers as a Resource that never heard of Co-organizers.
func TestOrganizers_NoneDecidesAsBefore(t *testing.T) {
	t.Parallel()
	auth := inheritingAuthorizer(realmTree())
	for _, tt := range allowCases() {
		r := tt.r
		r.CoOwnerTeams, r.CoOrganizerIDs = []CoTeam{}, []string{}
		if got := auth.Allow(tt.p, r, tt.a); got != tt.want {
			t.Errorf("%s: %v, want %v", tt.name, got, tt.want)
		}
	}
}

// A member's membership counts in a "members" team alone: there the
// certificate roles make a Certificate manager, Team door scan lets them
// scan and team_competition_manage manage Competitions; in a "leaders" team
// none of these gives anything.
func TestOrganizers_MembershipCountsInAMembersTeamAlone(t *testing.T) {
	t.Parallel()
	auth := NewAuthorizer(DefaultPolicy(), WithTeamTree(realmTree()), WithTeamSettings(membersManage{"MOBIL": true, "GECEKODU": true}))
	r := Resource{
		OwnerTeam:         "GAMELAB",
		CoOwnerTeams:      []CoTeam{{Code: "MOBIL", MembersManage: true}, {Code: "GECEKODU"}},
		TeamDoorScanTeams: []string{"MOBIL", "GECEKODU"},
	}
	at := func(typ Type) Resource { out := r; out.Type = typ; return out }
	roles := []string{"certificate:issue", "certificate:revoke", "certificate:binding:manage"}
	for _, c := range []struct {
		name string
		p    Principal
		want bool
	}{
		{"members team's member", Principal{ID: "mm", Groups: []string{"/UYELER/ARGE/WEBLAB/MOBIL"}, Roles: roles}, true},
		{"leaders team's member", Principal{ID: "lm", Groups: []string{"/UYELER/ORGANIZASYON/GECEKODU"}, Roles: roles}, false},
	} {
		for name, got := range map[string]bool{
			"Certificate.Issue":          auth.Allow(c.p, at(TypeCertificate), Issue),
			"Certificate.Revoke":         auth.Allow(c.p, at(TypeCertificate), Revoke),
			"CertificateTemplate.Assign": auth.Allow(c.p, at(TypeCertificateTemplate), Assign),
			"Ticket.Validate":            auth.Allow(c.p, at(TypeTicket), Validate),
			"Competition.Update":         auth.Allow(c.p, at(TypeCompetition), Update),
			"Competitor.Create":          auth.Allow(c.p, at(TypeCompetitor), Create),
		} {
			if got != c.want {
				t.Errorf("%s: %s = %v, want %v", c.name, name, got, c.want)
			}
		}
		if got, want := auth.CompetitionBasis(c.p, at(TypeCompetition)), map[bool]CompetitionBasis{true: BasisMemberSetting, false: ""}[c.want]; got != want {
			t.Errorf("%s: basis %q, want %q", c.name, got, want)
		}
	}
	// Without its team's Team door scan, a "members" team's member does not
	// scan: the door stays a Leader's, door staff's or Team door scan's.
	noScan := at(TypeTicket)
	noScan.TeamDoorScanTeams = []string{"GECEKODU"}
	if auth.Allow(Principal{ID: "mm", Groups: []string{"/UYELER/ARGE/WEBLAB/MOBIL"}}, noScan, Validate) {
		t.Error("a members team's member scans without the team's Team door scan")
	}
}

// What stays the Owner team's: deleting and archiving the Event, its
// Co-organizer list and its door staff; on an Event without an Owner team,
// Privileged alone, whoever its Co-organizers are.
func TestOrganizers_ReservedToTheOwner(t *testing.T) {
	t.Parallel()
	auth := NewAuthorizer(DefaultPolicy())
	coLeader := Principal{ID: "cl", Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}
	for _, owner := range []string{"GAMELAB", ""} {
		r := Resource{Type: TypeEvent, OwnerTeam: owner, CoOwnerTeams: []CoTeam{{Code: "WEBLAB", MembersManage: true}}, CoOrganizerIDs: []string{"cl"}}
		for _, a := range []Action{Delete, ManageOrganizers, Assign} {
			if auth.Allow(coLeader, r, a) {
				t.Errorf("owner %q: a Co-organizer may %s", owner, a)
			}
		}
		if !auth.Allow(coLeader, r, Update) {
			t.Errorf("owner %q: a Co-organizer may not edit", owner)
		}
	}
	yk := Principal{ID: "yk", Groups: []string{"/UYELER/YK"}}
	if !auth.Allow(yk, Resource{Type: TypeEvent}, ManageOrganizers) {
		t.Error("YK may not manage an Event without an Owner team's list")
	}
	if auth.Allow(Principal{ID: "l", Groups: []string{"/UYELER/ARGE/GAMELAB/LIDERLER"}}, Resource{Type: TypeEvent}, ManageOrganizers) {
		t.Error("a Leader manages an Event without an Owner team's list")
	}
	// The Owner team named as a Co-organizer too gives nothing more.
	member := Principal{ID: "m", Groups: []string{"/UYELER/ARGE/GAMELAB"}}
	if auth.Allow(member, Resource{Type: TypeEvent, OwnerTeam: "GAMELAB", CoOwnerTeams: []CoTeam{{Code: "gamelab", MembersManage: true}}}, Update) {
		t.Error("the Owner team listed as a members Co-organizer opened editing to its members")
	}
}

// EventVia names the strongest seat that lets p edit.
func TestOrganizers_EventVia(t *testing.T) {
	t.Parallel()
	auth := inheritingAuthorizer(realmTree())
	r := Resource{Type: TypeEvent, OwnerTeam: "GAMELAB", CoOwnerTeams: []CoTeam{{Code: "MOBIL", MembersManage: true}, {Code: "GECEKODU"}}, CoOrganizerIDs: []string{"person", "ml"}}
	for _, c := range []struct {
		p    Principal
		want Via
	}{
		{Principal{ID: "yk", Groups: []string{"/UYELER/YK"}}, ViaPrivileged},
		{Principal{ID: "ol", Groups: []string{"/UYELER/ARGE/GAMELAB/LIDERLER"}}, ViaOwner},
		{Principal{ID: "ml", Groups: []string{"/UYELER/ARGE/WEBLAB/MOBIL/LIDERLER", "/UYELER/ARGE/GAMELAB/LIDERLER"}}, ViaOwner},
		{Principal{ID: "ml", Groups: []string{"/UYELER/ARGE/WEBLAB/MOBIL"}}, ViaCoTeam},
		{Principal{ID: "person"}, ViaCoPerson},
		{Principal{ID: "lm", Groups: []string{"/UYELER/ORGANIZASYON/GECEKODU"}}, ViaNone},
		{Principal{ID: "om", Groups: []string{"/UYELER/ARGE/GAMELAB"}}, ViaNone},
		{Principal{ID: "dk", Groups: []string{"/UYELER/DK"}}, ViaNone},
	} {
		if got := auth.EventVia(c.p, r); got != c.want {
			t.Errorf("%s %v: %s, want %s", c.p.ID, c.p.Groups, got, c.want)
		}
	}
}

// seats answers who holds a Co-organizer seat, by id.
type seats map[string]bool

func (s seats) HasCoOrganizerSeat(p Principal) bool { return s[p.ID] }

// The Media upload rules of an Event's editor name no Event: a person who
// may create none for any team uploads while they hold a Co-organizer seat
// (docs/co-organizers.md); which Event the Media ends up on is decided
// when it is linked. A service account never does.
func TestOrganizers_UploadWithACoOrganizerSeat(t *testing.T) {
	t.Parallel()
	auth := NewAuthorizer(DefaultPolicy(), WithCoOrganizerSeats(seats{"person": true, "svc": true}))
	for _, rule := range []MediaUploader{MediaUploaderEventEditor, MediaUploaderCertificateTemplateEditor} {
		r := Resource{Type: TypeMedia, MediaUploader: rule}
		if !auth.Allow(Principal{ID: "person"}, r, Upload) {
			t.Errorf("%s: a seated person may not upload", rule)
		}
		if auth.Allow(Principal{ID: "nobody"}, r, Upload) {
			t.Errorf("%s: a person without a seat uploads", rule)
		}
		if auth.Allow(Principal{ID: "svc", ServiceAccount: true}, r, Upload) {
			t.Errorf("%s: a service account uploads", rule)
		}
	}
	if NewAuthorizer(DefaultPolicy()).Allow(Principal{ID: "person"}, Resource{Type: TypeMedia, MediaUploader: MediaUploaderEventEditor}, Upload) {
		t.Error("without the seats a person uploads")
	}
}

// An Owner team whose rule opens editing to its members (GECEKODU): its
// member edits and reads Tickets, as before, and nothing more.
func TestOrganizers_OwnerMemberOfAMembersTeam(t *testing.T) {
	t.Parallel()
	auth := inheritingAuthorizer(realmTree())
	member := Principal{ID: "gm", Groups: []string{"/UYELER/ORGANIZASYON/GECEKODU"}}
	event := Resource{OwnerTeam: "GECEKODU", CoOwnerTeams: []CoTeam{{Code: "GAMELAB"}}}
	for _, c := range []struct {
		typ  Type
		a    Action
		want bool
	}{
		{TypeEvent, Update, true}, {TypeEvent, ReadFull, true}, {TypeEvent, Delete, false},
		{TypeEvent, ManageOrganizers, false}, {TypeEvent, Assign, false}, {TypeEventDay, Delete, false},
		{TypeTicket, Read, true}, {TypeTicket, Assign, false}, {TypeTicket, Validate, false},
		{TypeCertificate, Issue, false}, {TypeCertificate, Revoke, false}, {TypeCertificateTemplate, Assign, false},
	} {
		r := event
		r.Type = c.typ
		if got := auth.Allow(member, r, c.a); got != c.want {
			t.Errorf("%s %s = %v, want %v", c.typ, c.a, got, c.want)
		}
	}
}

// A Leader of a Team above the Owner team is its Leader (ADR-0065 decision
// 2): they assign its door staff and manage its Co-organizer list.
func TestOrganizers_LeaderAboveTheOwnerTeam(t *testing.T) {
	t.Parallel()
	auth := inheritingAuthorizer(realmTree())
	above := Principal{ID: "wl", Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}
	r := Resource{Type: TypeEvent, OwnerTeam: "MOBIL"}
	for _, a := range []Action{Assign, ManageOrganizers, Delete} {
		if !auth.Allow(above, r, a) {
			t.Errorf("%s: no", a)
		}
	}
	if via := auth.EventVia(above, r); via != ViaOwner {
		t.Errorf("via %s", via)
	}
}
