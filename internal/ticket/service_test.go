package ticket_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/identity"
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

func seedDay(t *testing.T, events event.Store, eventID uuid.UUID, name string) event.Day {
	t.Helper()
	day, err := events.CreateDay(context.Background(), event.Day{EventID: eventID, Name: name})
	if err != nil {
		t.Fatal(err)
	}
	return day
}

func seedSession(t *testing.T, events event.Store, dayID uuid.UUID, title string) event.Session {
	t.Helper()
	sess, err := events.CreateSession(context.Background(), event.Session{
		EventDayID: dayID, Title: title, SpeakerName: "Ada", SessionType: "PRESENTATION",
	})
	if err != nil {
		t.Fatal(err)
	}
	return sess
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
	day := seedDay(t, events, ev.ID, "Day 1")
	sess := seedSession(t, events, day.ID, "Opening")
	userID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	tk, err := svc.Apply(ctx, authz.Principal{ID: userID.String()}, ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	leader := authz.Principal{ID: "lead", Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}

	ci, err := svc.CheckIn(ctx, leader, tk.ID, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ci.SessionID != sess.ID || ci.EventDayID != day.ID {
		t.Fatalf("checkin %+v", ci)
	}
	_, err = svc.CheckIn(ctx, leader, tk.ID, sess.ID)
	if !errors.Is(err, ticket.ErrConflict) {
		t.Fatalf("dup checkin: %v", err)
	}
}

func TestService_CheckInTwoSessionsOnSameDay(t *testing.T) {
	t.Parallel()
	events, svc := setup(t)
	ctx := context.Background()
	ev := seedEvent(t, events, "WEBLAB")
	day := seedDay(t, events, ev.ID, "Day 1")
	first := seedSession(t, events, day.ID, "Talk 1")
	second := seedSession(t, events, day.ID, "Talk 2")
	userID := uuid.MustParse("66666666-6666-6666-6666-666666666666")
	tk, err := svc.Apply(ctx, authz.Principal{ID: userID.String()}, ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	leader := authz.Principal{ID: "lead", Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}
	a, err := svc.CheckIn(ctx, leader, tk.ID, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	b, err := svc.CheckIn(ctx, leader, tk.ID, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if a.SessionID != first.ID || b.SessionID != second.ID {
		t.Fatalf("sessions %v %v", a, b)
	}
	if a.EventDayID != day.ID || b.EventDayID != day.ID {
		t.Fatalf("days %v %v", a, b)
	}
}

func TestService_CheckInForbiddenForMember(t *testing.T) {
	t.Parallel()
	events, svc := setup(t)
	ctx := context.Background()
	ev := seedEvent(t, events, "WEBLAB")
	day := seedDay(t, events, ev.ID, "Day 1")
	sess := seedSession(t, events, day.ID, "Opening")
	userID := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	tk, err := svc.Apply(ctx, authz.Principal{ID: userID.String()}, ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	member := authz.Principal{ID: "mem", Groups: []string{"/UYELER/ARGE/WEBLAB"}}
	_, err = svc.CheckIn(ctx, member, tk.ID, sess.ID)
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

func TestService_CheckInWrongSession(t *testing.T) {
	t.Parallel()
	events, svc := setup(t)
	ctx := context.Background()
	ev := seedEvent(t, events, "WEBLAB")
	other := seedEvent(t, events, "SKYSEC")
	day := seedDay(t, events, other.ID, "Other")
	sess := seedSession(t, events, day.ID, "Other talk")
	userID := uuid.MustParse("44444444-4444-4444-4444-444444444444")
	tk, err := svc.Apply(ctx, authz.Principal{ID: userID.String()}, ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	leader := authz.Principal{ID: "lead", Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}
	_, err = svc.CheckIn(ctx, leader, tk.ID, sess.ID)
	if !errors.Is(err, ticket.ErrInvalid) {
		t.Fatalf("got %v", err)
	}
}

func TestService_CheckInMeOwnerAndDuplicate(t *testing.T) {
	t.Parallel()
	events, svc := setup(t)
	ctx := context.Background()
	ev := seedEvent(t, events, "WEBLAB")
	day, err := events.CreateDay(ctx, event.Day{EventID: ev.ID, Name: "Day 1"})
	if err != nil {
		t.Fatal(err)
	}
	sess := seedSession(t, events, day.ID, "Opening")
	userID := uuid.MustParse("77777777-7777-7777-7777-777777777777")
	owner := authz.Principal{ID: userID.String()}
	tk, err := svc.Apply(ctx, owner, ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	ci, err := svc.CheckInMe(ctx, owner, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ci.TicketID != tk.ID || ci.SessionID != sess.ID {
		t.Fatalf("me %+v", ci)
	}
	_, err = svc.CheckInMe(ctx, owner, sess.ID)
	if !errors.Is(err, ticket.ErrConflict) {
		t.Fatalf("dup me: %v", err)
	}
	stranger := authz.Principal{ID: uuid.MustParse("88888888-8888-8888-8888-888888888888").String()}
	_, err = svc.CheckInMe(ctx, stranger, sess.ID)
	if !errors.Is(err, ticket.ErrNotFound) {
		t.Fatalf("stranger: %v", err)
	}
}

func TestService_CheckInGuestByEmail(t *testing.T) {
	t.Parallel()
	events, svc := setup(t)
	ctx := context.Background()
	ev := seedEvent(t, events, "WEBLAB")
	day, err := events.CreateDay(ctx, event.Day{EventID: ev.ID, Name: "Day 1"})
	if err != nil {
		t.Fatal(err)
	}
	sess := seedSession(t, events, day.ID, "Opening")
	created, err := svc.ApplyGuest(ctx, ev.ID, ticket.GuestInfo{
		FirstName: "Ada", LastName: "Lovelace", Email: "ada@example.com", PhoneNumber: "555",
	})
	if err != nil {
		t.Fatal(err)
	}
	ci, err := svc.CheckInGuest(ctx, sess.ID, "ada@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if ci.TicketID != created.ID || ci.SessionID != sess.ID {
		t.Fatalf("guest %+v", ci)
	}
	_, err = svc.CheckInGuest(ctx, sess.ID, "ada@example.com")
	if !errors.Is(err, ticket.ErrConflict) {
		t.Fatalf("dup guest: %v", err)
	}
	_, err = svc.CheckInGuest(ctx, sess.ID, "nobody@example.com")
	if !errors.Is(err, ticket.ErrNotFound) {
		t.Fatalf("missing guest: %v", err)
	}
}

func TestService_CheckInPrivilegedAndEmptyOwnerTeam(t *testing.T) {
	t.Parallel()
	events, svc := setup(t)
	ctx := context.Background()
	ev := seedEvent(t, events, "WEBLAB")
	day := seedDay(t, events, ev.ID, "Day 1")
	sess := seedSession(t, events, day.ID, "Opening")
	userID := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaa1")
	tk, err := svc.Apply(ctx, authz.Principal{ID: userID.String()}, ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	yk := authz.Principal{ID: "yk", Groups: []string{"/UYELER/YK"}}
	ci, err := svc.CheckIn(ctx, yk, tk.ID, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ci.SessionID != sess.ID {
		t.Fatalf("yk %+v", ci)
	}

	open := seedEvent(t, events, "")
	openDay := seedDay(t, events, open.ID, "Day 1")
	openSess := seedSession(t, events, openDay.ID, "Talk")
	openUser := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaa2")
	openTk, err := svc.Apply(ctx, authz.Principal{ID: openUser.String()}, open.ID)
	if err != nil {
		t.Fatal(err)
	}
	leader := authz.Principal{ID: "lead", Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}
	_, err = svc.CheckIn(ctx, leader, openTk.ID, openSess.ID)
	if !errors.Is(err, ticket.ErrForbidden) {
		t.Fatalf("leader empty owner: %v", err)
	}
	ci, err = svc.CheckIn(ctx, yk, openTk.ID, openSess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ci.SessionID != openSess.ID {
		t.Fatalf("yk empty owner %+v", ci)
	}
}

func TestService_CheckInMissingSessionAndStaffGuest(t *testing.T) {
	t.Parallel()
	events, svc := setup(t)
	ctx := context.Background()
	ev := seedEvent(t, events, "WEBLAB")
	day := seedDay(t, events, ev.ID, "Day 1")
	sess := seedSession(t, events, day.ID, "Opening")
	userID := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbb1")
	tk, err := svc.Apply(ctx, authz.Principal{ID: userID.String()}, ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	leader := authz.Principal{ID: "lead", Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}
	_, err = svc.CheckIn(ctx, leader, tk.ID, uuid.MustParse("99999999-9999-9999-9999-999999999999"))
	if !errors.Is(err, ticket.ErrNotFound) {
		t.Fatalf("missing session: %v", err)
	}
	guest, err := svc.ApplyGuest(ctx, ev.ID, ticket.GuestInfo{
		FirstName: "Ada", LastName: "Lovelace", Email: "guest@example.com", PhoneNumber: "555",
	})
	if err != nil {
		t.Fatal(err)
	}
	ci, err := svc.CheckIn(ctx, leader, guest.ID, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ci.TicketID != guest.ID || ci.SessionID != sess.ID {
		t.Fatalf("staff guest %+v", ci)
	}
}

func TestService_CheckInAssignedDoorStaff(t *testing.T) {
	t.Parallel()
	events, svc := setup(t)
	ctx := context.Background()
	staff := uuid.MustParse("dddddddd-dddd-dddd-dddd-dddddddddddd")
	ev, err := events.Create(ctx, event.Event{
		Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB", DoorStaffIDs: []uuid.UUID{staff},
	})
	if err != nil {
		t.Fatal(err)
	}
	day := seedDay(t, events, ev.ID, "Day 1")
	sess := seedSession(t, events, day.ID, "Opening")
	userID := uuid.MustParse("eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee")
	tk, err := svc.Apply(ctx, authz.Principal{ID: userID.String()}, ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.CheckIn(ctx, authz.Principal{ID: staff.String()}, tk.ID, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
}

func TestService_CheckInDoorStaffEmptyOwnerTeam(t *testing.T) {
	t.Parallel()
	events, svc := setup(t)
	ctx := context.Background()
	staff := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	ev, err := events.Create(ctx, event.Event{
		Name: "Seminer", Location: "YTÜ", DoorStaffIDs: []uuid.UUID{staff},
	})
	if err != nil {
		t.Fatal(err)
	}
	day := seedDay(t, events, ev.ID, "Day 1")
	sess := seedSession(t, events, day.ID, "Talk")
	userID := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	tk, err := svc.Apply(ctx, authz.Principal{ID: userID.String()}, ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CheckIn(ctx, authz.Principal{ID: staff.String()}, tk.ID, sess.ID); err != nil {
		t.Fatal(err)
	}
}

func TestService_CheckInOwnerMemberWithTeamDoorScan(t *testing.T) {
	t.Parallel()
	events := event.NewMemoryStore()
	dir := identity.NewMemory()
	dir.PutGroup(identity.Group{
		ID:         "g-weblab",
		Name:       "WEBLAB",
		Path:       "/UYELER/ARGE/WEBLAB",
		Attributes: map[string]string{"team_door_scan": "true"},
	})
	svc := ticket.NewService(ticket.NewMemoryStore(), events, authz.NewAuthorizer(authz.DefaultPolicy()), dir)
	ctx := context.Background()
	ev := seedEvent(t, events, "WEBLAB")
	day := seedDay(t, events, ev.ID, "Day 1")
	sess := seedSession(t, events, day.ID, "Opening")
	userID := uuid.MustParse("cccccccc-cccc-cccc-cccc-cccccccccccc")
	tk, err := svc.Apply(ctx, authz.Principal{ID: userID.String()}, ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	member := authz.Principal{ID: "mem", Groups: []string{"/UYELER/ARGE/WEBLAB"}}
	if _, err := svc.CheckIn(ctx, member, tk.ID, sess.ID); err != nil {
		t.Fatal(err)
	}
}

func TestService_CheckInOwnerMemberWithoutTeamDoorScan(t *testing.T) {
	t.Parallel()
	events := event.NewMemoryStore()
	dir := identity.NewMemory()
	dir.PutGroup(identity.Group{ID: "g-weblab", Name: "WEBLAB", Path: "/UYELER/ARGE/WEBLAB"})
	svc := ticket.NewService(ticket.NewMemoryStore(), events, authz.NewAuthorizer(authz.DefaultPolicy()), dir)
	ctx := context.Background()
	ev := seedEvent(t, events, "WEBLAB")
	day := seedDay(t, events, ev.ID, "Day 1")
	sess := seedSession(t, events, day.ID, "Opening")
	userID := uuid.MustParse("ffffffff-ffff-ffff-ffff-ffffffffffff")
	tk, err := svc.Apply(ctx, authz.Principal{ID: userID.String()}, ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	member := authz.Principal{ID: "mem", Groups: []string{"/UYELER/ARGE/WEBLAB"}}
	if _, err := svc.CheckIn(ctx, member, tk.ID, sess.ID); !errors.Is(err, ticket.ErrForbidden) {
		t.Fatalf("got %v", err)
	}
}

func TestService_CheckInUserFindsTicketAndUsesDoorStaff(t *testing.T) {
	t.Parallel()
	events, svc := setup(t)
	ctx := context.Background()
	staff := uuid.MustParse("12121212-1212-1212-1212-121212121212")
	ev, err := events.Create(ctx, event.Event{
		Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB", DoorStaffIDs: []uuid.UUID{staff},
	})
	if err != nil {
		t.Fatal(err)
	}
	day := seedDay(t, events, ev.ID, "Day 1")
	sess := seedSession(t, events, day.ID, "Opening")
	userID := uuid.MustParse("13131313-1313-1313-1313-131313131313")
	tk, err := svc.Apply(ctx, authz.Principal{ID: userID.String()}, ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	member := authz.Principal{ID: "mem", Groups: []string{"/UYELER/ARGE/WEBLAB"}}
	if _, err := svc.CheckInUser(ctx, member, sess.ID, userID); !errors.Is(err, ticket.ErrForbidden) {
		t.Fatalf("member %v", err)
	}
	ci, err := svc.CheckInUser(ctx, authz.Principal{ID: staff.String()}, sess.ID, userID)
	if err != nil {
		t.Fatal(err)
	}
	if ci.TicketID != tk.ID || ci.SessionID != sess.ID {
		t.Fatalf("check-in %+v", ci)
	}
	if _, err := svc.CheckInUser(ctx, authz.Principal{ID: staff.String()}, sess.ID, userID); !errors.Is(err, ticket.ErrConflict) {
		t.Fatalf("dup %v", err)
	}
	missing := uuid.MustParse("14141414-1414-1414-1414-141414141414")
	if _, err := svc.CheckInUser(ctx, authz.Principal{ID: staff.String()}, sess.ID, missing); !errors.Is(err, ticket.ErrNotFound) {
		t.Fatalf("missing ticket %v", err)
	}
}
