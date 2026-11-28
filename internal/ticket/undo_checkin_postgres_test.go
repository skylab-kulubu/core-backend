package ticket_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
)

// On the database an undo deletes the one check-in row: the door activity
// of the Session (DoorSessionActivity) drops by one, the Ticket reads no
// check-in there, the other Session's check-in stays, a second undo finds
// nothing, and the unique (ticket, session) pair takes a new check-in.
func TestUndoCheckInDeletesTheRowOnPostgres(t *testing.T) {
	f := startFormResponsePostgres(t)
	ctx := context.Background()
	store := ticket.NewPostgresStore(f.pool)
	svc := f.service(store)
	ev := seedEvent(t, f.events, "WEBLAB")
	day := seedDay(t, f.events, ev.ID, "Day 1")
	sess := seedSession(t, f.events, day.ID, "Opening")
	other := seedSession(t, f.events, day.ID, "Closing")
	tk, err := store.Create(ctx, ticket.Ticket{EventID: ev.ID, TicketType: ticket.Guest, GuestFirstName: "Ada", GuestLastName: "Lovelace", GuestEmail: "ada@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []uuid.UUID{sess.ID, other.ID} {
		if _, err := svc.CheckIn(ctx, weblabLead, tk.ID, id); err != nil {
			t.Fatal(err)
		}
	}
	before, err := svc.DoorActivity(ctx, weblabLead, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if before.Total != 1 {
		t.Fatalf("before %+v", before)
	}

	if err := svc.UndoCheckIn(ctx, weblabLead, tk.ID, sess.ID); err != nil {
		t.Fatal(err)
	}
	after, err := svc.DoorActivity(ctx, weblabLead, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Total != 0 || len(after.Items) != 0 {
		t.Fatalf("after %+v", after)
	}
	got, err := store.Get(ctx, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.CheckIns) != 1 || got.CheckIns[0].SessionID != other.ID {
		t.Fatalf("check-ins %+v", got.CheckIns)
	}
	if removed, err := store.RemoveCheckIn(ctx, tk.ID, sess.ID, nil); err != nil || removed {
		t.Fatalf("second remove %v %v", removed, err)
	}
	if err := svc.UndoCheckIn(ctx, weblabLead, tk.ID, sess.ID); !errors.Is(err, ticket.ErrNotFound) {
		t.Fatalf("second undo %v", err)
	}
	again, err := svc.CheckIn(ctx, weblabLead, tk.ID, sess.ID)
	if err != nil {
		t.Fatalf("check in again: %v", err)
	}

	// By the check-in's own id: read, refused for another Session, deleted.
	if got, err := store.CheckInByID(ctx, again.ID); err != nil || got.TicketID != tk.ID || got.SessionID != sess.ID {
		t.Fatalf("by id %+v %v", got, err)
	}
	if err := svc.UndoCheckInByID(ctx, weblabLead, other.ID, again.ID); !errors.Is(err, ticket.ErrNotFound) {
		t.Fatalf("another session's: %v", err)
	}
	if err := svc.UndoCheckInByID(ctx, weblabLead, sess.ID, again.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CheckInByID(ctx, again.ID); !errors.Is(err, ticket.ErrNotFound) {
		t.Fatalf("after undo by id: %v", err)
	}
	if removed, err := store.RemoveCheckInByID(ctx, again.ID, nil); err != nil || removed {
		t.Fatalf("second remove by id %v %v", removed, err)
	}
	if after, err := svc.DoorActivity(ctx, weblabLead, sess.ID); err != nil || after.Total != 0 {
		t.Fatalf("after by id %+v %v", after, err)
	}
}
