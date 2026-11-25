package ticket

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// FormRespondent is an Unticketed respondent an organizer sends a Ticket
// (docs/form-respondents.md): the answer, and who it names, a signed-in
// person or a guest.
type FormRespondent struct {
	FormID     uuid.UUID
	ResponseID uuid.UUID
	UserID     *uuid.UUID
	Guest      *GuestInfo
}

// FormRespondentOutcome is what sending one Unticketed respondent's Ticket
// did. It names nobody.
type FormRespondentOutcome string

const (
	// FormRespondentRecorded is a new (or reopened) active Ticket.
	FormRespondentRecorded FormRespondentOutcome = "recorded"
	// FormRespondentWaitlisted is a Ticket on the Event's waiting list.
	FormRespondentWaitlisted FormRespondentOutcome = "waitlisted"
	// FormRespondentFull is a full Event with its waiting list off: no
	// Ticket; the respondent stays for a place that frees.
	FormRespondentFull FormRespondentOutcome = "full"
	// FormRespondentExisting is a person who holds a Ticket on the Event
	// already (another way wrote it, or a second answer of theirs): no
	// second one (the one Ticket rule).
	FormRespondentExisting FormRespondentOutcome = "existing"
	// FormRespondentUnavailable is a signed-in person core cannot find, or
	// whose account is blocked or erased, and a guest without a name or an
	// address: nobody to write a Ticket for.
	FormRespondentUnavailable FormRespondentOutcome = "unavailable"
)

// AdmitFormRespondent writes the Ticket of an Unticketed respondent on the
// Event, as the forms report's rule writes an accepted answer's on one
// Event (admitFormAnswer): REGISTERED for a person, a guest Ticket for a
// guest (untrusted: an existing guest is never renamed), on the waiting list
// when full and it is on, kept on the answer (Admission.FormResponse) so
// that Forms' later decline marks it. The same person twice writes no second
// Ticket. The organizer is adding the person, so closing applying does not
// refuse it (Registration closure); a cancelled Event is ErrEventCancelled,
// an ended one ErrEventEnded, no such current Event ErrNotFound. The
// Ticket's mail goes as any other's (at once, or at publishing on a draft).
//
// It checks nobody: formsfeed.Respondents asks first whether the caller may
// edit the Event, and its background job runs what such a caller asked.
func (s *service) AdmitFormRespondent(ctx context.Context, eventID uuid.UUID, r FormRespondent) (FormRespondentOutcome, error) {
	ev, err := s.events.Get(ctx, eventID)
	if errors.Is(err, event.ErrNotFound) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	switch {
	case ev.ArchivedAt != nil:
		return "", ErrNotFound
	case ev.Status == event.StatusCancelled:
		return "", ErrEventCancelled
	case ev.EndedAt(time.Now()):
		return "", ErrEventEnded
	}
	var guest GuestInfo
	switch {
	case r.UserID != nil && *r.UserID != uuid.Nil:
		if err := s.resolveApplyTarget(ctx, *r.UserID); err != nil {
			if err := settledFormResponse(err); err != nil {
				return "", err
			}
			return FormRespondentUnavailable, nil
		}
	case r.Guest != nil:
		guest = normalizeGuest(*r.Guest)
		if guest.Email == "" || guest.FirstName == "" || guest.LastName == "" {
			return FormRespondentUnavailable, nil
		}
	default:
		return FormRespondentUnavailable, nil
	}
	userID := r.UserID
	if userID != nil && *userID == uuid.Nil {
		userID = nil
	}
	got, written, err := s.admitFormAnswer(ctx, eventID, userID, guest, Admission{Waitlist: true, FormResponse: r.ResponseID})
	if err := s.keepAcceptedAnswer(ctx, got, FormResponse{FormID: r.FormID, ResponseID: r.ResponseID, UserID: userID}); err != nil {
		return "", err
	}
	switch {
	case errors.Is(err, ErrEventFull):
		return FormRespondentFull, nil
	case errors.Is(err, ErrConflict), errors.Is(err, ErrTicketWaitlisted):
		return FormRespondentExisting, nil
	case errors.Is(err, ErrNotFound) || errors.Is(err, user.ErrAccountBlocked):
		return FormRespondentUnavailable, nil
	case err != nil:
		return "", err
	case !written:
		return FormRespondentExisting, nil
	case got.StatusOf() == StatusWaitlisted:
		return FormRespondentWaitlisted, nil
	}
	return FormRespondentRecorded, nil
}
