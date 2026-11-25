package ticket_test

import (
	"context"
	"errors"
	"strings"
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
	// The same scan sent again: a duplicate naming the record there, the
	// scan resent.
	_, err = d.svc.CheckInUser(ctx, d.staff, d.open.ID, d.memberID)
	var dup *ticket.DuplicateScanError
	if !errors.As(err, &dup) || !errors.Is(err, ticket.ErrAlreadyCheckedIn) || !errors.Is(err, ticket.ErrConflict) {
		t.Fatalf("again: %v", err)
	}
	if dup.Kind != ticket.KindCheckIn || dup.ID != ci.ID || dup.ScannedAt == nil || !dup.ScannedAt.Equal(at) || !dup.Resent {
		t.Fatalf("resent %+v", dup)
	}
	// Another door's scan of the same Ticket, taken a minute later: the
	// first record stands, and it is not a resend.
	_, err = d.svc.CheckInUser(ticket.WithScannedAt(context.Background(), at.Add(time.Minute)), d.staff, d.open.ID, d.memberID)
	if !errors.As(err, &dup) || dup.Resent || !dup.ScannedAt.Equal(at) {
		t.Fatalf("another door %+v %v", dup, err)
	}
	// The Event's door: today's entry, then another door's scan of it.
	in, err := d.svc.EnterUser(ticket.WithScannedAt(context.Background(), at.Add(2*time.Minute)), d.staff, d.ev.ID, d.memberID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = d.svc.EnterUser(ticket.WithScannedAt(context.Background(), at.Add(3*time.Minute)), d.staff, d.ev.ID, d.memberID)
	if !errors.As(err, &dup) || dup.Kind != ticket.KindEntry || dup.Name != "Grace Hopper" || dup.ID != in.ID || dup.Resent ||
		!dup.ScannedAt.Equal(at.Add(2*time.Minute)) {
		t.Fatalf("entry %+v %v", dup, err)
	}
	metrics := d.scans.Prometheus()
	for _, want := range []string{
		`skylab_door_offline_scans_total{outcome="written"} 2`,
		`skylab_door_offline_scans_total{outcome="resent"} 1`,
		`skylab_door_offline_scans_total{outcome="other_door"} 2`,
	} {
		if !strings.Contains(metrics, want) {
			t.Fatalf("metrics lack %s:\n%s", want, metrics)
		}
	}
	// Online, as it came: no moment kept.
	online, err := d.svc.CheckInUser(context.Background(), d.staff, d.talk.ID, d.memberID)
	if err != nil || online.ScannedAt != nil {
		t.Fatalf("online %+v %v", online, err)
	}
}
