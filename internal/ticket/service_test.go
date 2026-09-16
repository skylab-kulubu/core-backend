package ticket_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
)

func setup(t *testing.T) (event.Store, ticket.Service) {
	t.Helper()
	events := event.NewMemoryStore()
	svc := ticket.NewService(ticket.NewMemoryStore(), events, authz.NewAuthorizer(authz.DefaultPolicy()))
	return events, svc
}

func seedEvent(t *testing.T, events event.Store, owner string) event.Event {
	t.Helper()
	ev, err := events.Create(context.Background(), event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: owner})
	if err != nil {
		t.Fatal(err)
	}
	return ev
}

func TestService_ApplyThenListMine(t *testing.T) {
	t.Parallel()
	events, svc := setup(t)
	ctx := context.Background()
	ev := seedEvent(t, events, "WEBLAB")
	userID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	p := authz.Principal{ID: userID.String()}

	created, err := svc.Apply(ctx, p, ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	if created.TicketType != ticket.Registered || created.OwnerID == nil || *created.OwnerID != userID {
		t.Fatalf("created %+v", created)
	}

	mine, err := svc.Mine(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if len(mine) != 1 || mine[0].ID != created.ID {
		t.Fatalf("mine %+v", mine)
	}

	_, err = svc.Apply(ctx, p, ev.ID)
	if !errors.Is(err, ticket.ErrConflict) {
		t.Fatalf("dup apply: %v", err)
	}
}

func TestService_ApplyGuest(t *testing.T) {
	t.Parallel()
	events, svc := setup(t)
	ctx := context.Background()
	ev := seedEvent(t, events, "WEBLAB")

	created, err := svc.ApplyGuest(ctx, ev.ID, ticket.GuestInfo{
		FirstName: "Ada", LastName: "Lovelace", Email: "ada@example.com", PhoneNumber: "555",
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.TicketType != ticket.Guest || created.GuestEmail != "ada@example.com" {
		t.Fatalf("created %+v", created)
	}

	_, err = svc.ApplyGuest(ctx, ev.ID, ticket.GuestInfo{
		FirstName: "Ada", LastName: "Lovelace", Email: "ada@example.com", PhoneNumber: "555",
	})
	if !errors.Is(err, ticket.ErrConflict) {
		t.Fatalf("dup guest: %v", err)
	}
}

func TestService_AnonymousCannotApply(t *testing.T) {
	t.Parallel()
	events, svc := setup(t)
	ev := seedEvent(t, events, "WEBLAB")
	_, err := svc.Apply(context.Background(), authz.Principal{}, ev.ID)
	if !errors.Is(err, ticket.ErrForbidden) {
		t.Fatalf("got %v", err)
	}
}

func TestService_CheckInLeaderAndDuplicate(t *testing.T) {
	t.Parallel()
	events, svc := setup(t)
	ctx := context.Background()
	ev := seedEvent(t, events, "WEBLAB")
	day, err := events.CreateDay(ctx, event.Day{EventID: ev.ID, Name: "Day 1"})
	if err != nil {
		t.Fatal(err)
	}
	userID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	tk, err := svc.Apply(ctx, authz.Principal{ID: userID.String()}, ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	leader := authz.Principal{ID: "lead", Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}

	ci, err := svc.CheckIn(ctx, leader, tk.ID, day.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ci.EventDayID != day.ID {
		t.Fatalf("checkin %+v", ci)
	}
	_, err = svc.CheckIn(ctx, leader, tk.ID, day.ID)
	if !errors.Is(err, ticket.ErrConflict) {
		t.Fatalf("dup checkin: %v", err)
	}
}

func TestService_CheckInForbiddenForMember(t *testing.T) {
	t.Parallel()
	events, svc := setup(t)
	ctx := context.Background()
	ev := seedEvent(t, events, "WEBLAB")
	day, err := events.CreateDay(ctx, event.Day{EventID: ev.ID, Name: "Day 1"})
	if err != nil {
		t.Fatal(err)
	}
	userID := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	tk, err := svc.Apply(ctx, authz.Principal{ID: userID.String()}, ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	member := authz.Principal{ID: "mem", Groups: []string{"/UYELER/ARGE/WEBLAB"}}
	_, err = svc.CheckIn(ctx, member, tk.ID, day.ID)
	if !errors.Is(err, ticket.ErrForbidden) {
		t.Fatalf("got %v", err)
	}
}

func TestService_ListByEventLeaderAndMember(t *testing.T) {
	t.Parallel()
	events, svc := setup(t)
	ctx := context.Background()
	ev := seedEvent(t, events, "WEBLAB")
	userID := uuid.MustParse("55555555-5555-5555-5555-555555555555")
	tk, err := svc.Apply(ctx, authz.Principal{ID: userID.String()}, ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	leader := authz.Principal{ID: "lead", Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}
	got, err := svc.ListByEvent(ctx, leader, ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != tk.ID {
		t.Fatalf("leader list %+v", got)
	}
	member := authz.Principal{ID: "mem", Groups: []string{"/UYELER/ARGE/WEBLAB"}}
	_, err = svc.ListByEvent(ctx, member, ev.ID)
	if !errors.Is(err, ticket.ErrForbidden) {
		t.Fatalf("member got %v", err)
	}
}

func TestService_CheckInWrongEventDay(t *testing.T) {
	t.Parallel()
	events, svc := setup(t)
	ctx := context.Background()
	ev := seedEvent(t, events, "WEBLAB")
	other := seedEvent(t, events, "SKYSEC")
	day, err := events.CreateDay(ctx, event.Day{EventID: other.ID, Name: "Other"})
	if err != nil {
		t.Fatal(err)
	}
	userID := uuid.MustParse("44444444-4444-4444-4444-444444444444")
	tk, err := svc.Apply(ctx, authz.Principal{ID: userID.String()}, ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	leader := authz.Principal{ID: "lead", Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}
	_, err = svc.CheckIn(ctx, leader, tk.ID, day.ID)
	if !errors.Is(err, ticket.ErrInvalid) {
		t.Fatalf("got %v", err)
	}
}
