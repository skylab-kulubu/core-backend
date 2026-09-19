package ticket_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

func setup(t *testing.T) (event.Store, ticket.Service) {
	t.Helper()
	events := event.NewMemoryStore()
	svc := ticket.NewService(ticket.NewMemoryStore(), events, authz.NewAuthorizer(authz.DefaultPolicy()))
	return events, svc
}

func setupApplyForOther(t *testing.T) (event.Store, user.Store, ticket.Service) {
	t.Helper()
	events := event.NewMemoryStore()
	users := user.NewMemoryStore()
	svc := ticket.NewService(ticket.NewMemoryStore(), events, authz.NewAuthorizer(authz.DefaultPolicy()), users)
	return events, users, svc
}

func seedUser(t *testing.T, users user.Store, id uuid.UUID) {
	t.Helper()
	if _, _, err := user.NewService(users).Ensure(context.Background(), id, user.Profile{
		Email: "ada@example.com", FirstName: "Ada", LastName: "Lovelace",
	}); err != nil {
		t.Fatal(err)
	}
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

type unavailablePersonReader struct{}

func (unavailablePersonReader) GetUser(context.Context, uuid.UUID) (identity.Person, error) {
	return identity.Person{}, errors.New("directory unavailable")
}

type optimizedDoorStore struct {
	*ticket.MemoryStore
	identity         ticket.DoorTicketIdentity
	activity         ticket.DoorActivity
	listCalls        int
	searchCalls      int
	ownerLookupCalls int
	activityCalls    int
}

func (s *optimizedDoorStore) ListByEvent(ctx context.Context, eventID uuid.UUID) ([]ticket.Ticket, error) {
	s.listCalls++
	return s.MemoryStore.ListByEvent(ctx, eventID)
}

func (s *optimizedDoorStore) SearchDoorTickets(
	context.Context,
	uuid.UUID,
	string,
	bool,
	int,
) ([]ticket.DoorTicketIdentity, error) {
	s.searchCalls++
	return []ticket.DoorTicketIdentity{s.identity}, nil
}

func (s *optimizedDoorStore) DoorTicketsByOwners(
	context.Context,
	uuid.UUID,
	[]uuid.UUID,
) ([]ticket.DoorTicketIdentity, error) {
	s.ownerLookupCalls++
	return []ticket.DoorTicketIdentity{s.identity}, nil
}

func (s *optimizedDoorStore) DoorSessionActivity(
	context.Context,
	uuid.UUID,
	int,
) (ticket.DoorActivity, error) {
	s.activityCalls++
	return s.activity, nil
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

func TestService_ApplyForOtherLeaderCreatesRegistered(t *testing.T) {
	t.Parallel()
	events, users, svc := setupApplyForOther(t)
	ctx := context.Background()
	ev := seedEvent(t, events, "WEBLAB")
	target := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	seedUser(t, users, target)
	leader := authz.Principal{ID: "lead", Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}

	created, err := svc.ApplyForOther(ctx, leader, ev.ID, target)
	if err != nil {
		t.Fatal(err)
	}
	if created.TicketType != ticket.Registered || created.OwnerID == nil || *created.OwnerID != target {
		t.Fatalf("created %+v", created)
	}
	if created.GuestEmail != "" {
		t.Fatalf("dumped onto guest apply %+v", created)
	}

	got, err := svc.GetByUserEvent(ctx, leader, target, ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != created.ID {
		t.Fatalf("got %+v", got)
	}
}

func TestService_ListByEventIncludesRegisteredOwnerSummary(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	events := event.NewMemoryStore()
	users := user.NewMemoryStore()
	dir := identity.NewMemory()
	tickets := ticket.NewMemoryStore()
	svc := ticket.NewService(tickets, events, authz.NewAuthorizer(authz.DefaultPolicy()), users, dir)
	ev := seedEvent(t, events, "WEBLAB")
	target := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	dir.PutUser(identity.Person{ID: target, Email: "ada@example.com", FirstName: "Ada", LastName: "Lovelace", SchoolEmail: "private@std.yildiz.edu.tr"})
	leader := authz.Principal{ID: "lead", Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}
	if _, err := svc.ApplyForOther(ctx, leader, ev.ID, target); err != nil {
		t.Fatal(err)
	}

	rows, err := svc.ListByEvent(ctx, leader, ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Owner == nil {
		t.Fatalf("rows %+v", rows)
	}
	if rows[0].Owner.ID != target || rows[0].Owner.Email != "ada@example.com" || rows[0].Owner.FirstName != "Ada" {
		t.Fatalf("owner %+v", rows[0].Owner)
	}
}

func TestService_ListByEventUsesShadowOwnerWhenDirectoryIsUnavailable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	events := event.NewMemoryStore()
	users := user.NewMemoryStore()
	tickets := ticket.NewMemoryStore()
	svc := ticket.NewService(tickets, events, authz.NewAuthorizer(authz.DefaultPolicy()), users, unavailablePersonReader{})
	ev := seedEvent(t, events, "WEBLAB")
	ownerID := uuid.New()
	seedUser(t, users, ownerID)
	if _, err := tickets.Create(ctx, ticket.Ticket{EventID: ev.ID, TicketType: ticket.Registered, OwnerID: &ownerID}); err != nil {
		t.Fatal(err)
	}

	leader := authz.Principal{ID: "lead", Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}
	rows, err := svc.ListByEvent(ctx, leader, ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Owner == nil || rows[0].Owner.FirstName != "Ada" || rows[0].Owner.LastName != "Lovelace" {
		t.Fatalf("owner summary missing during directory outage: %+v", rows)
	}
}

func TestService_ListAssignableUsersCapsDirectoryResults(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	events := event.NewMemoryStore()
	dir := identity.NewMemory()
	svc := ticket.NewService(ticket.NewMemoryStore(), events, authz.NewAuthorizer(authz.DefaultPolicy()), dir)
	ev := seedEvent(t, events, "WEBLAB")
	for i := range 25 {
		dir.PutUser(identity.Person{
			ID:        uuid.New(),
			Email:     fmt.Sprintf("member%02d@example.com", i),
			FirstName: "Member",
			LastName:  fmt.Sprintf("%02d", i),
		})
	}
	leader := authz.Principal{ID: "lead", Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}
	people, err := svc.ListAssignableUsers(ctx, leader, ev.ID, "member")
	if err != nil {
		t.Fatal(err)
	}
	if len(people) != 20 {
		t.Fatalf("got %d assignable users, want 20", len(people))
	}
	if slices.Contains(dir.Ops, "ListUsers") || !slices.Contains(dir.Ops, "SearchUsers") {
		t.Fatalf("directory operations = %v, want bounded SearchUsers only", dir.Ops)
	}
}

func TestService_ListAssignableUsersDoesNotMatchStaleDirectoryNames(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	events := event.NewMemoryStore()
	dir := identity.NewMemory()
	users := user.NewMemoryStore()
	target := uuid.New()
	dir.PutUser(identity.Person{
		ID: target, Email: "ada@example.com", FirstName: "Legacy", LastName: "Name",
	})
	if _, _, err := user.NewService(users).Ensure(ctx, target, user.Profile{
		Email: "ada@example.com", FirstName: "Grace", LastName: "Hopper",
	}); err != nil {
		t.Fatal(err)
	}
	svc := ticket.NewService(ticket.NewMemoryStore(), events, authz.NewAuthorizer(authz.DefaultPolicy()), users, dir)
	ev := seedEvent(t, events, "WEBLAB")
	leader := authz.Principal{ID: "lead", Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}

	stale, err := svc.ListAssignableUsers(ctx, leader, ev.ID, "Legacy")
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 0 {
		t.Fatalf("stale directory name matched shadow profile: %+v", stale)
	}
	current, err := svc.ListAssignableUsers(ctx, leader, ev.ID, "Grace")
	if err != nil {
		t.Fatal(err)
	}
	if len(current) != 1 || current[0].ID != target || current[0].FirstName != "Grace" {
		t.Fatalf("current shadow name missing: %+v", current)
	}
}

func TestService_ApplyForOtherPrivilegedCreatesRegistered(t *testing.T) {
	t.Parallel()
	events, users, svc := setupApplyForOther(t)
	ctx := context.Background()
	ev := seedEvent(t, events, "WEBLAB")
	target := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	seedUser(t, users, target)
	yk := authz.Principal{ID: "yk", Groups: []string{"/UYELER/YK"}}

	created, err := svc.ApplyForOther(ctx, yk, ev.ID, target)
	if err != nil {
		t.Fatal(err)
	}
	if created.TicketType != ticket.Registered || created.OwnerID == nil || *created.OwnerID != target {
		t.Fatalf("created %+v", created)
	}
}

func TestService_ApplyForOtherDuplicateConflict(t *testing.T) {
	t.Parallel()
	events, users, svc := setupApplyForOther(t)
	ctx := context.Background()
	ev := seedEvent(t, events, "WEBLAB")
	target := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	seedUser(t, users, target)
	leader := authz.Principal{ID: "lead", Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}

	if _, err := svc.ApplyForOther(ctx, leader, ev.ID, target); err != nil {
		t.Fatal(err)
	}
	_, err := svc.ApplyForOther(ctx, leader, ev.ID, target)
	if !errors.Is(err, ticket.ErrConflict) {
		t.Fatalf("dup: %v", err)
	}
}

func TestService_ApplyForOtherMemberForbidden(t *testing.T) {
	t.Parallel()
	events, users, svc := setupApplyForOther(t)
	ev := seedEvent(t, events, "WEBLAB")
	target := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	seedUser(t, users, target)
	member := authz.Principal{ID: "mem", Groups: []string{"/UYELER/ARGE/WEBLAB"}}
	_, err := svc.ApplyForOther(context.Background(), member, ev.ID, target)
	if !errors.Is(err, ticket.ErrForbidden) {
		t.Fatalf("got %v", err)
	}
}

func TestService_ApplyForOtherUnknownUser(t *testing.T) {
	t.Parallel()
	events, _, svc := setupApplyForOther(t)
	ev := seedEvent(t, events, "WEBLAB")
	missing := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	leader := authz.Principal{ID: "lead", Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}
	_, err := svc.ApplyForOther(context.Background(), leader, ev.ID, missing)
	if !errors.Is(err, ticket.ErrNotFound) {
		t.Fatalf("got %v", err)
	}
}

func TestService_ApplyForOtherDirectoryOnlyCreatesRegistered(t *testing.T) {
	t.Parallel()
	events := event.NewMemoryStore()
	users := user.NewMemoryStore()
	dir := identity.NewMemory()
	svc := ticket.NewService(ticket.NewMemoryStore(), events, authz.NewAuthorizer(authz.DefaultPolicy()), users, dir)
	ctx := context.Background()
	ev := seedEvent(t, events, "WEBLAB")
	target := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	dir.PutUser(identity.Person{ID: target, Email: "ada@example.com", FirstName: "Ada", LastName: "Lovelace"})
	if _, err := users.Get(ctx, target); !errors.Is(err, user.ErrNotFound) {
		t.Fatalf("shadow should be missing: %v", err)
	}
	leader := authz.Principal{ID: "lead", Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}
	created, err := svc.ApplyForOther(ctx, leader, ev.ID, target)
	if err != nil {
		t.Fatal(err)
	}
	if created.TicketType != ticket.Registered || created.OwnerID == nil || *created.OwnerID != target {
		t.Fatalf("created %+v", created)
	}
}

func TestService_ApplyForOtherDoorStaffForbidden(t *testing.T) {
	t.Parallel()
	events, users, svc := setupApplyForOther(t)
	ctx := context.Background()
	staff := uuid.MustParse("dddddddd-dddd-dddd-dddd-dddddddddddd")
	ev, err := events.Create(ctx, event.Event{
		Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB", DoorStaffIDs: []uuid.UUID{staff},
	})
	if err != nil {
		t.Fatal(err)
	}
	target := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	seedUser(t, users, target)
	_, err = svc.ApplyForOther(ctx, authz.Principal{ID: staff.String(), Groups: []string{"/UYELER"}}, ev.ID, target)
	if !errors.Is(err, ticket.ErrForbidden) {
		t.Fatalf("got %v", err)
	}
}

func TestService_ApplyForOtherGecekoduMemberForbidden(t *testing.T) {
	t.Parallel()
	events, users, svc := setupApplyForOther(t)
	ev := seedEvent(t, events, "GECEKODU")
	target := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	seedUser(t, users, target)
	member := authz.Principal{ID: "mem", Groups: []string{"/UYELER/ORGANIZASYON/GECEKODU"}}
	_, err := svc.ApplyForOther(context.Background(), member, ev.ID, target)
	if !errors.Is(err, ticket.ErrForbidden) {
		t.Fatalf("got %v", err)
	}
}

func TestService_ApplyForOtherEmptyOwnerTeam(t *testing.T) {
	t.Parallel()
	events, users, svc := setupApplyForOther(t)
	ctx := context.Background()
	ev, err := events.Create(ctx, event.Event{Name: "Seminer", Location: "YTÜ"})
	if err != nil {
		t.Fatal(err)
	}
	target := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	seedUser(t, users, target)
	leader := authz.Principal{ID: "lead", Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}
	if _, err := svc.ApplyForOther(ctx, leader, ev.ID, target); !errors.Is(err, ticket.ErrForbidden) {
		t.Fatalf("leader empty owner: %v", err)
	}
	yk := authz.Principal{ID: "yk", Groups: []string{"/UYELER/YK"}}
	created, err := svc.ApplyForOther(ctx, yk, ev.ID, target)
	if err != nil {
		t.Fatal(err)
	}
	if created.TicketType != ticket.Registered || created.OwnerID == nil || *created.OwnerID != target {
		t.Fatalf("created %+v", created)
	}
}

func TestService_ApplyGuestFromAdminDoesNotOwnTicket(t *testing.T) {
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
	if created.OwnerID != nil {
		t.Fatalf("admin became guest owner %+v", created)
	}
}

func TestService_ApplyGuest(t *testing.T) {
	t.Parallel()
	events, svc := setup(t)
	ctx := context.Background()
	ev := seedEvent(t, events, "WEBLAB")

	created, err := svc.ApplyGuest(ctx, ev.ID, ticket.GuestInfo{
		FirstName: "Ada", LastName: "Lovelace", Email: "Ada@Example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.TicketType != ticket.Guest || created.GuestEmail != "ada@example.com" {
		t.Fatalf("created %+v", created)
	}

	updated, err := svc.ApplyGuest(ctx, ev.ID, ticket.GuestInfo{
		FirstName: "Augusta", LastName: "Byron", Email: "ADA@example.com", PhoneNumber: "555",
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.ID != created.ID {
		t.Fatalf("upsert id %s want %s", updated.ID, created.ID)
	}
	if updated.GuestFirstName != "Augusta" || updated.GuestLastName != "Byron" || updated.GuestPhoneNumber != "555" {
		t.Fatalf("updated %+v", updated)
	}

	listed, err := svc.ListByEvent(ctx, authz.Principal{ID: "lead", Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}, ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].GuestEmail != "ada@example.com" {
		t.Fatalf("roster %+v", listed)
	}

	_, err = svc.ApplyGuest(ctx, ev.ID, ticket.GuestInfo{FirstName: "Ada", Email: "ada@example.com"})
	if !errors.Is(err, ticket.ErrInvalid) {
		t.Fatalf("missing last name: %v", err)
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

func TestService_ResolveAndCheckInUsesValidateScopeAndExactUniqueMatch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	events := event.NewMemoryStore()
	tickets := ticket.NewMemoryStore()
	users := user.NewMemoryStore()
	staff := uuid.MustParse("15151515-1515-1515-1515-151515151515")
	ev, err := events.Create(ctx, event.Event{
		Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB", DoorStaffIDs: []uuid.UUID{staff},
	})
	if err != nil {
		t.Fatal(err)
	}
	day := seedDay(t, events, ev.ID, "Day 1")
	sess := seedSession(t, events, day.ID, "Opening")
	owner := uuid.MustParse("16161616-1616-1616-1616-161616161616")
	seedUser(t, users, owner)
	registered, err := tickets.Create(ctx, ticket.Ticket{EventID: ev.ID, TicketType: ticket.Registered, OwnerID: &owner})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tickets.Create(ctx, ticket.Ticket{
		EventID: ev.ID, TicketType: ticket.Guest, GuestFirstName: "Ada", GuestLastName: "Lovelace", GuestEmail: "guest@example.com",
	}); err != nil {
		t.Fatal(err)
	}
	svc := ticket.NewService(tickets, events, authz.NewAuthorizer(authz.DefaultPolicy()), users)
	principal := authz.Principal{ID: staff.String()}

	attendees, err := svc.SearchDoorAttendees(ctx, principal, ev.ID, "ada")
	if err != nil {
		t.Fatal(err)
	}
	if len(attendees) != 2 || attendees[0].Name == "" || attendees[1].Name == "" {
		t.Fatalf("attendees %+v", attendees)
	}
	if _, err := svc.SearchDoorAttendees(ctx, authz.Principal{ID: "unassigned"}, ev.ID, "ada"); !errors.Is(err, ticket.ErrForbidden) {
		t.Fatalf("unassigned attendee search got %v", err)
	}
	if _, err := svc.ResolveAndCheckIn(ctx, authz.Principal{ID: "unassigned"}, sess.ID, ticket.DoorCheckInTarget{Query: "ada@example.com"}); !errors.Is(err, ticket.ErrForbidden) {
		t.Fatalf("unassigned got %v", err)
	}
	if _, err := svc.ResolveAndCheckIn(ctx, principal, sess.ID, ticket.DoorCheckInTarget{Query: "Ada Lovelace"}); !errors.Is(err, ticket.ErrAmbiguous) {
		t.Fatalf("ambiguous got %v", err)
	}
	resolved, err := svc.ResolveAndCheckIn(ctx, principal, sess.ID, ticket.DoorCheckInTarget{Query: " ADA@EXAMPLE.COM "})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.TicketID != registered.ID || resolved.PersonName != "Ada Lovelace" || resolved.SessionID != sess.ID {
		t.Fatalf("resolved %+v", resolved)
	}
	activity, err := svc.DoorActivity(ctx, principal, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if activity.Total != 1 || len(activity.Items) != 1 || activity.Items[0].PersonName != "Ada Lovelace" {
		t.Fatalf("activity %+v", activity)
	}
	if _, err := svc.DoorActivity(ctx, authz.Principal{ID: "unassigned"}, sess.ID); !errors.Is(err, ticket.ErrForbidden) {
		t.Fatalf("unassigned activity got %v", err)
	}
}

func TestService_ListDoorEventsUsesExplicitAssignmentBeforeTeamLookup(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	events := event.NewMemoryStore()
	dir := identity.NewMemory()
	staff := uuid.New()
	ev, err := events.Create(ctx, event.Event{
		Name: "Assigned", Location: "YTÜ", OwnerTeam: "WEBLAB", DoorStaffIDs: []uuid.UUID{staff},
	})
	if err != nil {
		t.Fatal(err)
	}
	svc := ticket.NewService(ticket.NewMemoryStore(), events, authz.NewAuthorizer(authz.DefaultPolicy()), dir)
	listed, err := svc.ListDoorEvents(ctx, authz.Principal{ID: staff.String()})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].ID != ev.ID {
		t.Fatalf("listed %+v", listed)
	}
	if slices.Contains(dir.Ops, "GetGroup") {
		t.Fatalf("explicit assignment performed remote team lookup: %v", dir.Ops)
	}
}

func TestService_ListDoorEventsIncludesTeamDoorScanMembers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	events := event.NewMemoryStore()
	dir := identity.NewMemory()
	dir.PutGroup(identity.Group{
		ID: "g-weblab", Name: "WEBLAB", Path: "/UYELER/ARGE/WEBLAB",
		Attributes: map[string]string{"team_door_scan": "true"},
	})
	ev := seedEvent(t, events, "WEBLAB")
	svc := ticket.NewService(ticket.NewMemoryStore(), events, authz.NewAuthorizer(authz.DefaultPolicy()), dir)
	listed, err := svc.ListDoorEvents(ctx, authz.Principal{
		ID: "member", Groups: []string{"/UYELER/ARGE/WEBLAB"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].ID != ev.ID {
		t.Fatalf("listed %+v", listed)
	}
	if !slices.Contains(dir.Ops, "GetGroup") {
		t.Fatalf("team door grant was not checked: %v", dir.Ops)
	}
}

func TestService_DoorOperationsUseBoundedStoreQueries(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	events := event.NewMemoryStore()
	staff := uuid.New()
	ev, err := events.Create(ctx, event.Event{
		Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB", DoorStaffIDs: []uuid.UUID{staff},
	})
	if err != nil {
		t.Fatal(err)
	}
	day := seedDay(t, events, ev.ID, "Day 1")
	session := seedSession(t, events, day.ID, "Opening")
	ownerID := uuid.New()
	store := &optimizedDoorStore{MemoryStore: ticket.NewMemoryStore()}
	created, err := store.Create(ctx, ticket.Ticket{
		EventID: ev.ID, TicketType: ticket.Registered, OwnerID: &ownerID,
	})
	if err != nil {
		t.Fatal(err)
	}
	store.identity = ticket.DoorTicketIdentity{
		Ticket: created, Name: "Ada Lovelace", Email: "ada@example.com", Found: true,
	}
	store.activity = ticket.DoorActivity{
		Total: 1,
		Items: []ticket.DoorCheckIn{{
			ID: uuid.New(), SessionID: session.ID, EventDayID: day.ID,
			PersonName: "Ada Lovelace",
		}},
	}
	svc := ticket.NewService(store, events, authz.NewAuthorizer(authz.DefaultPolicy()))
	principal := authz.Principal{ID: staff.String()}

	attendees, err := svc.SearchDoorAttendees(ctx, principal, ev.ID, "ada")
	if err != nil || len(attendees) != 1 || attendees[0].Name != "Ada Lovelace" {
		t.Fatalf("attendees=%+v err=%v", attendees, err)
	}
	if _, err := svc.ResolveAndCheckIn(
		ctx,
		principal,
		session.ID,
		ticket.DoorCheckInTarget{Query: "ada@example.com"},
	); err != nil {
		t.Fatal(err)
	}
	activity, err := svc.DoorActivity(ctx, principal, session.ID)
	if err != nil || activity.Total != 1 {
		t.Fatalf("activity=%+v err=%v", activity, err)
	}
	if store.listCalls != 0 || store.searchCalls != 2 || store.activityCalls != 1 {
		t.Fatalf(
			"list=%d search=%d owner=%d activity=%d",
			store.listCalls,
			store.searchCalls,
			store.ownerLookupCalls,
			store.activityCalls,
		)
	}
}
