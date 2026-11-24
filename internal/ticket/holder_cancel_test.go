package ticket_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
)

// The holder's last moment (kontenjan-iptal-hatirlatma 03; Yusuf,
// 2026-11-24): an active Ticket until the Event starts, a waitlisted one
// until it ends; an undated Event never starts; never on a cancelled Event,
// never a cancelled Ticket.
func TestHolderCancellableLastMoment(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 12, 12, 10, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *time.Time {
		v := now.Add(d)
		return &v
	}
	active := ticket.Ticket{Status: ticket.StatusActive}
	waiting := ticket.Ticket{Status: ticket.StatusWaitlisted}
	cancelled := ticket.Ticket{Status: ticket.StatusCancelled}
	for name, tc := range map[string]struct {
		ev   event.Event
		t    ticket.Ticket
		want bool
	}{
		"a minute before the start":         {event.Event{StartDate: at(time.Minute), EndDate: at(time.Hour)}, active, true},
		"at the start":                      {event.Event{StartDate: at(0), EndDate: at(time.Hour)}, active, false},
		"a minute after the start":          {event.Event{StartDate: at(-time.Minute), EndDate: at(time.Hour)}, active, false},
		"waitlisted, a minute after start":  {event.Event{StartDate: at(-time.Minute), EndDate: at(time.Hour)}, waiting, true},
		"waitlisted, after the end":         {event.Event{StartDate: at(-2 * time.Hour), EndDate: at(-time.Minute)}, waiting, false},
		"undated":                           {event.Event{}, active, true},
		"undated, waitlisted":               {event.Event{}, waiting, true},
		"a cancelled Event":                 {event.Event{Status: event.StatusCancelled, StartDate: at(time.Hour)}, active, false},
		"a cancelled Ticket":                {event.Event{StartDate: at(time.Hour)}, cancelled, false},
		"a Ticket before statuses (active)": {event.Event{StartDate: at(time.Hour)}, ticket.Ticket{}, true},
	} {
		if got := ticket.HolderCancellable(tc.ev, tc.t, now); got != tc.want {
			t.Errorf("%s: %v, want %v", name, got, tc.want)
		}
	}
}

// The holder cancels their own Ticket on the shared route: cancelled by
// the holder, no reason kept, the mail queued; again answers the same
// without a second mail. Someone else's Ticket is 404 to a member who may
// not read it; DK, who reads it, is refused; a Ticket with a check-in is
// 409.
func TestHolderCancelsTheirOwnTicket(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newCancelEnv(t)

	got, err := e.svc.CancelTicketTelling(ctx, e.holder, e.member.ID, "kendi gerekçem", false)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != ticket.StatusCancelled || got.CancelledBy != ticket.CancelledByHolder || got.CancelReason != "" || got.CancelledAt == nil {
		t.Fatalf("cancelled %+v", got)
	}
	if e.store.CancelMails[e.member.ID] != 1 {
		t.Fatalf("the holder is always told: %v", e.store.CancelMails)
	}
	again, err := e.svc.CancelTicketTelling(ctx, e.holder, e.member.ID, "", false)
	if err != nil || again.Status != ticket.StatusCancelled || !again.CancelledAt.Equal(*got.CancelledAt) || e.store.CancelMails[e.member.ID] != 1 {
		t.Fatalf("again %+v %v (mails %v)", again, err, e.store.CancelMails)
	}

	if _, err := e.svc.CancelTicketTelling(ctx, e.holder, e.guest.ID, "", false); !errors.Is(err, ticket.ErrNotFound) {
		t.Fatalf("someone else's Ticket: %v", err)
	}
	if _, err := e.svc.CancelTicketTelling(ctx, otherMember, e.guest.ID, "", false); !errors.Is(err, ticket.ErrNotFound) {
		t.Fatalf("another team's member: %v", err)
	}
	dk := authz.Principal{ID: "dk", Groups: []string{"/UYELER/DK"}}
	if _, err := e.svc.CancelTicketTelling(ctx, dk, e.guest.ID, "", false); !errors.Is(err, ticket.ErrForbidden) {
		t.Fatalf("DK: %v", err)
	}
	if _, err := e.svc.CancelTicketTelling(ctx, e.holder, uuid.New(), "", false); !errors.Is(err, ticket.ErrNotFound) {
		t.Fatalf("no such Ticket: %v", err)
	}
	if got := e.store.CancelMails[e.guest.ID]; got != 0 {
		t.Fatalf("refusals queued %d mails", got)
	}

	// A Ticket with a check-in stays, for its holder too.
	f := newCancelEnv(t)
	if _, err := f.svc.CheckIn(ctx, weblabLead, f.member.ID, f.sess.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.CancelTicketTelling(ctx, f.holder, f.member.ID, "", false); !errors.Is(err, ticket.ErrTicketCheckedIn) {
		t.Fatalf("checked in: %v", err)
	}
}

// An operator tells the holder only when notify; the reason is theirs.
func TestOperatorCancelTellsWhenAsked(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newCancelEnv(t)
	got, err := e.svc.CancelTicketTelling(ctx, weblabLead, e.guest.ID, " Yer yok ", true)
	if err != nil || got.CancelledBy != ticket.CancelledByOperator || got.CancelReason != "Yer yok" || e.store.CancelMails[e.guest.ID] != 1 {
		t.Fatalf("notify: %+v %v (mails %v)", got, err, e.store.CancelMails)
	}
	got, err = e.svc.CancelTicketTelling(ctx, weblabLead, e.member.ID, "", false)
	if err != nil || got.CancelledBy != ticket.CancelledByOperator || e.store.CancelMails[e.member.ID] != 0 {
		t.Fatalf("no notify: %+v %v (mails %v)", got, err, e.store.CancelMails)
	}
}

// Once the Event has started the holder's active Ticket stays (409
// ticket_cancel_closed); the operator may still cancel it until the end.
func TestHolderCancelClosesAtTheStart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newCancelEnv(t)
	start, end := time.Now().Add(-time.Minute), time.Now().Add(time.Hour)
	started, err := e.events.Create(ctx, event.Event{Name: "Başladı", Location: "YTÜ", OwnerTeam: "WEBLAB", StartDate: &start, EndDate: &end})
	if err != nil {
		t.Fatal(err)
	}
	mine, err := e.store.Create(ctx, ticket.Ticket{EventID: started.ID, TicketType: ticket.Registered, OwnerID: &e.holderID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.CancelTicketTelling(ctx, e.holder, mine.ID, "", false); !errors.Is(err, ticket.ErrCancelClosed) {
		t.Fatalf("started: %v", err)
	}
	if got, err := e.svc.CancelTicketTelling(ctx, weblabLead, mine.ID, "", false); err != nil || got.Status != ticket.StatusCancelled {
		t.Fatalf("operator after the start: %+v %v", got, err)
	}
}

// The holder's cancellation of an active Ticket gives its place to the
// waiting list in the same step; leaving the list frees none.
func TestHolderCancelPromotesTheWaitingList(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newCapacityEnv(t, 1, true, false)
	holder := e.member(t)
	mine, err := e.svc.ApplyJoining(ctx, holder, e.ev.ID, true)
	if err != nil || mine.StatusOf() != ticket.StatusActive {
		t.Fatalf("%+v %v", mine, err)
	}
	waiter := e.member(t)
	waiting, err := e.svc.ApplyJoining(ctx, waiter, e.ev.ID, true)
	if err != nil || waiting.StatusOf() != ticket.StatusWaitlisted {
		t.Fatalf("%+v %v", waiting, err)
	}
	if _, err := e.svc.CancelTicketTelling(ctx, holder, mine.ID, "", false); err != nil {
		t.Fatal(err)
	}
	promoted, err := e.tickets.Get(ctx, waiting.ID)
	if err != nil || promoted.StatusOf() != ticket.StatusActive {
		t.Fatalf("the next in line: %+v %v", promoted, err)
	}

	// The promoted holder's place is now theirs; a new waiter leaves the
	// list by cancelling, which promotes nobody.
	third := e.member(t)
	queued, err := e.svc.ApplyJoining(ctx, third, e.ev.ID, true)
	if err != nil || queued.StatusOf() != ticket.StatusWaitlisted {
		t.Fatalf("%+v %v", queued, err)
	}
	left, err := e.svc.CancelTicketTelling(ctx, third, queued.ID, "", false)
	if err != nil || left.StatusOf() != ticket.StatusCancelled || left.CancelledBy != ticket.CancelledByHolder {
		t.Fatalf("leaving the list: %+v %v", left, err)
	}
	if s := e.statuses(t); s[ticket.StatusActive] != 1 || s[ticket.StatusWaitlisted] != 0 || s[ticket.StatusCancelled] != 2 {
		t.Fatalf("statuses %v", s)
	}
}
