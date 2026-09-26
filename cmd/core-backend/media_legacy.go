package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/media"
)

// The legacy Media commands run instead of the server, inside the running
// core container, which already has the environment (docs/media-lifecycle.md,
// Legacy Media):
//
//	core-backend media-legacy-report > report.tsv
//	core-backend media-legacy-expire [-apply] < reviewed.tsv
const (
	mediaLegacyReportCommandName = "media-legacy-report"
	mediaLegacyExpireCommandName = "media-legacy-expire"
)

// runMediaLegacyReport wires the report to the database and, when core's
// Keycloak service account is configured, to Keycloak for the uploaders'
// current groups (read-only).
func runMediaLegacyReport(args []string, getenv func(string) string, out io.Writer) int {
	if len(args) > 0 {
		fmt.Fprintf(out, "%s takes no arguments\n", mediaLegacyReportCommandName)
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	store, closeStore, code := legacyMediaStore(ctx, mediaLegacyReportCommandName, getenv, out)
	if store == nil {
		return code
	}
	defer closeStore()
	var teams func(context.Context, uuid.UUID) ([]string, error)
	if keycloakConfigured(getenv) {
		keycloak := identity.NewKeycloak(identity.KeycloakConfig{
			URL:          getenv("KEYCLOAK_URL"),
			Realm:        getenv("KEYCLOAK_REALM"),
			ClientID:     getenv("KEYCLOAK_CLIENT_ID"),
			ClientSecret: getenv("KEYCLOAK_CLIENT_SECRET"),
		})
		teams = func(ctx context.Context, id uuid.UUID) ([]string, error) {
			groups, err := keycloak.GroupsForUser(ctx, id)
			paths := make([]string, 0, len(groups))
			for _, group := range groups {
				paths = append(paths, group.Path)
			}
			return paths, err
		}
	}
	return mediaLegacyReportCommand(ctx, out, time.Now(), func(ctx context.Context) (media.LegacyReport, error) {
		return media.ReportLegacy(ctx, store)
	}, teams)
}

// runMediaLegacyExpire wires the switch to the database.
func runMediaLegacyExpire(args []string, getenv func(string) string, in io.Reader, out io.Writer) int {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	store, closeStore, code := legacyMediaStore(ctx, mediaLegacyExpireCommandName, getenv, out)
	if store == nil {
		return code
	}
	defer closeStore()
	return mediaLegacyExpireCommand(ctx, args, in, out, func(ctx context.Context, ids []uuid.UUID, apply bool) (media.LegacyExpiryReport, error) {
		return media.ExpireLegacyOrphans(ctx, store, ids, time.Now(), apply, func(err error) {
			fmt.Fprintln(out, err)
		})
	})
}

// legacyMediaStore opens core's database for a legacy Media command. It
// applies no migration: the running server has.
func legacyMediaStore(ctx context.Context, command string, getenv func(string) string, out io.Writer) (*media.PostgresStore, func(), int) {
	if strings.TrimSpace(getenv("DATABASE_URL")) == "" {
		fmt.Fprintf(out, "%s needs DATABASE_URL\n", command)
		return nil, nil, 2
	}
	pool, err := pgxpool.New(ctx, getenv("DATABASE_URL"))
	if err != nil {
		fmt.Fprintln(out, "database: cannot open the connection pool")
		return nil, nil, 1
	}
	return media.NewPostgresStore(pool), pool.Close, 0
}

// keycloakConfigured reports whether core's Keycloak service account is
// configured, as the server requires it.
func keycloakConfigured(getenv func(string) string) bool {
	for _, name := range []string{"KEYCLOAK_URL", "KEYCLOAK_CLIENT_ID", "KEYCLOAK_CLIENT_SECRET"} {
		if strings.TrimSpace(getenv(name)) == "" {
			return false
		}
	}
	return getenv("KEYCLOAK_REALM") != "" || strings.Contains(getenv("KEYCLOAK_URL"), "/realms/")
}

// mediaLegacyReportCommand writes the legacy report as tab-separated rows
// under '#' comment lines: one row per orphan, with the uploader's current
// Keycloak groups when teams can read them (nil: not configured). It prints
// no person and no configuration value.
func mediaLegacyReportCommand(ctx context.Context, out io.Writer, now time.Time, read func(context.Context) (media.LegacyReport, error), teams func(context.Context, uuid.UUID) ([]string, error)) int {
	report, err := read(ctx)
	if err != nil {
		fmt.Fprintf(out, "core: %v\n", err)
		return 1
	}
	var total int64
	for _, orphan := range report.Orphans {
		total += orphan.Size
	}
	uploaderTeams, unread := orphanTeams(ctx, report.Orphans, teams)

	fmt.Fprintf(out, "# SKY LAB core legacy Media report, %s\n", now.UTC().Format(time.RFC3339))
	fmt.Fprintln(out, "# Orphans are legacy Media nothing in core uses. Skyforms answers and CMS content use Media by address, which core cannot see: an orphan may still be used there.")
	fmt.Fprintln(out, "# This report changes nothing. See docs/media-lifecycle.md, Legacy Media.")
	fmt.Fprintf(out, "# orphans: %d (%.1f MiB)\n", len(report.Orphans), float64(total)/(1<<20))
	fmt.Fprintf(out, "# legacy Media core attaches: %d\n", report.AttachedByCore)
	fmt.Fprintf(out, "# core links without a Media attachment: %d\n", report.CoreLinksWithoutAttachment)
	if teams == nil {
		fmt.Fprintln(out, "# uploader teams: not read (Keycloak is not configured)")
	} else {
		fmt.Fprintf(out, "# uploader teams unread: %d\n", unread)
	}
	fmt.Fprintln(out, "id\tcreated_at\ttype\tsize\tname\tuploader_teams\tstatus\texpires_at\tkey")
	for _, orphan := range report.Orphans {
		expires := "-"
		if orphan.ExpiresAt != nil {
			expires = orphan.ExpiresAt.UTC().Format(time.RFC3339)
		}
		fmt.Fprintf(out, "%s\t%s\t%s\t%d\t%s\t%s\t%s\t%s\t%s\n",
			orphan.ID, orphan.CreatedAt.UTC().Format(time.RFC3339), field(orphan.Type), orphan.Size, field(orphan.Name),
			uploaderTeams[orphan.UploadedBy], orphan.Status, expires, field(orphan.Key))
	}
	return 0
}

// orphanTeams reads each uploader's current groups once: their paths, "-"
// for none (or an account that is gone), "?" when they could not be read.
func orphanTeams(ctx context.Context, orphans []media.Media, teams func(context.Context, uuid.UUID) ([]string, error)) (map[uuid.UUID]string, int) {
	out := map[uuid.UUID]string{}
	unread := 0
	for _, orphan := range orphans {
		if _, seen := out[orphan.UploadedBy]; seen {
			continue
		}
		if teams == nil {
			out[orphan.UploadedBy] = "-"
			continue
		}
		paths, err := teams(ctx, orphan.UploadedBy)
		switch {
		case errors.Is(err, identity.ErrNotFound) || (err == nil && len(paths) == 0):
			out[orphan.UploadedBy] = "-"
		case err != nil:
			out[orphan.UploadedBy] = "?"
			unread++
		default:
			paths = slices.Clone(paths)
			slices.Sort(paths)
			out[orphan.UploadedBy] = field(strings.Join(slices.Compact(paths), ","))
		}
	}
	return out, unread
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
// of the report, '#' lines, the header and empty lines skipped, so the
// reviewed report (rows to keep deleted) can be fed back as it is. Without
// -apply it only counts.
func mediaLegacyExpireCommand(ctx context.Context, args []string, in io.Reader, out io.Writer, expire func(context.Context, []uuid.UUID, bool) (media.LegacyExpiryReport, error)) int {
	flags := flag.NewFlagSet(mediaLegacyExpireCommandName, flag.ContinueOnError)
	flags.SetOutput(out)
	apply := flags.Bool("apply", false, "start the windows; without it the command only counts")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	ids, err := reviewedIDs(in)
	if err != nil {
		fmt.Fprintln(out, err)
		return 2
	}
	if len(ids) == 0 {
		fmt.Fprintln(out, "no Media ids on standard input: feed it the reviewed report")
		return 2
	}
	report, err := expire(ctx, ids, *apply)
	if err != nil {
		fmt.Fprintf(out, "core: %v\n", err)
		return 1
	}
	if *apply {
		fmt.Fprintln(out, "Legacy orphan expiry")
		fmt.Fprintf(out, "given 30 days: %d\n", report.Expiring)
	} else {
		fmt.Fprintln(out, "Legacy orphan expiry, dry run (add -apply to write)")
		fmt.Fprintf(out, "would get 30 days: %d\n", report.Expiring)
	}
	fmt.Fprintf(out, "Media ids read: %d\n", len(ids))
	fmt.Fprintf(out, "already expiring (window kept): %d\n", report.AlreadyExpiring)
	fmt.Fprintf(out, "not legacy orphans (left alone): %d\n", len(report.NotOrphans))
	for _, id := range report.NotOrphans {
		fmt.Fprintf(out, "  %s\n", id)
	}
	fmt.Fprintf(out, "failed: %d\n", report.Failed)
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
