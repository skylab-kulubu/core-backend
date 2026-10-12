package retention_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/retention"
	"github.com/skylab-kulubu/core-backend/internal/teamaudit"
)

// team_audit_age deletes the Team audit record's rows three years old,
// pending ones too, and leaves younger rows: the dry run counts what apply
// then deletes, and a second apply finds nothing.
func TestTeamAuditAgeDeletesRowsThreeYearsOld(t *testing.T) {
	pool := migrated(t)
	ctx := context.Background()
	writer := teamaudit.NewPostgres(pool)
	at := func(age time.Duration, finish bool) uuid.UUID {
		t.Helper()
		writer.SetClock(func() time.Time { return testNow.Add(-age) })
		id, err := writer.Begin(ctx, teamaudit.Record{
			Actor:  teamaudit.Actor{ID: uuid.New(), Basis: teamaudit.BasisPrivileged},
			Action: teamaudit.ActionMemberAdd, GroupID: uuid.NewString(), GroupPath: "/UYELER/ARGE/WEBLAB", TargetUserID: uuid.New(),
		})
		if err != nil {
			t.Fatal(err)
		}
		if finish {
			if err := writer.Finish(ctx, id, teamaudit.OutcomeOK, "", nil); err != nil {
				t.Fatal(err)
			}
		}
		return id
	}
	old := at(3*365*day+2*day, true)
	oldPending := at(3*365*day+2*day, false)
	young := at(3*365*day-day, true)
	recent := at(time.Hour, false)

	now := testNow
	s := sweeper(pool, dryRunConfig(), &now, nil)
	only := func(mode retention.Mode) retention.RuleResult {
		t.Helper()
		report, err := s.Run(ctx, retention.RunOptions{Mode: mode, Trigger: retention.TriggerCLI, Rule: "team_audit_age"})
		if err != nil || report.Status != retention.RunOK {
			t.Fatalf("%s: %v %+v", mode, err, report)
		}
		return results(report)["team_audit_age"]
	}
	if r := only(retention.ModeDryRun); r.Matched != 2 || r.Changed != 0 || r.Overdue != 2 {
		t.Fatalf("dry run %+v", r)
	}
	if n := scalar[int64](t, pool, `SELECT count(*) FROM team_audit_records`); n != 4 {
		t.Fatalf("the dry run deleted rows: %d left", n)
	}
	if r := only(retention.ModeApply); r.Matched != 2 || r.Changed != 2 || r.Overdue != 0 {
		t.Fatalf("apply %+v", r)
	}
	for id, kept := range map[uuid.UUID]bool{old: false, oldPending: false, young: true, recent: true} {
		if got := scalar[bool](t, pool, `SELECT EXISTS (SELECT 1 FROM team_audit_records WHERE id = $1)`, id); got != kept {
			t.Errorf("row %s kept %v, want %v", id, got, kept)
		}
	}
	if r := only(retention.ModeApply); r.Matched != 0 || r.Changed != 0 {
		t.Fatalf("second apply %+v", r)
	}
}
