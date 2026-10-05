package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/internal/erasure"
	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/retention"
)

// retentionSweepCommandName runs the periodic destruction run once, by hand
// (ADR-0062, docs/retention-sweep.md):
//
//	core-backend retention-sweep [--rule NAME]                  # dry run: counts, changes nothing
//	core-backend retention-sweep --apply [--rule NAME]          # changes the rows, under the brake
//	core-backend retention-sweep --apply --allow-large          # past the brake: the first backlog, on purpose
//
// It runs inside the core container, which has the environment: the rules'
// windows follow RETENTION_SWEEP_MODE as the server's do, whatever the mode.
// The run takes the same lock and writes the same record as the schedule's
// (triggered_by cli). It is also the step after restoring a backup: one
// --apply run, so the restore brings back nothing past its period. It prints
// rule names and counts only.
const retentionSweepCommandName = "retention-sweep"

// Exit codes beyond 0 (done; a dry run, counted), 1 (a rule failed or was
// refused, or the run failed) and 2 (usage or configuration).
const retentionExitLocked = 3

type retentionSweepOptions struct {
	apply      bool
	allowLarge bool
	rule       string
}

// retentionRunner is the sweeper, as the command uses it.
type retentionRunner interface {
	Run(context.Context, retention.RunOptions) (retention.Report, error)
}

func runRetentionSweep(args []string, getenv func(string) string, out io.Writer) int {
	mediaPurge, err := media.BlobPurgeConfigFromEnv(getenv)
	if err != nil {
		fmt.Fprintln(out, err)
		return 2
	}
	config, err := retention.ConfigFromEnv(getenv, mediaPurge.RecoveryWindow)
	if err != nil {
		fmt.Fprintln(out, err)
		return 2
	}
	// The periods are the configured interval whatever the mode, so a run by
	// hand with the schedule off still keeps the record's periods right.
	if config.Period, err = erasure.PeriodicDestructionIntervalFromEnv(getenv); err != nil {
		fmt.Fprintln(out, err)
		return 2
	}
	options, code := parseRetentionSweepOptions(args, out, retention.RuleNames(config))
	if code != 0 {
		return code
	}
	if strings.TrimSpace(getenv("DATABASE_URL")) == "" {
		fmt.Fprintf(out, "%s needs DATABASE_URL\n", retentionSweepCommandName)
		return 2
	}
	ctx, stop := commandContext()
	defer stop()
	pool, err := pgxpool.New(ctx, getenv("DATABASE_URL"))
	if err != nil {
		fmt.Fprintln(out, "database: cannot open the connection pool")
		return 1
	}
	defer pool.Close()
	return retentionSweepCommand(ctx, out, retention.NewSweeper(pool, config, nil), config, options)
}

func parseRetentionSweepOptions(args []string, out io.Writer, rules []string) (retentionSweepOptions, int) {
	flags := flag.NewFlagSet(retentionSweepCommandName, flag.ContinueOnError)
	flags.SetOutput(out)
	flags.Usage = func() {
		fmt.Fprintf(out, "usage: core-backend %s [--apply [--allow-large]] [--rule NAME]\n", retentionSweepCommandName)
		fmt.Fprintln(out, "Without --apply it is a dry run: it counts what apply would change and changes nothing.")
		fmt.Fprintf(out, "Rules: %s\n", strings.Join(rules, ", "))
		flags.PrintDefaults()
	}
	var options retentionSweepOptions
	flags.BoolVar(&options.apply, "apply", false, "change the rows; without it the run only counts")
	flags.BoolVar(&options.allowLarge, "allow-large", false, "with --apply: go past the brake (more than a fifth of a table or 50,000 rows), for the first backlog")
	flags.StringVar(&options.rule, "rule", "", "run only this rule; such a run is not the day's run")
	if err := flags.Parse(args); err != nil {
		return options, 2
	}
	if flags.NArg() > 0 {
		flags.Usage()
		return options, 2
	}
	if options.allowLarge && !options.apply {
		fmt.Fprintln(out, "--allow-large needs --apply")
		return options, 2
	}
	if options.rule != "" {
		known := false
		for _, rule := range rules {
			known = known || rule == options.rule
		}
		if !known {
			fmt.Fprintf(out, "no rule %q; the rules are %s\n", options.rule, strings.Join(rules, ", "))
			return options, 2
		}
	}
	return options, 0
}

// retentionSweepCommand runs once and prints what each rule did.
func retentionSweepCommand(ctx context.Context, out io.Writer, runner retentionRunner, config retention.Config, options retentionSweepOptions) int {
	mode := retention.ModeDryRun
	if options.apply {
		mode = retention.ModeApply
	}
	report, err := runner.Run(ctx, retention.RunOptions{
		Mode: mode, Trigger: retention.TriggerCLI, Rule: options.rule, AllowLarge: options.allowLarge,
	})
	if errors.Is(err, retention.ErrLocked) {
		fmt.Fprintln(out, "another retention sweep run holds the lock (the schedule, another replica or another command); nothing was done")
		return retentionExitLocked
	}
	switch mode {
	case retention.ModeDryRun:
		fmt.Fprintf(out, "%s: dry run, nothing was changed (rule set v%d, schedule %s=%s)\n", retentionSweepCommandName, retention.RuleSetVersion, retention.ModeEnv, config.Mode)
	default:
		fmt.Fprintf(out, "%s: apply (rule set v%d, schedule %s=%s)\n", retentionSweepCommandName, retention.RuleSetVersion, retention.ModeEnv, config.Mode)
	}
	if len(report.Rules) > 0 {
		table := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(table, "RULE\tV\tKIND\tACTION\tTABLE\tCUTOFF\tMATCHED\tCHANGED\tRELATED\tOVERDUE\tANCHORLESS\tTABLE_ROWS\tSTATUS")
		for _, r := range report.Rules {
			cutoff, rows, status := "-", "-", string(r.Status)
			if r.Cutoff != nil {
				cutoff = r.Cutoff.UTC().Format(time.RFC3339)
			}
			if r.TableRows != nil {
				rows = fmt.Sprint(*r.TableRows)
			}
			if r.ErrorCode != "" {
				status += " (" + r.ErrorCode + ")"
			}
			fmt.Fprintf(table, "%s\t%d\t%s\t%s\t%s\t%s\t%d\t%d\t%d\t%d\t%d\t%s\t%s\n", r.Rule.Name, r.Rule.Version, r.Rule.Kind, r.Rule.Action,
				r.Rule.Table, cutoff, r.Matched, r.Changed, r.RelatedChanged, r.Overdue, r.Anchorless, rows, status)
		}
		_ = table.Flush()
		for _, r := range report.Rules {
			if r.Rule.NotApplicable != "" {
				fmt.Fprintf(out, "%s: not applicable: %s\n", r.Rule.Name, r.Rule.NotApplicable)
			}
		}
	}
	for _, period := range report.ClosedPeriods {
		fmt.Fprintf(out, "closed period %s: %s to %s, %d apply and %d dry runs, %d rows changed\n", period.ID,
			period.StartedAt.UTC().Format(time.RFC3339), period.EndsAt.UTC().Format(time.RFC3339), period.ApplyRuns, period.DryRuns, period.RowsChanged)
	}
	if err != nil {
		fmt.Fprintf(out, "run %s failed: %v\n", report.RunID, err)
		return 1
	}
	fmt.Fprintf(out, "run %s: %s\n", report.RunID, report.Status)
	if report.Status != retention.RunOK {
		return 1
	}
	return 0
}
