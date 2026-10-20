package ticket

import (
	"fmt"
	"time"

	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
)

var (
	// ErrEventNotPublished refuses a self registration on a draft its caller
	// may see (ADR 0066): 409, problem code event_not_published.
	ErrEventNotPublished = fmt.Errorf("%w: %w", ErrConflict, event.ErrNotPublished)
	// ErrSessionNotPublished refuses a check-in on a draft its caller may
	// see: the door's session_closed (403), with reason event_not_published.
	ErrSessionNotPublished = fmt.Errorf("%w: %w", ErrSessionClosed, event.ErrNotPublished)
	// ErrRegistrationClosed refuses a self registration while the Event's
	// applying is not open (Registration closure, ticket 12): 409, problem
	// code registration_closed.
	ErrRegistrationClosed = fmt.Errorf("%w: %w", ErrConflict, event.ErrRegistrationClosed)
	// ErrEventCancelled refuses a registration on a cancelled Event, from
	// the panel too (ticket 04): 409, problem code event_cancelled.
	ErrEventCancelled = fmt.Errorf("%w: %w", ErrConflict, event.ErrCancelled)
	// ErrSessionCancelled refuses a check-in on a cancelled Event: the
	// door's session_closed (403), with reason event_cancelled.
	ErrSessionCancelled = fmt.Errorf("%w: %w", ErrSessionClosed, event.ErrCancelled)
)

// seen is nil when p (nil when signed out) may see ev, ErrNotFound for a
// draft p may not see (event.VisibleTo): a draft does not exist for them.
func (s *service) seen(p *authz.Principal, ev event.Event) error {
	if !event.VisibleTo(s.authz, p, ev) {
		return ErrNotFound
	}
	return nil
}

// openForRegistration is nil when people may register themselves on ev:
// a draft takes no self registration (ErrEventNotPublished to who may see
// it, ErrNotFound to anyone else); a cancelled Event takes no registration
// at all, from the panel neither (ErrEventCancelled, checked first); and an
// Event whose applying is not open (ended, or closed by its organizer by
// hand or by the deadline) takes no person's own application
// (ErrRegistrationClosed). The closure stops a person's own application
// only: fromPanel is an operator adding a guest from the panel
// (operatorOf), which passes it, as adding a person does; the forms
// service's report has its own rule (openListing).
func (s *service) openForRegistration(p *authz.Principal, ev event.Event, fromPanel bool) error {
	if ev.Status == event.StatusCancelled {
		return ErrEventCancelled
	}
	if ev.Status == event.StatusDraft {
		if err := s.seen(p, ev); err != nil {
			return err
		}
		return ErrEventNotPublished
	}
	if !ev.RegistrationOpenAt(time.Now()) && !fromPanel {
		return ErrRegistrationClosed
	}
	return nil
}

// openForCheckIn is nil when ev's door may take check-ins: a draft takes
// none (ErrSessionNotPublished to who may see it, ErrNotFound to anyone
// else, door staff included), nor does a cancelled Event
// (ErrSessionCancelled).
func (s *service) openForCheckIn(p *authz.Principal, ev event.Event) error {
	if ev.Status == event.StatusCancelled {
		return ErrSessionCancelled
	}
	if ev.Status != event.StatusDraft {
		return nil
	}
	if err := s.seen(p, ev); err != nil {
		return err
	}
	return ErrSessionNotPublished
}
