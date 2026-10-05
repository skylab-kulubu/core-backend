package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/retention"
)

func retentionEnv(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}

func TestRetentionSweepRefusesBadUsageAndConfiguration(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		args []string
		env  map[string]string
		want string
	}{
		"allow-large without apply": {[]string{"--allow-large"}, nil, "--allow-large needs --apply"},
		"unknown rule":              {[]string{"--rule", "guests"}, nil, `no rule "guests"; the rules are guest_phone, guest_identity, door_staff`},
		"stray argument":            {[]string{"apply"}, nil, "usage: core-backend retention-sweep"},
		"unknown flag":              {[]string{"--force"}, nil, "flag provided but not defined"},
		"bad mode":                  {nil, map[string]string{"RETENTION_SWEEP_MODE": "on"}, "RETENTION_SWEEP_MODE must be off, dry-run or apply"},
		"bad interval":              {nil, map[string]string{"PERIODIC_DESTRUCTION_INTERVAL": "1y"}, "PERIODIC_DESTRUCTION_INTERVAL must be a positive duration"},
		"no database":               {[]string{"--apply"}, nil, "retention-sweep needs DATABASE_URL"},
	} {
		var out bytes.Buffer
		if code := runRetentionSweep(c.args, retentionEnv(c.env), &out); code != 2 || !strings.Contains(out.String(), c.want) {
			t.Errorf("%s: exit %d\n%s", name, code, out.String())
		}
	}
}

type fakeRunner struct {
	got    retention.RunOptions
	report retention.Report
	err    error
}

func (f *fakeRunner) Run(_ context.Context, options retention.RunOptions) (retention.Report, error) {
	f.got = options
	return f.report, f.err
}

func TestRetentionSweepPrintsCountsAndExitsByOutcome(t *testing.T) {
	t.Parallel()
	cutoff := time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC)
	rows := int64(5000)
	config := retention.Config{Mode: retention.ModeDryRun, Period: 90 * 24 * time.Hour}
	runID := uuid.MustParse("6f0b8c4e-2a7d-4d0e-9a51-1f0c2b3d4e5f")
	phone := retention.RuleResult{Rule: ruleNamed(t, "guest_phone"), Cutoff: &cutoff, Matched: 12, Overdue: 10, Anchorless: 1, TableRows: &rows, Status: retention.RuleDryRun}
	hitsAge := retention.RuleResult{Rule: ruleNamed(t, "url_hits_age"), Status: retention.RuleNotApplicable}
	hitsAge.Rule.NotApplicable = "click rows are kept in apply mode"

	runner := &fakeRunner{report: retention.Report{RunID: runID, Status: retention.RunOK, Rules: []retention.RuleResult{phone, hitsAge}}}
	var out bytes.Buffer
	if code := retentionSweepCommand(context.Background(), &out, runner, config, retentionSweepOptions{rule: "guest_phone"}); code != 0 {
		t.Fatalf("exit %d\n%s", code, out.String())
	}
	if runner.got != (retention.RunOptions{Mode: retention.ModeDryRun, Trigger: retention.TriggerCLI, Rule: "guest_phone"}) {
		t.Fatalf("options %+v", runner.got)
	}
	for _, want := range []string{
		"retention-sweep: dry run, nothing was changed (rule set v1, schedule RETENTION_SWEEP_MODE=dry-run)",
		"guest_phone   1  sweep  scrub   tickets   2026-07-07T12:00:00Z  12       0        0        10       1           5000        dry_run",
		"url_hits_age: not applicable: click rows are kept in apply mode",
		"run 6f0b8c4e-2a7d-4d0e-9a51-1f0c2b3d4e5f: ok",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}

	runner = &fakeRunner{report: retention.Report{RunID: runID, Status: retention.RunPartial, Rules: []retention.RuleResult{
		{Rule: ruleNamed(t, "door_staff"), Cutoff: &cutoff, Status: retention.RuleFailed, ErrorCode: "sqlstate_57014"},
	}}}
	out.Reset()
	if code := retentionSweepCommand(context.Background(), &out, runner, config, retentionSweepOptions{apply: true, allowLarge: true}); code != 1 ||
		!strings.Contains(out.String(), "failed (sqlstate_57014)") || !strings.Contains(out.String(), "partial") {
		t.Fatalf("partial: exit %d\n%s", code, out.String())
	}
	if runner.got != (retention.RunOptions{Mode: retention.ModeApply, Trigger: retention.TriggerCLI, AllowLarge: true}) {
		t.Fatalf("options %+v", runner.got)
	}

	out.Reset()
	locked := &fakeRunner{report: retention.Report{Status: retention.RunSkippedLocked}, err: retention.ErrLocked}
	if code := retentionSweepCommand(context.Background(), &out, locked, config, retentionSweepOptions{apply: true}); code != 3 ||
		!strings.Contains(out.String(), "holds the lock") {
		t.Fatalf("locked: exit %d\n%s", code, out.String())
	}

	out.Reset()
	failed := &fakeRunner{report: retention.Report{RunID: runID}, err: errors.New("database went away")}
	if code := retentionSweepCommand(context.Background(), &out, failed, config, retentionSweepOptions{}); code != 1 {
		t.Fatalf("failed: exit %d\n%s", code, out.String())
	}
}

func ruleNamed(t *testing.T, name string) retention.Rule {
	t.Helper()
	for _, rule := range retention.Rules(retention.Config{Mode: retention.ModeDryRun}, retention.Schema{}) {
		if rule.Name == name {
			return rule
		}
	}
	t.Fatalf("no rule %s", name)
	return retention.Rule{}
}
