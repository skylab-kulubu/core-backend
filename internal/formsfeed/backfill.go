package formsfeed

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
)

// Refusals of a backfill request (the handler's problem codes).
var (
	ErrBackfillEventNotFound = errors.New("formsfeed: no such current Event")
	ErrBackfillForbidden     = errors.New("formsfeed: may not edit the Event")
	ErrBackfillCancelled     = errors.New("formsfeed: the Event is cancelled")
	ErrBackfillEnded         = errors.New("formsfeed: the Event has ended")
	ErrBackfillNotListed     = errors.New("formsfeed: the Event does not list the form")
	ErrBackfillFormInUse     = errors.New("formsfeed: another current Event lists the form")
	// ErrBackfillOff is a core without Forms' internal address or its
	// service account: nothing could pull.
	ErrBackfillOff = errors.New("formsfeed: pulling from Forms is off")
)

// BackfillEvents reads an Event and the current Events (event.PostgresStore).
type BackfillEvents interface {
	Get(ctx context.Context, id uuid.UUID) (event.Event, error)
	ListIncludingDrafts(ctx context.Context, ownerTeam string) ([]event.Event, error)
}

// Backfills takes an organizer's request to gather a form's answers again
// (SPEC §10 eki, R13 eki 1; since hub bilet 04, K1 of 2026-11-25): it
// queues the gathering (PullCollect) of every answer of a form an Event
// lists, kept as Unticketed respondents; it writes no Ticket
// (docs/form-respondents.md).
type Backfills struct {
	Events BackfillEvents
	Authz  authz.Authorizer
	Queue  PullQueue
	// On is false without Forms' internal address or core's service
	// account: every request is ErrBackfillOff.
	On  bool
	Now func() time.Time
}

// Request checks and queues. The caller must be a person who may edit the
// Event (a product's service account never asks); the
// Event must be current (not archived), not cancelled, not ended, and list
// the form as its application or an extra form, and no other current Event
// may list it (the one-place rule, ADR 0069 addendum of 2026-11-22, K4:
// an old form linked to a new Event must not ticket last year's applicants
// there through another Event). True when it queued, false when a backfill
// of the form waits already.
func (b *Backfills) Request(ctx context.Context, p authz.Principal, eventID, formID uuid.UUID) (bool, error) {
	ev, err := b.Events.Get(ctx, eventID)
	if errors.Is(err, event.ErrNotFound) {
		return false, ErrBackfillEventNotFound
	}
	if err != nil {
		return false, err
	}
	if p.ServiceAccount || !b.Authz.Allow(p, authz.Resource{Type: authz.TypeEvent, OwnerTeam: ev.OwnerTeam}, authz.Update) {
		if ev.Status == event.StatusDraft {
			// A draft is shown to its editors only (ADR 0066).
			return false, ErrBackfillEventNotFound
		}
		return false, ErrBackfillForbidden
	}
	now := time.Now()
	if b.Now != nil {
		now = b.Now()
	}
	switch {
	case ev.ArchivedAt != nil:
		return false, ErrBackfillEventNotFound
	case ev.Status == event.StatusCancelled:
		return false, ErrBackfillCancelled
	case ev.EndedAt(now):
		return false, ErrBackfillEnded
	case !ev.ListsForm(formID):
		return false, ErrBackfillNotListed
	}
	evs, err := b.Events.ListIncludingDrafts(ctx, "")
	if err != nil {
		return false, err
	}
	for _, other := range evs {
		if other.ID != ev.ID && other.ArchivedAt == nil && (other.ListsForm(formID) || other.HasFeedbackForm(formID)) {
			return false, ErrBackfillFormInUse
		}
	}
	if !b.On {
		return false, ErrBackfillOff
	}
	requestedBy, err := uuid.Parse(p.ID)
	if err != nil {
		// A person's sub is a UUID; anything else is not a person.
		return false, ErrBackfillForbidden
	}
	return b.Queue.Backfill(ctx, formID, ev.ID, requestedBy, now.UTC())
}
