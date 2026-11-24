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
	return newDoorFixtureWith(t, doorqr.Config{Mode: mode, MaxUses: 3, SessionGrace: 30 * time.Minute})
}

func newDoorFixtureWith(t *testing.T, cfg doorqr.Config) doorFixture {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 10, 3, 18, 0, 0, 0, time.UTC)
	gate := doorqr.NewGate([]byte("door qr test key, door qr test k"), cfg)
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
	for _, g := range []ticket.GuestInfo{
		{FirstName: "Grace", LastName: "Hopper", Email: "grace@example.com"},
		{FirstName: "Alan", LastName: "Turing", Email: "alan@example.com"},
	} {
		if _, err := svc.ApplyGuest(ctx, weblabLead, ev.ID, g); err != nil {
			t.Fatal(err)
		}
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
		pass, err := f.svc.MintDoorQR(ctx, p, f.sess.ID, doorqr.ClassDoor)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if pass.Token == "" || pass.URL == "" || pass.SessionID != f.sess.ID || pass.EventID != f.ev.ID {
			t.Fatalf("%s: %+v", name, pass)
		}
		if !pass.ExpiresAt.Equal(f.minted.Add(doorqr.DefaultTTL)) || pass.RefreshAfterSeconds != 15 {
			t.Fatalf("%s: times %+v", name, pass)
		}
	}
	for name, p := range map[string]authz.Principal{
		"other member": otherMember, "anonymous": {}, "forms": formsHop,
	} {
		if _, err := f.svc.MintDoorQR(ctx, p, f.sess.ID, doorqr.ClassDoor); !errors.Is(err, ticket.ErrForbidden) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if _, err := f.svc.MintDoorQR(ctx, f.staff, uuid.New(), doorqr.ClassDoor); !errors.Is(err, ticket.ErrNotFound) {
		t.Fatalf("unknown session: %v", err)
	}
}

// Door staff of one Event cannot mint a door QR for another Event's Session.
func TestMintDoorQRRefusesAnotherEventsSession(t *testing.T) {
	t.Parallel()
	f := newDoorFixture(t, doorqr.ModeQR)
	ctx := context.Background()
	other, err := f.events.Create(ctx, event.Event{Name: "Other", Location: "YTÜ", OwnerTeam: "GAMELAB"})
	if err != nil {
		t.Fatal(err)
	}
	day := seedDay(t, f.events, other.ID, "Day 1")
	sess := seedSession(t, f.events, day.ID, "Elsewhere")
	if _, err := f.svc.MintDoorQR(ctx, f.staff, sess.ID, doorqr.ClassDoor); !errors.Is(err, ticket.ErrForbidden) {
		t.Fatalf("cross-event mint: %v", err)
	}
	if _, err := f.svc.MintDoorQR(ctx, weblabLead, sess.ID, doorqr.ClassDoor); !errors.Is(err, ticket.ErrForbidden) {
		t.Fatalf("other team's leader: %v", err)
	}
}

func TestGuestCheckInAndMintOnlyWhileTheSessionIsOpen(t *testing.T) {
	t.Parallel()
	f := newDoorFixture(t, doorqr.ModeOpen)
	ctx := context.Background()
	at := func(d time.Duration) *time.Time { v := f.minted.Add(d); return &v }
	schedule := func(start, end *time.Time, cancelled bool) {
		t.Helper()
		s := f.sess
		s.StartTime, s.EndTime, s.Cancelled = start, end, cancelled
		if _, err := f.events.UpdateSession(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	guest := ticket.GuestCheckIn{Email: "ada@example.com"}

	schedule(at(2*time.Hour), at(3*time.Hour), false)
	if _, err := f.svc.CheckInGuest(ctx, f.sess.ID, guest); !errors.Is(err, ticket.ErrSessionClosed) {
		t.Fatalf("before the window: %v", err)
	}
	if _, err := f.svc.MintDoorQR(ctx, f.staff, f.sess.ID, doorqr.ClassDoor); !errors.Is(err, ticket.ErrSessionClosed) {
		t.Fatalf("mint before the window: %v", err)
	}
	schedule(at(-3*time.Hour), at(-2*time.Hour), false)
	if _, err := f.svc.CheckInGuest(ctx, f.sess.ID, guest); !errors.Is(err, ticket.ErrSessionClosed) {
		t.Fatalf("after the window: %v", err)
	}
	schedule(at(-time.Hour), at(time.Hour), true)
	if _, err := f.svc.CheckInGuest(ctx, f.sess.ID, guest); !errors.Is(err, ticket.ErrSessionClosed) {
		t.Fatalf("cancelled: %v", err)
	}
	// Within the grace before the start.
	schedule(at(10*time.Minute), at(time.Hour), false)
	if _, err := f.svc.MintDoorQR(ctx, f.staff, f.sess.ID, doorqr.ClassDoor); err != nil {
		t.Fatalf("mint within grace: %v", err)
	}
	if _, err := f.svc.CheckInGuest(ctx, f.sess.ID, guest); err != nil {
		t.Fatalf("within grace: %v", err)
	}
}

// With the deployed defaults the guest door opens 12 hours before a Session
// starts and stays open until 12 hours after it ends. Minting the door QR and
// checking in with it answer alike at every point, so the screen never shows
// a QR the check-in would refuse; a cancelled Session stays closed.
func TestGuestDoorWindowIsTwelveHoursEachSideForMintAndCheckIn(t *testing.T) {
	t.Parallel()
	cfg, err := doorqr.ConfigFromEnv(func(k string) string {
		if k == doorqr.ModeEnv {
			return string(doorqr.ModeQR)
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	f := newDoorFixtureWith(t, cfg)
	ctx := context.Background()
	at := func(d time.Duration) *time.Time { v := f.minted.Add(d); return &v }
	cases := []struct {
		name       string
		start, end *time.Time
		cancelled  bool
		email      string
		open       bool
	}{
		{"11h59m before the start", at(11*time.Hour + 59*time.Minute), at(14 * time.Hour), false, "ada@example.com", true},
		{"12h01m before the start", at(12*time.Hour + time.Minute), at(14 * time.Hour), false, "", false},
		{"11h59m after the end", at(-14 * time.Hour), at(-11*time.Hour - 59*time.Minute), false, "grace@example.com", true},
		{"12h01m after the end", at(-14 * time.Hour), at(-12*time.Hour - time.Minute), false, "", false},
		{"cancelled, within the window", at(time.Hour), at(2 * time.Hour), true, "", false},
	}
	for _, tc := range cases {
		s := f.sess
		s.StartTime, s.EndTime, s.Cancelled = tc.start, tc.end, tc.cancelled
		if _, err := f.events.UpdateSession(ctx, s); err != nil {
			t.Fatal(err)
		}
		pass, mintErr := f.svc.MintDoorQR(ctx, f.staff, f.sess.ID, doorqr.ClassDoor)
		if !tc.open {
			if !errors.Is(mintErr, ticket.ErrSessionClosed) {
				t.Fatalf("%s: mint %v", tc.name, mintErr)
			}
			// A token minted while the door was open does not reopen it.
			open := f.sess
			open.StartTime, open.EndTime, open.Cancelled = at(-time.Hour), at(time.Hour), false
			if _, err := f.events.UpdateSession(ctx, open); err != nil {
				t.Fatal(err)
			}
			valid, err := f.svc.MintDoorQR(ctx, f.staff, f.sess.ID, doorqr.ClassDoor)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.events.UpdateSession(ctx, s); err != nil {
				t.Fatal(err)
			}
			guest := ticket.GuestCheckIn{Email: "alan@example.com", DoorToken: valid.Token}
			if _, err := f.svc.CheckInGuest(ctx, f.sess.ID, guest); !errors.Is(err, ticket.ErrSessionClosed) {
				t.Fatalf("%s: check-in %v", tc.name, err)
			}
			continue
		}
		if mintErr != nil {
			t.Fatalf("%s: mint %v", tc.name, mintErr)
		}
		guest := ticket.GuestCheckIn{Email: tc.email, DoorToken: pass.Token}
		if _, err := f.svc.CheckInGuest(ctx, f.sess.ID, guest); err != nil {
			t.Fatalf("%s: check-in %v", tc.name, err)
		}
	}
}

func TestGuestCheckInQRModeRequiresDoorQR(t *testing.T) {
	t.Parallel()
	f := newDoorFixture(t, doorqr.ModeQR)
	ctx := context.Background()
	if _, err := f.svc.CheckInGuest(ctx, f.sess.ID, ticket.GuestCheckIn{Email: "ada@example.com"}); !errors.Is(err, ticket.ErrDoorQRRequired) {
		t.Fatalf("no token: %v", err)
	}
	pass, err := f.svc.MintDoorQR(ctx, f.staff, f.sess.ID, doorqr.ClassDoor)
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
	// An e-mail without a Ticket gives its use back: junk e-mails cannot use
	// a door QR up (the route's per-address budget limits them instead).
	for range 5 {
		_, err = f.svc.CheckInGuest(ctx, f.sess.ID, ticket.GuestCheckIn{Email: "nobody@example.com", DoorToken: pass.Token})
		if !errors.Is(err, ticket.ErrNotFound) {
			t.Fatalf("nobody: %v", err)
		}
	}
	// Ada (1), Ada again (2: already checked in counts), Grace (3).
	if _, err := f.svc.CheckInGuest(ctx, f.sess.ID, ticket.GuestCheckIn{Email: "grace@example.com", DoorToken: pass.Token}); err != nil {
		t.Fatalf("third guest: %v", err)
	}
	_, err = f.svc.CheckInGuest(ctx, f.sess.ID, ticket.GuestCheckIn{Email: "alan@example.com", DoorToken: pass.Token})
	if !errors.Is(err, ticket.ErrDoorQRUsedUp) {
		t.Fatalf("fourth guest: %v", err)
	}
}

func TestGuestCheckInRefusesExpiredAndForeignDoorQR(t *testing.T) {
	t.Parallel()
	f := newDoorFixture(t, doorqr.ModeQR)
	ctx := context.Background()
	pass, err := f.svc.MintDoorQR(ctx, f.staff, f.sess.ID, doorqr.ClassDoor)
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
	pass, err := f.svc.MintDoorQR(ctx, f.staff, f.sess.ID, doorqr.ClassDoor)
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
	pass, err := f.svc.MintDoorQR(ctx, f.staff, f.sess.ID, doorqr.ClassDoor)
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
	if _, err := f.svc.CheckInMe(ctx, member, f.sess.ID, ""); err != nil {
		t.Fatalf("member: %v", err)
	}
}

// memberWithTicket registers a member on the fixture's Event.
func memberWithTicket(t *testing.T, f doorFixture, id string) authz.Principal {
	t.Helper()
	member := authz.Principal{ID: uuid.MustParse(id).String()}
	if _, err := f.svc.Apply(context.Background(), member, f.ev.ID); err != nil {
		t.Fatal(err)
	}
	return member
}

// SESSION_QR_MODE=open keeps the static Session QR (no token) working for
// sky-app versions that read only that, but a token that is sent is checked.
func TestMemberSelfCheckInOpenModeTakesNoTokenButChecksOne(t *testing.T) {
	t.Parallel()
	f := newDoorFixtureWith(t, doorqr.Config{Mode: doorqr.ModeQR, SessionMode: doorqr.ModeOpen, MaxUses: 3, SessionGrace: 30 * time.Minute})
	ctx := context.Background()
	ada := memberWithTicket(t, f, "71111111-1111-1111-1111-111111111111")
	bob := memberWithTicket(t, f, "72222222-2222-2222-2222-222222222222")
	if _, err := f.svc.CheckInMe(ctx, ada, f.sess.ID, ""); err != nil {
		t.Fatalf("open, no token: %v", err)
	}
	if _, err := f.svc.CheckInMe(ctx, bob, f.sess.ID, "v1.garbage"); !errors.Is(err, ticket.ErrDoorQRInvalid) {
		t.Fatalf("open, bad token: %v", err)
	}
	pass, err := f.svc.MintDoorQR(ctx, f.staff, f.sess.ID, doorqr.ClassDisplay)
	if err != nil {
		t.Fatal(err)
	}
	if pass.SessionQRMode != "open" || pass.Use != "display" {
		t.Fatalf("pass %+v", pass)
	}
	if _, err := f.svc.CheckInMe(ctx, bob, f.sess.ID, pass.Token); err != nil {
		t.Fatalf("open, good token: %v", err)
	}
}

// SESSION_QR_MODE=qr retires the static Session QR: check-in/me needs a
// fresh door or attendance token of this Session, within its window.
func TestMemberSelfCheckInQRModeRequiresARotatingToken(t *testing.T) {
	t.Parallel()
	f := newDoorFixtureWith(t, doorqr.Config{Mode: doorqr.ModeOpen, SessionMode: doorqr.ModeQR, MaxUses: 1, DisplayMaxUses: 2, SessionGrace: 30 * time.Minute})
	ctx := context.Background()
	ada := memberWithTicket(t, f, "71111111-1111-1111-1111-111111111111")
	bob := memberWithTicket(t, f, "72222222-2222-2222-2222-222222222222")
	cem := memberWithTicket(t, f, "73333333-3333-3333-3333-333333333333")
	stranger := authz.Principal{ID: uuid.MustParse("74444444-4444-4444-4444-444444444444").String()}

	if _, err := f.svc.CheckInMe(ctx, ada, f.sess.ID, ""); !errors.Is(err, ticket.ErrDoorQRRequired) {
		t.Fatalf("static QR in qr mode: %v", err)
	}
	display, err := f.svc.MintDoorQR(ctx, f.staff, f.sess.ID, doorqr.ClassDisplay)
	if err != nil {
		t.Fatal(err)
	}
	if display.SessionQRMode != "qr" || display.MaxUses != 2 {
		t.Fatalf("display %+v", display)
	}
	// Someone without a Ticket gives the use back.
	for range 3 {
		if _, err := f.svc.CheckInMe(ctx, stranger, f.sess.ID, display.Token); !errors.Is(err, ticket.ErrNotFound) {
			t.Fatalf("no ticket: %v", err)
		}
	}
	if _, err := f.svc.CheckInMe(ctx, ada, f.sess.ID, display.Token); err != nil {
		t.Fatalf("ada: %v", err)
	}
	if _, err := f.svc.CheckInMe(ctx, ada, f.sess.ID, display.Token); !errors.Is(err, ticket.ErrConflict) {
		t.Fatalf("ada again: %v", err)
	}
	if _, err := f.svc.CheckInMe(ctx, bob, f.sess.ID, display.Token); !errors.Is(err, ticket.ErrDoorQRUsedUp) {
		t.Fatalf("over the display cap: %v", err)
	}
	// The door screen's token works too, with its own cap.
	door, err := f.svc.MintDoorQR(ctx, f.staff, f.sess.ID, doorqr.ClassDoor)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.CheckInMe(ctx, bob, f.sess.ID, door.Token); err != nil {
		t.Fatalf("door token: %v", err)
	}
	// A photo of the screen sent home dies with the token.
	f.advance(doorqr.DefaultTTL)
	if _, err := f.svc.CheckInMe(ctx, cem, f.sess.ID, door.Token); !errors.Is(err, ticket.ErrDoorQRExpired) {
		t.Fatalf("expired: %v", err)
	}
	// Another Session's token does not count here.
	other := seedSession(t, f.events, f.sess.EventDayID, "Closing")
	foreign, err := f.svc.MintDoorQR(ctx, f.staff, other.ID, doorqr.ClassDisplay)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.CheckInMe(ctx, cem, f.sess.ID, foreign.Token); !errors.Is(err, ticket.ErrDoorQRInvalid) {
		t.Fatalf("foreign: %v", err)
	}
}

// With a token or in qr mode, check-in/me keeps to the Session's window, as
// guest check-in does; open mode without a token is unchanged.
func TestMemberSelfCheckInWindowAppliesWithTokens(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, mode := range []doorqr.Mode{doorqr.ModeOpen, doorqr.ModeQR} {
		f := newDoorFixtureWith(t, doorqr.Config{SessionMode: mode, SessionGrace: 30 * time.Minute})
		ada := memberWithTicket(t, f, "71111111-1111-1111-1111-111111111111")
		pass, err := f.svc.MintDoorQR(ctx, f.staff, f.sess.ID, doorqr.ClassDisplay)
		if err != nil {
			t.Fatal(err)
		}
		end := f.minted.Add(-time.Hour)
		s := f.sess
		s.EndTime = &end
		if _, err := f.events.UpdateSession(ctx, s); err != nil {
			t.Fatal(err)
		}
		if _, err := f.svc.CheckInMe(ctx, ada, f.sess.ID, pass.Token); !errors.Is(err, ticket.ErrSessionClosed) {
			t.Fatalf("%s, with token after the window: %v", mode, err)
		}
		_, err = f.svc.CheckInMe(ctx, ada, f.sess.ID, "")
		switch mode {
		case doorqr.ModeOpen:
			if err != nil {
				t.Fatalf("open, static QR after the window (unchanged): %v", err)
			}
		case doorqr.ModeQR:
			if !errors.Is(err, ticket.ErrSessionClosed) {
				t.Fatalf("qr, no token after the window: %v", err)
			}
		}
	}
}
