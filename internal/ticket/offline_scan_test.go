package ticket_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/skylab-kulubu/core-backend/internal/ticket"
)

// The bounds of a scannedAt: five minutes ahead of core's clock, 72 hours
// behind.
func TestCheckScannedAt(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 12, 12, 12, 0, 0, 0, time.UTC)
	for at, want := range map[time.Time]error{
		now:                                nil,
		now.Add(ticket.ScanFutureSkew):     nil,
		now.Add(ticket.ScanFutureSkew + 1): ticket.ErrScannedAtFuture,
		now.Add(-ticket.ScanMaxAge):        nil,
		now.Add(-ticket.ScanMaxAge - 1):    ticket.ErrScannedAtTooOld,
		now.Add(-3 * time.Hour):            nil,
	} {
		if err := ticket.CheckScannedAt(at, now); !errors.Is(err, want) && !(want == nil && err == nil) {
			t.Errorf("%s: %v, want %v", at, err, want)
		}
	}
}

// A Session scan sent later keeps the moment the device took it, on the
// Check-in and on the day's entry it writes; a second Check-in of the
// Ticket at that Session is ErrAlreadyCheckedIn (ErrConflict, as before).
func TestOfflineSessionScanKeepsItsMoment(t *testing.T) {
	t.Parallel()
	d := newVenueDoor(t, nil)
	at := time.Now().Add(-20 * time.Minute).UTC().Truncate(time.Second)
	ctx := ticket.WithScannedAt(context.Background(), at)
	ci, err := d.svc.CheckInUser(ctx, d.staff, d.open.ID, d.memberID)
	if err != nil {
		t.Fatal(err)
	}
	if ci.Kind != ticket.KindCheckIn || ci.ScannedAt == nil || !ci.ScannedAt.Equal(at) {
		t.Fatalf("check-in %+v", ci)
	}
	entries := d.entries(t, firstDay)
	if len(entries) != 1 {
		t.Fatalf("entries %+v", entries)
	}
	stored, err := d.tickets.VenueEntryByID(context.Background(), entries[0].ID)
	if err != nil || stored.ScannedAt == nil || !stored.ScannedAt.Equal(at) {
		t.Fatalf("entry %+v %v", stored, err)
	}
	_, err = d.svc.CheckInUser(ctx, d.staff, d.open.ID, d.memberID)
	if !errors.Is(err, ticket.ErrAlreadyCheckedIn) || !errors.Is(err, ticket.ErrConflict) {
		t.Fatalf("again: %v", err)
	}
	// Online, as it came: no moment kept.
	online, err := d.svc.CheckInUser(context.Background(), d.staff, d.talk.ID, d.memberID)
	if err != nil || online.ScannedAt != nil {
		t.Fatalf("online %+v %v", online, err)
	}
}
