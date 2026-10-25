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

// feedback_mails_age deletes the feedback mails done with 90 days ago, or
// queued 90 days ago and never done with, and their items; it leaves
// younger ones and the targets.
func TestFeedbackMailsAgeDeletesMailsNinetyDaysOld(t *testing.T) {
	pool := migrated(t)
	ctx := context.Background()
	eventID, ticketID := uuid.New(), uuid.New()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO events (id, name, location, owner_team) VALUES ($1, 'Hack', 'YTÜ', 'WEBLAB')`, eventID)
	exec(`INSERT INTO tickets (id, event_id, ticket_type, guest_email) VALUES ($1, $2, 'GUEST', 'g@example.test')`, ticketID, eventID)
	exec(`INSERT INTO feedback_mail_targets (event_id, form_id, trigger) VALUES ($1, $2, 'auto')`, eventID, uuid.New())
	mail := func(created time.Duration, done *time.Duration) uuid.UUID {
		t.Helper()
		id := uuid.New()
		var doneAt *time.Time
		if done != nil {
			at := testNow.Add(-*done)
			doneAt = &at
		}
		exec(`INSERT INTO feedback_mails (id, event_id, ticket_id, next_attempt_at, created_at, done_at) VALUES ($1, $2, $3, $4, $4, $5)`,
			id, eventID, ticketID, testNow.Add(-created), doneAt)
		exec(`INSERT INTO feedback_mail_items (mail_id, event_id) VALUES ($1, $2)`, id, eventID)
		return id
	}
	old, young := 90*day+time.Hour, 90*day-time.Hour
	doneOld := mail(100*day, &old)
	doneYoung := mail(100*day, &young)
	neverDoneOld := mail(old, nil)
	neverDoneYoung := mail(young, nil)
	now := testNow
	s := sweeper(pool, dryRunConfig(), &now, nil)
	only := func(mode retention.Mode) retention.RuleResult {
		t.Helper()
		report, err := s.Run(ctx, retention.RunOptions{Mode: mode, Trigger: retention.TriggerCLI, Rule: "feedback_mails_age"})
		if err != nil || report.Status != retention.RunOK {
			t.Fatalf("%s: %v %+v", mode, err, report)
		}
		return results(report)["feedback_mails_age"]
	}
	if r := only(retention.ModeDryRun); r.Matched != 2 || r.Changed != 0 {
		t.Fatalf("dry run %+v", r)
	}
	if r := only(retention.ModeApply); r.Matched != 2 || r.Changed != 2 {
		t.Fatalf("apply %+v", r)
	}
	for id, kept := range map[uuid.UUID]bool{doneOld: false, neverDoneOld: false, doneYoung: true, neverDoneYoung: true} {
		if got := scalar[bool](t, pool, `SELECT EXISTS (SELECT 1 FROM feedback_mails WHERE id = $1)`, id); got != kept {
			t.Errorf("mail %s kept %v, want %v", id, got, kept)
		}
	}
	if items := scalar[int64](t, pool, `SELECT count(*) FROM feedback_mail_items`); items != 2 {
		t.Fatalf("items %d", items)
	}
	if targets := scalar[int64](t, pool, `SELECT count(*) FROM feedback_mail_targets`); targets != 1 {
		t.Fatalf("targets %d", targets)
	}
}
