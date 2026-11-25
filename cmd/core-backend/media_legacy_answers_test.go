package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/media"
)

var allLegacyAnswerSettings = legacyAnswerSettings{Private: true, Scan: true, CDNPurge: true}

func legacyAnswerExport() string {
	return "media_id\tresponse_id\n" + uuid.NewString() + "\t" + uuid.NewString() + "\n" + uuid.NewString() + "\t" + uuid.NewString() + "\n"
}

type legacyAnswerCall struct {
	called bool
	pairs  int
	apply  bool
	limit  int
}

func legacyAnswerMove(call *legacyAnswerCall, report media.LegacyAnswerReport, failures []error, err error) func(context.Context, []media.LegacyAnswerPair, bool, int, func(error)) (media.LegacyAnswerReport, error) {
	return func(_ context.Context, pairs []media.LegacyAnswerPair, apply bool, limit int, onError func(error)) (media.LegacyAnswerReport, error) {
		*call = legacyAnswerCall{called: true, pairs: len(pairs), apply: apply, limit: limit}
		for _, failure := range failures {
			onError(failure)
		}
		return report, err
	}
}

// Without -apply the command counts, whatever is configured; its last line
// is the SUMMARY the hub's wizard reads. Nothing names a person, a file or
// an address.
func TestMediaLegacyAnswersIsADryRunUnlessApplied(t *testing.T) {
	var call legacyAnswerCall
	var out bytes.Buffer
	report := media.LegacyAnswerReport{Pairs: 2, Media: 2, ToMove: 1, ToMoveBytes: 3 << 20, OutsideTypeRule: 1, NotLegacy: 1}

	code := mediaLegacyAnswersCommand(context.Background(), nil, strings.NewReader(legacyAnswerExport()), &out, legacyAnswerSettings{},
		legacyAnswerMove(&call, report, nil, nil))

	if code != 0 || !call.called || call.apply || call.pairs != 2 {
		t.Fatalf("exit %d, call %+v: %s", code, call, out.String())
	}
	for _, line := range []string{
		"Legacy Answer files, dry run (add -apply to write)",
		"private media: off, malware scan: off, CDN purge: off",
		"to move: 1 (3.0 MiB)",
		"  outside the answer_file type rule (moved all the same): 1",
		"left alone, not a legacy public Media: 1",
		"SUMMARY mode=dry-run pairs=2 media=2 to_move=1 to_move_bytes=3145728 outside_size=0 outside_type=1 moved=0 delete_pending=0 not_legacy=1 not_found=0 archived=0 shared=0 video=0 too_large=0 object_missing=0 moved_now=0 deleted_now=0 changed=0 remaining=0 failed=0",
	} {
		if !strings.Contains(out.String(), line+"\n") {
			t.Errorf("missing %q in:\n%s", line, out.String())
		}
	}
	if strings.Contains(out.String(), "moved now") {
		t.Errorf("a dry run reports apply counts:\n%s", out.String())
	}
}

// -apply needs private media, the scan and the CDN purge: missing any, it
// changes nothing. With them it passes the limit (25 by default) on.
func TestMediaLegacyAnswersApplyNeedsEveryService(t *testing.T) {
	for name, settings := range map[string]legacyAnswerSettings{
		"no private media": {Scan: true, CDNPurge: true},
		"no scanner":       {Private: true, CDNPurge: true},
		"no CDN purge":     {Private: true, Scan: true},
	} {
		var call legacyAnswerCall
		var out bytes.Buffer
		code := mediaLegacyAnswersCommand(context.Background(), []string{"-apply"}, strings.NewReader(legacyAnswerExport()), &out, settings,
			legacyAnswerMove(&call, media.LegacyAnswerReport{}, nil, nil))
		if code != 2 || call.called || !strings.Contains(out.String(), "nothing was changed") {
			t.Errorf("%s: exit %d, called %v: %s", name, code, call.called, out.String())
		}
	}

	var call legacyAnswerCall
	var out bytes.Buffer
	report := media.LegacyAnswerReport{Pairs: 2, Media: 2, ToMove: 2, MovedNow: 1, PublicDeletedNow: 1, Remaining: 1}
	code := mediaLegacyAnswersCommand(context.Background(), []string{"-apply"}, strings.NewReader(legacyAnswerExport()), &out, allLegacyAnswerSettings,
		legacyAnswerMove(&call, report, nil, nil))
	if code != 0 || !call.apply || call.limit != legacyAnswersDefaultLimit {
		t.Fatalf("exit %d, call %+v: %s", code, call, out.String())
	}
	for _, line := range []string{"Legacy Answer files (-limit 25)", "moved now: 1", "public objects deleted now (CDN purge queued): 1", "left for the next run: 1"} {
		if !strings.Contains(out.String(), line+"\n") {
			t.Errorf("missing %q in:\n%s", line, out.String())
		}
	}

	code = mediaLegacyAnswersCommand(context.Background(), []string{"-apply", "-limit", "5"}, strings.NewReader(legacyAnswerExport()), &out, allLegacyAnswerSettings,
		legacyAnswerMove(&call, media.LegacyAnswerReport{}, nil, nil))
	if code != 0 || call.limit != 5 {
		t.Fatalf("-limit 5: exit %d, call %+v", code, call)
	}
}

// A Media that failed (named by its id only) or a run cut short exits 1,
// after the counts.
func TestMediaLegacyAnswersFailsOnFailuresAndInterruptions(t *testing.T) {
	var call legacyAnswerCall
	var out bytes.Buffer
	failed := errors.New("media 00000000-0000-0000-0000-000000000009: storage is down")
	code := mediaLegacyAnswersCommand(context.Background(), []string{"-apply"}, strings.NewReader(legacyAnswerExport()), &out, allLegacyAnswerSettings,
		legacyAnswerMove(&call, media.LegacyAnswerReport{Failed: 1}, []error{failed}, nil))
	if code != 1 || !strings.Contains(out.String(), failed.Error()) || !strings.Contains(out.String(), "failed=1") {
		t.Fatalf("failure: exit %d: %s", code, out.String())
	}

	out.Reset()
	code = mediaLegacyAnswersCommand(context.Background(), []string{"-apply"}, strings.NewReader(legacyAnswerExport()), &out, allLegacyAnswerSettings,
		legacyAnswerMove(&call, media.LegacyAnswerReport{MovedNow: 3, Remaining: 2}, nil, context.DeadlineExceeded))
	if code != 1 || !strings.Contains(out.String(), "moved now: 3") || !strings.Contains(out.String(), "stopped before the end") {
		t.Fatalf("interrupted: exit %d: %s", code, out.String())
	}
}

// Input it cannot read, no input, a bad limit or a stray argument is a usage
// error, and the move is not called.
func TestMediaLegacyAnswersRefusesWhatItCannotRead(t *testing.T) {
	for name, tc := range map[string]struct {
		args  []string
		input string
	}{
		"empty input":    {nil, "media_id\tresponse_id\n"},
		"bad line":       {nil, "x\ty\n"},
		"limit zero":     {[]string{"-apply", "-limit", "0"}, legacyAnswerExport()},
		"limit too high": {[]string{"-apply", "-limit", "201"}, legacyAnswerExport()},
		"stray argument": {[]string{"answers.tsv"}, legacyAnswerExport()},
		"unknown flag":   {[]string{"-force"}, legacyAnswerExport()},
	} {
		var call legacyAnswerCall
		var out bytes.Buffer
		code := mediaLegacyAnswersCommand(context.Background(), tc.args, strings.NewReader(tc.input), &out, allLegacyAnswerSettings,
			legacyAnswerMove(&call, media.LegacyAnswerReport{}, nil, nil))
		if code != 2 || call.called {
			t.Errorf("%s: exit %d, called %v: %s", name, code, call.called, out.String())
		}
	}
}

func TestMediaLegacyAnswersNeedsTheDatabase(t *testing.T) {
	var out bytes.Buffer
	code := runMediaLegacyAnswers(nil, func(string) string { return "" }, strings.NewReader(legacyAnswerExport()), &out)
	if code != 2 || !strings.Contains(out.String(), "DATABASE_URL") {
		t.Fatalf("exit %d: %s", code, out.String())
	}
}
