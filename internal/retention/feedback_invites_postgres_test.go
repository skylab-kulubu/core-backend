package retention_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/retention"
)

// feedback_invites_age deletes the feedback invites sent 90 days ago and
// leaves younger ones and the counts.
func TestFeedbackInvitesAgeDeletesInvitesNinetyDaysOld(t *testing.T) {
	pool := migrated(t)
	ctx := context.Background()
	eventID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO events (id, name, location, owner_team) VALUES ($1, 'Hack', 'YTÜ', 'WEBLAB')`, eventID); err != nil {
		t.Fatal(err)
	}
	invite := func(age time.Duration) uuid.UUID {
		t.Helper()
		ticketID, id := uuid.New(), uuid.New()
		if _, err := pool.Exec(ctx, `INSERT INTO tickets (id, event_id, ticket_type, guest_email) VALUES ($1, $2, 'GUEST', $3)`, ticketID, eventID, ticketID.String()+"@example.test"); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO feedback_invites (id, ticket_id, event_id, form_id, code, sent_at) VALUES ($1, $2, $3, $4, $5, $6)`,
			id, ticketID, eventID, uuid.New(), "A"+id.String()[:8]+"AAAAAAAAAAAAA", testNow.Add(-age)); err != nil {
			t.Fatal(err)
		}
		return id
	}
	old := invite(90*day + time.Hour)
	young := invite(90*day - time.Hour)
	now := testNow
	s := sweeper(pool, dryRunConfig(), &now, nil)
	only := func(mode retention.Mode) retention.RuleResult {
		t.Helper()
		report, err := s.Run(ctx, retention.RunOptions{Mode: mode, Trigger: retention.TriggerCLI, Rule: "feedback_invites_age"})
		if err != nil || report.Status != retention.RunOK {
			t.Fatalf("%s: %v %+v", mode, err, report)
		}
		return results(report)["feedback_invites_age"]
	}
	if r := only(retention.ModeDryRun); r.Matched != 1 || r.Changed != 0 {
		t.Fatalf("dry run %+v", r)
	}
	if r := only(retention.ModeApply); r.Matched != 1 || r.Changed != 1 {
		t.Fatalf("apply %+v", r)
	}
	for id, kept := range map[uuid.UUID]bool{old: false, young: true} {
		if got := scalar[bool](t, pool, `SELECT EXISTS (SELECT 1 FROM feedback_invites WHERE id = $1)`, id); got != kept {
			t.Errorf("invite %s kept %v, want %v", id, got, kept)
		}
	}
	if sent := scalar[int32](t, pool, `SELECT sent FROM feedback_invite_counts WHERE event_id = $1`, eventID); sent != 2 {
		t.Fatalf("counts sent %d", sent)
	}
}
