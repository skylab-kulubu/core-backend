package raffle_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/certificate"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/raffle"
	"github.com/skylab-kulubu/core-backend/internal/teamaudit"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

type oneEvent struct{ ev event.Event }

func (o oneEvent) Get(_ context.Context, id uuid.UUID) (event.Event, error) {
	if id != o.ev.ID {
		return event.Event{}, event.ErrNotFound
	}
	return o.ev, nil
}

type attendance struct{ a certificate.Attendance }

func (a attendance) EventAttendance(context.Context, uuid.UUID) (certificate.Attendance, error) {
	return a.a, nil
}

// fixture is an ARTLAB-like Event of the WEBLAB team: five counted
// Sessions, its door staff, a Co-organizer team and person, and its
// Tickets' attendance.
type fixture struct {
	ev       event.Event
	store    *raffle.Memory
	svc      *raffle.Service
	audit    *teamaudit.Memory
	dir      *identity.Memory
	sessions []uuid.UUID
	lead     authz.Principal
	// tickets by name
	t map[string]raffle.Candidate
}

var day = "2026-11-27"

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{store: raffle.NewMemory(), audit: teamaudit.NewMemory(), dir: identity.NewMemory(), t: map[string]raffle.Candidate{}}
	for range 5 {
		f.sessions = append(f.sessions, uuid.New())
	}
	weblab := identity.Group{ID: uuid.NewString(), Name: "WEBLAB", Path: "/UYELER/ARGE/WEBLAB"}
	weblabSub := identity.Group{ID: uuid.NewString(), Name: "LIDERLER", Path: "/UYELER/ARGE/WEBLAB/LIDERLER"}
	gdg := identity.Group{ID: uuid.NewString(), Name: "GDG", Path: "/UYELER/ARGE/GDG"}
	f.dir.PutGroup(weblab)
	f.dir.PutGroup(weblabSub)
	f.dir.PutGroup(gdg)
	// People: an organizer (WEBLAB, sub-team), a Co-organizer team member
	// (GDG), a door staffer, a Co-organizer person, members and guests.
	organizer := identity.Person{ID: uuid.New(), Email: "org@example.com", SchoolEmail: "org@std.yildiz.edu.tr", Enabled: true}
	coMember := identity.Person{ID: uuid.New(), Email: "co@example.com", Enabled: true}
	f.dir.PutUser(organizer)
	f.dir.PutUser(coMember)
	ctx := context.Background()
	if err := f.dir.AddMember(ctx, weblabSub.ID, organizer.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.dir.AddMember(ctx, gdg.ID, coMember.ID); err != nil {
		t.Fatal(err)
	}
	door, coPerson := uuid.New(), uuid.New()
	f.ev = event.Event{
		ID: uuid.New(), Name: "ARTLAB", OwnerTeam: "WEBLAB", Status: event.StatusPublished,
		AttendanceRule: certificate.RuleCount, AttendanceMinCount: new(int),
		DoorStaffIDs: []uuid.UUID{door}, CoOrganizers: []event.CoOrganizerTeam{{Code: "GDG"}},
		CoOrganizerIDs: []uuid.UUID{coPerson},
	}
	*f.ev.AttendanceMinCount = 4
	member := func(name string, owner uuid.UUID, email string, sessions int, entered bool) {
		c := raffle.Candidate{TicketID: uuid.New(), TicketType: raffle.TypeRegistered, OwnerID: &owner, Name: name, Email: email,
			Sessions: slices.Clone(f.sessions[:sessions])}
		if entered {
			c.Entered = []string{day}
		}
		f.t[name] = c
	}
	guest := func(name, email string, sessions int, entered bool) {
		c := raffle.Candidate{TicketID: uuid.New(), TicketType: raffle.TypeGuest, Name: name, Email: email,
			Sessions: slices.Clone(f.sessions[:sessions])}
		if entered {
			c.Entered = []string{day}
		}
		f.t[name] = c
	}
	member("Ada Lovelace", uuid.New(), "ada@example.com", 5, true)
	member("Alan Turing", uuid.New(), "alan@example.com", 3, true)
	member("Grace Hopper", uuid.New(), "grace@example.com", 2, true)
	guest("Edsger Dijkstra", "edsger@example.com", 4, true)
	guest("Barbara Liskov", "barbara@example.com", 0, false)
	member("Organizer Uye", organizer.ID, "org@example.com", 5, true)
	guest("Organizer Misafir", "ORG@std.yildiz.edu.tr", 5, true) // the organizer's school address
	member("Co Uye", coMember.ID, "co@example.com", 5, true)
	member("Door Staff", door, "door@example.com", 5, true)
	member("Co Person", coPerson, "coperson@example.com", 5, true)
	var cands []raffle.Candidate
	var att certificate.Attendance
	att.CountedTotal = 5
	for _, c := range f.t {
		cands = append(cands, c)
		att.Tickets = append(att.Tickets, certificate.TicketAttendance{TicketID: c.TicketID, TicketType: c.TicketType, Attended: len(c.Sessions), MissingRequired: []uuid.UUID{}})
	}
	f.store.SetCandidates(f.ev.ID, cands)
	az := authz.NewAuthorizer(authz.DefaultPolicy())
	tickets := ticket.NewService(ticket.NewMemoryStore(), event.NewMemoryStore(), az, f.dir, f.audit)
	auditor, ok := tickets.(ticket.EventAuditor)
	if !ok {
		t.Fatal("the ticket service opens no event audit rows")
	}
	f.svc = &raffle.Service{
		Store: f.store, Events: oneEvent{f.ev}, Authz: az, Attendance: attendance{att}, Audit: auditor,
		Teams: raffle.DirectoryTeams{Dir: f.dir},
		People: func(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]user.Account, error) {
			out := map[uuid.UUID]user.Account{}
			for _, id := range ids {
				if id == door {
					out[id] = user.Account{User: user.User{ID: id, Email: "door@example.com"}, Stored: true}
				}
			}
			return out, nil
		},
		Now: func() time.Time { return time.Date(2026, 11, 27, 13, 0, 0, 0, time.UTC) },
	}
	f.lead = authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}
	return f
}

// actor is the context an audited route hands the service.
func actor(p authz.Principal) context.Context {
	return teamaudit.WithActor(context.Background(), teamaudit.Actor{ID: uuid.MustParse(p.ID), Client: "core-frontend"})
}

func everyone() raffle.Criteria { return raffle.Criteria{} }

func TestPreviewCriteria(t *testing.T) {
	f := newFixture(t)
	three := 3
	cases := []struct {
		name string
		c    raffle.Criteria
		want int
	}{
		{"everyone", everyone(), 10},
		{"3 of 5 counted Sessions", raffle.Criteria{MinCounted: &three}, 8},
		{"the certificate rule (4 of 5)", raffle.Criteria{CertificateEligible: true}, 7},
		{"Session 3 and 4", raffle.Criteria{SessionIDs: []uuid.UUID{f.sessions[2], f.sessions[3]}, SessionMatch: raffle.MatchAll}, 7},
		{"Session 3 or 5", raffle.Criteria{SessionIDs: []uuid.UUID{f.sessions[2], f.sessions[4]}, SessionMatch: raffle.MatchAny}, 8},
		{"entered that day", raffle.Criteria{EnteredOn: day}, 9},
		{"entered another day", raffle.Criteria{EnteredOn: "2026-11-28"}, 0},
		{"entered any day", raffle.Criteria{EnteredOn: raffle.EnteredAny}, 9},
		{"guests", raffle.Criteria{TicketType: raffle.TypeGuest}, 3},
		{"members, 3 Sessions, no organizers", raffle.Criteria{MinCounted: &three, TicketType: raffle.TypeRegistered,
			ExcludeOrganizers: true, ExcludeDoorStaff: true, ExcludeCoOrganizers: true}, 2},
	}
	for _, tc := range cases {
		got, err := f.svc.Preview(context.Background(), f.lead, f.ev.ID, tc.c)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got.Eligible != tc.want || len(got.Candidates) != tc.want {
			t.Errorf("%s: eligible %d (%d listed), want %d", tc.name, got.Eligible, len(got.Candidates), tc.want)
		}
	}
}

func TestPreviewExcludesAndMasks(t *testing.T) {
	f := newFixture(t)
	got, err := f.svc.Preview(context.Background(), f.lead, f.ev.ID, raffle.Criteria{
		ExcludeOrganizers: true, ExcludeDoorStaff: true, ExcludeCoOrganizers: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	// The organizer's member Ticket and their guest Ticket (school
	// address, another case) through the sub-team; the door staffer; the
	// Co-organizer team's member and the Co-organizer person.
	want := raffle.Excluded{Organizers: 2, DoorStaff: 1, CoOrganizers: 2}
	if got.Excluded != want || got.Eligible != 5 {
		t.Fatalf("excluded %+v, eligible %d", got.Excluded, got.Eligible)
	}
	for _, c := range got.Candidates {
		if c.Name == "" || slices.Contains([]string{"Ada Lovelace", "Alan Turing"}, c.Name) {
			t.Fatalf("unmasked %q", c.Name)
		}
	}
	if got.Candidates[0].Name != "Ad*** Lo***" {
		t.Fatalf("first %q", got.Candidates[0].Name)
	}
}

func TestDrawAndForfeit(t *testing.T) {
	f := newFixture(t)
	ctx := actor(f.lead)
	three := 3
	r, err := f.svc.Draw(ctx, f.lead, f.ev.ID, raffle.Request{
		Title: "Kapanış", Criteria: raffle.Criteria{MinCounted: &three, ExcludePastWinners: true}, Winners: 2, Alternates: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.EligibleCount != 8 || r.WinnerCount != 2 || r.AlternateCount != 2 || len(r.Entries) != 4 ||
		r.Algorithm != raffle.Algorithm || len(r.Seed) != 64 || len(r.ListHash) != 64 || r.DrawnBy == nil {
		t.Fatalf("raffle %+v", r)
	}
	for i, e := range r.Entries {
		want := raffle.StatusWinner
		if i >= 2 {
			want = raffle.StatusAlternate
		}
		if e.Position != i+1 || e.Status != want || e.Name == "" || e.MaskedMail == "" {
			t.Fatalf("entry %d %+v", i, e)
		}
	}
	// Anyone given the verification draws the same order.
	v, err := f.svc.Verification(ctx, f.lead, f.ev.ID, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(v.TicketIDs) != 8 || len(v.Order) != 4 {
		t.Fatalf("verification %+v", v)
	}
	if err := raffle.Verify(v); err != nil {
		t.Fatal(err)
	}
	// A winner who did not come: the first alternate takes the place.
	if _, err := f.svc.Forfeit(ctx, f.lead, f.ev.ID, r.ID, 1, "x"); !errors.Is(err, raffle.ErrInvalid) {
		t.Fatalf("short reason: %v", err)
	}
	if _, err := f.svc.Forfeit(ctx, f.lead, f.ev.ID, r.ID, 3, "Salonda değildi"); !errors.Is(err, raffle.ErrNotWinner) {
		t.Fatalf("an alternate forfeited: %v", err)
	}
	after, err := f.svc.Forfeit(ctx, f.lead, f.ev.ID, r.ID, 1, "Salonda değildi")
	if err != nil {
		t.Fatal(err)
	}
	if e := after.Entries[0]; e.Status != raffle.StatusForfeited || e.ForfeitReason == nil || *e.ForfeitReason != "Salonda değildi" || e.ForfeitedBy == nil {
		t.Fatalf("forfeited %+v", e)
	}
	if e := after.Entries[2]; e.Status != raffle.StatusWinner || e.PromotedAt == nil {
		t.Fatalf("promoted %+v", e)
	}
	if _, err := f.svc.Forfeit(ctx, f.lead, f.ev.ID, r.ID, 2, "Gelmedi"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Forfeit(ctx, f.lead, f.ev.ID, r.ID, 3, "Gelmedi"); !errors.Is(err, raffle.ErrNoAlternates) {
		t.Fatalf("no alternates left: %v", err)
	}
	// The next raffle leaves out who holds a winner's place (positions 3
	// and 4 now), not who forfeited.
	next, err := f.svc.Preview(ctx, f.lead, f.ev.ID, raffle.Criteria{MinCounted: &three, ExcludePastWinners: true})
	if err != nil {
		t.Fatal(err)
	}
	if next.Eligible != 6 || next.Excluded.PastWinners != 2 {
		t.Fatalf("next preview %+v", next)
	}
	// The record: the draw's and the two forfeits' rows (a refused forfeit
	// opens none), the caller's, as the Event's Leader, ids and counts only.
	rows := f.audit.Rows()
	wantActions := []teamaudit.Action{teamaudit.ActionRaffleDraw, teamaudit.ActionRaffleForfeit, teamaudit.ActionRaffleForfeit}
	if len(rows) != len(wantActions) {
		t.Fatalf("%d audit rows", len(rows))
	}
	for i, row := range rows {
		if row.Action != wantActions[i] || row.Actor.ID.String() != f.lead.ID || row.Actor.Basis != teamaudit.BasisLeader ||
			row.After == nil || row.After.Raffle == nil || row.After.Raffle.ID != r.ID.String() || row.After.EventID != f.ev.ID.String() {
			t.Fatalf("row %d: %+v", i, row)
		}
	}
	if rows[0].Outcome != teamaudit.OutcomeOK || rows[0].After.Raffle.Eligible != 8 || rows[1].Outcome != teamaudit.OutcomeOK ||
		rows[1].After.Raffle.Forfeited != 1 || rows[1].After.Raffle.Promoted != 3 || rows[2].After.Raffle.Promoted != 4 {
		t.Fatalf("rows %+v %+v %+v", rows[0], rows[1], rows[2])
	}
	list, err := f.svc.List(ctx, f.lead, f.ev.ID)
	if err != nil || len(list) != 1 || list[0].ID != r.ID || list[0].Title != "Kapanış" {
		t.Fatalf("list %v %+v", err, list)
	}
}

func TestDrawTooFewAndAlternatesCapped(t *testing.T) {
	f := newFixture(t)
	ctx := actor(f.lead)
	guests := raffle.Criteria{TicketType: raffle.TypeGuest}
	_, err := f.svc.Draw(ctx, f.lead, f.ev.ID, raffle.Request{Criteria: guests, Winners: 4})
	var few *raffle.TooFewError
	if !errors.As(err, &few) || few.Eligible != 3 {
		t.Fatalf("too few: %v", err)
	}
	r, err := f.svc.Draw(ctx, f.lead, f.ev.ID, raffle.Request{Criteria: guests, Winners: 2, Alternates: 5})
	if err != nil {
		t.Fatal(err)
	}
	if r.AlternateCount != 1 || len(r.Entries) != 3 || r.Title != raffle.DefaultTitle {
		t.Fatalf("raffle %+v", r)
	}
	for _, bad := range []raffle.Request{
		{Winners: 0}, {Winners: 101}, {Winners: 1, Alternates: -1}, {Winners: 1, Alternates: 101},
		{Winners: 1, Criteria: raffle.Criteria{SessionMatch: "some"}},
		{Winners: 1, Criteria: raffle.Criteria{EnteredOn: "27.11.2026"}},
		{Winners: 1, Criteria: raffle.Criteria{TicketType: "VIP"}},
	} {
		if _, err := f.svc.Draw(ctx, f.lead, f.ev.ID, bad); !errors.Is(err, raffle.ErrInvalid) {
			t.Errorf("%+v: %v", bad, err)
		}
	}
}

// Who may draw: who edits the Event or assigns Tickets. The door staff, a
// member of another team and a service account may not; one who cannot
// see a draft Event is told it is not there.
func TestWhoMayDraw(t *testing.T) {
	f := newFixture(t)
	yk := authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/YK"}}
	if _, err := f.svc.List(context.Background(), yk, f.ev.ID); err != nil {
		t.Fatalf("YK: %v", err)
	}
	door := authz.Principal{ID: f.ev.DoorStaffIDs[0].String(), Groups: []string{"/UYELER"}}
	other := authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/ARGE/GECEKODU"}}
	service := authz.Principal{ID: uuid.NewString(), ServiceAccount: true, Groups: []string{"/UYELER/YK"}}
	for name, p := range map[string]authz.Principal{"door staff": door, "another team": other, "service account": service} {
		if _, err := f.svc.Preview(context.Background(), p, f.ev.ID, everyone()); !errors.Is(err, raffle.ErrForbidden) {
			t.Errorf("%s preview: %v", name, err)
		}
		if _, err := f.svc.Draw(actor(f.lead), p, f.ev.ID, raffle.Request{Winners: 1}); !errors.Is(err, raffle.ErrForbidden) {
			t.Errorf("%s draw: %v", name, err)
		}
	}
	if _, err := f.svc.List(context.Background(), f.lead, uuid.New()); !errors.Is(err, raffle.ErrEventNotFound) {
		t.Fatalf("unknown event: %v", err)
	}
}

// Without the caller in the context (an audited route that forgot
// auditContext) the draw is refused before anything is drawn.
func TestDrawWithoutTheAuditActorDrawsNothing(t *testing.T) {
	f := newFixture(t)
	_, err := f.svc.Draw(context.Background(), f.lead, f.ev.ID, raffle.Request{Winners: 1})
	if !errors.Is(err, teamaudit.ErrUnavailable) {
		t.Fatalf("draw: %v", err)
	}
	f.audit.SetUnavailable(true)
	if _, err := f.svc.Draw(actor(f.lead), f.lead, f.ev.ID, raffle.Request{Winners: 1}); !errors.Is(err, teamaudit.ErrUnavailable) {
		t.Fatalf("audit down: %v", err)
	}
	list, err := f.svc.List(context.Background(), f.lead, f.ev.ID)
	if err != nil || len(list) != 0 {
		t.Fatalf("drawn anyway: %v %d", err, len(list))
	}
}
