package retention_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/retention"
)

// door_undos_age deletes a door mark taken back three years after it was
// taken back; a younger one stays, and so do the Ticket and its Check-ins.
// The dry run counts what apply then deletes; a second apply finds nothing.
func TestDoorUndosGoThreeYearsAfterTheUndo(t *testing.T) {
	pool := migrated(t)
	ctx := context.Background()
	eventID, ticketID := uuid.New(), uuid.New()
	exec(t, pool, `INSERT INTO events (id, name, location, owner_team) VALUES ($1, 'Hack', 'YTÜ', 'YK')`, eventID)
	exec(t, pool, `INSERT INTO tickets (id, event_id, ticket_type, guest_first_name, guest_last_name, guest_email, guest_phone_number)
		VALUES ($1, $2, 'GUEST', 'Ada', 'Lovelace', 'ada@example.com', '')`, ticketID, eventID)
	newUndo := func(ago time.Duration) uuid.UUID {
		t.Helper()
		id := uuid.New()
		exec(t, pool, `INSERT INTO door_undos (id, kind, ticket_id, event_id, entry_date, source, written_at, undone_at)
			VALUES ($1, 'entry', $2, $3, '2026-12-12', 'scan', $4, $4)`, id, ticketID, eventID, testNow.Add(-ago))
		return id
	}
	old := newUndo(3*365*24*time.Hour + 48*time.Hour)
	young := newUndo(2 * 365 * 24 * time.Hour)

	now := testNow
	s := sweeper(pool, dryRunConfig(), &now, nil)
	only := func(mode retention.Mode) retention.RuleResult {
		t.Helper()
		report, err := s.Run(ctx, retention.RunOptions{Mode: mode, Trigger: retention.TriggerCLI, Rule: "door_undos_age"})
		if err != nil || report.Status != retention.RunOK {
			t.Fatalf("%s: %v %+v", mode, err, report)
		}
		return results(report)["door_undos_age"]
	}
	if r := only(retention.ModeDryRun); r.Matched != 1 || r.Changed != 0 {
		t.Fatalf("dry run %+v", r)
	}
	if r := only(retention.ModeApply); r.Matched != 1 || r.Changed != 1 {
		t.Fatalf("apply %+v", r)
	}
	for id, want := range map[uuid.UUID]int{old: 0, young: 1} {
		if got := scalar[int](t, pool, `SELECT count(*) FROM door_undos WHERE id = $1`, id); got != want {
			t.Errorf("row %s: %d, want %d", id, got, want)
		}
	}
	if got := scalar[int](t, pool, `SELECT count(*) FROM tickets WHERE id = $1`, ticketID); got != 1 {
		t.Fatalf("ticket gone")
	}
	if r := only(retention.ModeApply); r.Matched != 0 || r.Changed != 0 {
		t.Fatalf("second apply %+v", r)
	}
}
