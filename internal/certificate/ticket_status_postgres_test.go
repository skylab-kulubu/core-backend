package certificate_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/certificate"
	"github.com/skylab-kulubu/core-backend/internal/event"
)

// A cancelled Ticket (docs/ticket-cancellation.md) is out of the
// certificate's attendance: not on the list, not in the automatic
// finalization, not in the rescan, and not given one by hand. The Ticket is
// cancelled in the database directly, check-ins and all, which the
// cancellation itself refuses: the filter must hold anyway.
func TestPostgresCertificatesSkipCancelledTickets(t *testing.T) {
	ctx := context.Background()
	f := newPGAuto(t)
	five := time.Date(2031, 3, 14, 17, 0, 0, 0, time.UTC)
	eventID := f.create(t, autoEvent{name: "İptalli", rule: certificate.RuleOnce, status: event.StatusPublished, auto: true, ends: []time.Time{five}})
	var dayID, sessionID uuid.UUID
	if err := f.pool.QueryRow(ctx, `SELECT d.id, s.id FROM event_days d JOIN sessions s ON s.event_day_id = d.id WHERE d.event_id = $1`, eventID).Scan(&dayID, &sessionID); err != nil {
		t.Fatal(err)
	}
	// guest writes a Guest checked in on the Session, cancelled or not.
	guest := func(email string, cancelled bool) uuid.UUID {
		t.Helper()
		id := uuid.New()
		if _, err := f.pool.Exec(ctx, `INSERT INTO tickets (id, event_id, ticket_type, guest_first_name, guest_last_name, guest_email) VALUES ($1, $2, 'GUEST', 'Can', 'Er', $3)`, id, eventID, email); err != nil {
			t.Fatal(err)
		}
		if _, err := f.pool.Exec(ctx, `INSERT INTO ticket_checkins (id, ticket_id, event_day_id, session_id) VALUES ($1, $2, $3, $4)`, uuid.New(), id, dayID, sessionID); err != nil {
			t.Fatal(err)
		}
		if cancelled {
			if _, err := f.pool.Exec(ctx, `UPDATE tickets SET status = 'cancelled', cancelled_at = now(), cancelled_by = 'operator' WHERE id = $1`, id); err != nil {
				t.Fatal(err)
			}
		}
		return id
	}
	cancelled := guest("can@example.com", true)

	list, err := f.svc.Attendance(ctx, f.leader, eventID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Rows) != 1 || list.Rows[0].TicketID == cancelled {
		t.Fatalf("attendance rows %+v", list.Rows)
	}
	result, err := f.svc.AutoFinalize(ctx, five.Add(time.Hour))
	if err != nil || result.Finalized != 1 {
		t.Fatalf("finalize %v %+v", err, result)
	}
	if _, _, batches := f.closed(t, eventID); strings.Join(batches, ",") != "finalization/system/1" {
		t.Fatalf("batches %v", batches)
	}

	late := guest("geç@example.com", true)
	rescan, err := f.svc.Rescan(ctx, f.leader, eventID)
	if err != nil || rescan.Queued != 0 {
		t.Fatalf("rescan with a cancelled Ticket: %v %+v", err, rescan)
	}
	if _, err := f.svc.QueueManual(ctx, f.leader, eventID, late, "Gerekçeli"); !errors.Is(err, certificate.ErrNotFound) {
		t.Fatalf("by hand: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE tickets SET status = 'active', cancelled_at = NULL, cancelled_by = NULL WHERE id = $1`, late); err != nil {
		t.Fatal(err)
	}
	if rescan, err := f.svc.Rescan(ctx, f.leader, eventID); err != nil || rescan.Queued != 1 {
		t.Fatalf("rescan once restored: %v %+v", err, rescan)
	}
}
