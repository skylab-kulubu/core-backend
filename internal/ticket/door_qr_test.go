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
)

type doorFixture struct {
	svc     ticket.Service
	gate    *doorqr.Gate
	events  event.Store
	ev      event.Event
	sess    event.Session
	staff   authz.Principal
	minted  time.Time
	advance func(time.Duration)
}

func newDoorFixture(t *testing.T, mode doorqr.Mode) doorFixture {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 10, 3, 18, 0, 0, 0, time.UTC)
	gate := doorqr.NewGate([]byte("door qr test key, door qr test k"), doorqr.Config{Mode: mode, MaxUses: 3})
	gate.Now = func() time.Time { return now }
	events := event.NewMemoryStore()
	svc := ticket.NewService(ticket.NewMemoryStore(), events, authz.NewAuthorizer(authz.DefaultPolicy()), gate)
	staffID := uuid.MustParse("99999999-9999-9999-9999-999999999999")
	ev, err := events.Create(ctx, event.Event{
		Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB", DoorStaffIDs: []uuid.UUID{staffID},
	})
	if err != nil {
		t.Fatal(err)
	}
	day := seedDay(t, events, ev.ID, "Day 1")
	sess := seedSession(t, events, day.ID, "Opening")
	if _, err := svc.ApplyGuest(ctx, weblabLead, ev.ID, ticket.GuestInfo{
		FirstName: "Ada", LastName: "Lovelace", Email: "ada@example.com",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ApplyGuest(ctx, weblabLead, ev.ID, ticket.GuestInfo{
		FirstName: "Grace", LastName: "Hopper", Email: "grace@example.com",
	}); err != nil {
		t.Fatal(err)
	}
	return doorFixture{
		svc: svc, gate: gate, events: events, ev: ev, sess: sess,
		staff: authz.Principal{ID: staffID.String()}, minted: now,
		advance: func(d time.Duration) { now = now.Add(d) },
	}
}

func TestMintDoorQRDoorStaffOnly(t *testing.T) {
	t.Parallel()
	f := newDoorFixture(t, doorqr.ModeQR)
	ctx := context.Background()
	for name, p := range map[string]authz.Principal{
		"door staff": f.staff, "team leader": weblabLead, "yk": yk,
	} {
		pass, err := f.svc.MintDoorQR(ctx, p, f.sess.ID)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if pass.Token == "" || pass.URL == "" || pass.SessionID != f.sess.ID || pass.EventID != f.ev.ID {
			t.Fatalf("%s: %+v", name, pass)
		}
		if !pass.ExpiresAt.Equal(f.minted.Add(doorqr.DefaultTTL)) || pass.RefreshAfterSeconds != 30 {
			t.Fatalf("%s: times %+v", name, pass)
		}
	}
	for name, p := range map[string]authz.Principal{
		"other member": otherMember, "anonymous": {}, "forms": formsHop,
	} {
		if _, err := f.svc.MintDoorQR(ctx, p, f.sess.ID); !errors.Is(err, ticket.ErrForbidden) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if _, err := f.svc.MintDoorQR(ctx, f.staff, uuid.New()); !errors.Is(err, ticket.ErrNotFound) {
		t.Fatalf("unknown session: %v", err)
	}
}

func TestGuestCheckInQRModeRequiresDoorQR(t *testing.T) {
	t.Parallel()
	f := newDoorFixture(t, doorqr.ModeQR)
	ctx := context.Background()
	if _, err := f.svc.CheckInGuest(ctx, f.sess.ID, ticket.GuestCheckIn{Email: "ada@example.com"}); !errors.Is(err, ticket.ErrDoorQRRequired) {
		t.Fatalf("no token: %v", err)
	}
	pass, err := f.svc.MintDoorQR(ctx, f.staff, f.sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	ci, err := f.svc.CheckInGuest(ctx, f.sess.ID, ticket.GuestCheckIn{Email: "ada@example.com", DoorToken: pass.Token})
	if err != nil {
		t.Fatal(err)
	}
	if ci.SessionID != f.sess.ID {
		t.Fatalf("check-in %+v", ci)
	}
	_, err = f.svc.CheckInGuest(ctx, f.sess.ID, ticket.GuestCheckIn{Email: "ada@example.com", DoorToken: pass.Token})
	if !errors.Is(err, ticket.ErrConflict) {
		t.Fatalf("dup: %v", err)
	}
	// An e-mail without a Ticket still spends a use: the token is not an
	// oracle for which e-mails are registered.
	_, err = f.svc.CheckInGuest(ctx, f.sess.ID, ticket.GuestCheckIn{Email: "nobody@example.com", DoorToken: pass.Token})
	if !errors.Is(err, ticket.ErrNotFound) {
		t.Fatalf("nobody: %v", err)
	}
	_, err = f.svc.CheckInGuest(ctx, f.sess.ID, ticket.GuestCheckIn{Email: "grace@example.com", DoorToken: pass.Token})
	if !errors.Is(err, ticket.ErrDoorQRUsedUp) {
		t.Fatalf("fourth use: %v", err)
	}
}

func TestGuestCheckInRefusesExpiredAndForeignDoorQR(t *testing.T) {
	t.Parallel()
	f := newDoorFixture(t, doorqr.ModeQR)
	ctx := context.Background()
	pass, err := f.svc.MintDoorQR(ctx, f.staff, f.sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Another Session of the same Event: the token names the first one.
	day2 := seedDay(t, f.events, f.ev.ID, "Day 2")
	other := seedSession(t, f.events, day2.ID, "Closing")
	_, err = f.svc.CheckInGuest(ctx, other.ID, ticket.GuestCheckIn{Email: "ada@example.com", DoorToken: pass.Token})
	if !errors.Is(err, ticket.ErrDoorQRInvalid) {
		t.Fatalf("other session: %v", err)
	}
	_, err = f.svc.CheckInGuest(ctx, f.sess.ID, ticket.GuestCheckIn{Email: "ada@example.com", DoorToken: "x.y.z"})
	if !errors.Is(err, ticket.ErrDoorQRInvalid) {
		t.Fatalf("garbage: %v", err)
	}
	f.advance(doorqr.DefaultTTL + time.Second)
	_, err = f.svc.CheckInGuest(ctx, f.sess.ID, ticket.GuestCheckIn{Email: "ada@example.com", DoorToken: pass.Token})
	if !errors.Is(err, ticket.ErrDoorQRExpired) {
		t.Fatalf("expired: %v", err)
	}
}

func TestGuestCheckInRefusesDoorQRAfterSessionMovesEvent(t *testing.T) {
	t.Parallel()
	f := newDoorFixture(t, doorqr.ModeQR)
	ctx := context.Background()
	pass, err := f.svc.MintDoorQR(ctx, f.staff, f.sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	ev2, err := f.events.Create(ctx, event.Event{Name: "Other", Location: "YTÜ", OwnerTeam: "WEBLAB"})
	if err != nil {
		t.Fatal(err)
	}
	day := seedDay(t, f.events, ev2.ID, "Elsewhere")
	moved := f.sess
	moved.EventDayID = day.ID
	if _, err := f.events.UpdateSession(ctx, moved); err != nil {
		t.Fatal(err)
	}
	_, err = f.svc.CheckInGuest(ctx, f.sess.ID, ticket.GuestCheckIn{Email: "ada@example.com", DoorToken: pass.Token})
	if !errors.Is(err, ticket.ErrDoorQRInvalid) {
		t.Fatalf("moved session: %v", err)
	}
}

func TestGuestCheckInOpenModeTakesEmailAloneButChecksAToken(t *testing.T) {
	t.Parallel()
	f := newDoorFixture(t, doorqr.ModeOpen)
	ctx := context.Background()
	if _, err := f.svc.CheckInGuest(ctx, f.sess.ID, ticket.GuestCheckIn{Email: "ada@example.com"}); err != nil {
		t.Fatalf("open, no token: %v", err)
	}
	_, err := f.svc.CheckInGuest(ctx, f.sess.ID, ticket.GuestCheckIn{Email: "grace@example.com", DoorToken: "x.y.z"})
	if !errors.Is(err, ticket.ErrDoorQRInvalid) {
		t.Fatalf("open, bad token: %v", err)
	}
	pass, err := f.svc.MintDoorQR(ctx, f.staff, f.sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.CheckInGuest(ctx, f.sess.ID, ticket.GuestCheckIn{Email: "grace@example.com", DoorToken: pass.Token}); err != nil {
		t.Fatalf("open, good token: %v", err)
	}
}

func TestMemberSelfCheckInNeedsNoDoorQR(t *testing.T) {
	t.Parallel()
	f := newDoorFixture(t, doorqr.ModeQR)
	ctx := context.Background()
	member := authz.Principal{ID: uuid.MustParse("77777777-7777-7777-7777-777777777777").String()}
	if _, err := f.svc.Apply(ctx, member, f.ev.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.CheckInMe(ctx, member, f.sess.ID); err != nil {
		t.Fatalf("member: %v", err)
	}
}
