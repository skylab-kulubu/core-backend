package certificate

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/event"
)

// StandingsOf is the certificate list's count: the rule's eligibility and
// the list's status, an active certificate before a revoked one.
func TestStandingsOf(t *testing.T) {
	t.Parallel()
	required := uuid.New()
	issued, revoked, below, missing := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	at := time.Now()
	a := Attendance{CountedTotal: 4, Required: []RequiredSession{{ID: required, Title: "Açılış"}}, Tickets: []TicketAttendance{
		{TicketID: issued, Attended: 3, Active: &CertificateRef{Serial: "A"}},
		{TicketID: revoked, Attended: 4, Revoked: &CertificateRef{Serial: "B", RevokedAt: &at}},
		{TicketID: below, Attended: 2},
		{TicketID: missing, Attended: 4, MissingRequired: []uuid.UUID{required}},
	}}
	got := StandingsOf(event.Event{AttendanceRule: event.RuleCount}, a)
	if got.CountedTotal != 4 || got.EffectiveMin != 3 || got.Rule != event.RuleCount || len(got.Tickets) != 4 {
		t.Fatalf("%+v", got)
	}
	for id, want := range map[uuid.UUID]struct {
		eligible bool
		status   string
		serial   string
	}{
		issued: {true, StatusIssued, "A"}, revoked: {true, StatusRevoked, "B"},
		below: {false, StatusBelow, ""}, missing: {false, StatusBelow, ""},
	} {
		s := got.Tickets[id]
		serial := ""
		if s.Certificate != nil {
			serial = s.Certificate.Serial
		}
		if s.Eligible != want.eligible || s.Status != want.status || serial != want.serial {
			t.Errorf("%+v, want %+v", s, want)
		}
	}
}
