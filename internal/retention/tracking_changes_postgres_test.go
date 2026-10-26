package retention_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/retention"
)

// tracking_changes_age deletes the tracking preference's audit rows three
// years old and leaves younger rows and the preference itself: the dry run
// counts what apply then deletes, and a second apply finds nothing.
func TestTrackingChangesAgeDeletesRowsThreeYearsOld(t *testing.T) {
	pool := migrated(t)
	ctx := context.Background()
	person := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO communication_preferences (user_id, channel, scope, group_id, news_kind, enabled, source, updated_at)
		VALUES ($1, 'email', 'tracking', NULL, NULL, false, 'account_center', $2)`, person, testNow.Add(-4*365*day)); err != nil {
		t.Fatal(err)
	}
	change := func(age time.Duration, enabled bool) int64 {
		t.Helper()
		var id int64
		if err := pool.QueryRow(ctx, `
			INSERT INTO communication_tracking_changes (user_id, enabled, previous, source, changed_at)
			VALUES ($1, $2, NULL, 'account_center', $3) RETURNING id`, person, enabled, testNow.Add(-age)).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	old := change(3*365*day+2*day, false)
	oldOn := change(3*365*day+time.Hour, true)
	young := change(3*365*day-day, false)
	recent := change(time.Hour, true)

	now := testNow
	s := sweeper(pool, dryRunConfig(), &now, nil)
	only := func(mode retention.Mode) retention.RuleResult {
		t.Helper()
		report, err := s.Run(ctx, retention.RunOptions{Mode: mode, Trigger: retention.TriggerCLI, Rule: "tracking_changes_age"})
		if err != nil || report.Status != retention.RunOK {
			t.Fatalf("%s: %v %+v", mode, err, report)
		}
		return results(report)["tracking_changes_age"]
	}
	if r := only(retention.ModeDryRun); r.Matched != 2 || r.Changed != 0 || r.Overdue != 1 {
		t.Fatalf("dry run %+v", r)
	}
	if n := scalar[int64](t, pool, `SELECT count(*) FROM communication_tracking_changes`); n != 4 {
		t.Fatalf("the dry run deleted rows: %d left", n)
	}
	if r := only(retention.ModeApply); r.Matched != 2 || r.Changed != 2 || r.Overdue != 0 {
		t.Fatalf("apply %+v", r)
	}
	for id, kept := range map[int64]bool{old: false, oldOn: false, young: true, recent: true} {
		if got := scalar[bool](t, pool, `SELECT EXISTS (SELECT 1 FROM communication_tracking_changes WHERE id = $1)`, id); got != kept {
			t.Errorf("row %d kept %v, want %v", id, got, kept)
		}
	}
	if got := scalar[bool](t, pool, `SELECT enabled FROM communication_preferences WHERE user_id = $1 AND scope = 'tracking'`, person); got {
		t.Fatal("the objection itself changed")
	}
	if r := only(retention.ModeApply); r.Matched != 0 || r.Changed != 0 {
		t.Fatalf("second apply %+v", r)
	}
}
