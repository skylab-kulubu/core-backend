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
// characters.
func (s *service) Cancel(ctx context.Context, p authz.Principal, id uuid.UUID, reason string) (Event, error) {
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
		changed, err := s.store.Cancel(ctx, id, lifecycle.ActorID(p.ID), reason)
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
