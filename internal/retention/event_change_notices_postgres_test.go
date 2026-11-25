package retention_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/retention"
)

// event_change_notices_age deletes a record of an editor's answer three
// years after it was given (decided_at) and keeps younger ones: the dry run
// counts what apply then deletes, and a second apply finds nothing.
func TestEventChangeNoticesAgeDeletesAnswersThreeYearsOld(t *testing.T) {
	pool := migrated(t)
	ctx := context.Background()
	eventID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO events (id, name, location, owner_team) VALUES ($1, 'Gece Kodu', 'D-Blok', 'WEBLAB')`, eventID); err != nil {
		t.Fatal(err)
	}
	answer := func(ago time.Duration) uuid.UUID {
		t.Helper()
		id := uuid.New()
		if _, err := pool.Exec(ctx, `
			INSERT INTO event_change_notice_choices (id, event_id, decided_at, fields, notify, queued)
			VALUES ($1, $2, $3, '{location}', false, false)`, id, eventID, testNow.Add(-ago)); err != nil {
			t.Fatal(err)
		}
		return id
	}
	old := answer(3*365*day + 2*day)
	// Given just past three years: due, not yet overdue (a day's grace).
	oldEdge := answer(3*365*day + 12*time.Hour)
	young := answer(3*365*day - day)
	recent := answer(day)

	now := testNow
	s := sweeper(pool, dryRunConfig(), &now, nil)
	only := func(mode retention.Mode) retention.RuleResult {
		t.Helper()
		report, err := s.Run(ctx, retention.RunOptions{Mode: mode, Trigger: retention.TriggerCLI, Rule: "event_change_notices_age"})
		if err != nil || report.Status != retention.RunOK {
			t.Fatalf("%s: %v %+v", mode, err, report)
		}
		return results(report)["event_change_notices_age"]
	}
	if r := only(retention.ModeDryRun); r.Matched != 2 || r.Changed != 0 || r.Overdue != 1 {
		t.Fatalf("dry run %+v", r)
	}
	if r := only(retention.ModeApply); r.Matched != 2 || r.Changed != 2 || r.Overdue != 0 {
		t.Fatalf("apply %+v", r)
	}
	for id, kept := range map[uuid.UUID]bool{old: false, oldEdge: false, young: true, recent: true} {
		if got := scalar[bool](t, pool, `SELECT EXISTS (SELECT 1 FROM event_change_notice_choices WHERE id = $1)`, id); got != kept {
			t.Errorf("answer %s kept %v, want %v", id, got, kept)
		}
	}
	if r := only(retention.ModeApply); r.Matched != 0 || r.Changed != 0 {
		t.Fatalf("second apply %+v", r)
	}
}
