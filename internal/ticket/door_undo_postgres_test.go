package ticket_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
)

// Who wrote a door mark and what becomes of one taken back
// (docs/venue-entry.md): a Check-in keeps its source and its door staff;
// undoing it moves it, and the entry it wrote, to door_undos with who took
// them back, out of every live read; the Ticket can be checked in again
// (a new row). An entry taken back at the Event's desk goes the same way.
func TestDoorUndosKeepWhatTheDoorTookBackOnPostgres(t *testing.T) {
	f := startFormResponsePostgres(t)
	ctx := context.Background()
	store := ticket.NewPostgresStore(f.pool)
	svc := f.service(store)
	staffID := f.person(t, "kapi@example.com")
	staff := authz.Principal{ID: staffID.String()}
	undoerID := f.person(t, "lider@example.com")
	undoer := authz.Principal{ID: undoerID.String(), Groups: weblabLead.Groups}
	start := time.Now().Add(-time.Hour)
	end := time.Now().Add(72 * time.Hour)
	ev, err := f.events.Create(ctx, event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB", StartDate: &start, EndDate: &end, DoorStaffIDs: []uuid.UUID{staffID}})
	if err != nil {
		t.Fatal(err)
	}
	day, err := f.events.CreateDay(ctx, event.Day{EventID: ev.ID, Name: "1. gün", StartDate: &start})
	if err != nil {
		t.Fatal(err)
	}
	open := seedSession(t, f.events, day.ID, "Açılış")
	guest, err := store.Create(ctx, ticket.Ticket{EventID: ev.ID, TicketType: ticket.Guest, GuestFirstName: "Ada", GuestLastName: "Lovelace", GuestEmail: "ada@example.com"})
	if err != nil {
		t.Fatal(err)
	}

	desk, err := svc.CheckIn(ctx, staff, guest.ID, open.ID)
	if err != nil {
		t.Fatal(err)
	}
	var source string
	var by *uuid.UUID
	if err := f.pool.QueryRow(ctx, `SELECT source, by_user_id FROM ticket_checkins WHERE id = $1`, desk.ID).Scan(&source, &by); err != nil {
		t.Fatal(err)
	}
	if source != "desk" || by == nil || *by != staffID {
		t.Fatalf("check-in written %q by %v", source, by)
	}

	if err := svc.UndoCheckInByID(ctx, undoer, open.ID, desk.ID); err != nil {
		t.Fatal(err)
	}
	type undo struct {
		kind, source string
		by, undoneBy *uuid.UUID
	}
	undos := func() []undo {
		t.Helper()
		rows, err := f.pool.Query(ctx, `SELECT kind, COALESCE(source, ''), by_user_id, undone_by FROM door_undos WHERE ticket_id = $1 ORDER BY undone_at, kind`, guest.ID)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []undo
		for rows.Next() {
			var u undo
			if err := rows.Scan(&u.kind, &u.source, &u.by, &u.undoneBy); err != nil {
				t.Fatal(err)
			}
			out = append(out, u)
		}
		return out
	}
	got := undos()
	if len(got) != 2 || got[0].kind != "check_in" || got[1].kind != "entry" {
		t.Fatalf("undos %+v", got)
	}
	for _, u := range got {
		if u.by == nil || *u.by != staffID || u.undoneBy == nil || *u.undoneBy != undoerID || u.source != "desk" {
			t.Fatalf("%+v", u)
		}
	}
	live := func(table string) int {
		t.Helper()
		var n int
		if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM `+table+` WHERE ticket_id = $1`, guest.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if live("ticket_checkins") != 0 || live("venue_entries") != 0 {
		t.Fatalf("live rows after undo: %d check-ins, %d entries", live("ticket_checkins"), live("venue_entries"))
	}

	// Checked in again: a new row, the undone one kept.
	again, err := svc.CheckIn(ctx, staff, guest.ID, open.ID)
	if err != nil {
		t.Fatalf("check in again: %v", err)
	}
	if again.ID == desk.ID || live("ticket_checkins") != 1 || live("venue_entries") != 1 {
		t.Fatalf("again %+v", again)
	}

	// The Event's desk takes the entry back; the day's Check-in stays.
	entries, err := svc.ListEntries(ctx, undoer, ev.ID, "")
	if err != nil || len(entries) != 1 {
		t.Fatalf("entries %+v %v", entries, err)
	}
	if err := svc.UndoEntry(ctx, undoer, ev.ID, entries[0].ID); err != nil {
		t.Fatal(err)
	}
	if live("ticket_checkins") != 1 || live("venue_entries") != 0 || len(undos()) != 3 {
		t.Fatalf("after the desk's undo: %d check-ins, %d entries, %d undos", live("ticket_checkins"), live("venue_entries"), len(undos()))
	}

	// A writer whose account is being erased is not named in the undo,
	// and the undo still goes through.
	if _, err := f.users.RequestDeletion(ctx, staffID, nil); err != nil {
		t.Fatal(err)
	}
	if err := svc.UndoCheckInByID(ctx, undoer, open.ID, again.ID); err != nil {
		t.Fatal(err)
	}
	var named int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM door_undos WHERE id = $1 AND by_user_id IS NULL AND undone_by = $2`, again.ID, undoerID).Scan(&named); err != nil || named != 1 {
		t.Fatalf("undo of a writer being erased: %d %v", named, err)
	}
}
