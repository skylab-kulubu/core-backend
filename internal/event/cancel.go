package event

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/lifecycle"
)

// ErrEnded refuses to cancel an Event that has ended (problem code
// event_ended): cancelling it would blur its certificate history.
var ErrEnded = fmt.Errorf("event: ended: %w", ErrConflict)

// CancelReasonMax is the longest cancel reason, in characters: a short
// public text ("Hava koşulları nedeniyle").
const CancelReasonMax = 500

// CompetitionCancelReason is the reason a Competition cancelled with its
// Event gets when the Event's cancellation gives none: a cancelled
// Competition must have one.
const CompetitionCancelReason = "Etkinlik iptal edildi."

// Cancel cancels a published Event that has not ended (ADR 0066; ticket
// 04): it stays visible as cancelled, takes no new application, check-in or
// certificate, and nothing it holds is deleted or closed for good (ADR 0066
// addendum 2026-10-19: a cancellation can be undone). Its Competitions not
// cancelled already are cancelled in the same write, with their earlier
// status kept. Cancelling a cancelled Event answers it as it is; a draft is
// refused (ErrNotPublished; a draft is deleted, not cancelled), an ended
// Event too (ErrEnded). reason is optional, at most CancelReasonMax
// characters. With notify the Ticket holders' mail is queued in the same
// write (ticket 14); an Event cancelled already queues nothing again.
func (s *service) Cancel(ctx context.Context, p authz.Principal, id uuid.UUID, reason string, notify bool) (Event, error) {
	e, err := s.statusChange(ctx, p, id)
	if err != nil {
		return Event{}, err
	}
	reason = strings.TrimSpace(reason)
	if utf8.RuneCountInString(reason) > CancelReasonMax {
		return Event{}, ErrInvalid
	}
	if e.Status == StatusPublished {
		if e.EndedAt(time.Now()) {
			return Event{}, ErrEnded
		}
		changed, err := s.store.Cancel(ctx, id, lifecycle.ActorID(p.ID), reason, notify)
		if err != nil {
			return Event{}, err
		}
		if !changed {
			// Something changed it since the read: decide on what it is now.
			if e, err = s.store.Get(ctx, id); err != nil {
				return Event{}, err
			}
			if e.Status == StatusPublished {
				if e.EndedAt(time.Now()) {
					return Event{}, ErrEnded
				}
				return Event{}, ErrConflict
			}
		}
	}
	if e.Status == StatusDraft {
		return Event{}, ErrNotPublished
	}
	e, err = s.store.Get(ctx, id)
	return s.detail(ctx, e, err)
}

// ErrNotCancelled refuses to undo the cancellation of a draft (problem code
// event_not_cancelled).
var ErrNotCancelled = fmt.Errorf("event: not cancelled: %w", ErrConflict)

// Uncancel undoes a cancellation, always (ADR 0066 addendum 2026-10-19;
// ticket 15): the Event is published again, an ended one too, and works as
// before it was cancelled, since the cancellation deleted and closed
// nothing. Its Competitions cancelled with it go back to their earlier
// status; held certificate jobs go on by themselves. The cancellation's
// time, actor and reason are cleared; the publication's stay. Undoing on a
// published Event answers it as it is; a draft is refused
// (ErrNotCancelled). With notify, when the cancellation was mailed, the
// mail that the Event is back is queued for whom that mail reached
// (ticket 14); otherwise notify does nothing.
func (s *service) Uncancel(ctx context.Context, p authz.Principal, id uuid.UUID, notify bool) (Event, error) {
	e, err := s.statusChange(ctx, p, id)
	if err != nil {
		return Event{}, err
	}
	if e.Status == StatusCancelled {
		changed, err := s.store.Uncancel(ctx, id, notify)
		if err != nil {
			return Event{}, err
		}
		if !changed {
			if e, err = s.store.Get(ctx, id); err != nil {
				return Event{}, err
			}
			if e.Status == StatusCancelled {
				return Event{}, ErrConflict
			}
		}
	}
	if e.Status == StatusDraft {
		return Event{}, ErrNotCancelled
	}
	e, err = s.store.Get(ctx, id)
	return s.detail(ctx, e, err)
}
