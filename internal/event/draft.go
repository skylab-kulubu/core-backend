package event

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/lifecycle"
)

// ErrHasTickets refuses to make a published Event a draft again once it has
// a Ticket or a check-in (problem code event_has_tickets).
var ErrHasTickets = fmt.Errorf("event: has tickets: %w", ErrConflict)

// IncompleteError refuses to publish a draft that lacks what a published
// Event must have: a name, a location and a start date. Missing names the
// JSON fields.
type IncompleteError struct {
	Missing []string
}

func (e *IncompleteError) Error() string {
	return "event: incomplete: " + strings.Join(e.Missing, ", ")
}

func (e *IncompleteError) Unwrap() error { return ErrInvalid }

// missing names the JSON fields a published Event must have that e lacks.
func (e Event) missing() []string {
	var out []string
	if strings.TrimSpace(e.Name) == "" {
		out = append(out, "name")
	}
	if strings.TrimSpace(e.Location) == "" {
		out = append(out, "location")
	}
	if e.StartDate == nil {
		out = append(out, "startDate")
	}
	return out
}

func (e Event) publishable() bool {
	return len(e.missing()) == 0
}

// VisibleTo reports whether p (nil when signed out) may see e (ADR 0066,
// decisions of 2026-10-19): a published or cancelled Event everyone, a draft
// only who may edit it (authz.Update on its Owner team, the team authority
// handover included) and a Privileged reader (DK, global:read), who only
// reads it. The authorizer decides both as ReadFull on the Event. Who may
// not see a draft is answered as if it did not exist.
func VisibleTo(az authz.Authorizer, p *authz.Principal, e Event) bool {
	if e.Status != StatusDraft {
		return true
	}
	return p != nil && az.Allow(*p, resource(e.OwnerTeam), authz.ReadFull)
}

func (s *service) VisibleTo(p *authz.Principal, e Event) bool {
	return VisibleTo(s.authz, p, e)
}

// visible is the current Event id names, ErrNotFound when p may not see it.
func (s *service) visible(ctx context.Context, p *authz.Principal, id uuid.UUID) (Event, error) {
	e, err := s.store.Get(ctx, id)
	if err != nil {
		return Event{}, err
	}
	if !s.VisibleTo(p, e) {
		return Event{}, ErrNotFound
	}
	return e, nil
}

// parentVisible checks that p may see the Event a day or Session belongs to:
// ErrNotFound for a draft p may not see. It reads the Event archived or not,
// which the reads of days and Sessions never looked at.
func (s *service) parentVisible(ctx context.Context, p *authz.Principal, eventID uuid.UUID) error {
	e, err := s.store.GetIncludingArchived(ctx, eventID)
	if err != nil {
		return err
	}
	if !s.VisibleTo(p, e) {
		return ErrNotFound
	}
	return nil
}

// statusChange is the Event p is about to publish or unpublish: one p may
// see (ErrNotFound otherwise) and edit (ErrForbidden; a Privileged reader
// sees a draft but changes nothing).
func (s *service) statusChange(ctx context.Context, p authz.Principal, id uuid.UUID) (Event, error) {
	e, err := s.visible(ctx, &p, id)
	if err != nil {
		return Event{}, err
	}
	if !s.authz.Allow(p, resource(e.OwnerTeam), authz.Update) {
		return Event{}, ErrForbidden
	}
	return e, nil
}

// Publish publishes a draft. Publishing a published Event answers it as it
// is; a cancelled one is refused (ErrCancelled); a draft without a name, a
// location or a start date is refused with what it lacks.
func (s *service) Publish(ctx context.Context, p authz.Principal, id uuid.UUID) (Event, error) {
	e, err := s.statusChange(ctx, p, id)
	if err != nil {
		return Event{}, err
	}
	if e.Status == StatusDraft {
		if missing := e.missing(); len(missing) > 0 {
			return Event{}, &IncompleteError{Missing: missing}
		}
		changed, err := s.store.Publish(ctx, id, lifecycle.ActorID(p.ID))
		if err != nil {
			return Event{}, err
		}
		if !changed {
			// Something changed it since the read: decide on what it is now.
			if e, err = s.store.Get(ctx, id); err != nil {
				return Event{}, err
			}
			if e.Status == StatusDraft {
				if missing := e.missing(); len(missing) > 0 {
					return Event{}, &IncompleteError{Missing: missing}
				}
				return Event{}, ErrConflict
			}
		}
	}
	if e.Status == StatusCancelled {
		return Event{}, ErrCancelled
	}
	e, err = s.store.Get(ctx, id)
	return s.detail(ctx, e, err)
}

// Unpublish makes a published Event a draft again, only while it has no
// Ticket (ErrHasTickets). Unpublishing a draft answers it as it is; a
// cancelled Event is refused (ErrCancelled).
func (s *service) Unpublish(ctx context.Context, p authz.Principal, id uuid.UUID) (Event, error) {
	e, err := s.statusChange(ctx, p, id)
	if err != nil {
		return Event{}, err
	}
	if e.Status == StatusPublished {
		changed, err := s.store.Unpublish(ctx, id)
		if err != nil {
			return Event{}, err
		}
		if !changed {
			if e, err = s.store.Get(ctx, id); err != nil {
				return Event{}, err
			}
			if e.Status == StatusPublished {
				return Event{}, ErrHasTickets
			}
		}
	}
	if e.Status == StatusCancelled {
		return Event{}, ErrCancelled
	}
	e, err = s.store.Get(ctx, id)
	return s.detail(ctx, e, err)
}
