package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/skylab-kulubu/core-backend/internal/erasure"
	"github.com/skylab-kulubu/core-backend/internal/erasurereplay"
)

// replayFromBackupCommandName erases again, in a service restored from a dump
// taken at T, the people whose deletion requests completed at or after T
// (ADR-0053, docs/account-lifecycle.md, Replay after a restore):
//
//	core-backend replay-from-backup --service skymail --restored-at 2026-09-20T03:00:00Z \
//	    --core-snapshot-dsn <dsn> --keycloak-snapshot-dsn <dsn> [--apply]
//
// It runs inside the core container, which has the environment. The two
// snapshots are the core and Keycloak dumps taken with the service dump at
// T, restored into temporary databases with no network.
const replayFromBackupCommandName = "replay-from-backup"

// Exit codes beyond 0 (every request done, or resolved in a dry run), 1 (a
// request failed) and 2 (usage or configuration).
const replayExitRetryLater = 3

type replayOptions struct {
	service     erasure.Service
	restoredAt  time.Time
	coreDSN     string
	keycloakDSN string
	realm       string
	apply       bool
}

// runReplayFromBackup wires the replay to the three databases, all read-only,
// and with --apply to the service through the core-erasure client. It prints
// counts and request ids only: never an address, a name, a subject id or a
// configuration value.
func runReplayFromBackup(args []string, getenv func(string) string, out io.Writer) int {
	options, code := parseReplayOptions(args, out, time.Now())
	if code != 0 {
		return code
	}
	liveDSN := strings.TrimSpace(getenv("DATABASE_URL"))
	if liveDSN == "" {
		fmt.Fprintf(out, "%s needs DATABASE_URL\n", replayFromBackupCommandName)
		return 2
	}
	if options.coreDSN == liveDSN || options.keycloakDSN == liveDSN {
		fmt.Fprintln(out, "a snapshot DSN is DATABASE_URL: the snapshots are the dumps taken with the service dump, restored elsewhere")
		return 2
	}
	var sender *erasure.Client
	if options.apply {
		tokenURL, ok := keycloakTokenURL(getenv)
		if !ok {
			fmt.Fprintln(out, "--apply needs KEYCLOAK_URL and KEYCLOAK_REALM")
			return 2
		}
		client, err := erasure.ClientFromEnv(getenv, options.service, tokenURL)
		if err != nil {
			fmt.Fprintf(out, "--apply: %v\n", err)
			return 2
		}
		sender = client
	}

	ctx, stop := commandContext()
	defer stop()
	live, err := erasurereplay.OpenReadOnly(ctx, "live core", liveDSN)
	if err != nil {
		fmt.Fprintln(out, err)
		return 1
	}
	defer live.Close()
	core, err := erasurereplay.OpenReadOnly(ctx, "core snapshot", options.coreDSN)
	if err != nil {
		fmt.Fprintln(out, err)
		return 1
	}
	defer core.Close()
	keycloak, err := erasurereplay.OpenReadOnly(ctx, "keycloak snapshot", options.keycloakDSN)
	if err != nil {
		fmt.Fprintln(out, err)
		return 1
	}
	defer keycloak.Close()

	replay := erasurereplay.Replay{
		Service:    options.service,
		RestoredAt: options.restoredAt,
		Requests:   erasurereplay.LiveRequests{DB: live},
		Core:       erasurereplay.PostgresCoreSnapshot{DB: core},
		Keycloak:   erasurereplay.PostgresKeycloakSnapshot{DB: keycloak, Realm: options.realm},
		Apply:      options.apply,
	}
	if sender != nil {
		replay.Sender = sender
	}
	return replayFromBackupCommand(ctx, out, replay)
}

func parseReplayOptions(args []string, out io.Writer, now time.Time) (replayOptions, int) {
	flags := flag.NewFlagSet(replayFromBackupCommandName, flag.ContinueOnError)
	flags.SetOutput(out)
	service := flags.String("service", "", "the restored service: skymail, cms or forms")
	restoredAt := flags.String("restored-at", "", "the time T of the restored service dump (RFC 3339)")
	coreDSN := flags.String("core-snapshot-dsn", "", "the core dump taken at T, restored into a temporary database")
	keycloakDSN := flags.String("keycloak-snapshot-dsn", "", "the Keycloak dump taken at T, restored into a temporary database")
	realm := flags.String("keycloak-realm", "e-skylab", "the realm of the people in the Keycloak dump")
	apply := flags.Bool("apply", false, "send the Erasure command; without it the command only counts")
	if err := flags.Parse(args); err != nil {
		return replayOptions{}, 2
	}
	usage := func(message string) (replayOptions, int) {
		fmt.Fprintf(out, "%s: %s\n", replayFromBackupCommandName, message)
		return replayOptions{}, 2
	}
	if flags.NArg() > 0 {
		return usage("takes no arguments besides its flags")
	}
	options := replayOptions{
		coreDSN: strings.TrimSpace(*coreDSN), keycloakDSN: strings.TrimSpace(*keycloakDSN),
		realm: strings.TrimSpace(*realm), apply: *apply,
	}
	var ok bool
	if options.service, ok = erasure.ServiceNamed(strings.TrimSpace(*service)); !ok {
		return usage("--service must be skymail, cms or forms")
	}
	at, err := time.Parse(time.RFC3339, strings.TrimSpace(*restoredAt))
	if err != nil {
		return usage("--restored-at must be an RFC 3339 time, such as 2026-09-20T03:00:00Z")
	}
	if at.After(now) {
		return usage("--restored-at is in the future")
	}
	options.restoredAt = at
	if options.coreDSN == "" || options.keycloakDSN == "" {
		return usage("--core-snapshot-dsn and --keycloak-snapshot-dsn are required")
	}
	if options.coreDSN == options.keycloakDSN {
		return usage("--core-snapshot-dsn and --keycloak-snapshot-dsn name the same database")
	}
	if options.realm == "" {
		return usage("--keycloak-realm is empty")
	}
	return options, 0
}

// replayFromBackupCommand runs the replay and prints one record line per
// request (in a dry run, only the ones that need an operator) and the counts.
func replayFromBackupCommand(ctx context.Context, out io.Writer, replay erasurereplay.Replay) int {
	mode := "dry run, nothing is sent (add --apply to send)"
	if replay.Apply {
		mode = "sending the Erasure command"
	}
	fmt.Fprintf(out, "account erasure replay into %s restored at %s: %s\n",
		replay.Service.Name, replay.RestoredAt.UTC().Format(time.RFC3339Nano), mode)
	replay.Record = func(record erasurereplay.Record) {
		if replay.Apply || record.Outcome != erasurereplay.OutcomeResolved {
			fmt.Fprintln(out, record)
		}
	}
	report, err := replay.Run(ctx)
	if err != nil {
		fmt.Fprintf(out, "%v\nnothing was replayed\n", err)
		return 1
	}

	fmt.Fprintf(out, "requests completed at or after the restore: %d\n", report.Requests)
	for n, count := range report.ByAddresses {
		fmt.Fprintf(out, "  with %d address(es) resolved: %d\n", n, count)
	}
	fmt.Fprintf(out, "subject in neither snapshot (FAIL): %d\n", report.Missing)
	fmt.Fprintf(out, "addresses unreadable (FAIL): %d\n", report.Unreadable)
	if replay.Apply {
		fmt.Fprintf(out, "done (200): %d\n", report.Done)
		fmt.Fprintf(out, "in progress (202, run again later): %d\n", report.RetryLater)
		fmt.Fprintf(out, "failed at the service (FAIL): %d\n", report.SendFailed)
	}
	fmt.Fprintf(out, "requests not completed yet whose %s ran at or after the restore (run again once they complete): %d\n",
		replay.Service.Step, report.Open)
	switch {
	case report.Failed() > 0:
		fmt.Fprintf(out, "FAIL: %d request(s) not replayed\n", report.Failed())
		return 1
	case report.RetryLater > 0 || report.Open > 0:
		fmt.Fprintln(out, "not finished: run the replay again later")
		return replayExitRetryLater
	}
	return 0
}

// keycloakTokenURL is the token endpoint of core's realm, derived from
// KEYCLOAK_URL and KEYCLOAK_REALM the way the server derives it.
func keycloakTokenURL(getenv func(string) string) (string, bool) {
	base := strings.TrimRight(strings.TrimSpace(getenv("KEYCLOAK_URL")), "/")
	realm := strings.TrimSpace(getenv("KEYCLOAK_REALM"))
	if parts := strings.SplitN(base, "/realms/", 2); len(parts) == 2 {
		base = parts[0]
		if realm == "" {
			realm = parts[1]
		}
	}
	if base == "" || realm == "" {
		return "", false
	}
	return base + "/realms/" + realm + "/protocol/openid-connect/token", true
}
