package retention_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/retention"
)

// subject_requests_age deletes a KVKK md. 11 request three years after it
// was answered or rejected, its trail with it (ADR-0062), and keeps younger
// ones and every open one, however old: the dry run counts what apply then
// deletes, and a second apply finds nothing.
func TestSubjectRequestsAgeDeletesRequestsThreeYearsAfterTheAnswer(t *testing.T) {
	pool := migrated(t)
	ctx := context.Background()
	request := func(receivedAgo time.Duration, answeredAgo *time.Duration) uuid.UUID {
		t.Helper()
		id := uuid.New()
		status, answered := "received", any(nil)
		if answeredAgo != nil {
			status, answered = "answered", testNow.Add(-*answeredAgo).Format(time.DateOnly)
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO subject_requests (id, received_on, channel, requester_name, requester_contact, request_types,
				status, answered_on, created_at, updated_at)
			VALUES ($1, $2::date, 'email', 'Ada Lovelace', 'ada@example.test', '{erase}', $3, $4::date, now(), now())`,
			id, testNow.Add(-receivedAgo).Format(time.DateOnly), status, answered); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO subject_request_events (request_id, occurred_at, action, fields, status)
			VALUES ($1, now(), 'create', '{status}', $2)`, id, status); err != nil {
			t.Fatal(err)
		}
		return id
	}
	ago := func(d time.Duration) *time.Duration { return &d }
	old := request(3*365*day+40*day, ago(3*365*day+3*day))
	// A day: due once it lies before the cutoff's day, so the edge is a
	// day and a half back, and not yet overdue.
	oldEdge := request(3*365*day+20*day, ago(3*365*day+day+12*time.Hour))
	young := request(3*365*day, ago(3*365*day-2*day))
	openOld := request(5*365*day, nil)
	recent := request(10*day, ago(day))

	now := testNow
	s := sweeper(pool, dryRunConfig(), &now, nil)
	only := func(mode retention.Mode) retention.RuleResult {
		t.Helper()
		report, err := s.Run(ctx, retention.RunOptions{Mode: mode, Trigger: retention.TriggerCLI, Rule: "subject_requests_age"})
		if err != nil || report.Status != retention.RunOK {
			t.Fatalf("%s: %v %+v", mode, err, report)
		}
		return results(report)["subject_requests_age"]
	}
	if r := only(retention.ModeDryRun); r.Matched != 2 || r.Changed != 0 || r.Overdue != 1 {
		t.Fatalf("dry run %+v", r)
	}
	if n := scalar[int64](t, pool, `SELECT count(*) FROM subject_requests`); n != 5 {
		t.Fatalf("the dry run deleted requests: %d left", n)
	}
	if r := only(retention.ModeApply); r.Matched != 2 || r.Changed != 2 || r.Overdue != 0 {
		t.Fatalf("apply %+v", r)
	}
	for id, kept := range map[uuid.UUID]bool{old: false, oldEdge: false, young: true, openOld: true, recent: true} {
		if got := scalar[bool](t, pool, `SELECT EXISTS (SELECT 1 FROM subject_requests WHERE id = $1)`, id); got != kept {
			t.Errorf("request %s kept %v, want %v", id, got, kept)
		}
		if got := scalar[bool](t, pool, `SELECT EXISTS (SELECT 1 FROM subject_request_events WHERE request_id = $1)`, id); got != kept {
			t.Errorf("trail of %s kept %v, want %v", id, got, kept)
		}
	}
	if r := only(retention.ModeApply); r.Matched != 0 || r.Changed != 0 {
		t.Fatalf("second apply %+v", r)
	}
}
