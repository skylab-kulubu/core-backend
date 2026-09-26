package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/media"
)

var reportTime = time.Date(2026, 9, 26, 10, 0, 0, 0, time.UTC)

func legacyReportFixture() (media.LegacyReport, uuid.UUID, uuid.UUID) {
	member, gone := uuid.New(), uuid.New()
	return media.LegacyReport{
		Orphans: []media.Media{
			{ID: uuid.MustParse("00000000-0000-0000-0000-000000000001"), Name: "cv\tfinal\nv2.pdf", Type: "application/pdf", Size: 3 << 20,
				Key: "media/cv.pdf", UploadedBy: member, Status: media.StatusPending, CreatedAt: time.Date(2025, 3, 1, 9, 0, 0, 0, time.UTC)},
			{ID: uuid.MustParse("00000000-0000-0000-0000-000000000002"), Name: "afis.png", Type: "image/png", Size: 1 << 20,
				Key: "media/afis.png", UploadedBy: gone, Status: media.StatusDetached, CreatedAt: time.Date(2025, 4, 1, 9, 0, 0, 0, time.UTC)},
		},
		CoreLinksWithoutAttachment: 0,
		AttachedByCore:             4,
		DetachExpiryHeld:           7,
	}, member, gone
}

func readFixture(report media.LegacyReport) func(context.Context) (media.LegacyReport, error) {
	return func(context.Context) (media.LegacyReport, error) { return report, nil }
}

// The report goes to standard output as plain tab-separated rows, so it can
// be redirected to a file; the summary goes to standard error.
func TestMediaLegacyReportWritesRowsToStdoutAndTheSummaryToStderr(t *testing.T) {
	report, member, gone := legacyReportFixture()
	groups := func(_ context.Context, id uuid.UUID) ([]string, error) {
		if id == member {
			return []string{"/WEBLAB", "/AGC"}, nil
		}
		return nil, errors.New("keycloak down")
	}
	var rows, summary bytes.Buffer

	code := mediaLegacyReportCommand(context.Background(), &rows, &summary, reportTime, readFixture(report), groups)

	if code != 0 {
		t.Fatalf("exit %d: %s", code, summary.String())
	}
	wantRows := "id\tcreated_at\ttype\tsize\tname\tuploader_groups_now\tstatus\texpires_at\tkey\n" +
		"00000000-0000-0000-0000-000000000001\t2025-03-01T09:00:00Z\tapplication/pdf\t3145728\tcv final v2.pdf\t/AGC,/WEBLAB\tpending\t-\tmedia/cv.pdf\n" +
		"00000000-0000-0000-0000-000000000002\t2025-04-01T09:00:00Z\timage/png\t1048576\tafis.png\t?\tdetached\t-\tmedia/afis.png\n"
	if rows.String() != wantRows {
		t.Fatalf("rows:\n%s\nwant:\n%s", rows.String(), wantRows)
	}
	for _, line := range []string{
		"orphans: 2 (4.0 MiB)",
		"legacy Media core attaches: 4",
		"core links without a Media attachment: 0",
		"Media whose detach expiry is held (released after stage 5, ticket 18): 7",
		"uploader groups unread: 1",
	} {
		if !strings.Contains(summary.String(), line+"\n") {
			t.Fatalf("summary lacks %q:\n%s", line, summary.String())
		}
	}
	if all := rows.String() + summary.String(); strings.Contains(all, gone.String()) || strings.Contains(all, member.String()) {
		t.Fatal("the report names an uploader")
	}
}

func TestMediaLegacyReportFailureGoesToStderr(t *testing.T) {
	var rows, summary bytes.Buffer
	code := mediaLegacyReportCommand(context.Background(), &rows, &summary, reportTime,
		func(context.Context) (media.LegacyReport, error) {
			return media.LegacyReport{}, errors.New("database down")
		}, nil)
	if code != 1 || rows.Len() != 0 || !strings.Contains(summary.String(), "database down") {
		t.Fatalf("exit %d rows %q summary %q", code, rows.String(), summary.String())
	}
}

// The switch reads the reviewed report back: the first column of each row.
// Without -apply it only counts.
func TestMediaLegacyExpireReadsTheReviewedReportAndIsADryRunUnlessApplied(t *testing.T) {
	report, _, _ := legacyReportFixture()
	var reviewed, ignored bytes.Buffer
	if code := mediaLegacyReportCommand(context.Background(), &reviewed, &ignored, reportTime, readFixture(report), nil); code != 0 {
		t.Fatal(ignored.String())
	}
	var gotIDs []uuid.UUID
	var gotApply []bool
	expire := func(_ context.Context, ids []uuid.UUID, apply bool) (media.LegacyExpiryReport, error) {
		gotIDs, gotApply = ids, append(gotApply, apply)
		return media.LegacyExpiryReport{Expiring: 1, NotOrphans: []uuid.UUID{ids[1]}}, nil
	}

	var out bytes.Buffer
	if code := mediaLegacyExpireCommand(context.Background(), nil, strings.NewReader(reviewed.String()), &out, expire); code != 0 {
		t.Fatalf("dry run exit %d: %s", code, out.String())
	}
	if len(gotIDs) != 2 || gotIDs[0] != report.Orphans[0].ID || gotIDs[1] != report.Orphans[1].ID {
		t.Fatalf("ids %v", gotIDs)
	}
	for _, line := range []string{"dry run", "would get 30 days: 1", "not legacy orphans (left alone): 1", "  " + report.Orphans[1].ID.String()} {
		if !strings.Contains(out.String(), line) {
			t.Fatalf("dry run output lacks %q:\n%s", line, out.String())
		}
	}

	out.Reset()
	if code := mediaLegacyExpireCommand(context.Background(), []string{"-apply"}, strings.NewReader(reviewed.String()), &out, expire); code != 0 {
		t.Fatalf("apply exit %d: %s", code, out.String())
	}
	if len(gotApply) != 2 || gotApply[0] || !gotApply[1] || !strings.Contains(out.String(), "given 30 days: 1") {
		t.Fatalf("apply %v output:\n%s", gotApply, out.String())
	}
}

// An interrupted run says how far it got before it fails.
func TestMediaLegacyExpireReportsWhatItAppliedBeforeAnInterruption(t *testing.T) {
	expire := func(context.Context, []uuid.UUID, bool) (media.LegacyExpiryReport, error) {
		return media.LegacyExpiryReport{Expiring: 3}, context.Canceled
	}
	var out bytes.Buffer
	code := mediaLegacyExpireCommand(context.Background(), []string{"-apply"}, strings.NewReader("00000000-0000-0000-0000-000000000001\n"), &out, expire)
	if code != 1 || !strings.Contains(out.String(), "given 30 days: 3") || !strings.Contains(out.String(), "context canceled") {
		t.Fatalf("exit %d:\n%s", code, out.String())
	}
}

func TestMediaLegacyExpireRefusesInputItCannotRead(t *testing.T) {
	called := false
	expire := func(context.Context, []uuid.UUID, bool) (media.LegacyExpiryReport, error) {
		called = true
		return media.LegacyExpiryReport{}, nil
	}
	for name, input := range map[string]string{
		"no ids":       "# nothing reviewed\n\n",
		"not an id":    "00000000-0000-0000-0000-000000000001\tok\nnot-an-id\tafis.png\n",
		"unknown flag": "",
	} {
		args := []string{"-apply"}
		if name == "unknown flag" {
			args = []string{"-force"}
		}
		var out bytes.Buffer
		if code := mediaLegacyExpireCommand(context.Background(), args, strings.NewReader(input), &out, expire); code != 2 {
			t.Errorf("%s: exit %d: %s", name, code, out.String())
		}
		if name == "not an id" && !strings.Contains(out.String(), "line 2") {
			t.Errorf("not an id: output does not name the line: %s", out.String())
		}
	}
	if called {
		t.Fatal("expired Media from input it could not read")
	}
}

// The release is a dry run unless applied, and prints counts only.
func TestMediaLegacyReleaseHoldIsADryRunUnlessApplied(t *testing.T) {
	var gotApply []bool
	release := func(_ context.Context, apply bool) (media.HoldReleaseReport, error) {
		gotApply = append(gotApply, apply)
		if !apply {
			return media.HoldReleaseReport{Held: 7, HeldDetached: 2}, nil
		}
		return media.HoldReleaseReport{Held: 7, HeldDetached: 2, Released: 7, WindowsStarted: 2}, nil
	}

	var out bytes.Buffer
	if code := mediaLegacyReleaseHoldCommand(context.Background(), nil, &out, release); code != 0 {
		t.Fatalf("dry run exit %d: %s", code, out.String())
	}
	for _, line := range []string{"dry run", "held: 7", "held and used by no record: 2"} {
		if !strings.Contains(out.String(), line) {
			t.Fatalf("dry run output lacks %q:\n%s", line, out.String())
		}
	}

	out.Reset()
	if code := mediaLegacyReleaseHoldCommand(context.Background(), []string{"-apply"}, &out, release); code != 0 {
		t.Fatalf("apply exit %d: %s", code, out.String())
	}
	for _, line := range []string{"released: 7", "30 days started: 2", "failed: 0"} {
		if !strings.Contains(out.String(), line+"\n") {
			t.Fatalf("apply output lacks %q:\n%s", line, out.String())
		}
	}
	if len(gotApply) != 2 || gotApply[0] || !gotApply[1] {
		t.Fatalf("apply flags %v", gotApply)
	}
	if code := mediaLegacyReleaseHoldCommand(context.Background(), []string{"-force"}, &out, release); code != 2 {
		t.Fatalf("unknown flag exit %d", code)
	}
}

func TestMediaLegacyReleaseHoldFailsOnFailuresAndInterruptions(t *testing.T) {
	for name, result := range map[string]struct {
		report media.HoldReleaseReport
		err    error
		line   string
	}{
		"a Media failed": {report: media.HoldReleaseReport{Held: 3, Released: 2, Failed: 1}, line: "failed: 1"},
		"interrupted":    {report: media.HoldReleaseReport{Held: 3, Released: 2}, err: context.Canceled, line: "released: 2"},
	} {
		var out bytes.Buffer
		code := mediaLegacyReleaseHoldCommand(context.Background(), []string{"-apply"}, &out,
			func(context.Context, bool) (media.HoldReleaseReport, error) { return result.report, result.err })
		if code != 1 || !strings.Contains(out.String(), result.line) {
			t.Errorf("%s: exit %d:\n%s", name, code, out.String())
		}
	}
}

func TestMediaLegacyCommandsNeedTheDatabase(t *testing.T) {
	env := map[string]string{"KEYCLOAK_URL": "https://e.example.test", "KEYCLOAK_CLIENT_SECRET": "s3cret"}
	getenv := func(k string) string { return env[k] }
	var rows, out bytes.Buffer
	if code := runMediaLegacyReport(nil, getenv, &rows, &out); code != 2 || !strings.Contains(out.String(), "DATABASE_URL") || rows.Len() != 0 {
		t.Fatalf("report exit %d: %s", code, out.String())
	}
	if code := runMediaLegacyExpire(nil, getenv, strings.NewReader(""), &out); code != 2 || !strings.Contains(out.String(), "DATABASE_URL") {
		t.Fatalf("expire exit %d: %s", code, out.String())
	}
	if code := runMediaLegacyReleaseHold(nil, getenv, &out); code != 2 || !strings.Contains(out.String(), "DATABASE_URL") {
		t.Fatalf("release exit %d: %s", code, out.String())
	}
	if strings.Contains(out.String(), "s3cret") || strings.Contains(out.String(), "e.example.test") {
		t.Fatal("the output echoes configuration values")
	}
}
