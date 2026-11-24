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

// On the database: a scan sent later keeps scanned_at on the Check-in and
// the entry it wrote; created_at stays core's. A scan an hour or more old
// queues none of the door's mails; a recent one queues them as before.
func TestOfflineScanOnPostgres(t *testing.T) {
	f := startFormResponsePostgres(t)
	ctx := context.Background()
	store := ticket.NewPostgresStore(f.pool)
	store.NotifyAttendance(true, true)
	svc := f.service(store)
	staffID := f.person(t, "kapi@example.com")
	staff := authz.Principal{ID: staffID.String()}
	start := time.Now().Add(-6 * time.Hour)
	end := time.Now().Add(6 * time.Hour)
	ev, err := f.events.Create(ctx, event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB", StartDate: &start, EndDate: &end, DoorStaffIDs: []uuid.UUID{staffID}})
	if err != nil {
		t.Fatal(err)
	}
	day, err := f.events.CreateDay(ctx, event.Day{EventID: ev.ID, Name: "1. gün", StartDate: &start})
	if err != nil {
		t.Fatal(err)
	}
	open := seedSession(t, f.events, day.ID, "Açılış")
	late, err := store.Create(ctx, ticket.Ticket{EventID: ev.ID, TicketType: ticket.Guest, GuestFirstName: "Ada", GuestLastName: "Lovelace", GuestEmail: "ada@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	recent, err := store.Create(ctx, ticket.Ticket{EventID: ev.ID, TicketType: ticket.Guest, GuestFirstName: "Grace", GuestLastName: "Hopper", GuestEmail: "grace@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	mails := func(id uuid.UUID) int {
		t.Helper()
		var n int
		if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM attendance_mails WHERE ticket_id = $1`, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	lateAt := time.Now().Add(-2 * time.Hour).UTC().Truncate(time.Microsecond)
	ci, err := svc.CheckIn(ticket.WithScannedAt(ctx, lateAt), staff, late.ID, open.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ci.ScannedAt == nil || !ci.ScannedAt.Equal(lateAt) || !ci.CreatedAt.After(lateAt.Add(time.Hour)) {
		t.Fatalf("check-in %+v", ci)
	}
	var entryAt *time.Time
	if err := f.pool.QueryRow(ctx, `SELECT scanned_at FROM venue_entries WHERE ticket_id = $1`, late.ID).Scan(&entryAt); err != nil {
		t.Fatal(err)
	}
	if entryAt == nil || !entryAt.Equal(lateAt) {
		t.Fatalf("entry's scanned_at %v", entryAt)
	}
	if n := mails(late.ID); n != 0 {
		t.Fatalf("a late scan queued %d mails", n)
	}

	recentAt := time.Now().Add(-10 * time.Minute).UTC()
	if _, err := svc.CheckIn(ticket.WithScannedAt(ctx, recentAt), staff, recent.ID, open.ID); err != nil {
		t.Fatal(err)
	}
	if n := mails(recent.ID); n != 2 {
		t.Fatalf("a recent scan queued %d mails, want the welcome and the receipt", n)
	}
}
