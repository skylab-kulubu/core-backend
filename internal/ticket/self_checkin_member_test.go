package ticket_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/doorqr"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// selfFixture is the attendance QR's page (docs/guest-self-check-in.md)
// with members: an Event that takes registrations on its form, a guest, a
// member with an account e-mail and a school e-mail, and a member of
// another Event.
type selfFixture struct {
	svc    ticket.Service
	ev     event.Event
	sess   event.Session
	staff  authz.Principal
	member uuid.UUID
	key    string
	events event.Store
}

func newSelfFixture(t *testing.T) selfFixture {
	t.Helper()
	return newSelfFixtureMode(t, doorqr.ModeQR)
}

func newSelfFixtureMode(t *testing.T, mode doorqr.Mode) selfFixture {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 10, 3, 18, 0, 0, 0, time.UTC)
	gate := doorqr.NewGate([]byte("door qr test key, door qr test k"), doorqr.Config{Mode: mode, MaxUses: 20, SessionGrace: 30 * time.Minute})
	gate.Now = func() time.Time { return now }
	events := event.NewMemoryStore()
	users := user.NewMemoryStore()
	svc := ticket.NewService(ticket.NewMemoryStore(), events, authz.NewAuthorizer(authz.DefaultPolicy()), gate, users)
	staffID := uuid.MustParse("99999999-9999-9999-9999-999999999999")
	ev, err := events.Create(ctx, event.Event{
		Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB", DoorStaffIDs: []uuid.UUID{staffID},
		FormURL: "https://forms.yildizskylab.com/f/hack",
	})
	if err != nil {
		t.Fatal(err)
	}
	day := seedDay(t, events, ev.ID, "Day 1")
	sess := seedSession(t, events, day.ID, "Opening")
	if _, err := svc.ApplyGuest(ctx, weblabLead, ev.ID, ticket.GuestInfo{FirstName: "Ada", LastName: "Lovelace", Email: "ada@example.com"}); err != nil {
		t.Fatal(err)
	}
	member := uuid.MustParse("77777777-7777-7777-7777-777777777777")
	if _, _, err := users.Upsert(ctx, user.User{ID: member, Email: "grace@example.com", SchoolEmail: "grace.hopper@std.yildiz.edu.tr", FirstName: "Grace", LastName: "Hopper"}); err != nil {
		t.Fatal(err)
	}
	mine, err := svc.Apply(ctx, authz.Principal{ID: member.String()}, ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	// A member with an account but no Ticket of this Event.
	if _, _, err := users.Upsert(ctx, user.User{ID: uuid.New(), Email: "alan@example.com", FirstName: "Alan", LastName: "Turing"}); err != nil {
		t.Fatal(err)
	}
	return selfFixture{svc: svc, ev: ev, sess: sess, staff: authz.Principal{ID: staffID.String()}, member: member, key: mine.AccessKey, events: events}
}

func (f selfFixture) token(t *testing.T) string {
	t.Helper()
	pass, err := f.svc.MintDoorQR(context.Background(), f.staff, f.sess.ID, doorqr.ClassDisplay)
	if err != nil {
		t.Fatal(err)
	}
	return pass.Token
}

// A member who scans the room's QR with the phone's camera lands on the
// page, not in sky-app: their account e-mail, primary or school, finds
// their Ticket. A guest Ticket of the same address comes first.
func TestSelfCheckInFindsAMemberByAccountEmail(t *testing.T) {
	t.Parallel()
	for _, email := range []string{"grace@example.com", " GRACE.HOPPER@std.yildiz.edu.tr "} {
		f := newSelfFixture(t)
		ci, err := f.svc.CheckInGuest(context.Background(), f.sess.ID, ticket.GuestCheckIn{Email: email, DoorToken: f.token(t)})
		if err != nil || ci.SessionID != f.sess.ID {
			t.Fatalf("%q: %+v %v", email, ci, err)
		}
		if _, err := f.svc.CheckInGuest(context.Background(), f.sess.ID, ticket.GuestCheckIn{Email: email, DoorToken: f.token(t)}); !errors.Is(err, ticket.ErrConflict) {
			t.Fatalf("%q twice: %v", email, err)
		}
	}
}

// The Ticket's access key (the ticket page's, kept by the phone) finds any
// Ticket of the Event, a member's too; another Event's key and an unknown
// one find nothing; the door token is checked first.
func TestSelfCheckInByTicketKey(t *testing.T) {
	t.Parallel()
	f := newSelfFixture(t)
	ctx := context.Background()
	key := f.key
	if key == "" {
		t.Fatal("the member's Ticket has no access key")
	}
	if _, err := f.svc.CheckInGuest(ctx, f.sess.ID, ticket.GuestCheckIn{TicketKey: key}); !errors.Is(err, ticket.ErrDoorQRRequired) {
		t.Fatalf("no token: %v", err)
	}
	if _, err := f.svc.CheckInGuest(ctx, f.sess.ID, ticket.GuestCheckIn{TicketKey: key, DoorToken: f.token(t)}); err != nil {
		t.Fatalf("key: %v", err)
	}
	if _, err := f.svc.CheckInGuest(ctx, f.sess.ID, ticket.GuestCheckIn{TicketKey: "unknown-key", DoorToken: f.token(t)}); !errors.Is(err, ticket.ErrNotFound) {
		t.Fatalf("unknown key: %v", err)
	}
	if _, err := f.svc.CheckInGuest(ctx, f.sess.ID, ticket.GuestCheckIn{DoorToken: f.token(t)}); !errors.Is(err, ticket.ErrInvalid) {
		t.Fatalf("neither: %v", err)
	}
}

// Nobody's Ticket: 404 with the Event's form while it takes registrations,
// without it once registration has closed. An account without a Ticket of
// the Event is nobody here.
func TestSelfCheckInWithoutATicketShowsWhereToRegister(t *testing.T) {
	t.Parallel()
	f := newSelfFixture(t)
	ctx := context.Background()
	_, err := f.svc.CheckInGuest(ctx, f.sess.ID, ticket.GuestCheckIn{Email: "alan@example.com", DoorToken: f.token(t)})
	var missing *ticket.GuestNotFoundError
	if !errors.Is(err, ticket.ErrNotFound) || !errors.As(err, &missing) || missing.RegistrationURL != "https://forms.yildizskylab.com/f/hack" {
		t.Fatalf("no ticket: %v %+v", err, missing)
	}
	closed := f.ev
	closed.RegistrationClosed = true
	if _, err := f.events.Update(ctx, closed); err != nil {
		t.Fatal(err)
	}
	_, err = f.svc.CheckInGuest(ctx, f.sess.ID, ticket.GuestCheckIn{Email: "nobody@example.com", DoorToken: f.token(t)})
	if !errors.As(err, &missing) || missing.RegistrationURL != "" {
		t.Fatalf("closed: %v %+v", err, missing)
	}
}

// GUEST_SELF_CHECKIN_MODE=open keeps the guest's e-mail without a token,
// as before; a member's e-mail and a ticket key need a valid token in
// every mode (bilet-qr-ve-pdf 19).
func TestOpenModeNeedsATokenForMembersAndKeys(t *testing.T) {
	t.Parallel()
	f := newSelfFixtureMode(t, doorqr.ModeOpen)
	ctx := context.Background()
	if _, err := f.svc.CheckInGuest(ctx, f.sess.ID, ticket.GuestCheckIn{Email: "ada@example.com"}); err != nil {
		t.Fatalf("guest without a token: %v", err)
	}
	// A member without a token is not looked for: 404, and no link to
	// register (they may well have registered).
	_, err := f.svc.CheckInGuest(ctx, f.sess.ID, ticket.GuestCheckIn{Email: "grace@example.com"})
	var missing *ticket.GuestNotFoundError
	if !errors.As(err, &missing) || missing.RegistrationURL != "" {
		t.Fatalf("member without a token: %v %+v", err, missing)
	}
	if _, err := f.svc.CheckInGuest(ctx, f.sess.ID, ticket.GuestCheckIn{TicketKey: f.key}); !errors.Is(err, ticket.ErrDoorQRRequired) {
		t.Fatalf("key without a token: %v", err)
	}
	if _, err := f.svc.CheckInGuest(ctx, f.sess.ID, ticket.GuestCheckIn{Email: "grace@example.com", DoorToken: "v1.garbled"}); !errors.Is(err, ticket.ErrDoorQRInvalid) {
		t.Fatalf("member with a bad token: %v", err)
	}
	if _, err := f.svc.CheckInGuest(ctx, f.sess.ID, ticket.GuestCheckIn{Email: "grace@example.com", DoorToken: f.token(t)}); err != nil {
		t.Fatalf("member with a token: %v", err)
	}
}
