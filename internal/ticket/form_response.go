package ticket

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/feedback"
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
	// Invite is the feedback invite's code the answer came with (the
	// ?davet= of the mail's link), when Forms reports it: it marks that
	// invite answered (etkinlik geri bildirim formu, ticket 04).
	Invite string
}

// FeedbackAnswers marks who answered a feedback form: the invite the code
// names when it is the form's, else the form's invites of the signed-in
// answerer's Tickets (feedback.PostgresStore).
type FeedbackAnswers interface {
	MarkAnswered(ctx context.Context, formID uuid.UUID, code string, userID *uuid.UUID) (bool, error)
}

// FeedbackLinks are the Tickets' feedback links, by Ticket
// (feedback.PostgresStore): what the holder's own tickets answer carries.
type FeedbackLinks interface {
	TicketLinks(ctx context.Context, ticketIDs []uuid.UUID, now time.Time) (map[uuid.UUID][]feedback.TicketLink, error)
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
	// FormResponseEventEnded is an answer to a form whose every Event left
	// (not cancelled) has ended (ADR 0069, addendum of 2026-11-22): an ended
	// Event takes no new application, so no Ticket is written; a 2xx too.
	FormResponseEventEnded FormResponseOutcome = "event_ended"
	// FormResponseFeedback is an answer to a feedback form, an Event's or a
	// Session's (etkinlik geri bildirim formu, tickets 02 and 03): it writes
	// no Ticket, whatever its status and whoever answered.
	FormResponseFeedback FormResponseOutcome = "feedback"
	// FormResponseWaitlisted is an accepted answer that found every Event
	// of its form full, and is on the waiting list of at least one
	// (docs/event-capacity.md): a waitlisted Ticket, a 2xx too.
	FormResponseWaitlisted FormResponseOutcome = "waitlisted"
	// FormResponseEventFull is an accepted answer that found every Event of
	// its form full with the waiting list off: no Ticket, a 2xx too.
	FormResponseEventFull FormResponseOutcome = "event_full"
	// FormResponseCancelled was a declined answer that cancelled the
	// Tickets it had written (kontenjan-iptal-hatirlatma 05). Since K6
	// (Yusuf, 2026-11-25) a decline cancels nothing; the outcome stays
	// named for the logs and metrics read before.
	FormResponseCancelled FormResponseOutcome = "cancelled"
	// FormResponseFormsRejected is a declined answer that marked the
	// Tickets it had written (Ticket.FormsRejectedAt; K6): the panel warns
	// and offers to cancel them. A 204 still.
	FormResponseFormsRejected FormResponseOutcome = "forms_rejected"
)

// FormResponseResult is what recording a report did.
type FormResponseResult struct {
	Outcome FormResponseOutcome
	// TicketsWritten counts the new Tickets: not the ones already there.
	TicketsWritten int
	// TicketsCancelled counts the Tickets a declined answer cancelled:
	// none since K6.
	TicketsCancelled int
	// TicketsMarked counts the Tickets a declined answer marked rejected in
	// Forms (K6).
	TicketsMarked int
	// ListingEvents counts the current Events that list the form as an
	// application or extra form, whatever their status: more than one is a
	// form shared before the one-place rule (ADR 0069, addendum of
	// 2026-11-22), which the log line shows.
	ListingEvents int
}

// RecordFormResponse writes nothing for an answer to a feedback form
// (FormResponseFeedback). Otherwise it writes the Ticket an accepted answer
// earns on every
// current Event that lists the form and is not cancelled (when every one
// is, the outcome is FormResponseEventCancelled) nor ended (when every one
// left has, FormResponseEventEnded), and, when the report says
// when the answer was sent, had not closed applying by then (when every one
// left had, FormResponseRegistrationClosed): a REGISTERED one for a person who
// answered signed in, a guest one for the e-mail a guest typed. An answer
// that is pending or declined, or that carries neither (a guest without an
// e-mail counts as neither), writes nothing, and
// a report sent again finds the Tickets already there. A declined answer
// cancels the Tickets the same answer wrote or reopened
// (declineFormResponse), and nothing else.
//
// A guest is written as Guest apply writes one for an untrusted caller: the
// report carries what an anonymous form filler typed, so it may only fill a
// detail the stored Ticket lacks and never renames a guest.
//
// Each Event decides under its own capacity (docs/event-capacity.md): a
// full one puts the answer on its waiting list while that is on, else
// writes nothing. The outcome is FormResponseRecorded when any Event has
// the person on an active Ticket, else FormResponseWaitlisted when any has
// them waiting, else FormResponseEventFull.
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
	isFeedback := false
	for _, ev := range events {
		isFeedback = isFeedback || ev.HasFeedbackForm(r.FormID)
	}
	if !isFeedback {
		sessionForms, err := s.events.ListSessionFeedbackForms(ctx, uuid.Nil)
		if err != nil {
			return FormResponseResult{}, err
		}
		for _, f := range sessionForms {
			if id, ok := event.FormIDFromURL(f.URL); ok && id == r.FormID {
				isFeedback = true
			}
		}
	}
	if isFeedback {
		return s.recordFeedbackAnswer(ctx, r)
	}
	if r.Status == FormResponseDeclined {
		return s.declineFormResponse(ctx, events, r)
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
	listed := len(listing)
	if listed == 0 {
		return FormResponseResult{Outcome: FormResponseNotListed}, nil
	}
	listing = notCancelled(listing)
	if len(listing) == 0 {
		return FormResponseResult{Outcome: FormResponseEventCancelled, ListingEvents: listed}, nil
	}
	now := time.Now()
	listing = notEnded(listing, now)
	if len(listing) == 0 {
		return FormResponseResult{Outcome: FormResponseEventEnded, ListingEvents: listed}, nil
	}
	if r.SubmittedAt != nil {
		// A time to come is taken as now: an answer is not sent later than
		// it is reported.
		listing = openWhenSubmitted(listing, earlier(*r.SubmittedAt, now))
		if len(listing) == 0 {
			return FormResponseResult{Outcome: FormResponseRegistrationClosed, ListingEvents: listed}, nil
		}
	}
	if r.UserID != nil {
		if err := s.resolveApplyTarget(ctx, *r.UserID); err != nil {
			if err := settledFormResponse(err); err != nil {
				return FormResponseResult{}, err
			}
			return FormResponseResult{Outcome: FormResponsePersonUnavailable, ListingEvents: listed}, nil
		}
	}
	result := FormResponseResult{Outcome: FormResponseRecorded, ListingEvents: listed}
	admission := Admission{Waitlist: true, FormResponse: r.ResponseID}
	var active, waitlisted, full int
	for _, ev := range listing {
		eventID := ev.ID
		written := false
		var got Ticket
		if r.UserID != nil {
			got, err = s.applyRegistered(ctx, eventID, *r.UserID, admission)
			written = err == nil
			if errors.Is(err, ErrConflict) || errors.Is(err, ErrTicketWaitlisted) {
				// The Ticket already there: its status tells the outcome.
				if existing, getErr := s.tickets.GetByOwnerEvent(ctx, *r.UserID, eventID); getErr == nil {
					got = existing
				}
			}
		} else {
			var applied GuestApplication
			applied, err = s.writeGuest(ctx, eventID, guest, false, admission)
			written = err == nil && applied.Result == GuestCreated
			got = applied.Ticket
		}
		switch {
		case errors.Is(err, ErrEventFull):
			full++
			continue
		case got.ID != uuid.Nil && got.StatusOf() == StatusCancelled:
			// A cancelled Ticket the answer did not reopen: counted neither way.
		case got.ID != uuid.Nil && got.StatusOf() == StatusWaitlisted:
			waitlisted++
		case errors.Is(err, ErrTicketWaitlisted):
			waitlisted++
		case err == nil || errors.Is(err, ErrConflict):
			active++
		}
		if err := settledFormResponse(err); err != nil {
			return FormResponseResult{}, err
		}
		if err := s.keepAcceptedAnswer(ctx, got, r.ResponseID); err != nil {
			return FormResponseResult{}, err
		}
		if written {
			result.TicketsWritten++
		}
	}
	switch {
	case active > 0:
	case waitlisted > 0:
		result.Outcome = FormResponseWaitlisted
	case full > 0:
		result.Outcome = FormResponseEventFull
	}
	return result, nil
}

// declineFormResponse marks what a declined answer had earned (Yusuf, K6
// 2026-11-25; before it, kontenjan-iptal-hatirlatma 05 cancelled it): the
// Tickets the same answer wrote or reopened, or the holder's newest
// accepted answer was kept on (tickets.form_response_id), on the Events
// that list the form and have not ended, active or waitlisted. Each gets
// FormsRejectedAt; none is cancelled, nobody is mailed. The panel shows
// "Forms'ta reddedildi" and an operator cancels with one click. A Ticket
// another way wrote, or written before the answer was kept, is not touched.
// The outcome is FormResponseFormsRejected when any was marked, else
// FormResponseNotAccepted.
func (s *service) declineFormResponse(ctx context.Context, events []event.Event, r FormResponse) (FormResponseResult, error) {
	result := FormResponseResult{Outcome: FormResponseNotAccepted}
	found, ok := s.tickets.(FormResponseStore)
	marks, marking := s.tickets.(FormsRejectionStore)
	if !ok || !marking || r.ResponseID == uuid.Nil {
		return result, nil
	}
	listing := listingForm(events, r.FormID)
	result.ListingEvents = len(listing)
	now := time.Now()
	listing = notEnded(listing, now)
	if len(listing) == 0 {
		return result, nil
	}
	tickets, err := found.FormResponseTickets(ctx, r.ResponseID)
	if err != nil {
		return FormResponseResult{}, err
	}
	for _, t := range tickets {
		if t.StatusOf() == StatusCancelled || !listsEvent(listing, t.EventID) {
			continue
		}
		marked, err := marks.MarkFormsRejected(ctx, t.ID, now)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return FormResponseResult{}, err
		}
		if marked {
			result.TicketsMarked++
		}
	}
	if result.TicketsMarked > 0 {
		result.Outcome = FormResponseFormsRejected
	}
	return result, nil
}

// keepAcceptedAnswer makes an accepted answer the one a Ticket it found or
// wrote keeps (FormsRejectionStore.KeepFormResponse), clearing a mark an
// earlier answer's decline left: a corrected answer approved after its
// first one was declined (Forms' Returned) keeps the Ticket unmarked, and
// the old answer's decline arriving late no longer reaches it.
func (s *service) keepAcceptedAnswer(ctx context.Context, t Ticket, responseID uuid.UUID) error {
	marks, ok := s.tickets.(FormsRejectionStore)
	if !ok || t.ID == uuid.Nil || responseID == uuid.Nil || t.StatusOf() == StatusCancelled {
		return nil
	}
	return marks.KeepFormResponse(ctx, t.ID, responseID)
}

func listsEvent(events []event.Event, id uuid.UUID) bool {
	for _, ev := range events {
		if ev.ID == id {
			return true
		}
	}
	return false
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

// notEnded leaves out the Events that have ended at now (event.EndedAt):
// an ended Event takes no new application from its forms (ADR 0069,
// addendum of 2026-11-22; feedback forms, answered after the Event, are
// apart and write no Ticket). An answer approved after the end earns no
// Ticket either: the door and the certificates need a Check-in by then. An
// Event without dates never ends. A draft is judged by its dates too.
func notEnded(listing []event.Event, now time.Time) []event.Event {
	out := listing[:0:0]
	for _, ev := range listing {
		if !ev.EndedAt(now) {
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
// one the forms service reports late still does.
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
// change: a Ticket already there (active or waitlisted), and a person core
// cannot find or whose account is blocked or no longer active (erased since
// they answered).
func settledFormResponse(err error) error {
	if errors.Is(err, ErrConflict) || errors.Is(err, ErrTicketWaitlisted) || errors.Is(err, ErrNotFound) || errors.Is(err, user.ErrAccountBlocked) {
		return nil
	}
	return err
}

// recordFeedbackAnswer marks the invite a feedback answer answers (ticket
// 04): a pending or accepted answer counts, a declined one changes nothing
// (and does not take back an earlier mark). Nothing matched is no error.
func (s *service) recordFeedbackAnswer(ctx context.Context, r FormResponse) (FormResponseResult, error) {
	result := FormResponseResult{Outcome: FormResponseFeedback}
	if s.feedback == nil || r.Status == FormResponseDeclined {
		return result, nil
	}
	code := r.Invite
	if len(code) > feedback.MaxReportedInvite {
		code = ""
	}
	if _, err := s.feedback.MarkAnswered(ctx, r.FormID, code, r.UserID); err != nil {
		return FormResponseResult{}, err
	}
	return result, nil
}
