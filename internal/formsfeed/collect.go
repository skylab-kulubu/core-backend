package formsfeed

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/events"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
)

// Collects queues the gathering of a form's answers (PullCollect) when an
// Event links it (event.FormsLinkedHook) or first saves its mapping. With
// EVENTS_FORMS_ENABLED off (On false) it queues nothing.
type Collects struct {
	Queue  PullQueue
	Events BackfillEvents
	On     bool
	Now    func() time.Time
	Logf   func(string, ...any)
}

func (c *Collects) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// FormsLinked queues each form's gathering on the Event: not for an
// archived, cancelled or ended Event, nor a form another current Event
// lists too (the one-place rule). It never fails the Event's write.
func (c *Collects) FormsLinked(ctx context.Context, e event.Event, formIDs []uuid.UUID) {
	if c == nil || !c.On || c.Queue == nil {
		return
	}
	now := c.now()
	if e.ArchivedAt != nil || e.Status == event.StatusCancelled || e.EndedAt(now) {
		return
	}
	for _, formID := range formIDs {
		outcome := "queued"
		if inUse, err := c.formInUse(ctx, e.ID, formID); err != nil {
			outcome = "failed"
		} else if inUse {
			outcome = "form_in_use"
		} else if queued, err := c.Queue.Collect(ctx, formID, e.ID, now.UTC()); err != nil {
			outcome = "failed"
		} else if !queued {
			outcome = "waiting"
		}
		if c.Logf != nil {
			c.Logf(`{"event":"form_collect_request","event_id":%q,"form_id":%q,"outcome":%q}`, e.ID.String(), formID.String(), outcome)
		}
	}
}

// formInUse reports whether another current Event lists the form.
func (c *Collects) formInUse(ctx context.Context, eventID, formID uuid.UUID) (bool, error) {
	evs, err := c.Events.ListIncludingDrafts(ctx, "")
	if err != nil {
		return false, err
	}
	for _, other := range evs {
		if other.ID != eventID && other.ArchivedAt == nil && (other.ListsForm(formID) || other.HasFeedbackForm(formID)) {
			return true, nil
		}
	}
	return false, nil
}

// collect runs a gathering (PullCollect, or a backfill the release before
// queued): every answer of the form that is not archived, a page at a
// time, each kept as an Unticketed respondent of the job's Event
// (keepCollected). It writes no Ticket and moves no watermark. An Event
// that no longer takes the form's answers (archived, cancelled, ended, the
// form gone from it or listed by another current Event too) ends the job
// with nothing done.
func (p *Puller) collect(ctx context.Context, job PullJob, res *pullResult) error {
	p.Recorder.init()
	rs := p.Recorder.Respondents
	if rs == nil || job.EventID == nil {
		res.outcomes["respondents_off"]++
		return nil
	}
	evs, err := p.Events.ListIncludingDrafts(ctx, "")
	if err != nil {
		return fmt.Errorf("reading the Events: %w", err)
	}
	var ev *event.Event
	now := p.now()
	for i := range evs {
		other := evs[i]
		if other.ArchivedAt != nil {
			continue
		}
		if other.ID == *job.EventID {
			ev = &evs[i]
		} else if other.ListsForm(job.FormID) || other.HasFeedbackForm(job.FormID) {
			res.outcomes["form_in_use"]++
			return nil
		}
	}
	switch {
	case ev == nil:
		res.outcomes["event_gone"]++
		return nil
	case ev.Status == event.StatusCancelled:
		res.outcomes["event_cancelled"]++
		return nil
	case ev.EndedAt(now):
		res.outcomes["event_ended"]++
		return nil
	case !ev.ListsForm(job.FormID):
		res.outcomes["not_listed"]++
		return nil
	}
	var mappings []Mapping
	all, err := p.Recorder.Mapped.Mappings(ctx, job.FormID)
	if err != nil {
		return fmt.Errorf("reading the form mappings: %w", err)
	}
	for _, m := range all {
		if m.EventID == ev.ID {
			mappings = append(mappings, m)
		}
	}
	q := ListQuery{After: job.Cursor, AllStatuses: true, Fields: MappedFields(mappings)}
	for {
		page, err := p.Forms.List(ctx, job.FormID, q)
		if err != nil {
			return err
		}
		res.pages++
		res.skipped += page.Skipped
		for _, item := range page.Items {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			outcome, err := p.keepCollected(ctx, rs, ev.ID, mappings, item)
			res.items++
			res.outcomes[outcome]++
			if err != nil {
				if !events.IsPermanent(err) {
					return err
				}
				res.failed++
			}
		}
		if page.NextCursor == "" {
			if job.Cursor != "" {
				// A run that went on from a saved page saw part of the
				// form: its counts are not the form's.
				return nil
			}
			return rs.Store.SaveCollectCounts(ctx, ev.ID, job.FormID, CollectCounts{
				MissingName: res.outcomes[ProblemMissingName], MissingEmail: res.outcomes[ProblemMissingEmail],
				NoPerson: res.outcomes["no_respondent"],
			}, p.now())
		}
		if page.NextCursor == q.After {
			return invalid("Forms' cursor did not move")
		}
		if res.pages >= maxPagesPerRun {
			return invalid("Forms' list goes on past %d pages", maxPagesPerRun)
		}
		if err := p.Queue.SaveCursor(ctx, job, page.NextCursor, p.now()); err != nil {
			return err
		}
		q.After = page.NextCursor
	}
}

// keepCollected keeps one gathered answer: a declined one's row goes; a
// guest's answer is read from the single answer endpoint for the guest's
// name and e-mail; the person (ResolveRespondents with the Event's mapping)
// is kept as an Unticketed respondent unless they hold a Ticket on the
// Event (RespondentStore.Keep). Its outcome is kept, ticketed, declined, or
// why nobody was kept (no_respondent, mapping_incomplete, gone).
func (p *Puller) keepCollected(ctx context.Context, rs *Respondents, eventID uuid.UUID, mappings []Mapping, item Response) (string, error) {
	if item.Status == StatusDeclined {
		if _, err := rs.Store.DropAnswer(ctx, item.ResponseID, []uuid.UUID{eventID}); err != nil {
			return OutcomeFailed, err
		}
		return "declined", nil
	}
	if item.Status != StatusAccepted && item.Status != StatusPending {
		return OutcomeInvalid, nil
	}
	var guest *Guest
	if item.UserID == nil && item.HasGuest {
		got, err := p.Recorder.read(ctx, item, MappedFields(mappings))
		switch {
		case errors.Is(err, ErrNotFound):
			return OutcomeGone, nil
		case err != nil:
			return OutcomeFailed, err
		}
		if got.Status == StatusDeclined {
			if _, err := rs.Store.DropAnswer(ctx, item.ResponseID, []uuid.UUID{eventID}); err != nil {
				return OutcomeFailed, err
			}
			return "declined", nil
		}
		guest, item.Status = got.Guest, got.Status
		if got.Answers != nil {
			item.Answers = got.Answers
		}
	}
	if item.UserID != nil || guest != nil {
		mappings = nil
	}
	respondents, unresolved := ResolveRespondents(item.UserID, guest, item.Answers, mappings)
	if unresolved != "" {
		// K3: why the mapping gives no guest, for the panel's counts.
		return MappedGuestProblem(mappings[0], item.Answers), nil
	}
	if len(respondents) == 0 || respondents[0].Source == RespondentNobody {
		return "no_respondent", nil
	}
	row, ok := respondentRow(respondents[0], eventID, item.FormID, item.ResponseID, item.SubmittedAt, item.Status, SourceCollect)
	if !ok {
		// Forms' named guest without a usable address or name.
		if guest == nil || !plainAddress(strings.ToLower(strings.TrimSpace(guest.Email))) {
			return ProblemMissingEmail, nil
		}
		return ProblemMissingName, nil
	}
	if row.UserID != nil && rs.Tickets != nil {
		if ok, err := rs.Tickets.EnsureFormRespondent(ctx, *row.UserID); err != nil {
			return OutcomeFailed, err
		} else if !ok {
			return string(ticket.FormResponsePersonUnavailable), nil
		}
	}
	kept, err := rs.keep(ctx, row)
	switch {
	case err != nil:
		return OutcomeFailed, err
	case kept:
		return "kept", nil
	}
	return "ticketed", nil
}
