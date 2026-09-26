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
	}, member, gone
}

func TestMediaLegacyReportPrintsOneRowPerOrphan(t *testing.T) {
	report, member, gone := legacyReportFixture()
	teams := func(_ context.Context, id uuid.UUID) ([]string, error) {
		if id == member {
			return []string{"/WEBLAB", "/AGC"}, nil
		}
		return nil, errors.New("keycloak down")
	}
	var out bytes.Buffer

	code := mediaLegacyReportCommand(context.Background(), &out, reportTime,
		func(context.Context) (media.LegacyReport, error) { return report, nil }, teams)

	if code != 0 {
		t.Fatalf("exit %d: %s", code, out.String())
	}
	for _, line := range []string{
		"# orphans: 2 (4.0 MiB)",
		"# legacy Media core attaches: 4",
		"# core links without a Media attachment: 0",
		"# uploader teams unread: 1",
		"id\tcreated_at\ttype\tsize\tname\tuploader_teams\tstatus\texpires_at\tkey",
		"00000000-0000-0000-0000-000000000001\t2025-03-01T09:00:00Z\tapplication/pdf\t3145728\tcv final v2.pdf\t/AGC,/WEBLAB\tpending\t-\tmedia/cv.pdf",
		"00000000-0000-0000-0000-000000000002\t2025-04-01T09:00:00Z\timage/png\t1048576\tafis.png\t?\tdetached\t-\tmedia/afis.png",
	} {
		if !strings.Contains(out.String(), line+"\n") {
			t.Fatalf("report lacks %q:\n%s", line, out.String())
		}
	}
	if strings.Contains(out.String(), gone.String()) || strings.Contains(out.String(), member.String()) {
		t.Fatal("the report names an uploader")
	}
}

// The switch reads the reviewed report back: the first column of each row.
// Without -apply it only counts.
func TestMediaLegacyExpireReadsTheReviewedReportAndIsADryRunUnlessApplied(t *testing.T) {
	report, _, _ := legacyReportFixture()
	var reviewed bytes.Buffer
	if code := mediaLegacyReportCommand(context.Background(), &reviewed, reportTime,
		func(context.Context) (media.LegacyReport, error) { return report, nil }, nil); code != 0 {
		t.Fatal(reviewed.String())
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

func TestMediaLegacyCommandsNeedTheDatabase(t *testing.T) {
	env := map[string]string{"KEYCLOAK_URL": "https://e.example.test", "KEYCLOAK_CLIENT_SECRET": "s3cret"}
	getenv := func(k string) string { return env[k] }
	var out bytes.Buffer
	if code := runMediaLegacyReport(nil, getenv, &out); code != 2 || !strings.Contains(out.String(), "DATABASE_URL") {
		t.Fatalf("report exit %d: %s", code, out.String())
	}
	if code := runMediaLegacyExpire(nil, getenv, strings.NewReader(""), &out); code != 2 || !strings.Contains(out.String(), "DATABASE_URL") {
		t.Fatalf("expire exit %d: %s", code, out.String())
	}
	if strings.Contains(out.String(), "s3cret") || strings.Contains(out.String(), "e.example.test") {
		t.Fatal("the output echoes configuration values")
	}
}
