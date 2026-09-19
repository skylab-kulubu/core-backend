package ticket

import (
	"context"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
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

func (a *afterCheckIn) ApplyForOther(ctx context.Context, p authz.Principal, eventID, userID uuid.UUID) (Ticket, error) {
	return a.inner.ApplyForOther(ctx, p, eventID, userID)
}

func (a *afterCheckIn) ApplyGuest(ctx context.Context, eventID uuid.UUID, g GuestInfo) (Ticket, error) {
	return a.inner.ApplyGuest(ctx, eventID, g)
}

func (a *afterCheckIn) Mine(ctx context.Context, p authz.Principal) ([]Ticket, error) {
	return a.inner.Mine(ctx, p)
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

func (a *afterCheckIn) CheckInMe(ctx context.Context, p authz.Principal, sessionID uuid.UUID) (CheckIn, error) {
	ci, err := a.inner.CheckInMe(ctx, p, sessionID)
	if err == nil {
		a.fire(ctx, ci.TicketID)
	}
	return ci, err
}

func (a *afterCheckIn) CheckInGuest(ctx context.Context, sessionID uuid.UUID, email string) (CheckIn, error) {
	ci, err := a.inner.CheckInGuest(ctx, sessionID, email)
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
