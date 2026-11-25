package ticket

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/teamaudit"
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

// KeepFormRespondent applies an accepted answer to the Ticket the person
// already holds on the Event, without writing one: the rule's
// keepAcceptedAnswer (#460), for an Event whose "Otomatik bilet gönder" is
// off. The answer accepted again clears Forms' rejection and is accepted
// again; a newer answer of the same holder to the same form takes the
// Ticket over when the Ticket's answer is no longer accepted (Returned). No
// Ticket there: nothing.
func (s *service) KeepFormRespondent(ctx context.Context, eventID uuid.UUID, r FormRespondent) error {
	var t Ticket
	var err error
	switch {
	case r.UserID != nil && *r.UserID != uuid.Nil:
		t, err = s.tickets.GetByOwnerEvent(ctx, *r.UserID, eventID)
	case r.Guest != nil:
		email := normalizeGuest(*r.Guest).Email
		if email == "" {
			return nil
		}
		t, err = s.tickets.GetByGuestEvent(ctx, email, eventID)
	default:
		return nil
	}
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	userID := r.UserID
	if userID != nil && *userID == uuid.Nil {
		userID = nil
	}
	return s.keepAcceptedAnswer(ctx, t, FormResponse{FormID: r.FormID, ResponseID: r.ResponseID, UserID: userID})
}

// EnsureFormRespondent makes sure core has the signed-in person's account
// row before an Unticketed respondent names them (the row's user_id is a
// current-identity link): as the rule does before it writes their Ticket.
// False for a person core cannot find, or whose account is blocked or no
// longer active.
func (s *service) EnsureFormRespondent(ctx context.Context, userID uuid.UUID) (bool, error) {
	if err := s.resolveApplyTarget(ctx, userID); err != nil {
		if err := settledFormResponse(err); err != nil {
			return false, err
		}
		return false, nil
	}
	return true, nil
}

// FormRespondentSummary counts an organizer's sending of Unticketed
// respondents' Tickets, for its Team audit row.
type FormRespondentSummary struct {
	Rows, Recorded, Waitlisted, Full, Existing, Members, Guests int
	// Skipped are a send-all job's rows sent no Ticket; JobID the job.
	Skipped int
	JobID   *uuid.UUID
}

// FormRespondentSend is the Team audit row of one sending
// (BeginFormRespondentSend); nil when the record is off.
type FormRespondentSend struct {
	s      *service
	record *auditRecord
}

// BeginFormRespondentSend opens the Team audit row of an organizer's sending
// of Unticketed respondents' Tickets (docs/form-respondents.md), as an
// attendee import's (ticket.import: the Event, no one Ticket; privileged,
// else the Leader, else the Event's editor). With the record on and no row,
// the sending is refused (teamaudit.ErrUnavailable): no row, no Ticket.
func (s *service) BeginFormRespondentSend(ctx context.Context, p authz.Principal, eventID uuid.UUID) (*FormRespondentSend, error) {
	ev, err := s.events.Get(ctx, eventID)
	if errors.Is(err, event.ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	record, err := s.beginTicketAuditBasis(ctx, p, ev, teamaudit.ActionTicketImport, uuid.Nil, s.privilegedOperator(p), true)
	if err != nil {
		return nil, err
	}
	return &FormRespondentSend{s: s, record: record}, nil
}

// ID is the sending's row id; uuid.Nil when the record is off. A send-all
// job takes it as its own id, so that the job's end finishes the row
// (FinishFormRespondentJob).
func (a *FormRespondentSend) ID() uuid.UUID {
	if a == nil || a.record == nil {
		return uuid.Nil
	}
	return a.record.id
}

// Finish writes the sending's outcome and counts to its row.
func (a *FormRespondentSend) Finish(ctx context.Context, sum FormRespondentSummary, err error) {
	if a == nil || a.record == nil {
		return
	}
	if ferr := a.finish(ctx, sum, err); ferr != nil {
		a.s.logger.Printf("tickets: audit record %s stays pending", a.record.id)
	}
}

func (a *FormRespondentSend) finish(ctx context.Context, sum FormRespondentSummary, err error) error {
	outcome, code := teamaudit.OutcomeOK, ""
	if err != nil {
		outcome, code = teamaudit.OutcomeFailed, "failed"
	}
	after := a.record.after
	after.Import = &teamaudit.ImportCounts{
		Rows: sum.Rows, Created: sum.Recorded, Waitlisted: sum.Waitlisted, Members: sum.Members, Guests: sum.Guests,
		Existing: sum.Existing, Full: sum.Full, Mail: true,
		Source: teamaudit.SourceFormRespondents, Skipped: sum.Skipped,
	}
	if sum.JobID != nil {
		after.Import.JobID = sum.JobID.String()
	}
	return a.s.audit.Finish(ctx, a.record.id, outcome, code, &after)
}

// FinishFormRespondentJob writes a send-all job's outcome and its own
// counts to the row its request opened, which stays pending while the job
// runs (the row's id is the job's). A job queued with the record off, or
// one another request had queued, has no pending row: nothing.
func (s *service) FinishFormRespondentJob(ctx context.Context, eventID, jobID uuid.UUID, sum FormRespondentSummary, err error) {
	if s.audit == nil {
		return
	}
	sum.JobID = &jobID
	a := &FormRespondentSend{s: s, record: &auditRecord{id: jobID, after: teamaudit.State{EventID: eventID.String()}}}
	if ferr := a.finish(ctx, sum, err); ferr != nil && !errors.Is(ferr, teamaudit.ErrNotPending) {
		s.logger.Printf("tickets: audit record %s stays pending", jobID)
	}
}
