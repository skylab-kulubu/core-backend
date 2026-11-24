package formsfeed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/events"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
)

// Ways an answer reaches core, the label of its log line and metric.
const (
	// ViaEvent is a core.forms event.
	ViaEvent = "event"
	// ViaPull is the internal list, read by the daily catch-up pass or an
	// organizer's backfill (rabbitmq-olaylar 18).
	ViaPull = "pull"
)

// Outcomes of an answer besides the Ticket rule's (ticket.FormResponseOutcome).
const (
	// OutcomeStale is an answer whose version is older than (an event: or
	// the same as) the one core handled: nothing changes.
	OutcomeStale = "stale"
	// OutcomeGone is an answer the single answer read did not find (404):
	// deleted since, or Forms has no such route yet.
	OutcomeGone = "gone"
	// OutcomeInvalid and OutcomeFailed are a report the Ticket rule refused
	// and one it could not finish.
	OutcomeInvalid = "invalid"
	OutcomeFailed  = "failed"
)

// EventLister reads the current Events and the Sessions' feedback forms
// (event.PostgresStore): which forms core knows.
type EventLister interface {
	ListIncludingDrafts(ctx context.Context, ownerTeam string) ([]event.Event, error)
	ListSessionFeedbackForms(ctx context.Context, eventID uuid.UUID) ([]event.SessionFeedbackForm, error)
}

// Reader reads one answer from Forms (Client).
type Reader interface {
	Response(ctx context.Context, formID, responseID uuid.UUID) (Response, error)
}

// RecordFunc is the Ticket rule (ticket.Service.RecordFormResponse with the
// forms service's authority): the one the HTTP report runs too.
type RecordFunc func(ctx context.Context, r ticket.FormResponse) (ticket.FormResponseResult, error)

// FormStateFunc takes a form's state (feedback.PostgresStore.ReportFormState):
// open with an optional closing time, or closed (a deleted form too).
type FormStateFunc func(ctx context.Context, formID uuid.UUID, open bool, closesAt *time.Time, now time.Time) (int, error)

// Recorder turns Forms' word about an answer or a form into what core does
// with it. Record and Events are required; Forms may be nil, and then an
// answer that needs the single answer read fails (transient).
type Recorder struct {
	Events EventLister
	Forms  Reader
	Record RecordFunc
	// States nil leaves form states unhandled (unrelated).
	States   FormStateFunc
	Versions Versions
	Now      func() time.Time
	Logf     func(string, ...any)

	once    sync.Once
	metrics *metrics
}

func (r *Recorder) init() {
	r.once.Do(func() {
		r.metrics = newMetrics()
		if r.Versions == nil {
			r.Versions = PostgresVersions{}
		}
	})
}

func (r *Recorder) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Recorder) logf(format string, args ...any) {
	if r.Logf != nil {
		r.Logf(format, args...)
	}
}

// formRole is what a form is to core.
type formRole int

const (
	roleNone formRole = iota
	// roleFeedback is a current Event's or Session's feedback form: its
	// answers mark invites, never write Tickets.
	roleFeedback
	// roleApplication is a form a current Event lists as its application
	// or an extra form.
	roleApplication
)

// role reads the Events as the Ticket rule does: a feedback form first.
func (r *Recorder) role(ctx context.Context, formID uuid.UUID) (formRole, error) {
	evs, err := r.Events.ListIncludingDrafts(ctx, "")
	if err != nil {
		return roleNone, err
	}
	listed := false
	for _, ev := range evs {
		if ev.HasFeedbackForm(formID) {
			return roleFeedback, nil
		}
		listed = listed || ev.ListsForm(formID)
	}
	sessionForms, err := r.Events.ListSessionFeedbackForms(ctx, uuid.Nil)
	if err != nil {
		return roleNone, err
	}
	for _, f := range sessionForms {
		if id, ok := event.FormIDFromURL(f.URL); ok && id == formID {
			return roleFeedback, nil
		}
	}
	if listed {
		return roleApplication, nil
	}
	return roleNone, nil
}

// Origin is where an answer came from, for its log line.
type Origin struct {
	Via string
	// EventID is the core.forms event's id; empty for a pull.
	EventID string
}

// Response handles one answer and answers its outcome (a
// ticket.FormResponseOutcome or one of the Outcome constants):
//
//  1. A form no current Event lists, nor uses for feedback, is
//     not_listed: nothing is read or written (an event of it is unrelated).
//  2. Its version, in tx: an event's older or equal to the one kept, or a
//     pull's older, is stale.
//  3. An accepted answer to an application form is read from Forms when the
//     data lacks what a Ticket needs: a guest's name and e-mail (never in an
//     event), and submittedAt and hidden fields, which a status change does
//     not carry (complete false). The read is the answer's state now; a
//     newer version than the event's is kept as the latest.
//  4. The Ticket rule, the HTTP report's: submittedAt against the
//     registration closure, the davet hidden field as the feedback invite,
//     capacity and waiting list, a declined answer's handling.
//
// An error the Ticket rule refuses (ticket.ErrInvalid) or a read that finds
// nothing (ErrNotFound) is permanent; any other is transient.
func (r *Recorder) Response(ctx context.Context, tx pgx.Tx, origin Origin, in Response, complete bool) (string, error) {
	r.init()
	in.Guest = nil
	role, err := r.role(ctx, in.FormID)
	if err != nil {
		return r.done(origin, in, OutcomeFailed, ticket.FormResponseResult{}, fmt.Errorf("reading the Events: %w", err))
	}
	if role == roleNone {
		return r.done(origin, in, string(ticket.FormResponseNotListed), ticket.FormResponseResult{}, nil)
	}
	advanced, err := r.Versions.Advance(ctx, tx, KindResponse, in.ResponseID, in.Version, origin.Via == ViaPull)
	if err != nil {
		return r.done(origin, in, OutcomeFailed, ticket.FormResponseResult{}, fmt.Errorf("keeping the answer's version: %w", err))
	}
	if !advanced {
		return r.done(origin, in, OutcomeStale, ticket.FormResponseResult{}, nil)
	}
	if role == roleApplication && in.Status == StatusAccepted && (!complete || (in.UserID == nil && in.HasGuest)) {
		got, err := r.read(ctx, in)
		if errors.Is(err, ErrNotFound) {
			return r.done(origin, in, OutcomeGone, ticket.FormResponseResult{}, events.Permanent(err))
		}
		if err != nil {
			return r.done(origin, in, OutcomeFailed, ticket.FormResponseResult{}, err)
		}
		if got.Version > in.Version {
			if _, err := r.Versions.Advance(ctx, tx, KindResponse, in.ResponseID, got.Version, true); err != nil {
				return r.done(origin, in, OutcomeFailed, ticket.FormResponseResult{}, fmt.Errorf("keeping the answer's version: %w", err))
			}
		}
		if got.HiddenFields == nil {
			got.HiddenFields = in.HiddenFields
		}
		in = got
	}
	report := ticket.FormResponse{
		FormID: in.FormID, ResponseID: in.ResponseID, Status: ticket.FormResponseStatus(in.Status),
		UserID: in.UserID, SubmittedAt: in.SubmittedAt, Invite: in.Invite(),
	}
	if in.UserID == nil && in.Guest != nil {
		report.Guest = &ticket.GuestInfo{FirstName: in.Guest.FirstName, LastName: in.Guest.LastName, Email: in.Guest.Email}
	}
	result, err := r.Record(ctx, report)
	switch {
	case errors.Is(err, ticket.ErrInvalid), errors.Is(err, ticket.ErrForbidden):
		return r.done(origin, in, OutcomeInvalid, result, events.Permanent(fmt.Errorf("the Ticket rule refused the answer: %w", err)))
	case err != nil:
		return r.done(origin, in, OutcomeFailed, result, fmt.Errorf("the Ticket rule: %w", err))
	}
	return r.done(origin, in, string(result.Outcome), result, nil)
}

// read is the single answer read; a guest's answer must name the guest.
func (r *Recorder) read(ctx context.Context, in Response) (Response, error) {
	if r.Forms == nil {
		r.metrics.read("off")
		return Response{}, errors.New("formsfeed: Forms' internal address is not set")
	}
	got, err := r.Forms.Response(ctx, in.FormID, in.ResponseID)
	switch {
	case errors.Is(err, ErrNotFound):
		r.metrics.read("not_found")
	case errors.Is(err, ErrForbidden):
		r.metrics.read("forbidden")
	case errors.Is(err, ErrInvalid):
		r.metrics.read("invalid")
		return Response{}, events.Permanent(err)
	case err != nil:
		r.metrics.read("failed")
	default:
		r.metrics.read("ok")
	}
	return got, err
}

// done writes the answer's log line and counts it. The line carries ids, the
// status and the outcome, never who answered (the userId, a guest's name or
// e-mail) and never err, which the consumer logs masked.
func (r *Recorder) done(origin Origin, in Response, outcome string, result ticket.FormResponseResult, err error) (string, error) {
	r.metrics.answer(origin.Via, outcome)
	if in.SubmittedAtIgnored {
		r.logf(`{"event":"form_response_submitted_at_ignored","level":"warn","form_id":%q,"reason":"not an RFC 3339 time with a zone"}`, in.FormID.String())
	}
	payload, marshalErr := json.Marshal(struct {
		Event          string `json:"event"`
		Via            string `json:"via"`
		EventID        string `json:"event_id,omitempty"`
		FormID         string `json:"form_id"`
		ResponseID     string `json:"response_id"`
		Status         string `json:"status"`
		Outcome        string `json:"outcome"`
		TicketsWritten int    `json:"tickets_written"`
		ListingEvents  int    `json:"listing_events"`
	}{"form_response", origin.Via, origin.EventID, in.FormID.String(), in.ResponseID.String(), in.Status, outcome, result.TicketsWritten, result.ListingEvents})
	if marshalErr == nil {
		r.logf("%s", payload)
	}
	return outcome, err
}

// FormState handles forms.form.updated: in version order, the state goes to
// the feedback reminders (a closed form's are cancelled for good; only a
// feedback form's state is kept, docs/event-feedback.md).
func (r *Recorder) FormState(ctx context.Context, tx pgx.Tx, origin Origin, st FormState) error {
	r.init()
	if r.States == nil {
		return events.ErrUnrelated
	}
	advanced, err := r.Versions.Advance(ctx, tx, KindForm, st.FormID, st.Version, false)
	if err != nil {
		return fmt.Errorf("keeping the form's version: %w", err)
	}
	if !advanced {
		r.metrics.formState(OutcomeStale)
		r.logf(`{"event":"form_state","via":%q,"event_id":%q,"form_id":%q,"status":%q,"outcome":"stale"}`, origin.Via, origin.EventID, st.FormID.String(), st.Status)
		return nil
	}
	cancelled, err := r.States(ctx, st.FormID, st.Status == FormOpen, st.ClosesAt, r.now().UTC())
	if err != nil {
		r.metrics.formState(OutcomeFailed)
		return fmt.Errorf("taking the form's state: %w", err)
	}
	r.metrics.formState("processed")
	r.logf(`{"event":"form_state","via":%q,"event_id":%q,"form_id":%q,"status":%q,"outcome":"processed","reminders_cancelled":%d}`,
		origin.Via, origin.EventID, st.FormID.String(), st.Status, cancelled)
	return nil
}

// Prometheus is the recorder's counters.
func (r *Recorder) Prometheus() string {
	r.init()
	return r.metrics.prometheus()
}
