package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// The commands as Yusuf runs them: the report from core's database, then
// the reviewed report fed back to the switch, which only counts without
// -apply.
func TestPostgresMediaLegacyCommandsReadCoresDatabase(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	uploader := uuid.New()
	if _, _, err := user.NewService(user.NewPostgresStore(pool)).Ensure(ctx, uploader, user.Profile{
		Email: "legacy-owner@example.com", FirstName: "Legacy", LastName: "Owner", Username: "legacy-owner",
	}); err != nil {
		t.Fatal(err)
	}
	orphan, err := media.NewPostgresStore(pool).Create(ctx, media.Media{Name: "basvuru.pdf", Type: "application/pdf", Kind: media.KindFile, Key: "files/basvuru.pdf", Size: 42, UploadedBy: uploader})
	if err != nil {
		t.Fatal(err)
	}
	getenv := func(name string) string {
		if name == "DATABASE_URL" {
			return pool.Config().ConnString()
		}
		return ""
	}

	var report bytes.Buffer
	if code := runMediaLegacyReport(nil, getenv, &report); code != 0 {
		t.Fatalf("report exit %d: %s", code, report.String())
	}
	for _, line := range []string{"# orphans: 1 (0.0 MiB)", "# uploader teams: not read", orphan.ID.String() + "\t", "\tbasvuru.pdf\t-\tpending\t-\tfiles/basvuru.pdf\n"} {
		if !strings.Contains(report.String(), line) {
			t.Fatalf("report lacks %q:\n%s", line, report.String())
		}
	}

	var out bytes.Buffer
	if code := runMediaLegacyExpire(nil, getenv, strings.NewReader(report.String()), &out); code != 0 {
		t.Fatalf("expire exit %d: %s", code, out.String())
	}
	if !strings.Contains(out.String(), "would get 30 days: 1") {
		t.Fatalf("expire output:\n%s", out.String())
	}
	if got, err := media.NewPostgresStore(pool).Get(ctx, orphan.ID); err != nil || got.ExpiresAt != nil {
		t.Fatalf("a dry run set expiry %v (err %v)", got.ExpiresAt, err)
	}
}
