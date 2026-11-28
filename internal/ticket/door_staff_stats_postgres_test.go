package ticket_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/teamaudit"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// The door staff statistics on the database (docs/door-staff-stats.md), an
// ARTLAB-like night: two door staff, a Walk-in, scans and desk entries, a
// Session's Check-ins (one written before the door kept its writer, named
// by the entry it wrote), the people's own, a mark taken back. The
// Event's leader reads every row with the others; a door staff reads
// their own row; a member of another team is refused.
func TestDoorStaffStatsOnPostgres(t *testing.T) {
	f := startFormResponsePostgres(t)
	ctx := context.Background()
	store := ticket.NewPostgresStore(f.pool)
	artlab := identity.Group{ID: uuid.NewString(), Name: "ARTLAB", Path: "/UYELER/SANAT/ARTLAB"}
	f.dir.PutGroup(artlab)
	svc := ticket.NewService(store, f.events, authz.NewAuthorizer(authz.DefaultPolicy()), f.users, f.dir, teamaudit.NewPostgres(f.pool))
	named := func(email, first, last string) uuid.UUID {
		t.Helper()
		id := uuid.New()
		if _, _, err := user.NewService(f.users).Ensure(ctx, id, user.Profile{Email: email, FirstName: first, LastName: last}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	aID := named("elif@example.com", "Elif", "Kaya")
	bID := named("mert@example.com", "Mert", "Demir")
	leadID := named("lider@example.com", "Selin", "Ak")
	a := authz.Principal{ID: aID.String()}
	b := authz.Principal{ID: bID.String()}
	lead := authz.Principal{ID: leadID.String(), Groups: []string{"/UYELER/SANAT/ARTLAB/LIDERLER"}}
	start := time.Now().Add(-2 * time.Hour)
	end := time.Now().Add(8 * time.Hour)
	venue := event.EntryVenue
	ev, err := f.events.Create(ctx, event.Event{Name: "ARTLAB", Location: "Tarihi Hamam", OwnerTeam: "ARTLAB",
		StartDate: &start, EndDate: &end, DoorStaffIDs: []uuid.UUID{aID, bID}, EntryMode: &venue})
	if err != nil {
		t.Fatal(err)
	}
	day, err := f.events.CreateDay(ctx, event.Day{EventID: ev.ID, Name: "1. gün", StartDate: &start})
	if err != nil {
		t.Fatal(err)
	}
	talk := seedSession(t, f.events, day.ID, "Konuşma")
	guest := func(email string) ticket.Ticket {
		t.Helper()
		tk, err := store.Create(ctx, ticket.Ticket{EventID: ev.ID, TicketType: ticket.Guest, GuestFirstName: "Konuk", GuestLastName: email, GuestEmail: email})
		if err != nil {
			t.Fatal(err)
		}
		return tk
	}

	// A: a Walk-in and two desk entries; B: one desk entry, and a desk
	// Check-in at the talk.
	as := teamaudit.WithActor(ctx, teamaudit.Actor{ID: aID, Client: "core-frontend", CorrelationID: "req-1"})
	if _, err := svc.WalkIn(as, a, ev.ID, ticket.WalkIn{FirstName: "Ayşe", LastName: "Yılmaz", Email: "ayse@example.com"}); err != nil {
		t.Fatal(err)
	}
	var g2Entry uuid.UUID
	for _, email := range []string{"g1@example.com", "g2@example.com"} {
		entry, err := svc.ResolveEntry(ctx, a, ev.ID, ticket.DoorCheckInTarget{Query: guest(email).GuestEmail})
		if err != nil {
			t.Fatal(err)
		}
		g2Entry = entry.ID
	}
	g3 := guest("g3@example.com")
	if _, err := svc.ResolveEntry(ctx, b, ev.ID, ticket.DoorCheckInTarget{Query: g3.GuestEmail}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CheckIn(ctx, b, g3.ID, talk.ID); err != nil {
		t.Fatal(err)
	}
	// Before 20261128120000: a desk Check-in by B with the entry it wrote
	// (same statement, same created_at), and one the person wrote.
	old, self := guest("g4@example.com"), guest("g5@example.com")
	written := time.Now().Add(-90 * time.Minute)
	date := ticket.IstanbulDate(start)
	for _, row := range []struct {
		tk     ticket.Ticket
		source string
		by     *uuid.UUID
	}{{old, "desk", &bID}, {self, "attendance", nil}} {
		checkIn := uuid.New()
		if _, err := f.pool.Exec(ctx, `INSERT INTO ticket_checkins (id, ticket_id, event_day_id, session_id, created_at) VALUES ($1, $2, $3, $4, $5)`,
			checkIn, row.tk.ID, day.ID, talk.ID, written); err != nil {
			t.Fatal(err)
		}
		if _, err := f.pool.Exec(ctx, `INSERT INTO venue_entries (id, ticket_id, event_id, entry_date, event_day_id, source, check_in_id, by_user_id, created_at)
			VALUES ($1, $2, $3, $4::date, $5, $6, $7, $8, $9)`, uuid.New(), row.tk.ID, ev.ID, date, day.ID, row.source, checkIn, row.by, written); err != nil {
			t.Fatal(err)
		}
	}
	// The leader takes back one of A's entries.
	if err := svc.UndoEntry(ctx, lead, ev.ID, g2Entry); err != nil {
		t.Fatal(err)
	}

	// The day's summary counts the Walk-in's entry under its own source.
	summary, err := svc.EntrySummary(ctx, lead, ev.ID)
	if err != nil || len(summary.Days) != 1 || summary.Days[0].BySource["walk_in"] != 1 || summary.Days[0].Total != 5 {
		t.Fatalf("summary %+v %v", summary, err)
	}

	all, err := svc.DoorStaffStats(ctx, lead, ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	if all.Scope != ticket.DoorStatsAll || all.Others == nil {
		t.Fatalf("scope %q others %v", all.Scope, all.Others)
	}
	rows := map[uuid.UUID]ticket.DoorStaffRow{}
	for _, r := range all.Staff {
		rows[r.UserID] = r
	}
	ra, rb, rl := rows[aID], rows[bID], rows[leadID]
	if ra.Name != "Elif Kaya" || ra.Entries != (ticket.DoorEntryCounts{Desk: 1, WalkIn: 1, Total: 2}) || ra.WalkIns != 1 || ra.Undone != 1 || ra.Undid != 0 {
		t.Fatalf("A %+v", ra)
	}
	if ra.FirstAt == nil || ra.LastAt == nil || len(ra.Hours) == 0 {
		t.Fatalf("A's times %+v", ra)
	}
	if rb.Name != "Mert Demir" || rb.Entries != (ticket.DoorEntryCounts{Desk: 2, Total: 2}) || rb.CheckIns != (ticket.DoorCheckInCounts{Desk: 2, Total: 2}) {
		t.Fatalf("B %+v", rb)
	}
	if rl.Name != "Selin Ak" || rl.Undid != 1 || rl.Entries.Total != 0 {
		t.Fatalf("leader %+v", rl)
	}
	if all.Staff[0].UserID != bID && all.Staff[0].UserID != aID {
		t.Fatalf("order %+v", all.Staff)
	}
	if *all.Others != (ticket.DoorStatsOthers{SelfEntries: 1, SelfCheckIns: 1}) {
		t.Fatalf("others %+v", *all.Others)
	}

	own, err := svc.DoorStaffStats(ctx, a, ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	if own.Scope != ticket.DoorStatsSelf || own.Others != nil || len(own.Staff) != 1 || own.Staff[0].UserID != aID {
		t.Fatalf("own %+v", own)
	}
	if _, err := svc.DoorStaffStats(ctx, otherMember, ev.ID); !errors.Is(err, ticket.ErrForbidden) {
		t.Fatalf("other member: %v", err)
	}
}
