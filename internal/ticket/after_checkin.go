package ticket

import (
	"context"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/doorqr"
	"github.com/skylab-kulubu/core-backend/internal/event"
)

type afterCheckIn struct {
	inner Service
	fn    func(context.Context, uuid.UUID)
}

func WithSettledCheckIn(inner Service, fn func(context.Context, uuid.UUID)) Service {
	if fn == nil {
		return inner
	}
	return &afterCheckIn{inner: inner, fn: fn}
}

func (a *afterCheckIn) Apply(ctx context.Context, p authz.Principal, eventID uuid.UUID) (Ticket, error) {
	return a.inner.Apply(ctx, p, eventID)
}

func (a *afterCheckIn) ApplyJoining(ctx context.Context, p authz.Principal, eventID uuid.UUID, joinWaitlist bool) (Ticket, error) {
	return a.inner.ApplyJoining(ctx, p, eventID, joinWaitlist)
}

func (a *afterCheckIn) ApplyGuestJoining(ctx context.Context, p authz.Principal, eventID uuid.UUID, g GuestInfo, joinWaitlist bool) (GuestApplication, error) {
	return a.inner.ApplyGuestJoining(ctx, p, eventID, g, joinWaitlist)
}

func (a *afterCheckIn) PreviewImport(ctx context.Context, p authz.Principal, eventID uuid.UUID, in ImportInput) (ImportReport, error) {
	return a.inner.PreviewImport(ctx, p, eventID, in)
}

func (a *afterCheckIn) ImportAttendees(ctx context.Context, p authz.Principal, eventID uuid.UUID, in ImportInput) (ImportReport, error) {
	return a.inner.ImportAttendees(ctx, p, eventID, in)
}

func (a *afterCheckIn) PromoteTicket(ctx context.Context, p authz.Principal, id uuid.UUID) (Ticket, error) {
	return a.inner.PromoteTicket(ctx, p, id)
}

func (a *afterCheckIn) ApplyForOther(ctx context.Context, p authz.Principal, eventID, userID uuid.UUID) (Ticket, error) {
	return a.inner.ApplyForOther(ctx, p, eventID, userID)
}

func (a *afterCheckIn) ListAssignableUsers(ctx context.Context, p authz.Principal, eventID uuid.UUID, query string) ([]PersonSummary, error) {
	return a.inner.ListAssignableUsers(ctx, p, eventID, query)
}

func (a *afterCheckIn) ListDoorEvents(ctx context.Context, p authz.Principal) ([]event.Resource, error) {
	return a.inner.ListDoorEvents(ctx, p)
}

func (a *afterCheckIn) SearchDoorAttendees(ctx context.Context, p authz.Principal, eventID uuid.UUID, query string) ([]DoorAttendee, error) {
	return a.inner.SearchDoorAttendees(ctx, p, eventID, query)
}

func (a *afterCheckIn) ApplyGuest(ctx context.Context, p authz.Principal, eventID uuid.UUID, g GuestInfo) (GuestApplication, error) {
	return a.inner.ApplyGuest(ctx, p, eventID, g)
}

func (a *afterCheckIn) RecordFormResponse(ctx context.Context, p authz.Principal, r FormResponse) (FormResponseResult, error) {
	return a.inner.RecordFormResponse(ctx, p, r)
}

func (a *afterCheckIn) CanRecordFormResponse(p authz.Principal) bool {
	return a.inner.CanRecordFormResponse(p)
}

func (a *afterCheckIn) KeepFormRespondent(ctx context.Context, eventID uuid.UUID, r FormRespondent) error {
	return a.inner.KeepFormRespondent(ctx, eventID, r)
}

func (a *afterCheckIn) EnsureFormRespondent(ctx context.Context, userID uuid.UUID) (bool, error) {
	return a.inner.EnsureFormRespondent(ctx, userID)
}

func (a *afterCheckIn) BeginFormRespondentSend(ctx context.Context, p authz.Principal, eventID uuid.UUID) (*FormRespondentSend, error) {
	return a.inner.BeginFormRespondentSend(ctx, p, eventID)
}

func (a *afterCheckIn) FinishFormRespondentJob(ctx context.Context, eventID, jobID uuid.UUID, sum FormRespondentSummary, err error) {
	a.inner.FinishFormRespondentJob(ctx, eventID, jobID, sum, err)
}

func (a *afterCheckIn) AdmitFormRespondent(ctx context.Context, eventID uuid.UUID, r FormRespondent) (FormRespondentOutcome, error) {
	return a.inner.AdmitFormRespondent(ctx, eventID, r)
}

func (a *afterCheckIn) Mine(ctx context.Context, p authz.Principal) ([]Ticket, error) {
	return a.inner.Mine(ctx, p)
}

func (a *afterCheckIn) MineIncluding(ctx context.Context, p authz.Principal, include []Status) ([]Ticket, error) {
	return a.inner.MineIncluding(ctx, p, include)
}

func (a *afterCheckIn) ListByEventStatus(ctx context.Context, p authz.Principal, eventID uuid.UUID, statuses []Status) ([]Ticket, error) {
	return a.inner.ListByEventStatus(ctx, p, eventID, statuses)
}

func (a *afterCheckIn) CancelTicket(ctx context.Context, p authz.Principal, id uuid.UUID, reason string) (Ticket, error) {
	return a.inner.CancelTicket(ctx, p, id, reason)
}

func (a *afterCheckIn) CancelTicketTelling(ctx context.Context, p authz.Principal, id uuid.UUID, reason string, notify bool) (Ticket, error) {
	return a.inner.CancelTicketTelling(ctx, p, id, reason, notify)
}

func (a *afterCheckIn) RestoreTicket(ctx context.Context, p authz.Principal, id uuid.UUID) (Ticket, error) {
	return a.inner.RestoreTicket(ctx, p, id)
}

func (a *afterCheckIn) ListByEvent(ctx context.Context, p authz.Principal, eventID uuid.UUID) ([]Ticket, error) {
	return a.inner.ListByEvent(ctx, p, eventID)
}

func (a *afterCheckIn) Get(ctx context.Context, p authz.Principal, id uuid.UUID) (Ticket, error) {
	return a.inner.Get(ctx, p, id)
}

func (a *afterCheckIn) GetByUserEvent(ctx context.Context, p authz.Principal, userID, eventID uuid.UUID) (Ticket, error) {
	return a.inner.GetByUserEvent(ctx, p, userID, eventID)
}

func (a *afterCheckIn) ListQuery(ctx context.Context, p authz.Principal, email string, userID *uuid.UUID) ([]Ticket, error) {
	return a.inner.ListQuery(ctx, p, email, userID)
}

func (a *afterCheckIn) fire(ctx context.Context, ticketID uuid.UUID) {
	defer func() { _ = recover() }()
	a.fn(ctx, ticketID)
}

func (a *afterCheckIn) CheckIn(ctx context.Context, p authz.Principal, ticketID, sessionID uuid.UUID) (CheckIn, error) {
	ci, err := a.inner.CheckIn(ctx, p, ticketID, sessionID)
	if err == nil {
		a.fire(ctx, ci.TicketID)
	}
	return ci, err
}

// UndoCheckIn passes through without firing: fn follows a check-in that
// settled, and this one is gone.
func (a *afterCheckIn) UndoCheckIn(ctx context.Context, p authz.Principal, ticketID, sessionID uuid.UUID) error {
	return a.inner.UndoCheckIn(ctx, p, ticketID, sessionID)
}

// UndoCheckInByID passes through without firing, as UndoCheckIn.
func (a *afterCheckIn) UndoCheckInByID(ctx context.Context, p authz.Principal, sessionID, checkInID uuid.UUID) error {
	return a.inner.UndoCheckInByID(ctx, p, sessionID, checkInID)
}

func (a *afterCheckIn) CheckInMe(ctx context.Context, p authz.Principal, sessionID uuid.UUID, doorToken string) (CheckIn, error) {
	ci, err := a.inner.CheckInMe(ctx, p, sessionID, doorToken)
	if err == nil {
		a.fire(ctx, ci.TicketID)
	}
	return ci, err
}

func (a *afterCheckIn) CheckInGuest(ctx context.Context, sessionID uuid.UUID, g GuestCheckIn) (CheckIn, error) {
	ci, err := a.inner.CheckInGuest(ctx, sessionID, g)
	if err == nil {
		a.fire(ctx, ci.TicketID)
	}
	return ci, err
}

func (a *afterCheckIn) CheckInUser(ctx context.Context, p authz.Principal, sessionID, userID uuid.UUID) (CheckIn, error) {
	ci, err := a.inner.CheckInUser(ctx, p, sessionID, userID)
	if err == nil {
		a.fire(ctx, ci.TicketID)
	}
	return ci, err
}

func (a *afterCheckIn) AuthorizeSessionDoor(ctx context.Context, p authz.Principal, sessionID uuid.UUID) error {
	return a.inner.AuthorizeSessionDoor(ctx, p, sessionID)
}

func (a *afterCheckIn) ResolveAndCheckIn(ctx context.Context, p authz.Principal, sessionID uuid.UUID, target DoorCheckInTarget) (DoorCheckIn, error) {
	ci, err := a.inner.ResolveAndCheckIn(ctx, p, sessionID, target)
	if err == nil {
		a.fire(ctx, ci.TicketID)
	}
	return ci, err
}

func (a *afterCheckIn) MintDoorQR(ctx context.Context, p authz.Principal, sessionID uuid.UUID, class doorqr.Class) (DoorQR, error) {
	return a.inner.MintDoorQR(ctx, p, sessionID, class)
}

func (a *afterCheckIn) DoorActivity(ctx context.Context, p authz.Principal, sessionID uuid.UUID) (DoorActivity, error) {
	return a.inner.DoorActivity(ctx, p, sessionID)
}

func (a *afterCheckIn) CheckInTicketQR(ctx context.Context, p authz.Principal, sessionID uuid.UUID, token string) (CheckIn, error) {
	ci, err := a.inner.CheckInTicketQR(ctx, p, sessionID, token)
	if err == nil {
		a.fire(ctx, ci.TicketID)
	}
	return ci, err
}

func (a *afterCheckIn) TicketQR(t Ticket, ev event.Event) (string, error) {
	return a.inner.TicketQR(t, ev)
}

func (a *afterCheckIn) ResendTicket(ctx context.Context, p authz.Principal, id uuid.UUID, requestKey string) (Resend, error) {
	return a.inner.ResendTicket(ctx, p, id, requestKey)
}

func (a *afterCheckIn) RenewTicketLink(ctx context.Context, p authz.Principal, id uuid.UUID, notify bool, requestKey string) (Ticket, Resend, error) {
	return a.inner.RenewTicketLink(ctx, p, id, notify, requestKey)
}

// The Event's door writes Venue entries, no check-in: nothing fires.

func (a *afterCheckIn) AuthorizeEventDoor(ctx context.Context, p authz.Principal, eventID uuid.UUID) error {
	return a.inner.AuthorizeEventDoor(ctx, p, eventID)
}

func (a *afterCheckIn) EnterTicketQR(ctx context.Context, p authz.Principal, eventID uuid.UUID, token string) (DoorEntry, error) {
	return a.inner.EnterTicketQR(ctx, p, eventID, token)
}

func (a *afterCheckIn) EnterUser(ctx context.Context, p authz.Principal, eventID, userID uuid.UUID) (DoorEntry, error) {
	return a.inner.EnterUser(ctx, p, eventID, userID)
}

func (a *afterCheckIn) ResolveEntry(ctx context.Context, p authz.Principal, eventID uuid.UUID, target DoorCheckInTarget) (DoorEntry, error) {
	return a.inner.ResolveEntry(ctx, p, eventID, target)
}

func (a *afterCheckIn) UndoEntry(ctx context.Context, p authz.Principal, eventID, entryID uuid.UUID) error {
	return a.inner.UndoEntry(ctx, p, eventID, entryID)
}

func (a *afterCheckIn) EntrySummary(ctx context.Context, p authz.Principal, eventID uuid.UUID) (EntrySummary, error) {
	return a.inner.EntrySummary(ctx, p, eventID)
}

func (a *afterCheckIn) ListEntries(ctx context.Context, p authz.Principal, eventID uuid.UUID, date string) ([]DoorEntry, error) {
	return a.inner.ListEntries(ctx, p, eventID, date)
}
