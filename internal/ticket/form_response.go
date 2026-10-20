package ticket

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// FormResponseStatus is where an answer to a Skyforms form stands in the
// form's review.
type FormResponseStatus string

const (
	// FormResponsePending is an answer that waits for the form team's review.
	FormResponsePending FormResponseStatus = "pending"
	// FormResponseAccepted is an answer the form takes: one that needs no
	// review, or one the team approved.
	FormResponseAccepted FormResponseStatus = "accepted"
	// FormResponseDeclined is an answer the team declined.
	FormResponseDeclined FormResponseStatus = "declined"
)

// FormResponse is what the forms service reports about one answer to one of
// its forms (docs/form-response-tickets.md).
type FormResponse struct {
	FormID     uuid.UUID
	ResponseID uuid.UUID
	Status     FormResponseStatus
	// UserID is the person who answered signed in. It wins over Guest.
	UserID *uuid.UUID
	// Guest is what a guest typed into the form's identity fields.
	Guest *GuestInfo
	// SubmittedAt is when the forms service first took the answer, when it
	// says: an answer sent after its Event closed applying earns no Ticket.
	// Without it closing applying refuses no answer (ticket 12); a time to
	// come counts as now.
	SubmittedAt *time.Time
}

// FormResponseOutcome is how recording a report ended, for the report's log
// line. It names nobody.
type FormResponseOutcome string

const (
	// FormResponseNotAccepted is a pending or declined answer: nothing to
	// write.
	FormResponseNotAccepted FormResponseOutcome = "not_accepted"
	// FormResponseNoRespondent is an answer with neither a userId nor a
	// guest.
	FormResponseNoRespondent FormResponseOutcome = "no_respondent"
	// FormResponseNotListed is a form no current Event lists.
	FormResponseNotListed FormResponseOutcome = "not_listed"
	// FormResponsePersonUnavailable is a person core cannot find, or whose
	// account is blocked or no longer active.
	FormResponsePersonUnavailable FormResponseOutcome = "person_unavailable"
	// FormResponseRecorded is a report that went through every Event that
	// lists the form and is neither cancelled nor, for an answer sent after
	// it, closed to applications.
	FormResponseRecorded FormResponseOutcome = "recorded"
	// FormResponseEventCancelled is an answer to a form whose every Event
	// is cancelled (ticket 04): no Ticket is written. The forms service is
	// told so in the answer, still a 2xx, so that it does not send the
	// report again.
	FormResponseEventCancelled FormResponseOutcome = "event_cancelled"
	// FormResponseRegistrationClosed is an answer the forms service says
	// was sent (submittedAt) once every Event of the form left that was not
	// cancelled had closed applying, by hand or by the deadline (ticket
	// 12): no Ticket is written; a 2xx too.
	FormResponseRegistrationClosed FormResponseOutcome = "registration_closed"
	// FormResponseFeedback is an answer to a feedback form (etkinlik geri
	// bildirim formu, ticket 02): it writes no Ticket, whatever its status
	// and whoever answered.
	FormResponseFeedback FormResponseOutcome = "feedback"
)

// FormResponseResult is what recording a report did.
type FormResponseResult struct {
	Outcome FormResponseOutcome
	// TicketsWritten counts the new Tickets: not the ones already there.
	TicketsWritten int
}

// RecordFormResponse writes nothing for an answer to a feedback form
// (FormResponseFeedback). Otherwise it writes the Ticket an accepted answer
// earns on every
// current Event that lists the form and is not cancelled (when every one
// is, the outcome is FormResponseEventCancelled), and, when the report says
// when the answer was sent, had not closed applying by then (when every one
// left had, FormResponseRegistrationClosed): a REGISTERED one for a person who
// answered signed in, a guest one for the e-mail a guest typed. An answer
// that is pending or declined, or that carries neither (a guest without an
// e-mail counts as neither), writes nothing, and
// a report sent again finds the Tickets already there. A declined answer
// does not take back a Ticket written earlier: Tickets cannot be cancelled.
//
// A guest is written as Guest apply writes one for an untrusted caller: the
// report carries what an anonymous form filler typed, so it may only fill a
// detail the stored Ticket lacks and never renames a guest.
func (s *service) RecordFormResponse(ctx context.Context, p authz.Principal, r FormResponse) (FormResponseResult, error) {
	if !s.CanRecordFormResponse(p) {
		return FormResponseResult{}, ErrForbidden
	}
	if r.FormID == uuid.Nil {
		return FormResponseResult{}, ErrInvalid
	}
	switch r.Status {
	case FormResponseAccepted, FormResponsePending, FormResponseDeclined:
	default:
		return FormResponseResult{}, ErrInvalid
	}
	// A draft's form writes Tickets too (ADR 0066): its editors linked the
	// form and shared it; an answer not written now would be lost.
	events, err := s.events.ListIncludingDrafts(ctx, "")
	if err != nil {
		return FormResponseResult{}, err
	}
	for _, ev := range events {
		if ev.HasFeedbackForm(r.FormID) {
			return FormResponseResult{Outcome: FormResponseFeedback}, nil
		}
	}
	if r.Status != FormResponseAccepted {
		return FormResponseResult{Outcome: FormResponseNotAccepted}, nil
	}
	var guest GuestInfo
	switch {
	case r.UserID != nil:
		if *r.UserID == uuid.Nil {
			return FormResponseResult{}, ErrInvalid
		}
	case r.Guest != nil:
		guest = normalizeGuest(*r.Guest)
		if guest.Email == "" {
			// Account erasure removed the address from a report Forms still
			// held (docs/account-erasure-command.md §10): nobody to ticket.
			return FormResponseResult{Outcome: FormResponseNoRespondent}, nil
		}
		if guest.FirstName == "" || guest.LastName == "" {
			return FormResponseResult{}, ErrInvalid
		}
	default:
		return FormResponseResult{Outcome: FormResponseNoRespondent}, nil
	}
	listing := listingForm(events, r.FormID)
	if len(listing) == 0 {
		return FormResponseResult{Outcome: FormResponseNotListed}, nil
	}
	listing = notCancelled(listing)
	if len(listing) == 0 {
		return FormResponseResult{Outcome: FormResponseEventCancelled}, nil
	}
	if r.SubmittedAt != nil {
		// A time to come is taken as now: an answer is not sent later than
		// it is reported.
		listing = openWhenSubmitted(listing, earlier(*r.SubmittedAt, time.Now()))
		if len(listing) == 0 {
			return FormResponseResult{Outcome: FormResponseRegistrationClosed}, nil
		}
	}
	if r.UserID != nil {
		if err := s.resolveApplyTarget(ctx, *r.UserID); err != nil {
			if err := settledFormResponse(err); err != nil {
				return FormResponseResult{}, err
			}
			return FormResponseResult{Outcome: FormResponsePersonUnavailable}, nil
		}
	}
	result := FormResponseResult{Outcome: FormResponseRecorded}
	for _, ev := range listing {
		eventID := ev.ID
		written := false
		if r.UserID != nil {
			_, err = s.applyRegistered(ctx, eventID, *r.UserID)
			written = err == nil
		} else {
			var applied GuestApplication
			applied, err = s.writeGuest(ctx, eventID, guest, false)
			written = err == nil && applied.Result == GuestCreated
		}
		if err := settledFormResponse(err); err != nil {
			return FormResponseResult{}, err
		}
		if written {
			result.TicketsWritten++
		}
	}
	return result, nil
}

// CanRecordFormResponse reports whether p may report form answers: the
// handler asks before it reads the request.
func (s *service) CanRecordFormResponse(p authz.Principal) bool {
	return s.authz.Allow(p, authz.Resource{Type: authz.TypeFormResponse}, authz.Create)
}

// listingForm are the events that list the form as an application or extra
// form.
func listingForm(events []event.Event, formID uuid.UUID) []event.Event {
	var listing []event.Event
	for _, ev := range events {
		if ev.ListsForm(formID) {
			listing = append(listing, ev)
		}
	}
	return listing
}

// notCancelled leaves out the cancelled Events: an answer to their form
// writes them no Ticket (ticket 04).
func notCancelled(listing []event.Event) []event.Event {
	out := listing[:0:0]
	for _, ev := range listing {
		if ev.Status != event.StatusCancelled {
			out = append(out, ev)
		}
	}
	return out
}

// openWhenSubmitted leaves out the Events whose organizer had closed
// applying, by hand or by the deadline, when the answer was sent
// (event.ApplyingClosedAt): Yusuf, 2026-10-20. Closing applying never
// refuses an answer whose report does not say when it was sent: an answer
// sent in time and approved after the closure still earns its Ticket, and
// one the forms service reports late still does. An ended Event takes its
// forms' answers (feedback forms come after it).
func openWhenSubmitted(listing []event.Event, submitted time.Time) []event.Event {
	out := listing[:0:0]
	for _, ev := range listing {
		if !ev.ApplyingClosedAt(submitted) {
			out = append(out, ev)
		}
	}
	return out
}

func earlier(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// settledFormResponse treats as done what sending the report again would not
// change: a Ticket already there, and a person core cannot find or whose
// account is blocked or no longer active (erased since they answered).
func settledFormResponse(err error) error {
	if errors.Is(err, ErrConflict) || errors.Is(err, ErrNotFound) || errors.Is(err, user.ErrAccountBlocked) {
		return nil
	}
	return err
}
