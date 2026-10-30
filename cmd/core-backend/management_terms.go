package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/mail"
	"github.com/skylab-kulubu/core-backend/internal/membershipterm"
	"github.com/skylab-kulubu/core-backend/internal/season"
	"github.com/skylab-kulubu/core-backend/internal/teamaudit"
	"github.com/skylab-kulubu/core-backend/internal/teammail"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// managementTermsDeps are what the management membership terms read and
// write (docs/management-membership-terms.md).
type managementTermsDeps struct {
	pool    *pgxpool.Pool
	dir     identity.Directory
	seasons season.Store
	users   user.Store
	sky     *mail.SkyMail
	audit   teamaudit.Writer
	az      authz.Authorizer
	rules   membershipterm.Rules
}

// newManagementTerms builds the terms' service in mode.
func newManagementTerms(mode membershipterm.Mode, weekly bool, deps managementTermsDeps, logf func(string, ...any)) (*membershipterm.Service, error) {
	return membershipterm.NewService(membershipterm.Config{
		Mode: mode, Weekly: weekly, Rules: deps.rules,
		Store: membershipterm.NewPostgresStore(deps.pool), Directory: deps.dir, Seasons: deps.seasons,
		Rosters:  teammail.DirectoryRosters{Directory: deps.dir},
		People:   teammail.DirectoryPeople{Directory: deps.dir, Accounts: deps.users},
		Accounts: deps.users, Mail: deps.sky, Audit: deps.audit, Authz: deps.az, Logf: logf,
	})
}

// managementTermsNotifier is terms as a membership notifier: only in apply,
// so that off and dry-run read no Group on an add (Notifies is false there).
func managementTermsNotifier(terms *membershipterm.Service) identity.MembershipNotifier {
	if terms == nil || terms.Mode() != membershipterm.ModeApply {
		return nil
	}
	return terms
}

// startManagementTerms reads MANAGEMENT_TERMS_MODE and
// MANAGEMENT_TERMS_WEEKLY_SUMMARY and starts the daily job and the mail
// worker until ctx is cancelled (none while off). The service always
// exists: off, it hears of no membership, serves no route and its metrics
// say off.
func startManagementTerms(ctx context.Context, getenv func(string) string, deps managementTermsDeps, worker func(string, <-chan struct{}), logf func(string, ...any)) (*membershipterm.Service, error) {
	mode, err := membershipterm.ModeFromEnv(getenv)
	if err != nil {
		return nil, err
	}
	weekly, err := membershipterm.WeeklySummaryFromEnv(getenv)
	if err != nil {
		return nil, err
	}
	terms, err := newManagementTerms(mode, weekly, deps, logf)
	if err != nil {
		return nil, err
	}
	worker("management terms", terms.Maintain(ctx, membershipterm.CheckInterval))
	worker("management term mail", terms.Work(ctx))
	switch {
	case mode == membershipterm.ModeOff:
		logf("management terms: off (%s is not dry-run or apply)", membershipterm.ModeEnv)
	case deps.sky.ManagementTermConfigured():
		logf("management terms: %s (%s), daily; mail %s, weekly summary %t", mode, membershipterm.ModeEnv, deps.sky.ManagementTermTemplateKey, weekly)
	default:
		logf("management terms: %s (%s), daily; mail off (SKYMAIL_MANAGEMENT_TERM_TEMPLATE_KEY empty or no SkyMail)", mode, membershipterm.ModeEnv)
	}
	return terms, nil
}

// managementTermsCommandName runs the management terms' daily job once, by
// hand (docs/management-membership-terms.md):
//
//	core-backend management-terms           # dry run: counts what apply would write and send
//	core-backend management-terms --apply   # writes, marks and queues; needs MANAGEMENT_TERMS_MODE=apply
//
// It runs inside the core container, which has the environment, takes the
// schedule's lock and records the run (triggered_by cli). It prints counts
// only: no person, Group or path.
const managementTermsCommandName = "management-terms"

const managementTermsExitLocked = 3

// managementTermsRunner is the service, as the command uses it.
type managementTermsRunner interface {
	Run(context.Context, membershipterm.RunOptions) (membershipterm.Run, error)
}

func runManagementTerms(args []string, getenv func(string) string, out io.Writer) int {
	apply, code := parseManagementTermsOptions(args, out)
	if code != 0 {
		return code
	}
	mode, err := membershipterm.ModeFromEnv(getenv)
	if err != nil {
		fmt.Fprintln(out, err)
		return 2
	}
	if apply && mode != membershipterm.ModeApply {
		fmt.Fprintf(out, "%s --apply needs %s=apply: terms written while the job is off would not be kept\n", managementTermsCommandName, membershipterm.ModeEnv)
		return 2
	}
	config, missing, err := keycloakFromEnv(getenv)
	if len(missing) > 0 {
		fmt.Fprintf(out, "%s needs %s\n", managementTermsCommandName, strings.Join(missing, ", "))
		return 2
	}
	if err != nil {
		fmt.Fprintf(out, "%s: %v\n", managementTermsCommandName, err)
		return 2
	}
	if strings.TrimSpace(getenv("DATABASE_URL")) == "" {
		fmt.Fprintf(out, "%s needs DATABASE_URL\n", managementTermsCommandName)
		return 2
	}
	roots, err := authz.PrivilegedRootsFromEnv(getenv)
	if err != nil {
		fmt.Fprintln(out, err)
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
	keycloak := identity.NewKeycloak(config)
	runMode := membershipterm.ModeDryRun
	if apply {
		runMode = membershipterm.ModeApply
	}
	terms, err := newManagementTerms(runMode, true, managementTermsDeps{
		pool: pool, dir: keycloak, seasons: season.NewPostgresStore(pool), users: user.NewPostgresStore(pool),
		audit: teamaudit.NewPostgres(pool),
		rules: membershipterm.Rules{PrivilegedRoots: roots, LeaderSubgroups: authz.DefaultPolicy().LeaderSubgroups},
	}, func(string, ...any) {})
	if err != nil {
		fmt.Fprintln(out, err)
		return 2
	}
	return managementTermsCommand(ctx, terms, runMode, out)
}

func parseManagementTermsOptions(args []string, out io.Writer) (bool, int) {
	flags := flag.NewFlagSet(managementTermsCommandName, flag.ContinueOnError)
	flags.SetOutput(out)
	apply := flags.Bool("apply", false, "write the terms, mark and queue the mails (MANAGEMENT_TERMS_MODE=apply)")
	if err := flags.Parse(args); err != nil {
		return false, 2
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(out, "%s takes no arguments, only flags\n", managementTermsCommandName)
		return false, 2
	}
	return *apply, 0
}

// managementTermsCommand runs the job once and prints its counts.
func managementTermsCommand(ctx context.Context, terms managementTermsRunner, mode membershipterm.Mode, out io.Writer) int {
	run, err := terms.Run(ctx, membershipterm.RunOptions{Mode: mode, Trigger: membershipterm.TriggerCLI})
	if errors.Is(err, membershipterm.ErrLocked) {
		fmt.Fprintln(out, "another run holds the lock; try again later")
		return managementTermsExitLocked
	}
	verb := "would write"
	if mode == membershipterm.ModeApply {
		verb = "wrote"
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "mode\t%s\n", mode)
	fmt.Fprintf(w, "active season with an end ahead\t%t\n", run.SeasonFound)
	fmt.Fprintf(w, "terms added (memberships without one, %s)\t%d\n", verb, run.Backfilled)
	fmt.Fprintf(w, "undated terms given the season's end\t%d\n", run.Dated)
	fmt.Fprintf(w, "terms ended (membership gone)\t%d\n", run.Ended)
	fmt.Fprintf(w, "14-day warnings\t%d\n", run.Warned14d)
	fmt.Fprintf(w, "1-day warnings\t%d\n", run.Warned1d)
	fmt.Fprintf(w, "terms marked expired\t%d\n", run.Expired)
	fmt.Fprintf(w, "mails queued\t%d\n", run.Mails)
	fmt.Fprintf(w, "weekly summary\t%s\n", run.Weekly)
	fmt.Fprintf(w, "status\t%s\n", run.Status)
	_ = w.Flush()
	if err != nil {
		fmt.Fprintf(out, "failed: %s\n", run.ErrorCode)
		return 1
	}
	return 0
}
