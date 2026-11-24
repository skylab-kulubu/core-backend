package membershipterm_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/membershipterm"
	"github.com/skylab-kulubu/core-backend/internal/teamaudit"
)

func runWith(t *testing.T, svc *membershipterm.Service, mode membershipterm.Mode) membershipterm.Run {
	t.Helper()
	run, err := svc.Run(context.Background(), membershipterm.RunOptions{Mode: mode, Trigger: membershipterm.TriggerCLI})
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func (f *fixture) members(t *testing.T, group uuid.UUID) []uuid.UUID {
	t.Helper()
	people, err := f.dir.Members(context.Background(), group.String())
	if err != nil {
		t.Fatal(err)
	}
	var out []uuid.UUID
	for _, p := range people {
		out = append(out, p.ID)
	}
	return sorted(out...)
}

// ADR-0078 karar 3 (2026-11-24): 30 days after its end an expired term's
// holder leaves the Group. Off (the default) nobody does; a dry run counts;
// apply takes each out through the audited system removal and ends the
// term. A term extended in those 30 days stays, and so does its holder; a
// holder taken out in the console meanwhile only has the term ended.
func TestRemovalThirtyDaysAfterTheEnd(t *testing.T) {
	f := newFixture(t, membershipterm.ModeApply)
	p := f.people
	ctx := context.Background()
	f.run(t, membershipterm.ModeApply)
	f.setNow(termEnd.Add(time.Second))
	if run := f.run(t, membershipterm.ModeApply); run.Expired != 5 || run.Removal != membershipterm.ModeOff {
		t.Fatalf("at the end %+v", run)
	}

	// DK's term extended; WEBLAB's Leader taken out in the console.
	ctxAdmin, admin := as(p.admin, "/ADMIN")
	if _, err := f.svc.Extend(ctxAdmin, admin, f.termOf(t, p.dk1, gDK).ID, day(t, "2027-09-30")); err != nil {
		t.Fatal(err)
	}
	if err := f.dir.RemoveMember(ctx, gWeblabL.String(), p.lead); err != nil {
		t.Fatal(err)
	}

	removing := f.serviceRemoving(t, membershipterm.ModeApply, f.dir)
	f.setNow(termEnd.Add(membershipterm.RemoveAfter - time.Hour))
	if run := runWith(t, removing, membershipterm.ModeApply); run.Removed != 0 || run.Ended != 1 || run.Removal != membershipterm.ModeApply {
		t.Fatalf("an hour before %+v", run)
	}

	f.setNow(termEnd.Add(membershipterm.RemoveAfter + time.Second))
	// Off: due, and nobody leaves.
	if run := f.run(t, membershipterm.ModeApply); run.Removal != membershipterm.ModeOff || run.Removed != 0 {
		t.Fatalf("off %+v", run)
	}
	f.svc.Refresh(ctx)
	if metrics := f.svc.Prometheus(); !contains(metrics, "skylab_management_terms_removal_due 3\n", `skylab_management_terms_auto_remove_mode{mode="off"} 1`) {
		t.Fatalf("metrics\n%s", metrics)
	}
	// Dry run: counted, nobody leaves; an apply run by hand (no writer)
	// only counts too.
	dry := f.serviceRemoving(t, membershipterm.ModeDryRun, f.dir)
	if run := runWith(t, dry, membershipterm.ModeApply); run.Removal != membershipterm.ModeDryRun || run.Removed != 3 {
		t.Fatalf("dry run %+v", run)
	}
	byHand := f.config
	byHand.AutoRemove = membershipterm.ModeApply
	command, err := membershipterm.NewService(byHand)
	if err != nil {
		t.Fatal(err)
	}
	if run := runWith(t, command, membershipterm.ModeApply); run.Removal != membershipterm.ModeDryRun || run.Removed != 3 {
		t.Fatalf("by hand %+v", run)
	}
	if got := f.members(t, gYK); !slices.Equal(got, sorted(p.yk1, p.yk2)) {
		t.Fatalf("YK after the counts %v", got)
	}

	f.dir.Ops = nil
	run := runWith(t, removing, membershipterm.ModeApply)
	if run.Removal != membershipterm.ModeApply || run.Removed != 3 || run.Status != "ok" {
		t.Fatalf("apply %+v", run)
	}
	if got := f.members(t, gYK); len(got) != 0 {
		t.Fatalf("YK after the removal %v", got)
	}
	if got := f.members(t, gAIL); len(got) != 0 {
		t.Fatalf("AI's Leaders after the removal %v", got)
	}
	if got := f.members(t, gDK); !slices.Equal(got, []uuid.UUID{p.dk1}) {
		t.Fatalf("DK, extended, after the removal %v", got)
	}
	if got := f.members(t, gAdmin); !slices.Equal(got, []uuid.UUID{p.admin}) {
		t.Fatalf("ADMIN after the removal %v", got)
	}
	open := f.open(t)
	if len(open) != 1 || open[0].UserID != p.dk1 {
		t.Fatalf("open terms %+v", open)
	}
	rows := f.auditRows(t, teamaudit.ActionMemberRemove)
	var targets []uuid.UUID
	for _, row := range rows {
		if row["actor"] != uuid.Nil.String() || row["basis"] != "system" || row["client"] != "system:term-removal" || row["outcome"] != "ok" {
			t.Fatalf("audit row %v", row)
		}
		targets = append(targets, uuid.MustParse(row["target"].(string)))
	}
	if !slices.Equal(sorted(targets...), sorted(p.yk1, p.yk2, p.sublead)) {
		t.Fatalf("removal rows of %v", targets)
	}
	// The runs' records: the dry run's and the command's counts, the
	// apply's removals.
	var dryRuns, applied int
	if err := f.pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE removal = 'dry-run' AND removed = 3), count(*) FILTER (WHERE removal = 'apply' AND removed = 3)
		FROM management_term_runs`).Scan(&dryRuns, &applied); err != nil {
		t.Fatal(err)
	}
	if dryRuns != 2 || applied != 1 {
		t.Fatalf("the runs' records: %d dry runs, %d applied", dryRuns, applied)
	}
	if again := runWith(t, removing, membershipterm.ModeApply); again.Removed != 0 {
		t.Fatalf("removed twice %+v", again)
	}
}

// A run whose Keycloak read failed takes nobody out; the next one does.
func TestAFailedReadRemovesNobody(t *testing.T) {
	f := newFixture(t, membershipterm.ModeApply)
	p := f.people
	f.run(t, membershipterm.ModeApply)
	f.setNow(termEnd.Add(time.Second))
	f.run(t, membershipterm.ModeApply)
	f.setNow(termEnd.Add(membershipterm.RemoveAfter + time.Second))

	unread := f.serviceRemoving(t, membershipterm.ModeApply, unreadGroup{Memory: f.dir, id: gDK})
	run, err := unread.Run(context.Background(), membershipterm.RunOptions{Mode: membershipterm.ModeApply, Trigger: membershipterm.TriggerCLI})
	if err == nil || run.ErrorCode != "reconcile_failed" || run.Removed != 0 {
		t.Fatalf("failed read %+v %v", run, err)
	}
	if got := f.members(t, gYK); !slices.Equal(got, sorted(p.yk1, p.yk2)) {
		t.Fatalf("YK after a failed read %v", got)
	}
	if run := runWith(t, f.serviceRemoving(t, membershipterm.ModeApply, f.dir), membershipterm.ModeApply); run.Removed != 5 {
		t.Fatalf("the next run %+v", run)
	}
}

func contains(text string, parts ...string) bool {
	for _, part := range parts {
		if !strings.Contains(text, part) {
			return false
		}
	}
	return true
}
