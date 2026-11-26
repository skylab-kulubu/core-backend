package retention_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/retention"
)

// co_organizer_people deletes an Event's Co-organizer person rows 90 days
// after the Event, as door_staff does: a younger Event's stay, an Event with
// no date is counted (anchorless) and left, and a team row is never
// touched. The dry run counts what apply then deletes; a second apply finds
// nothing.
func TestCoOrganizerPeopleGoNinetyDaysAfterTheEvent(t *testing.T) {
	pool := migrated(t)
	ctx := context.Background()
	person := uuid.New()
	exec(t, pool, `INSERT INTO users (id, email) VALUES ($1, 'co@example.com')`, person)
	newEvent := func(end *time.Time) uuid.UUID {
		t.Helper()
		id := uuid.New()
		exec(t, pool, `INSERT INTO events (id, name, location, owner_team, end_date) VALUES ($1, 'Hack', 'YTÜ', 'YK', $2)`, id, end)
		exec(t, pool, `INSERT INTO event_co_organizers (event_id, user_id) VALUES ($1, $2)`, id, person)
		exec(t, pool, `INSERT INTO event_co_organizers (event_id, team_code, management, visibility) VALUES ($1, 'WEBLAB', 'leaders', 'co_organizer')`, id)
		return id
	}
	at := func(ago time.Duration) *time.Time { v := testNow.Add(-ago); return &v }
	old := newEvent(at(100 * day))
	young := newEvent(at(80 * day))
	undated := newEvent(nil)

	now := testNow
	s := sweeper(pool, dryRunConfig(), &now, nil)
	only := func(mode retention.Mode) retention.RuleResult {
		t.Helper()
		report, err := s.Run(ctx, retention.RunOptions{Mode: mode, Trigger: retention.TriggerCLI, Rule: "co_organizer_people"})
		if err != nil || report.Status != retention.RunOK {
			t.Fatalf("%s: %v %+v", mode, err, report)
		}
		return results(report)["co_organizer_people"]
	}
	if r := only(retention.ModeDryRun); r.Matched != 1 || r.Changed != 0 || r.Anchorless != 1 {
		t.Fatalf("dry run %+v", r)
	}
	if r := only(retention.ModeApply); r.Matched != 1 || r.Changed != 1 || r.Anchorless != 1 {
		t.Fatalf("apply %+v", r)
	}
	for id, want := range map[uuid.UUID]string{old: "0|1", young: "1|1", undated: "1|1"} {
		got := scalar[string](t, pool, `SELECT count(*) FILTER (WHERE user_id IS NOT NULL) || '|' || count(*) FILTER (WHERE team_code IS NOT NULL)
			FROM event_co_organizers WHERE event_id = $1`, id)
		if got != want {
			t.Errorf("event %s: people|teams %q, want %q", id, got, want)
		}
	}
	if r := only(retention.ModeApply); r.Matched != 0 || r.Changed != 0 {
		t.Fatalf("second apply %+v", r)
	}
}
