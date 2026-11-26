package certificate

import (
	"context"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/event"
)

// Standing is one active Ticket's attendance by its Event's certificate
// rule (docs/certificate-rule.md): the one count every reader of "who
// attended how much" takes, so that the certificate list, the attendee
// exports (docs/attendee-export.md) and the draw never count apart.
type Standing struct {
	TicketID   uuid.UUID
	TicketType string
	// Attended is its check-ins on live counted Sessions; on a
	// Sessionless Event, 1 with a Venue entry.
	Attended int
	// MissingRequired are the live required Sessions it has no check-in on.
	MissingRequired []uuid.UUID
	// Eligible: the rule issues it a certificate (Eligible).
	Eligible bool
	// Status is its place on the certificate list: issued > queued >
	// failed > revoked > eligible > below.
	Status string
	// Certificate is its active certificate, else its latest revoked one;
	// nil for none.
	Certificate *CertificateRef
}

// Standings are an Event's Standings and the rule they were read by.
type Standings struct {
	Rule string
	// CountedTotal is N, the live counted Sessions (1 on a Sessionless
	// Event); EffectiveMin the count rule's threshold over them.
	CountedTotal int
	EffectiveMin int
	Sessionless  bool
	Required     []RequiredSession
	// Tickets are the active Tickets' Standings by Ticket; a cancelled or
	// waitlisted Ticket has none.
	Tickets map[uuid.UUID]Standing
}

// StandingsOf decides every Ticket of a by ev's rule.
func StandingsOf(ev event.Event, a Attendance) Standings {
	out := Standings{
		Rule: ev.AttendanceRule, CountedTotal: a.CountedTotal, EffectiveMin: event.RuleMin(ev, a.CountedTotal),
		Sessionless: a.Sessionless, Required: a.Required, Tickets: make(map[uuid.UUID]Standing, len(a.Tickets)),
	}
	for _, item := range a.Tickets {
		eligible := ticketEligible(ev, a, item)
		row := Standing{
			TicketID: item.TicketID, TicketType: item.TicketType, Attended: item.Attended,
			MissingRequired: item.MissingRequired, Eligible: eligible, Status: attendanceStatus(eligible, item),
		}
		switch {
		case item.Active != nil:
			row.Certificate = item.Active
		case item.Revoked != nil:
			row.Certificate = item.Revoked
		}
		out.Tickets[item.TicketID] = row
	}
	return out
}

// Standings reads ev's attendance once and decides every active Ticket. It
// checks no caller: whoever calls it has decided who may read it.
func (s *service) Standings(ctx context.Context, ev event.Event) (Standings, error) {
	attendance, err := s.attendance.EventAttendance(ctx, ev.ID)
	if err != nil {
		return Standings{}, err
	}
	return StandingsOf(ev, attendance), nil
}
