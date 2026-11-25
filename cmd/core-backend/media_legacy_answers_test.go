package main

import (
	"bytes"
	"context"
	"errors"
	"regexp"
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
	mode   media.LegacyAnswerMode
	limit  int
}

func fakeLegacyAnswerMove(call *legacyAnswerCall, report media.LegacyAnswerReport, failures []error, err error) legacyAnswerMove {
	return func(_ context.Context, pairs []media.LegacyAnswerPair, mode media.LegacyAnswerMode, limit int, onError func(error)) (media.LegacyAnswerReport, error) {
		*call = legacyAnswerCall{called: true, pairs: len(pairs), mode: mode, limit: limit}
		for _, failure := range failures {
			onError(failure)
		}
		return report, err
	}
}

// fakeLock is the advisory lock: free unless held, counting its takes and
// releases.
type fakeLock struct {
	held            bool
	taken, released int
}

func (l *fakeLock) take(context.Context) (func(), bool, error) {
	if l.held {
		return nil, false, nil
	}
	l.taken++
	return func() { l.released++ }, true, nil
}

func runLegacyAnswersCommand(args []string, input string, settings legacyAnswerSettings, lock *fakeLock, move legacyAnswerMove) (int, string) {
	var out bytes.Buffer
	code := mediaLegacyAnswersCommand(context.Background(), args, strings.NewReader(input), &out, settings, lock.take, move)
	return code, out.String()
}

// Without a flag the command counts, whatever is configured, and takes no
// lock; its last line is the SUMMARY the hub's wizard reads, keys of
// letters only. Nothing names a person, a file or an address.
func TestMediaLegacyAnswersIsADryRunUnlessApplied(t *testing.T) {
	var call legacyAnswerCall
	lock := &fakeLock{}
	report := media.LegacyAnswerReport{Pairs: 2, Media: 2, ToMove: 1, ToMoveBytes: 3 << 20, OutsideTypeRule: 1, NotLegacy: 1, PublicKept: 1}

	code, out := runLegacyAnswersCommand(nil, legacyAnswerExport(), legacyAnswerSettings{}, lock, fakeLegacyAnswerMove(&call, report, nil, nil))

	if code != 0 || !call.called || call.mode != media.LegacyAnswersCount || call.pairs != 2 || lock.taken != 0 {
		t.Fatalf("exit %d, call %+v, lock %+v: %s", code, call, lock, out)
	}
	for _, line := range []string{
		"private media: off, malware scan: off, CDN purge: off",
		"to move: 1 (3.0 MiB)",
		"  outside the answer_file type rule (moved all the same): 1",
		"moved, rejected for another reason, public object kept: 1",
		"left alone, not a legacy public Media: 1",
		"SUMMARY mode=count pairs=2 media=2 toMove=1 toMoveBytes=3145728 outsideSize=0 outsideType=1 awaitingScan=0 readyToFinalize=0 publicKept=1 done=0 notLegacy=1 notFound=0 archived=0 shared=0 video=0 tooLarge=0 objectMissing=0 movedNow=0 finalizedNow=0 changed=0 remaining=0 failed=0",
	} {
		if !strings.Contains(out, line+"\n") {
			t.Errorf("missing %q in:\n%s", line, out)
		}
	}
	summary := out[strings.Index(out, "SUMMARY "):]
	for _, field := range strings.Fields(strings.TrimSpace(summary))[1:] {
		if !regexp.MustCompile(`^[A-Za-z]+=[A-Za-z0-9-]+$`).MatchString(field) {
			t.Errorf("SUMMARY field %q: keys are letters only", field)
		}
	}
}

// -apply needs private media and the scan; -finalize the CDN purge. Missing
// one, nothing runs. Each takes the lock and releases it.
func TestMediaLegacyAnswersWritingRunsNeedTheirServicesAndTheLock(t *testing.T) {
	for name, tc := range map[string]struct {
		args     []string
		settings legacyAnswerSettings
	}{
		"apply without private media": {[]string{"-apply"}, legacyAnswerSettings{Scan: true, CDNPurge: true}},
		"apply without the scanner":   {[]string{"-apply"}, legacyAnswerSettings{Private: true, CDNPurge: true}},
		"finalize without CDN purge":  {[]string{"-finalize"}, legacyAnswerSettings{Private: true, Scan: true}},
	} {
		var call legacyAnswerCall
		lock := &fakeLock{}
		code, out := runLegacyAnswersCommand(tc.args, legacyAnswerExport(), tc.settings, lock, fakeLegacyAnswerMove(&call, media.LegacyAnswerReport{}, nil, nil))
		if code != 2 || call.called || lock.taken != 0 || !strings.Contains(out, "nothing was changed") {
			t.Errorf("%s: exit %d, called %v: %s", name, code, call.called, out)
		}
	}

	for args, want := range map[string]media.LegacyAnswerMode{"-apply": media.LegacyAnswersMove, "-finalize": media.LegacyAnswersFinalize} {
		var call legacyAnswerCall
		lock := &fakeLock{}
		report := media.LegacyAnswerReport{MovedNow: 1, FinalizedNow: 1, Remaining: 1}
		code, out := runLegacyAnswersCommand([]string{args}, legacyAnswerExport(), allLegacyAnswerSettings, lock, fakeLegacyAnswerMove(&call, report, nil, nil))
		if code != 0 || call.mode != want || call.limit != legacyAnswersDefaultLimit || lock.taken != 1 || lock.released != 1 {
			t.Fatalf("%s: exit %d, call %+v, lock %+v: %s", args, code, call, lock, out)
		}
		for _, line := range []string{"moved now: 1", "public objects deleted now (CDN purge queued): 1", "left for the next run: 1"} {
			if !strings.Contains(out, line+"\n") {
				t.Errorf("%s: missing %q in:\n%s", args, line, out)
			}
		}
	}

	var call legacyAnswerCall
	code, _ := runLegacyAnswersCommand([]string{"-apply", "-limit", "5"}, legacyAnswerExport(), allLegacyAnswerSettings, &fakeLock{}, fakeLegacyAnswerMove(&call, media.LegacyAnswerReport{}, nil, nil))
	if code != 0 || call.limit != 5 {
		t.Fatalf("-limit 5: exit %d, call %+v", code, call)
	}
}

// A second writing run while another holds the lock changes nothing and
// exits 2.
func TestMediaLegacyAnswersRefusesWhileAnotherRunHoldsTheLock(t *testing.T) {
	var call legacyAnswerCall
	code, out := runLegacyAnswersCommand([]string{"-apply"}, legacyAnswerExport(), allLegacyAnswerSettings, &fakeLock{held: true},
		fakeLegacyAnswerMove(&call, media.LegacyAnswerReport{}, nil, nil))
	if code != 2 || call.called || !strings.Contains(out, "another writing run holds the lock") {
		t.Fatalf("exit %d, called %v: %s", code, call.called, out)
	}
}

// A Media that failed (named by its id only) or a run cut short exits 1,
// after the counts.
func TestMediaLegacyAnswersFailsOnFailuresAndInterruptions(t *testing.T) {
	var call legacyAnswerCall
	failed := errors.New("media 00000000-0000-0000-0000-000000000009: storage is down")
	code, out := runLegacyAnswersCommand([]string{"-apply"}, legacyAnswerExport(), allLegacyAnswerSettings, &fakeLock{},
		fakeLegacyAnswerMove(&call, media.LegacyAnswerReport{Failed: 1}, []error{failed}, nil))
	if code != 1 || !strings.Contains(out, failed.Error()) || !strings.Contains(out, "failed=1") {
		t.Fatalf("failure: exit %d: %s", code, out)
	}

	code, out = runLegacyAnswersCommand([]string{"-apply"}, legacyAnswerExport(), allLegacyAnswerSettings, &fakeLock{},
		fakeLegacyAnswerMove(&call, media.LegacyAnswerReport{MovedNow: 3, Remaining: 2}, nil, context.DeadlineExceeded))
	if code != 1 || !strings.Contains(out, "moved now: 3") || !strings.Contains(out, "stopped before the end") {
		t.Fatalf("interrupted: exit %d: %s", code, out)
	}
}

// Input it cannot read, no input, a bad limit, both modes or a stray
// argument is a usage error, and the move is not called.
func TestMediaLegacyAnswersRefusesWhatItCannotRead(t *testing.T) {
	for name, tc := range map[string]struct {
		args  []string
		input string
	}{
		"empty input":        {nil, "media_id\tresponse_id\n"},
		"bad line":           {nil, "x\ty\n"},
		"limit zero":         {[]string{"-apply", "-limit", "0"}, legacyAnswerExport()},
		"limit too high":     {[]string{"-apply", "-limit", "201"}, legacyAnswerExport()},
		"apply and finalize": {[]string{"-apply", "-finalize"}, legacyAnswerExport()},
		"stray argument":     {[]string{"answers.tsv"}, legacyAnswerExport()},
		"unknown flag":       {[]string{"-force"}, legacyAnswerExport()},
	} {
		var call legacyAnswerCall
		code, out := runLegacyAnswersCommand(tc.args, tc.input, allLegacyAnswerSettings, &fakeLock{}, fakeLegacyAnswerMove(&call, media.LegacyAnswerReport{}, nil, nil))
		if code != 2 || call.called {
			t.Errorf("%s: exit %d, called %v: %s", name, code, call.called, out)
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
