package certificate_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/certificate"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
)

// sessionless turns the fixture into an Event with no live Session: every
// Session is cancelled, so the door writes Venue entries and the rule counts
// the Event itself (docs/certificate-rule.md, "An Event with no Session").
func (f attendanceFixture) sessionless(t *testing.T) attendanceFixture {
	t.Helper()
	ctx := context.Background()
	for _, sess := range f.sessions {
		sess.Cancelled = true
		if _, err := f.events.UpdateSession(ctx, sess); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

// enter writes the Ticket's Venue entry on a day.
func (f attendanceFixture) enter(t *testing.T, ticketID uuid.UUID, date string) {
	t.Helper()
	if _, err := f.tickets.AddVenueEntry(context.Background(), ticket.VenueEntry{TicketID: ticketID, EventID: f.ev.ID, EntryDate: date, Source: ticket.EntryScan}); err != nil {
		t.Fatal(err)
	}
}

// With no live Session, N is the Event itself (1) and a Venue entry is its
// attendance: who entered is eligible under once and count (whatever M),
// who did not is below. Two days' entries still count once.
func TestSessionlessEventCountsTheVenueEntry(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, rule := range []string{certificate.RuleOnce, certificate.RuleCount} {
		f := newAttendanceFixture(t, rule).sessionless(t)
		if rule == certificate.RuleCount {
			six := 6
			f.ev.AttendanceMinCount = &six
			if _, err := f.events.Update(ctx, f.ev); err != nil {
				t.Fatal(err)
			}
		}
		entered := f.member(t, "Ayşe", "Yılmaz")
		f.enter(t, entered, "2031-03-14")
		f.enter(t, entered, "2031-03-15")
		guest := f.guest(t, "Deniz", "Ak")
		f.enter(t, guest, "2031-03-14")
		absent := f.member(t, "Burak", "Kaya")

		list, err := f.svc.Attendance(ctx, f.leader, f.ev.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !list.Sessionless || list.CountedTotal != 1 || list.EffectiveMin != 1 {
			t.Fatalf("%s header %+v", rule, list)
		}
		rows := rowsByTicket(list)
		for id, want := range map[uuid.UUID]struct {
			attended int
			status   string
		}{
			entered: {1, certificate.StatusEligible},
			guest:   {1, certificate.StatusEligible},
			absent:  {0, certificate.StatusBelow},
		} {
			if rows[id].Attended != want.attended || rows[id].Status != want.status {
				t.Errorf("%s %s: %+v, want %+v", rule, rows[id].Name, rows[id], want)
			}
		}
		summary, err := f.svc.Summary(ctx, f.leader, f.ev.ID)
		if err != nil || !summary.Sessionless || summary.EligibleCount != 2 || summary.BelowCount != 1 || summary.CountedTotal != 1 {
			t.Fatalf("%s summary %v %+v", rule, err, summary)
		}
		batch, err := f.svc.Finalize(ctx, f.leader, f.ev.ID)
		if err != nil || batch.TotalCount != 2 {
			t.Fatalf("%s finalization %v %+v", rule, err, batch)
		}
		processAll(t, f)
		// "1 oturumdan 1'i" would name a Session the Event never had: the
		// certificate keeps no snapshot, and the verification shows none.
		issued, _ := certificatesOf(t, f, entered)
		if issued.Serial == "" || issued.Attendance != nil {
			t.Fatalf("%s certificate %+v", rule, issued)
		}
	}
}

// The none rule still issues nothing, entered or not.
func TestSessionlessNoneRuleIssuesNothing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newAttendanceFixture(t, certificate.RuleNone).sessionless(t)
	f.enter(t, f.member(t, "Ayşe", "Yılmaz"), "2031-03-14")
	summary, err := f.svc.Summary(ctx, f.leader, f.ev.ID)
	if err != nil || summary.EligibleCount != 0 {
		t.Fatalf("none %v %+v", err, summary)
	}
}

// An Event with a live Session keeps K3: a Venue entry is never counted.
func TestVenueEntryNotCountedWithSessions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newAttendanceFixture(t, certificate.RuleOnce)
	id := f.member(t, "Ayşe", "Yılmaz")
	f.enter(t, id, "2031-03-14")
	list, err := f.svc.Attendance(ctx, f.leader, f.ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	if list.Sessionless || list.CountedTotal != 4 || rowsByTicket(list)[id].Attended != 0 {
		t.Fatalf("with Sessions %+v", list)
	}
}
