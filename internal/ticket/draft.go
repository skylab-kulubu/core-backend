package ticket

import (
	"fmt"

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
// it, ErrNotFound to anyone else). Adding a person from the panel and the
// forms service's report still write Tickets on a draft.
func (s *service) openForRegistration(p *authz.Principal, ev event.Event) error {
	if ev.Status != event.StatusDraft {
		return nil
	}
	if err := s.seen(p, ev); err != nil {
		return err
	}
	return ErrEventNotPublished
}

// openForCheckIn is nil when ev's door may take check-ins: a draft takes
// none (ErrSessionNotPublished to who may see it, ErrNotFound to anyone
// else, door staff included).
func (s *service) openForCheckIn(p *authz.Principal, ev event.Event) error {
	if ev.Status != event.StatusDraft {
		return nil
	}
	if err := s.seen(p, ev); err != nil {
		return err
	}
	return ErrSessionNotPublished
}
