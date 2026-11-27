package ticket_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/teamaudit"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
)

// A Walk-in on the database (docs/walk-in.md): the guest Ticket with what
// the guest gave, its mail queued in ticket_notifications, the day's entry
// (walk_in, by the door staff) and the ticket.walk_in row as door_staff; a
// second time the same e-mail is the same Ticket, nothing queued again, 409
// already_entered; a full Event writes nothing and queues nothing.
func TestWalkInOnPostgres(t *testing.T) {
	f := startFormResponsePostgres(t)
	ctx := context.Background()
	store := ticket.NewPostgresStore(f.pool)
	store.NotifyCreated(true)
	artlab := identity.Group{ID: uuid.NewString(), Name: "ARTLAB", Path: "/UYELER/SANAT/ARTLAB"}
	f.dir.PutGroup(artlab)
	svc := ticket.NewService(store, f.events, authz.NewAuthorizer(authz.DefaultPolicy()), f.users, f.dir, teamaudit.NewPostgres(f.pool))
	staffID := f.person(t, "kapi@example.com")
	staff := authz.Principal{ID: staffID.String()}
	as := teamaudit.WithActor(ctx, teamaudit.Actor{ID: staffID, Client: "core-frontend", CorrelationID: "req-1"})
	end := time.Now().Add(8 * time.Hour)
	venue := event.EntryVenue
	ev, err := f.events.Create(ctx, event.Event{Name: "ARTLAB", Location: "Tarihi Hamam", OwnerTeam: "ARTLAB", EndDate: &end,
		DoorStaffIDs: []uuid.UUID{staffID}, EntryMode: &venue})
	if err != nil {
		t.Fatal(err)
	}
	queued := func() int {
		t.Helper()
		var n int
		if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM ticket_notifications n JOIN tickets t ON t.id = n.ticket_id WHERE t.event_id = $1`, ev.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	w := ticket.WalkIn{FirstName: "Ayşe", LastName: "Yılmaz", Email: "Ayse@Example.com", PhoneNumber: "+90 532 123 45 67",
		University: "Yıldız Teknik Üniversitesi", Department: "Bilgisayar Mühendisliği", Grade: "graduate"}
	out, err := svc.WalkIn(as, staff, ev.ID, w)
	if err != nil {
		t.Fatal(err)
	}
	if out.Result != ticket.WalkInCreated || !out.TicketMail || out.Entry == nil || out.Entry.Source != ticket.EntryWalkIn || out.TicketCode == "" {
		t.Fatalf("answer %+v", out)
	}
	if queued() != 1 {
		t.Fatalf("%d queued mails", queued())
	}
	got, err := store.GetByGuestEvent(ctx, "ayse@example.com", ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.GuestPhoneNumber != "+905321234567" || got.GuestUniversity != "Yıldız Teknik Üniversitesi" ||
		got.GuestDepartment != "Bilgisayar Mühendisliği" || got.GuestGrade != "graduate" || got.TicketType != ticket.Guest {
		t.Fatalf("stored %+v", got)
	}
	var source string
	var by *uuid.UUID
	if err := f.pool.QueryRow(ctx, `SELECT source, by_user_id FROM venue_entries WHERE ticket_id = $1`, got.ID).Scan(&source, &by); err != nil ||
		source != "walk_in" || by == nil || *by != staffID {
		t.Fatalf("entry %q %v %v", source, by, err)
	}
	var basis, outcome, after string
	var via uuid.UUID
	if err := f.pool.QueryRow(ctx, `SELECT actor_basis, actor_via_group_id, outcome, after::text FROM team_audit_records WHERE action = 'ticket.walk_in'`).
		Scan(&basis, &via, &outcome, &after); err != nil {
		t.Fatal(err)
	}
	if basis != "door_staff" || via.String() != artlab.ID || outcome != "ok" {
		t.Fatalf("audit row %s %s %s", basis, via, outcome)
	}
	for _, private := range []string{"ayse", "532", "Yıldız", "Ayşe"} {
		if strings.Contains(after, private) {
			t.Fatalf("the audit row names the guest: %s", after)
		}
	}

	again, err := svc.WalkIn(as, staff, ev.ID, ticket.WalkIn{FirstName: "Ayşe", LastName: "Y", Email: "ayse@example.com"})
	var entered *ticket.EntryConflictError
	if !errors.As(err, &entered) || entered.Name != "Ayşe Yılmaz" {
		t.Fatalf("again: %+v %v", again, err)
	}
	if queued() != 1 {
		t.Fatalf("%d queued mails after the second", queued())
	}

	current, err := f.events.Get(ctx, ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	current.Capacity = 1
	if _, err := f.events.Update(ctx, current); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.WalkIn(as, staff, ev.ID, ticket.WalkIn{FirstName: "Mert", LastName: "Şahin", Email: "mert@example.com"}); !errors.Is(err, ticket.ErrEventFull) {
		t.Fatalf("full: %v", err)
	}
	if _, err := store.GetByGuestEvent(ctx, "mert@example.com", ev.ID); !errors.Is(err, ticket.ErrNotFound) {
		t.Fatalf("a Ticket on a full Event: %v", err)
	}
	if queued() != 1 {
		t.Fatalf("%d queued mails after the full one", queued())
	}
}
