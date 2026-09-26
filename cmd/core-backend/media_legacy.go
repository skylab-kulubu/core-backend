package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/media"
)

// The legacy Media commands run instead of the server, inside the running
// core container, which already has the environment (docs/media-lifecycle.md,
// Legacy Media). Only the report writes to standard output, and only its
// rows; summaries and errors go to standard error.
//
//	core-backend media-legacy-report > report.tsv
//	core-backend media-legacy-expire [-apply] < reviewed.tsv
//	core-backend media-legacy-release-hold [-apply]
const (
	mediaLegacyReportCommandName      = "media-legacy-report"
	mediaLegacyExpireCommandName      = "media-legacy-expire"
	mediaLegacyReleaseHoldCommandName = "media-legacy-release-hold"
)

// runMediaLegacyReport wires the report to the database and, when core's
// Keycloak service account is configured, to Keycloak for the uploaders'
// current groups (read-only).
func runMediaLegacyReport(args []string, getenv func(string) string, out, errOut io.Writer) int {
	if len(args) > 0 {
		fmt.Fprintf(errOut, "%s takes no arguments\n", mediaLegacyReportCommandName)
		return 2
	}
	ctx, stop := commandContext()
	defer stop()
	store, closeStore, code := legacyMediaStore(ctx, mediaLegacyReportCommandName, getenv, errOut)
	if store == nil {
		return code
	}
	defer closeStore()
	var groups func(context.Context, uuid.UUID) ([]string, error)
	if config, missing := keycloakFromEnv(getenv); len(missing) == 0 {
		keycloak := identity.NewKeycloak(config)
		groups = func(ctx context.Context, id uuid.UUID) ([]string, error) {
			found, err := keycloak.GroupsForUser(ctx, id)
			paths := make([]string, 0, len(found))
			for _, group := range found {
				paths = append(paths, group.Path)
			}
			return paths, err
		}
	}
	return mediaLegacyReportCommand(ctx, out, errOut, time.Now(), func(ctx context.Context) (media.LegacyReport, error) {
		return media.ReportLegacy(ctx, store)
	}, groups)
}

// runMediaLegacyExpire wires the orphan switch to the database.
func runMediaLegacyExpire(args []string, getenv func(string) string, in io.Reader, errOut io.Writer) int {
	ctx, stop := commandContext()
	defer stop()
	store, closeStore, code := legacyMediaStore(ctx, mediaLegacyExpireCommandName, getenv, errOut)
	if store == nil {
		return code
	}
	defer closeStore()
	return mediaLegacyExpireCommand(ctx, args, in, errOut, func(ctx context.Context, ids []uuid.UUID, apply bool) (media.LegacyExpiryReport, error) {
		return media.ExpireLegacyOrphans(ctx, store, ids, time.Now(), apply, func(err error) {
			fmt.Fprintln(errOut, err)
		})
	})
}

// runMediaLegacyReleaseHold wires the hold release to the database.
func runMediaLegacyReleaseHold(args []string, getenv func(string) string, errOut io.Writer) int {
	ctx, stop := commandContext()
	defer stop()
	store, closeStore, code := legacyMediaStore(ctx, mediaLegacyReleaseHoldCommandName, getenv, errOut)
	if store == nil {
		return code
	}
	defer closeStore()
	return mediaLegacyReleaseHoldCommand(ctx, args, errOut, func(ctx context.Context, apply bool) (media.HoldReleaseReport, error) {
		return media.ReleaseDetachExpiryHold(ctx, store, time.Now(), apply, func(err error) {
			fmt.Fprintln(errOut, err)
		})
	})
}

// commandContext ends a command's work on an interrupt or after ten
// minutes; the command then reports what it did so far.
func commandContext() (context.Context, context.CancelFunc) {
	ctx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	return ctx, func() { cancel(); stopSignals() }
}

// legacyMediaStore opens core's database for a legacy Media command. It
// applies no migration: the running server has.
func legacyMediaStore(ctx context.Context, command string, getenv func(string) string, errOut io.Writer) (*media.PostgresStore, func(), int) {
	if strings.TrimSpace(getenv("DATABASE_URL")) == "" {
		fmt.Fprintf(errOut, "%s needs DATABASE_URL\n", command)
		return nil, nil, 2
	}
	pool, err := pgxpool.New(ctx, getenv("DATABASE_URL"))
	if err != nil {
		fmt.Fprintln(errOut, "database: cannot open the connection pool")
		return nil, nil, 1
	}
	return media.NewPostgresStore(pool), pool.Close, 0
}

// mediaLegacyReportCommand writes the legacy report: tab-separated rows, one
// per orphan under a header row, to out, and the summary to errOut. The
// uploader's current Keycloak group paths are read when groups can read them
// (nil: not configured). It prints no person and no configuration value.
func mediaLegacyReportCommand(ctx context.Context, out, errOut io.Writer, now time.Time, read func(context.Context) (media.LegacyReport, error), groups func(context.Context, uuid.UUID) ([]string, error)) int {
	report, err := read(ctx)
	if err != nil {
		fmt.Fprintf(errOut, "core: %v\n", err)
		return 1
	}
	var total int64
	for _, orphan := range report.Orphans {
		total += orphan.Size
	}
	uploaderGroups, unread, cutOff := orphanGroups(ctx, report.Orphans, groups)

	fmt.Fprintln(out, "id\tcreated_at\ttype\tsize\tname\tuploader_groups_now\tstatus\texpires_at\tkey")
	for _, orphan := range report.Orphans {
		expires := "-"
		if orphan.ExpiresAt != nil {
			expires = orphan.ExpiresAt.UTC().Format(time.RFC3339)
		}
		fmt.Fprintf(out, "%s\t%s\t%s\t%d\t%s\t%s\t%s\t%s\t%s\n",
			orphan.ID, orphan.CreatedAt.UTC().Format(time.RFC3339), field(orphan.Type), orphan.Size, field(orphan.Name),
			uploaderGroups[orphan.UploadedBy], orphan.Status, expires, field(orphan.Key))
	}

	fmt.Fprintf(errOut, "SKY LAB core legacy Media report, %s\n", now.UTC().Format(time.RFC3339))
	fmt.Fprintln(errOut, "Orphans are legacy Media nothing in core uses. Skyforms answers and CMS content use Media by address, which core cannot see: an orphan may still be used there. This report changes nothing.")
	fmt.Fprintf(errOut, "orphans: %d (%.1f MiB)\n", len(report.Orphans), float64(total)/(1<<20))
	fmt.Fprintf(errOut, "legacy Media core attaches: %d\n", report.AttachedByCore)
	fmt.Fprintf(errOut, "core links without a Media attachment: %d\n", report.CoreLinksWithoutAttachment)
	fmt.Fprintf(errOut, "Media whose detach expiry is held (released after stage 5, ticket 18): %d\n", report.DetachExpiryHeld)
	if report.HoldReleasedAt == nil {
		fmt.Fprintln(errOut, "detach expiry hold: not released")
	} else {
		fmt.Fprintf(errOut, "detach expiry hold: released %s\n", report.HoldReleasedAt.UTC().Format(time.RFC3339))
	}
	if groups == nil {
		fmt.Fprintln(errOut, "uploader groups: not read (Keycloak is not configured)")
	} else {
		fmt.Fprintf(errOut, "uploader groups unread: %d\n", unread)
	}
	if cutOff > 0 {
		fmt.Fprintf(errOut, "uploader groups cut off: %d (interrupted or out of time; their rows show ?): the report is incomplete\n", cutOff)
		return 1
	}
	return 0
}

// orphanGroups reads each uploader's current group paths once: "-" for
// none (or an account that is gone), "?" when they could not be read. It
// counts the lookups that failed, and those of them an interrupt or the time
// limit cut off.
func orphanGroups(ctx context.Context, orphans []media.Media, groups func(context.Context, uuid.UUID) ([]string, error)) (_ map[uuid.UUID]string, unread, cutOff int) {
	out := map[uuid.UUID]string{}
	for _, orphan := range orphans {
		if _, seen := out[orphan.UploadedBy]; seen {
			continue
		}
		if groups == nil {
			out[orphan.UploadedBy] = "-"
			continue
		}
		paths, err := groups(ctx, orphan.UploadedBy)
		switch {
		case errors.Is(err, identity.ErrNotFound) || (err == nil && len(paths) == 0):
			out[orphan.UploadedBy] = "-"
		case err != nil:
			out[orphan.UploadedBy] = "?"
			unread++
			if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				cutOff++
			}
		default:
			paths = slices.Clone(paths)
			slices.Sort(paths)
			out[orphan.UploadedBy] = field(strings.Join(slices.Compact(paths), ","))
		}
	}
	return out, unread, cutOff
}

// field keeps a value on its row: tabs, line breaks and other control
// characters become spaces.
func field(value string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, value)
}

// mediaLegacyExpireCommand starts the 30-day window of the legacy orphans
// Yusuf reviewed. It reads their ids from in: the first column of each row
// of the report, the header, '#' lines and empty lines skipped, so the
// reviewed report (rows to keep deleted) can be fed back as it is. Without
// -apply it only counts. It writes to errOut only.
func mediaLegacyExpireCommand(ctx context.Context, args []string, in io.Reader, errOut io.Writer, expire func(context.Context, []uuid.UUID, bool) (media.LegacyExpiryReport, error)) int {
	flags := flag.NewFlagSet(mediaLegacyExpireCommandName, flag.ContinueOnError)
	flags.SetOutput(errOut)
	apply := flags.Bool("apply", false, "start the windows; without it the command only counts")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	ids, err := reviewedIDs(in)
	if err != nil {
		fmt.Fprintln(errOut, err)
		return 2
	}
	if len(ids) == 0 {
		fmt.Fprintln(errOut, "no Media ids on standard input: feed it the reviewed report")
		return 2
	}
	report, err := expire(ctx, ids, *apply)
	if *apply {
		fmt.Fprintln(errOut, "Legacy orphan expiry")
		fmt.Fprintf(errOut, "given 30 days: %d\n", report.Expiring)
	} else {
		fmt.Fprintln(errOut, "Legacy orphan expiry, dry run (add -apply to write)")
		fmt.Fprintf(errOut, "would get 30 days: %d\n", report.Expiring)
	}
	fmt.Fprintf(errOut, "Media ids read: %d\n", len(ids))
	fmt.Fprintf(errOut, "already expiring (window kept): %d\n", report.AlreadyExpiring)
	fmt.Fprintf(errOut, "not legacy orphans (left alone): %d\n", len(report.NotOrphans))
	for _, id := range report.NotOrphans {
		fmt.Fprintf(errOut, "  %s\n", id)
	}
	fmt.Fprintf(errOut, "failed: %d\n", report.Failed)
	if err != nil {
		fmt.Fprintf(errOut, "stopped before the end: %v\n", err)
		return 1
	}
	if report.Failed > 0 {
		return 1
	}
	return 0
}

// reviewedIDs reads the first column of the reviewed report, each id once.
func reviewedIDs(in io.Reader) ([]uuid.UUID, error) {
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	seen := map[uuid.UUID]bool{}
	ids := make([]uuid.UUID, 0)
	for line := 1; scanner.Scan(); line++ {
		first, _, _ := strings.Cut(scanner.Text(), "\t")
		first = strings.TrimSpace(first)
		if first == "" || first == "id" || strings.HasPrefix(first, "#") {
			continue
		}
		id, err := uuid.Parse(first)
		if err != nil {
			return nil, fmt.Errorf("line %d: the first column is not a Media id", line)
		}
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("standard input: %w", err)
	}
	return ids, nil
}

// mediaLegacyReleaseHoldCommand ends the hold the legacy purpose backfill
// put on the detach expiry of the Media it gave a purpose (decision K2),
// after stage 5 (ticket 18). Without -apply it only counts. It writes counts
// to errOut only.
func mediaLegacyReleaseHoldCommand(ctx context.Context, args []string, errOut io.Writer, release func(context.Context, bool) (media.HoldReleaseReport, error)) int {
	flags := flag.NewFlagSet(mediaLegacyReleaseHoldCommandName, flag.ContinueOnError)
	flags.SetOutput(errOut)
	apply := flags.Bool("apply", false, "release the hold; without it the command only counts")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	report, err := release(ctx, *apply)
	if *apply {
		fmt.Fprintln(errOut, "Media detach expiry hold release")
	} else {
		fmt.Fprintln(errOut, "Media detach expiry hold release, dry run (add -apply to write)")
	}
	fmt.Fprintf(errOut, "held: %d\n", report.Held)
	fmt.Fprintf(errOut, "held and used by no record: %d (their 30 days start at the release)\n", report.HeldDetached)
	if *apply {
		fmt.Fprintf(errOut, "released: %d\n", report.Released)
		fmt.Fprintf(errOut, "30 days started: %d\n", report.WindowsStarted)
		fmt.Fprintf(errOut, "failed: %d\n", report.Failed)
	}
	if err != nil {
		fmt.Fprintf(errOut, "stopped before the end: %v\n", err)
		return 1
	}
	if report.Failed > 0 {
		return 1
	}
	return 0
}
