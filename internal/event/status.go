package event

import (
	"fmt"
	"time"
)

// Status is the Event's stored status (ADR 0066): a draft only its editors
// see, published, or cancelled (final). Whether a published Event has ended
// is not stored: it follows from its dates (EndsAt).
type Status string

const (
	StatusDraft     Status = "draft"
	StatusPublished Status = "published"
	StatusCancelled Status = "cancelled"
)

var (
	// ErrNotPublished refuses what only a published Event allows, on a
	// draft (problem code event_not_published).
	ErrNotPublished = fmt.Errorf("event: not published: %w", ErrConflict)
	// ErrCancelled refuses what a cancelled Event no longer allows (problem
	// code event_cancelled).
	ErrCancelled = fmt.Errorf("event: cancelled: %w", ErrConflict)
)

// undatedEndsAfter is how long after its start an Event without an end date
// lasts.
const undatedEndsAfter = 24 * time.Hour

// endsAtSQL and activeSQL are EndsAt and ActiveAt in SQL, on the events row
// e: what the lists filter with and the reads answer as active, so the two
// never disagree. The interval is in hours, not '1 day', so that it is the
// same duration as undatedEndsAfter whatever the session's time zone.
const (
	endsAtSQL = `COALESCE(e.end_date, e.start_date + interval '24 hours')`
	activeSQL = `(e.status = 'published' AND (` + endsAtSQL + ` IS NULL OR ` + endsAtSQL + ` > now()))`
)

// creatable reports whether an Event may be created with the status: a new
// Event is a draft or published (the default, which a client that sends no
// status gets); never cancelled.
func creatable(s Status) bool {
	return s == StatusDraft || s == StatusPublished
}

// EndsAt is when the Event ends: its end date, else a day after its start.
// An Event with neither is undated and never ends (nil).
func (e Event) EndsAt() *time.Time {
	if e.EndDate != nil {
		end := *e.EndDate
		return &end
	}
	if e.StartDate != nil {
		end := e.StartDate.Add(undatedEndsAfter)
		return &end
	}
	return nil
}

// EndedAt reports whether the Event, whatever its status, has ended at now.
// An undated Event never ends.
func (e Event) EndedAt(now time.Time) bool {
	end := e.EndsAt()
	return end != nil && !end.After(now)
}

// ActiveAt is what `active` means since ADR 0066: published and not ended
// at now. It is derived, never stored; drafts and cancelled Events are never
// active.
func (e Event) ActiveAt(now time.Time) bool {
	return e.Status == StatusPublished && !e.EndedAt(now)
}

// FinalizeOpenAt reports whether Attendance finalization may run at now: the
// Event is published and has ended, or is undated (which never ends, so its
// organizers decide when). Finalization stays a manual act: the clock only
// opens it.
func (e Event) FinalizeOpenAt(now time.Time) bool {
	return e.Status == StatusPublished && (e.EndsAt() == nil || e.EndedAt(now))
}

// StatusError is the error for what needs a published Event, on e: nil when
// e is published, ErrNotPublished for a draft, ErrCancelled when cancelled.
func (e Event) StatusError() error {
	switch e.Status {
	case StatusPublished:
		return nil
	case StatusCancelled:
		return ErrCancelled
	default:
		return ErrNotPublished
	}
}
