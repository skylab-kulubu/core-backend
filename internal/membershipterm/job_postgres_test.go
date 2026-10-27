package membershipterm_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/membershipterm"
	"github.com/skylab-kulubu/core-backend/internal/teamaudit"
)

// The first rollout: a dry run says how many terms apply would write and
// writes none; apply gives every YK, DK and Team Leader membership the
// active season's end, and none to ADMIN or an ordinary Team member. A
// membership taken away in Keycloak's console ends its term.
func TestBackfillDryRunCountsThenApplyWritesManagementTermsOnly(t *testing.T) {
	f := newFixture(t, membershipterm.ModeApply)
	p := f.people

	dry := f.run(t, membershipterm.ModeDryRun)
	if dry.Backfilled != 5 || dry.Ended != 0 || dry.Expired != 0 || dry.Mails != 0 || !dry.SeasonFound {
		t.Fatalf("dry run %+v", dry)
	}
	if terms := f.open(t); len(terms) != 0 {
		t.Fatalf("a dry run wrote %d terms", len(terms))
	}

	run := f.run(t, membershipterm.ModeApply)
	if run.Backfilled != 5 || run.Status != "ok" {
		t.Fatalf("apply %+v", run)
	}
	var holders []uuid.UUID
	for _, term := range f.open(t) {
		holders = append(holders, term.UserID)
		if term.EndsAt == nil || !term.EndsAt.Equal(termEnd) || term.Source != membershipterm.SourceBackfill || term.SeasonID == nil {
			t.Fatalf("term %+v", term)
		}
	}
	if !slices.Equal(sorted(holders...), sorted(p.yk1, p.yk2, p.dk1, p.lead, p.sublead)) {
		t.Fatalf("terms of %v", holders)
	}
	if again := f.run(t, membershipterm.ModeApply); again.Backfilled != 0 || again.Ended != 0 {
		t.Fatalf("second run %+v", again)
	}

	// Taken out of YK in Keycloak's console.
	if err := f.dir.RemoveMember(context.Background(), gYK.String(), p.yk2); err != nil {
		t.Fatal(err)
	}
	if run := f.run(t, membershipterm.ModeApply); run.Ended != 1 || run.Backfilled != 0 {
		t.Fatalf("after the console removal %+v", run)
	}
	if len(f.open(t)) != 4 {
		t.Fatal("the removed membership's term is open")
	}

	// WEBLAB renamed: its Leaders keep their terms, under the new path.
	f.dir.PutGroup(identity.Group{ID: gWeblab.String(), Name: "WEB", Path: "/UYELER/ARGE/WEB"})
	f.dir.PutGroup(identity.Group{ID: gWeblabL.String(), Name: "LIDERLER", Path: "/UYELER/ARGE/WEB/LIDERLER"})
	if run := f.run(t, membershipterm.ModeApply); run.Ended != 0 || run.Backfilled != 0 {
		t.Fatalf("after the rename %+v", run)
	}
	if term := f.termOf(t, p.lead, gWeblabL); term.GroupPath != "/UYELER/ARGE/WEB/LIDERLER" {
		t.Fatalf("renamed term %+v", term)
	}
}

// Clock moved: 14 days and 1 day before the end the holder and those over
// them get one warning each, never twice; at the end the term is only
// marked: nobody leaves a Group, Keycloak is not written, the mark is
// recorded and ADMIN gets the day's list; a week on, ADMIN gets the
// summary. ADMIN's and the ordinary member's memberships have no term.
func TestWarningsThenTheMarkWithoutRemoval(t *testing.T) {
	f := newFixture(t, membershipterm.ModeApply)
	p := f.people
	f.run(t, membershipterm.ModeApply)

	f.setNow(termEnd.Add(-membershipterm.WarnEarly))
	if run := f.run(t, membershipterm.ModeApply); run.Warned14d != 5 || run.Warned1d != 0 {
		t.Fatalf("14 days before %+v", run)
	}
	queued := f.queued(t)
	for subject, want := range map[uuid.UUID][]uuid.UUID{
		p.yk1:     {p.yk1, p.admin, p.yk2},           // ADMIN and YK
		p.dk1:     {p.dk1, p.admin},                  // ADMIN and DK
		p.lead:    {p.lead, p.yk1, p.yk2},            // no Team above WEBLAB: YK
		p.sublead: {p.sublead, p.lead, p.yk1, p.yk2}, // WEBLAB's Leaders and YK
	} {
		if got := recipients(queued[membershipterm.NoticeWarning14d], subject); !slices.Equal(got, sorted(want...)) {
			t.Errorf("14-day warning of %s to %v, want %v", subject, got, sorted(want...))
		}
	}
	first := len(queued[membershipterm.NoticeWarning14d])
	if run := f.run(t, membershipterm.ModeApply); run.Warned14d != 0 || run.Mails != 0 {
		t.Fatalf("warned twice %+v", run)
	}
	if again := f.queued(t); len(again[membershipterm.NoticeWarning14d]) != first {
		t.Fatal("a second 14-day warning was queued")
	}

	// The mails: the holder's own, and an overseer's.
	if _, err := f.svc.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	var own, other map[string]string
	for _, m := range f.mail.sent {
		if m.vars["Notice"] == "warning" && m.vars["DaysLeft"] == "14" && m.vars["SubjectName"] == "Yekta Test" {
			if m.recipient == "Yekta@example.test" {
				own = m.vars
			} else if m.recipient == "Yasemin@example.test" {
				other = m.vars
			}
		}
	}
	if own == nil || own["Self"] != "yes" || own["EndsAt"] != "31.05.2027" || own["Role"] != "board" || own["GroupName"] != "YK" || own["GroupId"] != gYK.String() {
		t.Fatalf("holder's warning %v", own)
	}
	if other == nil || other["Self"] != "no" {
		t.Fatalf("overseer's warning %v", other)
	}
	for _, m := range f.mail.sent {
		for _, v := range m.vars {
			if strings.Contains(v, "@") {
				t.Fatalf("an address in the mail's variables: %v", m.vars)
			}
		}
	}

	f.setNow(termEnd.Add(-membershipterm.WarnLate))
	if run := f.run(t, membershipterm.ModeApply); run.Warned1d != 5 || run.Warned14d != 0 {
		t.Fatalf("1 day before %+v", run)
	}
	if run := f.run(t, membershipterm.ModeApply); run.Warned1d != 0 {
		t.Fatalf("warned twice %+v", run)
	}
	if got := recipients(f.queued(t)[membershipterm.NoticeWarning1d], p.yk1); !slices.Equal(got, sorted(p.yk1, p.admin, p.yk2)) {
		t.Fatalf("1-day warning to %v", got)
	}

	f.dir.Ops = nil
	f.setNow(termEnd.Add(time.Second))
	run := f.run(t, membershipterm.ModeApply)
	if run.Expired != 5 || run.Ended != 0 {
		t.Fatalf("at the end %+v", run)
	}
	if slices.Contains(f.dir.Ops, "RemoveMember") || slices.Contains(f.dir.Ops, "AddMember") {
		t.Fatalf("Keycloak was written: %v", f.dir.Ops)
	}
	members, err := f.dir.Members(context.Background(), gYK.String())
	if err != nil || len(members) != 2 {
		t.Fatalf("YK's members %v %v", members, err)
	}
	for _, term := range f.open(t) {
		if term.ExpiredAt == nil || term.EndedAt != nil {
			t.Fatalf("term %+v", term)
		}
	}
	rows := f.auditRows(t, teamaudit.ActionTermExpired)
	if len(rows) != 5 {
		t.Fatalf("audit rows %v", rows)
	}
	for _, row := range rows {
		if row["actor"] != uuid.Nil.String() || row["basis"] != "system" || row["client"] != "system:term-expiry" || row["outcome"] != "ok" || row["target"] == "" {
			t.Fatalf("audit row %v", row)
		}
	}
	lists := f.queued(t)[membershipterm.NoticeExpiredList]
	if len(lists) != 1 || lists[0][0] != p.admin {
		t.Fatalf("lists %v", lists)
	}
	if run := f.run(t, membershipterm.ModeApply); run.Expired != 0 || run.Mails != 0 {
		t.Fatalf("marked twice %+v", run)
	}

	f.mail.sent = nil
	if _, err := f.svc.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	var lists2 []sentMail
	for _, m := range f.mail.sent {
		if m.vars["Notice"] == "expired-list" {
			lists2 = append(lists2, m)
		}
	}
	if len(lists2) != 1 || lists2[0].recipient != "Admin@example.test" || lists2[0].vars["Count"] != "5" ||
		!strings.Contains(lists2[0].vars["Items"], "Yekta Test · YK · 31.05.2027") {
		t.Fatalf("list mail %+v", lists2)
	}

	// The gauge: five expired terms whose holders are still in their Groups.
	f.svc.Refresh(context.Background())
	metrics := f.svc.Prometheus()
	if !strings.Contains(metrics, "skylab_management_terms_expired_in_group 5\n") || strings.Contains(metrics, "/UYELER") || strings.Contains(metrics, p.yk1.String()) {
		t.Fatalf("metrics\n%s", metrics)
	}

	// A week on, the summary.
	f.setNow(termEnd.Add(7 * 24 * time.Hour))
	if run := f.run(t, membershipterm.ModeApply); run.Weekly != membershipterm.WeeklySent {
		t.Fatalf("a week on %+v", run)
	}
	if weekly := f.queued(t)[membershipterm.NoticeWeeklySummary]; len(weekly) != 1 || weekly[0][0] != p.admin {
		t.Fatalf("weekly %v", weekly)
	}
	f.mail.sent = nil
	if _, err := f.svc.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.mail.sent) != 1 || f.mail.sent[0].vars["Notice"] != "weekly-summary" || f.mail.sent[0].vars["Count"] != "5" || !strings.Contains(f.mail.sent[0].vars["Items"], "YK: 2") {
		t.Fatalf("weekly mail %+v", f.mail.sent)
	}
	if run := f.run(t, membershipterm.ModeApply); run.Weekly != membershipterm.WeeklyNotDue {
		t.Fatalf("weekly twice %+v", run)
	}
}

// Only apply hears of the membership routes; dry-run and off write
// nothing and serve no route.
func TestOnlyApplyKeepsTerms(t *testing.T) {
	t.Parallel()
	for _, mode := range []membershipterm.Mode{membershipterm.ModeOff, membershipterm.ModeDryRun} {
		f := &fixture{}
		svc, err := membershipterm.NewService(membershipterm.Config{Mode: mode, Store: membershipterm.NewPostgresStore(nil),
			Directory: nilDirectory{}, Seasons: nilSeasons{}, Rosters: nilRosters{}, Audit: teamaudit.NewMemory(), Now: f.clock})
		if err != nil {
			t.Fatal(err)
		}
		if svc.Notifies(yk()) {
			t.Fatalf("%s hears of memberships", mode)
		}
		ctx, p := as(uuid.New(), "/UYELER/YK")
		if _, err := svc.List(ctx, p, membershipterm.Filter{}); err != membershipterm.ErrOff {
			t.Fatalf("%s lists: %v", mode, err)
		}
		if got, err := svc.ExpiredFor(ctx, uuid.New()); err != nil || len(got) != 0 {
			t.Fatalf("%s expired %v %v", mode, got, err)
		}
	}
}
