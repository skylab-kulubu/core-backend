package ticket_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/teamaudit"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// capacityEnv is an Event of capacity 1 already filled by one guest, with
// the waiting list on or off.
type capacityEnv struct {
	events  event.Store
	tickets *ticket.MemoryStore
	users   user.Store
	svc     ticket.Service
	ev      event.Event
	formID  uuid.UUID
}

func newCapacityEnv(t *testing.T, capacity int, waitlist, fill bool) capacityEnv {
	t.Helper()
	events := event.NewMemoryStore()
	users := user.NewMemoryStore()
	tickets := ticket.NewMemoryStore()
	svc := ticket.NewService(tickets, events, authz.NewAuthorizer(authz.DefaultPolicy()), users)
	formID := uuid.New()
	ev, err := events.Create(context.Background(), event.Event{
		Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB", Capacity: capacity,
		WaitlistEnabled: &waitlist, FormURL: formAddress(formID),
	})
	if err != nil {
		t.Fatal(err)
	}
	e := capacityEnv{events: events, tickets: tickets, users: users, svc: svc, ev: ev, formID: formID}
	if fill {
		if _, err := svc.ApplyGuest(context.Background(), anonymous, ev.ID, ticket.GuestInfo{FirstName: "İlk", LastName: "Kişi", Email: "first@example.com"}); err != nil {
			t.Fatal(err)
		}
	}
	return e
}

func (e capacityEnv) member(t *testing.T) authz.Principal {
	t.Helper()
	id := uuid.New()
	if _, _, err := user.NewService(e.users).Ensure(context.Background(), id, user.Profile{Email: id.String() + "@example.com", FirstName: "Üye", LastName: "Kişi"}); err != nil {
		t.Fatal(err)
	}
	return authz.Principal{ID: id.String(), Groups: []string{"/UYELER/ARGE/GAMELAB"}}
}

func (e capacityEnv) statuses(t *testing.T) map[ticket.Status]int {
	t.Helper()
	listed, err := e.svc.ListByEventStatus(context.Background(), yk, e.ev.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	out := map[ticket.Status]int{}
	for _, tk := range listed {
		out[tk.StatusOf()]++
	}
	return out
}

func fullError(t *testing.T, err error, waitlist bool) {
	t.Helper()
	var full *ticket.EventFullError
	if !errors.Is(err, ticket.ErrEventFull) || !errors.As(err, &full) || full.WaitlistEnabled != waitlist {
		t.Fatalf("want event_full (waitlistEnabled %v), got %v", waitlist, err)
	}
}

// The four ways a Ticket is written, on an Event with room, full with its
// waiting list on, and full with it off (docs/event-capacity.md).
func TestCapacityTableOfTheFourWays(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	type outcome string
	const (
		active     outcome = "active"
		waitlisted outcome = "waitlisted"
		over       outcome = "active over capacity"
		full       outcome = "event_full"
		noTicket   outcome = "no ticket (outcome event_full)"
	)
	ways := []struct {
		name string
		run  func(t *testing.T, e capacityEnv) (ticket.Ticket, ticket.FormResponseOutcome, error)
		want [3]outcome // room, full + waitlist on, full + waitlist off
	}{
		{"member apply with joinWaitlist", func(t *testing.T, e capacityEnv) (ticket.Ticket, ticket.FormResponseOutcome, error) {
			tk, err := e.svc.ApplyJoining(ctx, e.member(t), e.ev.ID, true)
			return tk, "", err
		}, [3]outcome{active, waitlisted, full}},
		{"member apply without joinWaitlist (sky-app before the list)", func(t *testing.T, e capacityEnv) (ticket.Ticket, ticket.FormResponseOutcome, error) {
			tk, err := e.svc.Apply(ctx, e.member(t), e.ev.ID)
			return tk, "", err
		}, [3]outcome{active, full, full}},
		{"public guest apply with joinWaitlist", func(t *testing.T, e capacityEnv) (ticket.Ticket, ticket.FormResponseOutcome, error) {
			applied, err := e.svc.ApplyGuestJoining(ctx, anonymous, e.ev.ID, ticket.GuestInfo{FirstName: "Ada", LastName: "L", Email: "ada@example.com"}, true)
			if err != nil {
				return ticket.Ticket{}, "", err
			}
			if applied.Ticket.ID != uuid.Nil {
				t.Fatal("an anonymous caller saw the Ticket")
			}
			tk := anyGuest(t, e, "ada@example.com")
			if applied.Waitlisted != (tk.StatusOf() == ticket.StatusWaitlisted) {
				t.Fatalf("answer waitlisted %v, Ticket %s", applied.Waitlisted, tk.Status)
			}
			return tk, "", nil
		}, [3]outcome{active, waitlisted, full}},
		{"apply-for-other from the panel", func(t *testing.T, e capacityEnv) (ticket.Ticket, ticket.FormResponseOutcome, error) {
			person := uuid.New()
			seedUser(t, e.users, person)
			tk, err := e.svc.ApplyForOther(ctx, weblabLead, e.ev.ID, person)
			return tk, "", err
		}, [3]outcome{active, over, over}},
		{"guest added from the panel", func(t *testing.T, e capacityEnv) (ticket.Ticket, ticket.FormResponseOutcome, error) {
			applied, err := e.svc.ApplyGuestJoining(ctx, weblabLead, e.ev.ID, ticket.GuestInfo{FirstName: "Ada", LastName: "L", Email: "ada@example.com"}, false)
			return applied.Ticket, "", err
		}, [3]outcome{active, over, over}},
		{"forms answer accepted", func(t *testing.T, e capacityEnv) (ticket.Ticket, ticket.FormResponseOutcome, error) {
			result, err := e.svc.RecordFormResponse(ctx, formsService, acceptedGuest(e.formID, "Ada", "L", "ada@example.com"))
			if err != nil {
				return ticket.Ticket{}, "", err
			}
			for _, tk := range mustList(t, e) {
				if tk.GuestEmail == "ada@example.com" {
					return tk, result.Outcome, nil
				}
			}
			return ticket.Ticket{}, result.Outcome, nil
		}, [3]outcome{active, waitlisted, noTicket}},
	}
	cases := []struct {
		name           string
		fill, waitlist bool
	}{{"room", false, true}, {"full, waitlist on", true, true}, {"full, waitlist off", true, false}}
	for _, way := range ways {
		for i, c := range cases {
			t.Run(way.name+"/"+c.name, func(t *testing.T) {
				e := newCapacityEnv(t, 1, c.waitlist, c.fill)
				before := e.statuses(t)
				tk, formOutcome, err := way.run(t, e)
				want := way.want[i]
				switch want {
				case full:
					fullError(t, err, c.waitlist)
					if after := e.statuses(t); after[ticket.StatusActive] != before[ticket.StatusActive] || len(after) != len(before) {
						t.Fatalf("a refused registration wrote %v (before %v)", after, before)
					}
					return
				case noTicket:
					if err != nil || formOutcome != ticket.FormResponseEventFull || tk.ID != uuid.Nil {
						t.Fatalf("forms: %v %s %+v", err, formOutcome, tk)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				switch want {
				case active:
					if tk.StatusOf() != ticket.StatusActive || tk.OverCapacity {
						t.Fatalf("%+v", tk)
					}
				case waitlisted:
					if tk.StatusOf() != ticket.StatusWaitlisted || tk.WaitlistedAt == nil {
						t.Fatalf("%+v", tk)
					}
					if formOutcome != "" && formOutcome != ticket.FormResponseWaitlisted {
						t.Fatalf("forms outcome %s", formOutcome)
					}
				case over:
					if tk.StatusOf() != ticket.StatusActive || !tk.OverCapacity {
						t.Fatalf("%+v", tk)
					}
				}
			})
		}
	}
}

// anyGuest is the guest Ticket of email, whatever its status.
func anyGuest(t *testing.T, e capacityEnv, email string) ticket.Ticket {
	t.Helper()
	for _, tk := range mustList(t, e) {
		if tk.GuestEmail == email {
			return tk
		}
	}
	t.Fatalf("no guest ticket for %s", email)
	return ticket.Ticket{}
}

func mustList(t *testing.T, e capacityEnv) []ticket.Ticket {
	t.Helper()
	listed, err := e.svc.ListByEventStatus(context.Background(), yk, e.ev.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	return listed
}

// An Event without a capacity (0) takes everyone, as before.
func TestCapacityZeroIsUnbounded(t *testing.T) {
	t.Parallel()
	e := newCapacityEnv(t, 0, false, true)
	for range 5 {
		if tk, err := e.svc.Apply(context.Background(), e.member(t), e.ev.ID); err != nil || tk.StatusOf() != ticket.StatusActive {
			t.Fatalf("%+v %v", tk, err)
		}
	}
}

// An untrusted Guest apply that finds the e-mail's Ticket already there is
// answered from the Event, as a new guest would be: whether the e-mail had
// applied stays untold.
func TestGuestApplyAnswerOnAFullEventDoesNotTellWhoApplied(t *testing.T) {
	t.Parallel()
	e := newCapacityEnv(t, 1, true, true)
	ctx := context.Background()
	// first@example.com holds the one active Ticket.
	if _, err := e.svc.ApplyGuestJoining(ctx, anonymous, e.ev.ID, ticket.GuestInfo{FirstName: "İlk", LastName: "Kişi", Email: "first@example.com"}, false); !errors.Is(err, ticket.ErrEventFull) {
		t.Fatalf("an existing holder without joinWaitlist: %v", err)
	}
	applied, err := e.svc.ApplyGuestJoining(ctx, anonymous, e.ev.ID, ticket.GuestInfo{FirstName: "İlk", LastName: "Kişi", Email: "first@example.com"}, true)
	if err != nil || !applied.Waitlisted {
		t.Fatalf("an existing holder with joinWaitlist: %+v %v", applied, err)
	}
	if got := storedGuest(t, e.svc, e.ev, "first@example.com"); got.StatusOf() != ticket.StatusActive {
		t.Fatalf("the holder's Ticket changed: %+v", got)
	}
}

// Promotion: the longest waiting first, one cancellation frees one place,
// and nothing moves on an Event that has started, is cancelled or a draft.
func TestWaitlistPromotionOrderAndOneForOne(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newCapacityEnv(t, 1, true, true)
	first := storedGuest(t, e.svc, e.ev, "first@example.com")
	var waiting []ticket.Ticket
	for range 3 {
		tk, err := e.svc.ApplyJoining(ctx, e.member(t), e.ev.ID, true)
		if err != nil || tk.StatusOf() != ticket.StatusWaitlisted {
			t.Fatalf("%+v %v", tk, err)
		}
		waiting = append(waiting, tk)
		time.Sleep(time.Millisecond)
	}
	as := teamaudit.WithActor(ctx, teamaudit.Actor{ID: uuid.New(), Client: "core-frontend"})
	if _, err := e.svc.CancelTicket(as, otherMember, first.ID, ""); !errors.Is(err, ticket.ErrForbidden) {
		t.Fatalf("another team's member may not cancel: %v", err)
	}
	svc := e.svc
	if _, err := svc.CancelTicket(as, weblabLead, first.ID, ""); err != nil {
		t.Fatal(err)
	}
	got := map[uuid.UUID]ticket.Status{}
	for _, tk := range mustList(t, e) {
		got[tk.ID] = tk.StatusOf()
	}
	if got[waiting[0].ID] != ticket.StatusActive || got[waiting[1].ID] != ticket.StatusWaitlisted || got[waiting[2].ID] != ticket.StatusWaitlisted {
		t.Fatalf("one cancellation, one promotion, the oldest: %v", got)
	}
	// Cancelling a waiting Ticket frees no place.
	if _, err := svc.CancelTicket(as, weblabLead, waiting[1].ID, ""); err != nil {
		t.Fatal(err)
	}
	if s := e.statuses(t); s[ticket.StatusActive] != 1 || s[ticket.StatusWaitlisted] != 1 {
		t.Fatalf("after leaving the list: %v", s)
	}
}

func TestWaitlistDoesNotMoveOnAStartedOrCancelledEvent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for name, change := range map[string]func(t *testing.T, events event.Store, ev event.Event){
		"started": func(t *testing.T, events event.Store, ev event.Event) {
			start, end := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
			ev.StartDate, ev.EndDate = &start, &end
			if _, err := events.Update(ctx, ev); err != nil {
				t.Fatal(err)
			}
		},
		"cancelled": func(t *testing.T, events event.Store, ev event.Event) {
			if _, err := events.Cancel(ctx, ev.ID, nil, "", false); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := newCapacityEnv(t, 1, true, true)
			first := storedGuest(t, e.svc, e.ev, "first@example.com")
			waiting, err := e.svc.ApplyJoining(ctx, e.member(t), e.ev.ID, true)
			if err != nil {
				t.Fatal(err)
			}
			change(t, e.events, e.ev)
			if _, _, err := e.tickets.Cancel(ctx, first.ID, ticket.CancelledByOperator, ""); err != nil {
				t.Fatal(err)
			}
			after, _ := e.tickets.Get(ctx, waiting.ID)
			if after.StatusOf() != ticket.StatusWaitlisted {
				t.Fatalf("promoted on a %s Event: %+v", name, after)
			}
		})
	}
}

// "Öne al": an operator moves a waiting Ticket to active past the capacity,
// and is told so; a DK reader may not.
func TestPromoteTicketByAnOperator(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newCapacityEnv(t, 1, true, true)
	waiting, err := e.svc.ApplyJoining(ctx, e.member(t), e.ev.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	as := teamaudit.WithActor(ctx, teamaudit.Actor{ID: uuid.New(), Client: "core-frontend"})
	if _, err := e.svc.PromoteTicket(as, otherMember, waiting.ID); !errors.Is(err, ticket.ErrForbidden) {
		t.Fatalf("another team's member: %v", err)
	}
	promoted, err := e.svc.PromoteTicket(as, weblabLead, waiting.ID)
	if err != nil {
		t.Fatal(err)
	}
	if promoted.StatusOf() != ticket.StatusActive || !promoted.OverCapacity {
		t.Fatalf("%+v", promoted)
	}
	again, err := e.svc.PromoteTicket(as, weblabLead, waiting.ID)
	if err != nil || again.StatusOf() != ticket.StatusActive {
		t.Fatalf("again %+v %v", again, err)
	}
}

// A cancelled Ticket registered again goes on the waiting list when the
// Event is full, not past it.
func TestReRegistrationOnAFullEventWaits(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newCapacityEnv(t, 1, true, false)
	member := e.member(t)
	mine, err := e.svc.Apply(ctx, member, e.ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	as := teamaudit.WithActor(ctx, teamaudit.Actor{ID: uuid.New(), Client: "core-frontend"})
	if _, err := e.svc.CancelTicket(as, weblabLead, mine.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.ApplyGuest(ctx, anonymous, e.ev.ID, ticket.GuestInfo{FirstName: "Ada", LastName: "L", Email: "ada@example.com"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Apply(ctx, member, e.ev.ID); !errors.Is(err, ticket.ErrEventFull) {
		t.Fatalf("again without joinWaitlist: %v", err)
	}
	again, err := e.svc.ApplyJoining(ctx, member, e.ev.ID, true)
	if err != nil || again.ID != mine.ID || again.StatusOf() != ticket.StatusWaitlisted || again.QRVersion != mine.QRVersion+1 {
		t.Fatalf("reopened %+v %v", again, err)
	}
	// The operator's restore goes past the capacity and says so.
	if _, err := e.svc.CancelTicket(as, weblabLead, again.ID, ""); err != nil {
		t.Fatal(err)
	}
	restored, err := e.svc.RestoreTicket(as, weblabLead, again.ID)
	if err != nil || restored.StatusOf() != ticket.StatusActive || !restored.OverCapacity {
		t.Fatalf("restored %+v %v", restored, err)
	}
}
