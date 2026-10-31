package retention_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/retention"
)

// management_terms_age deletes a management membership term three years
// after its membership ended (ended_at), and keeps younger ended terms and
// every open one, however old, expired or not: the dry run counts what
// apply then deletes, and a second apply finds nothing.
func TestManagementTermsAgeDeletesTermsThreeYearsAfterTheyEnded(t *testing.T) {
	pool := migrated(t)
	ctx := context.Background()
	term := func(startedAgo time.Duration, expiredAgo, endedAgo *time.Duration) uuid.UUID {
		t.Helper()
		id := uuid.New()
		ago := func(d *time.Duration) any {
			if d == nil {
				return nil
			}
			return testNow.Add(-*d)
		}
		started := testNow.Add(-startedAgo)
		if _, err := pool.Exec(ctx, `
			INSERT INTO management_membership_terms (id, user_id, group_id, group_path, scope, ends_at, source,
				started_at, expired_at, ended_at, updated_at)
			VALUES ($1, $2, $3, '/UYELER/YK', 'privileged', $4, 'route', $4, $5, $6, now())`,
			id, uuid.New(), uuid.New(), started, ago(expiredAgo), ago(endedAgo)); err != nil {
			t.Fatal(err)
		}
		return id
	}
	ago := func(d time.Duration) *time.Duration { return &d }
	old := term(4*365*day, ago(3*365*day+20*day), ago(3*365*day+2*day))
	// Ended just past three years: due, not yet overdue (a day's grace).
	oldEdge := term(4*365*day, nil, ago(3*365*day+12*time.Hour))
	young := term(4*365*day, ago(3*365*day+20*day), ago(3*365*day-day))
	// Expired five years ago and still in the Group: open, never selected.
	openExpired := term(6*365*day, ago(5*365*day), nil)
	openOld := term(6*365*day, nil, nil)
	recent := term(30*day, nil, ago(day))

	now := testNow
	s := sweeper(pool, dryRunConfig(), &now, nil)
	only := func(mode retention.Mode) retention.RuleResult {
		t.Helper()
		report, err := s.Run(ctx, retention.RunOptions{Mode: mode, Trigger: retention.TriggerCLI, Rule: "management_terms_age"})
		if err != nil || report.Status != retention.RunOK {
			t.Fatalf("%s: %v %+v", mode, err, report)
		}
		return results(report)["management_terms_age"]
	}
	if r := only(retention.ModeDryRun); r.Matched != 2 || r.Changed != 0 || r.Overdue != 1 {
		t.Fatalf("dry run %+v", r)
	}
	if n := scalar[int64](t, pool, `SELECT count(*) FROM management_membership_terms`); n != 6 {
		t.Fatalf("the dry run deleted terms: %d left", n)
	}
	if r := only(retention.ModeApply); r.Matched != 2 || r.Changed != 2 || r.Overdue != 0 {
		t.Fatalf("apply %+v", r)
	}
	for id, kept := range map[uuid.UUID]bool{old: false, oldEdge: false, young: true, openExpired: true, openOld: true, recent: true} {
		if got := scalar[bool](t, pool, `SELECT EXISTS (SELECT 1 FROM management_membership_terms WHERE id = $1)`, id); got != kept {
			t.Errorf("term %s kept %v, want %v", id, got, kept)
		}
	}
	if r := only(retention.ModeApply); r.Matched != 0 || r.Changed != 0 {
		t.Fatalf("second apply %+v", r)
	}
}
