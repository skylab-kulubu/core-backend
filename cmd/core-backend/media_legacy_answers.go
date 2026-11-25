package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/transit"
)

// mediaLegacyAnswersCommandName moves legacy Skyforms Answer files into
// private storage instead of running the server (docs/media-lifecycle.md,
// "Legacy Answer files"):
//
//	core-backend media-legacy-answers [-apply | -finalize] [-limit N] < answers.tsv
//
// Standard input is Skyforms' read-only export, one "<Media id>\t<response
// id>" per line. Without a flag it only counts. -apply moves at most -limit
// Media (25 by default) into private storage, deleting no public object;
// -finalize deletes the public object of moved Media whose scan has ended
// clean or infected. Each says how many remain: run it again until none do.
// A writing run holds an advisory lock: a second one exits 2. It writes
// counts to standard error only: no person, file name or address.
const mediaLegacyAnswersCommandName = "media-legacy-answers"

// legacyAnswersDefaultLimit is how many Media one writing run works on by
// default; legacyAnswersMaxLimit bounds -limit, so a run ends well within the
// command's ten minutes.
const (
	legacyAnswersDefaultLimit = 25
	legacyAnswersMaxLimit     = 200
)

// legacyAnswerSettings is which of the move's services this core has
// configured. -apply needs the private bucket and OpenBao and the malware
// scanner (the running core's scan worker scans what the move stores);
// -finalize needs the CDN purge (the public objects it deletes leave the CDN
// cache).
type legacyAnswerSettings struct {
	Private, Scan, CDNPurge bool
}

func onOffWord(on bool) string {
	if on {
		return "on"
	}
	return "off"
}

type legacyAnswerMove func(ctx context.Context, pairs []media.LegacyAnswerPair, mode media.LegacyAnswerMode, limit int, onError func(error)) (media.LegacyAnswerReport, error)

// runMediaLegacyAnswers wires the move to core's database, the public
// bucket (its deletes queued for the CDN purge, which the running core's
// purge worker sends), the private bucket and OpenBao.
func runMediaLegacyAnswers(args []string, getenv func(string) string, in io.Reader, errOut io.Writer) int {
	fail := func(code int, format string, a ...any) int {
		fmt.Fprintf(errOut, "%s: %s\n", mediaLegacyAnswersCommandName, fmt.Sprintf(format, a...))
		return code
	}
	if strings.TrimSpace(getenv("DATABASE_URL")) == "" {
		return fail(2, "needs DATABASE_URL")
	}
	catalogue, err := media.LoadCatalogue()
	if err != nil {
		return fail(2, "%v", err)
	}
	blobs, cdnBase, err := media.BlobAndCDN(getenv)
	if err != nil {
		return fail(2, "%v", err)
	}
	r2, ok := blobs.(*media.R2)
	if !ok {
		return fail(2, "core has no R2 (R2_ENDPOINT, R2_BUCKET, R2_ACCESS_KEY, R2_SECRET_KEY)")
	}
	privateConfig, err := media.PrivateConfigFromEnv(getenv)
	if err != nil {
		return fail(2, "%v", err)
	}
	scanConfig, err := media.ScanConfigFromEnv(getenv)
	if err != nil {
		return fail(2, "%v", err)
	}
	cdnConfig, err := media.CDNPurgeConfigFromEnv(getenv)
	if err != nil {
		return fail(2, "%v", err)
	}
	settings := legacyAnswerSettings{Private: privateConfig.Enabled, Scan: scanConfig.Enabled(), CDNPurge: cdnConfig.Enabled()}

	ctx, stop := commandContext()
	defer stop()
	pool, err := pgxpool.New(ctx, getenv("DATABASE_URL"))
	if err != nil {
		return fail(1, "database: cannot open the connection pool")
	}
	defer pool.Close()
	store := media.NewPostgresStore(pool)
	if settings.CDNPurge {
		purger, err := media.NewCDNPurger(media.CDNPurgerConfig{
			Base: cdnBase, Queue: store, Client: media.NewCloudflarePurge(cdnConfig),
		})
		if err != nil {
			return fail(2, "%v", err)
		}
		// Only queued: the running core's purge worker sends them.
		r2.PurgeCDNOnChange(purger)
	}
	storage := media.LegacyAnswerStorage{Public: r2}
	if settings.Private {
		storage.Private = media.NewPrivateStorage(media.NewR2(privateConfig.Bucket), transit.New(privateConfig.Transit))
	}
	return mediaLegacyAnswersCommand(ctx, args, in, errOut, settings, store.LockLegacyAnswers,
		func(ctx context.Context, pairs []media.LegacyAnswerPair, mode media.LegacyAnswerMode, limit int, onError func(error)) (media.LegacyAnswerReport, error) {
			return media.MoveLegacyAnswers(ctx, store, storage, catalogue, pairs, mode, limit, time.Now, onError)
		})
}

// mediaLegacyAnswersCommand reads the flags and the export, refuses a
// writing run unless the services it needs are configured and it holds the
// move's lock, runs the move and writes its counts to errOut, ending with
// one SUMMARY line of key=value counts (keys of letters only) for the hub's
// wizard. It exits 0 when nothing failed, 1 when a Media failed or the run
// was cut short, and 2 on a usage or configuration error or a lock another
// run holds.
func mediaLegacyAnswersCommand(ctx context.Context, args []string, in io.Reader, errOut io.Writer, settings legacyAnswerSettings,
	lock func(context.Context) (func(), bool, error), move legacyAnswerMove) int {
	flags := flag.NewFlagSet(mediaLegacyAnswersCommandName, flag.ContinueOnError)
	flags.SetOutput(errOut)
	apply := flags.Bool("apply", false, "move the files into private storage; deletes no public object")
	finalize := flags.Bool("finalize", false, "delete the public object of moved files whose scan ended clean or infected")
	limit := flags.Int("limit", legacyAnswersDefaultLimit, fmt.Sprintf("with -apply or -finalize, how many Media this run works on (1 to %d)", legacyAnswersMaxLimit))
	if err := flags.Parse(args); err != nil {
		return 2
	}
	usage := func(format string, a ...any) int {
		fmt.Fprintf(errOut, "%s: %s\n", mediaLegacyAnswersCommandName, fmt.Sprintf(format, a...))
		return 2
	}
	if flags.NArg() > 0 {
		return usage("unexpected argument; the export comes on standard input")
	}
	if *apply && *finalize {
		return usage("-apply and -finalize are separate runs")
	}
	if *limit < 1 || *limit > legacyAnswersMaxLimit {
		return usage("-limit must be between 1 and %d", legacyAnswersMaxLimit)
	}
	pairs, err := media.ReadLegacyAnswerPairs(in)
	if err != nil {
		return usage("%v", err)
	}
	if len(pairs) == 0 {
		return usage("no Media id and response id on standard input: feed it Skyforms' export")
	}
	settingsLine := fmt.Sprintf("private media: %s, malware scan: %s, CDN purge: %s", onOffWord(settings.Private), onOffWord(settings.Scan), onOffWord(settings.CDNPurge))
	mode, modeName := media.LegacyAnswersCount, "count"
	switch {
	case *apply:
		mode, modeName = media.LegacyAnswersMove, "apply"
		if !settings.Private || !settings.Scan {
			return usage("-apply needs private media and the malware scan on (%s); nothing was changed", settingsLine)
		}
	case *finalize:
		mode, modeName = media.LegacyAnswersFinalize, "finalize"
		if !settings.CDNPurge {
			return usage("-finalize needs the CDN purge on (%s); nothing was changed", settingsLine)
		}
	}
	if mode != media.LegacyAnswersCount {
		release, locked, err := lock(ctx)
		if err != nil {
			fmt.Fprintf(errOut, "%s: the lock: %v\n", mediaLegacyAnswersCommandName, err)
			return 1
		}
		if !locked {
			return usage("another writing run holds the lock; nothing was changed")
		}
		defer release()
	}

	report, err := move(ctx, pairs, mode, *limit, func(err error) { fmt.Fprintln(errOut, err) })

	switch mode {
	case media.LegacyAnswersMove:
		fmt.Fprintf(errOut, "Legacy Answer files, move (-limit %d)\n", *limit)
	case media.LegacyAnswersFinalize:
		fmt.Fprintf(errOut, "Legacy Answer files, finalize (-limit %d)\n", *limit)
	default:
		fmt.Fprintln(errOut, "Legacy Answer files, dry run (-apply moves, -finalize deletes public objects)")
	}
	fmt.Fprintln(errOut, settingsLine)
	fmt.Fprintf(errOut, "pairs read: %d (Media: %d)\n", report.Pairs, report.Media)
	fmt.Fprintf(errOut, "to move: %d (%.1f MiB)\n", report.ToMove, float64(report.ToMoveBytes)/(1<<20))
	fmt.Fprintf(errOut, "  outside the answer_file size rule (moved all the same): %d\n", report.OutsideSizeRule)
	fmt.Fprintf(errOut, "  outside the answer_file type rule (moved all the same): %d\n", report.OutsideTypeRule)
	fmt.Fprintf(errOut, "moved, waiting for their scan: %d\n", report.AwaitingScan)
	fmt.Fprintf(errOut, "moved, scanned clean or infected, public object to delete: %d\n", report.ReadyToFinalize)
	fmt.Fprintf(errOut, "moved, rejected for another reason, public object kept: %d\n", report.PublicKept)
	fmt.Fprintf(errOut, "done (public object deleted): %d\n", report.Moved)
	fmt.Fprintf(errOut, "left alone, not a legacy public Media: %d\n", report.NotLegacy)
	fmt.Fprintf(errOut, "left alone, not in core: %d\n", report.NotFound)
	fmt.Fprintf(errOut, "left alone, archived or being purged: %d\n", report.Archived)
	fmt.Fprintf(errOut, "left alone, used by core or another product too: %d\n", report.Shared)
	fmt.Fprintf(errOut, "left alone, a video: %d\n", report.Video)
	fmt.Fprintf(errOut, "left alone, larger than %d MiB: %d\n", media.LegacyAnswerMaxBytes>>20, report.TooLarge)
	fmt.Fprintf(errOut, "left alone, public object missing: %d\n", report.ObjectMissing)
	if mode != media.LegacyAnswersCount {
		fmt.Fprintf(errOut, "moved now: %d\n", report.MovedNow)
		fmt.Fprintf(errOut, "public objects deleted now (CDN purge queued): %d\n", report.FinalizedNow)
		fmt.Fprintf(errOut, "changed while being moved (left as they were): %d\n", report.Changed)
		fmt.Fprintf(errOut, "left for the next run: %d\n", report.Remaining)
	}
	fmt.Fprintf(errOut, "failed: %d\n", report.Failed)
	fmt.Fprintf(errOut, "SUMMARY mode=%s pairs=%d media=%d toMove=%d toMoveBytes=%d outsideSize=%d outsideType=%d awaitingScan=%d readyToFinalize=%d publicKept=%d done=%d notLegacy=%d notFound=%d archived=%d shared=%d video=%d tooLarge=%d objectMissing=%d movedNow=%d finalizedNow=%d changed=%d remaining=%d failed=%d\n",
		modeName, report.Pairs, report.Media, report.ToMove, report.ToMoveBytes, report.OutsideSizeRule, report.OutsideTypeRule,
		report.AwaitingScan, report.ReadyToFinalize, report.PublicKept, report.Moved, report.NotLegacy, report.NotFound, report.Archived,
		report.Shared, report.Video, report.TooLarge, report.ObjectMissing, report.MovedNow, report.FinalizedNow, report.Changed,
		report.Remaining, report.Failed)
	if err != nil {
		fmt.Fprintf(errOut, "stopped before the end: %v\n", err)
		return 1
	}
	if report.Failed > 0 {
		return 1
	}
	return 0
}
