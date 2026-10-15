package ticket

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
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
	// lists the form.
	FormResponseRecorded FormResponseOutcome = "recorded"
)

// FormResponseResult is what recording a report did.
type FormResponseResult struct {
	Outcome FormResponseOutcome
	// TicketsWritten counts the new Tickets: not the ones already there.
	TicketsWritten int
}

// RecordFormResponse writes the Ticket an accepted answer earns on every
// current Event that lists the form: a REGISTERED one for a person who
// answered signed in, a guest one for the e-mail a guest typed. An answer
// that is pending or declined, or that carries neither, writes nothing, and
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
	case FormResponseAccepted:
	case FormResponsePending, FormResponseDeclined:
		return FormResponseResult{Outcome: FormResponseNotAccepted}, nil
	default:
		return FormResponseResult{}, ErrInvalid
	}
	var guest GuestInfo
	switch {
	case r.UserID != nil:
		if *r.UserID == uuid.Nil {
			return FormResponseResult{}, ErrInvalid
		}
	case r.Guest != nil:
		guest = normalizeGuest(*r.Guest)
		if guest.FirstName == "" || guest.LastName == "" || guest.Email == "" {
			return FormResponseResult{}, ErrInvalid
		}
	default:
		return FormResponseResult{Outcome: FormResponseNoRespondent}, nil
	}
	listing, err := s.eventsListingForm(ctx, r.FormID)
	if err != nil {
		return FormResponseResult{}, err
	}
	if len(listing) == 0 {
		return FormResponseResult{Outcome: FormResponseNotListed}, nil
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
	for _, eventID := range listing {
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

func (s *service) eventsListingForm(ctx context.Context, formID uuid.UUID) ([]uuid.UUID, error) {
	events, err := s.events.List(ctx, "", false)
	if err != nil {
		return nil, err
	}
	var listing []uuid.UUID
	for _, ev := range events {
		if ev.ListsForm(formID) {
			listing = append(listing, ev.ID)
		}
	}
	return listing, nil
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
