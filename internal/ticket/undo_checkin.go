package ticket

import (
	"context"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
)

// UndoCheckIn takes back a check-in at the door desk by its Ticket
// (DELETE /v1/tickets/{ticketId}/sessions/{sessionId}/check-in,
// docs/ticket-qr.md); undoCheckIn decides.
func (s *service) UndoCheckIn(ctx context.Context, p authz.Principal, ticketID, sessionID uuid.UUID) error {
	t, err := s.tickets.Get(ctx, ticketID)
	if err != nil {
		return err
	}
	return s.undoCheckIn(ctx, p, t, sessionID, func() (bool, error) {
		return s.tickets.RemoveCheckIn(ctx, t.ID, sessionID)
	})
}

// UndoCheckInByID takes back a check-in by its own id, the one the door
// holds (DELETE /v1/sessions/{sessionId}/check-ins/{checkInId}): the
// Session's door activity and the resolve and scan answers name the
// check-in, not its Ticket. A check-in of another Session is ErrNotFound,
// as one that does not exist; undoCheckIn decides the rest.
func (s *service) UndoCheckInByID(ctx context.Context, p authz.Principal, sessionID, checkInID uuid.UUID) error {
	ci, err := s.tickets.CheckInByID(ctx, checkInID)
	if err != nil {
		return err
	}
	if ci.SessionID != sessionID {
		return ErrNotFound
	}
	t, err := s.tickets.Get(ctx, ci.TicketID)
	if err != nil {
		return err
	}
	return s.undoCheckIn(ctx, p, t, sessionID, func() (bool, error) {
		return s.tickets.RemoveCheckInByID(ctx, checkInID)
	})
}

// undoCheckIn asks what CheckIn asks, in the same order: t's Event open
// for check-ins (openForCheckIn), the caller at its door (canUseDoor), the
// Session of that Event. Then remove must find the check-in: ErrNotFound
// otherwise. One log line names who took back which; ids only.
func (s *service) undoCheckIn(ctx context.Context, p authz.Principal, t Ticket, sessionID uuid.UUID, remove func() (bool, error)) error {
	ev, err := s.eventOf(ctx, t.EventID)
	if err != nil {
		return err
	}
	if err := s.openForCheckIn(&p, ev); err != nil {
		return err
	}
	if !s.canUseDoor(ctx, p, ev) {
		return ErrForbidden
	}
	if _, err := s.ticketSession(ctx, t, sessionID); err != nil {
		return err
	}
	removed, err := remove()
	if err != nil {
		return err
	}
	if !removed {
		return ErrNotFound
	}
	s.logger.Printf(`{"event":"ticket_checkin_undo","actorId":%q,"eventId":%q,"ticketId":%q,"sessionId":%q}`,
		p.ID, ev.ID, t.ID, sessionID)
	return nil
}
