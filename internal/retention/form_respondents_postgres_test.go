package retention_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/retention"
)

// form_respondents deletes an Unticketed respondent two years after its
// Event's end; a younger Event's stay, and an Event with no date counts
// from the row's created_at. The dry run counts what apply then deletes; a
// second apply finds nothing.
func TestFormRespondentsGoTwoYearsAfterTheEvent(t *testing.T) {
	pool := migrated(t)
	ctx := context.Background()
	at := func(ago time.Duration) *time.Time { v := testNow.Add(-ago); return &v }
	newRow := func(end *time.Time, created time.Time) uuid.UUID {
		t.Helper()
		eventID, id := uuid.New(), uuid.New()
		exec(t, pool, `INSERT INTO events (id, name, location, owner_team, end_date) VALUES ($1, 'Hack', 'YTÜ', 'YK', $2)`, eventID, end)
		exec(t, pool, `INSERT INTO event_form_respondents (id, event_id, form_id, form_response_id, first_name, last_name, email, forms_status, source, created_at)
			VALUES ($1, $2, $3, $4, 'Ada', 'Lovelace', 'ada@example.com', 'accepted', 'collect', $5)`, id, eventID, uuid.New(), uuid.New(), created)
		return id
	}
	old := newRow(at(2*365*24*time.Hour+48*time.Hour), testNow.Add(-3*365*24*time.Hour))
	young := newRow(at(300*24*time.Hour), testNow.Add(-3*365*24*time.Hour))
	undatedOld := newRow(nil, testNow.Add(-2*365*24*time.Hour-48*time.Hour))
	undatedYoung := newRow(nil, testNow.Add(-24*time.Hour))

	now := testNow
	s := sweeper(pool, dryRunConfig(), &now, nil)
	only := func(mode retention.Mode) retention.RuleResult {
		t.Helper()
		report, err := s.Run(ctx, retention.RunOptions{Mode: mode, Trigger: retention.TriggerCLI, Rule: "form_respondents"})
		if err != nil || report.Status != retention.RunOK {
			t.Fatalf("%s: %v %+v", mode, err, report)
		}
		return results(report)["form_respondents"]
	}
	if r := only(retention.ModeDryRun); r.Matched != 2 || r.Changed != 0 {
		t.Fatalf("dry run %+v", r)
	}
	if r := only(retention.ModeApply); r.Matched != 2 || r.Changed != 2 {
		t.Fatalf("apply %+v", r)
	}
	for id, want := range map[uuid.UUID]int{old: 0, young: 1, undatedOld: 0, undatedYoung: 1} {
		if got := scalar[int](t, pool, `SELECT count(*) FROM event_form_respondents WHERE id = $1`, id); got != want {
			t.Errorf("row %s: %d, want %d", id, got, want)
		}
	}
	if r := only(retention.ModeApply); r.Matched != 0 || r.Changed != 0 {
		t.Fatalf("second apply %+v", r)
	}
}
