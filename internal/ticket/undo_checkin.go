package ticket

import (
	"context"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
)

// UndoCheckIn takes back a check-in at the door desk
// (DELETE /v1/tickets/{ticketId}/sessions/{sessionId}/check-in, docs/ticket-qr.md).
// It asks what CheckIn asks, in the same order: the Ticket, its Event open
// for check-ins (openForCheckIn), the caller at its door (canUseDoor), the
// Session of that Event. Then the check-in must be there: ErrNotFound
// otherwise. One log line names who took back which; ids only.
func (s *service) UndoCheckIn(ctx context.Context, p authz.Principal, ticketID, sessionID uuid.UUID) error {
	t, err := s.tickets.Get(ctx, ticketID)
	if err != nil {
		return err
	}
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
	removed, err := s.tickets.RemoveCheckIn(ctx, t.ID, sessionID)
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
