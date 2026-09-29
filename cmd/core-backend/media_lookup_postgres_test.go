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

// The operator's lookup, as Yusuf runs it for a migration that does not
// know the uploader: addresses on standard input, one tab-separated row per
// address on standard output (Media id, purpose, status, whether it has an
// uploader), counts on standard error, and nothing that names a person or
// a file.
func TestPostgresMediaLookupCommand(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	uploader := uuid.New()
	if _, _, err := user.NewService(user.NewPostgresStore(pool)).Ensure(ctx, uploader, user.Profile{
		Email: "ada@example.com", FirstName: "Ada", LastName: "Lovelace", Username: "ada",
	}); err != nil {
		t.Fatal(err)
	}
	store := media.NewPostgresStore(pool)
	stored := func(key, purpose string, by uuid.UUID) media.Media {
		t.Helper()
		m, err := store.Create(ctx, media.Media{Name: "cv-ada-lovelace.pdf", Type: "image/png", Kind: media.KindImage,
			Key: key, Size: 10, UploadedBy: by, Purpose: purpose})
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	cmsImage := stored("images/"+uuid.NewString(), "cms_image", uploader)
	legacy := stored("images/"+uuid.NewString(), "legacy", uploader)
	orphan := stored("files/"+uuid.NewString(), "legacy", uuid.Nil)
	cover := stored("images/"+uuid.NewString(), "event_cover", uploader)
	// Media stored with a full address instead of a key: no address finds
	// them, and the summary counts them.
	fullAddress := "https://cdn.yildizskylab.com/images/" + uuid.NewString()
	stored(fullAddress, "legacy", uploader)
	archived := stored("https://cdn.yildizskylab.com/images/"+uuid.NewString(), "legacy", uploader)
	if err := store.Archive(ctx, archived.ID, nil); err != nil {
		t.Fatal(err)
	}

	address := func(m media.Media) string { return media.DefaultPublicBase + "/" + m.Key }
	input := strings.Join([]string{
		address(cmsImage), address(legacy) + "/card.jpg?v=2", "", address(orphan) + "\r",
		address(cover), fullAddress, "https://example.com/logo.png\twith a tab",
	}, "\n") + "\n"
	getenv := func(name string) string {
		if name == "DATABASE_URL" {
			return pool.Config().ConnString()
		}
		return ""
	}
	run := func(args ...string) (string, string) {
		t.Helper()
		var out, summary bytes.Buffer
		if code := runMediaLookup(args, getenv, strings.NewReader(input), &out, &summary); code != 0 {
			t.Fatalf("%v: exit %d: %s", args, code, summary.String())
		}
		for _, private := range []string{"cv-ada", "ada@example.com", "Ada", "Lovelace", uploader.String()} {
			if strings.Contains(out.String()+summary.String(), private) {
				t.Fatalf("%v names %q:\n%s%s", args, private, out.String(), summary.String())
			}
		}
		return out.String(), summary.String()
	}

	rows, summary := run()
	want := "address\tmedia_id\tpurpose\tstatus\tuploader\n" +
		address(cmsImage) + "\t" + cmsImage.ID.String() + "\tcms_image\tpending\ty\n" +
		address(legacy) + "/card.jpg?v=2\t" + legacy.ID.String() + "\tlegacy\tpending\ty\n" +
		address(orphan) + "\t" + orphan.ID.String() + "\tlegacy\tpending\tn\n" +
		address(cover) + "\t" + cover.ID.String() + "\tevent_cover\tpending\ty\n" +
		fullAddress + "\t-\t-\t-\t-\n" +
		"https://example.com/logo.png with a tab\t-\t-\t-\t-\n"
	if rows != want {
		t.Fatalf("rows:\n%s\nwant:\n%s", rows, want)
	}
	for _, line := range []string{
		"addresses: 6\n", "named a Media: 4\n", "named none: 2 (not a core address: 1)\n",
		"Media stored with a full address, which no lookup finds: 2 (1 current)\n",
	} {
		if !strings.Contains(summary, line) {
			t.Fatalf("summary lacks %q:\n%s", line, summary)
		}
	}

	// For a product: only what it may link for the Media's uploader, or
	// holds, as stage 5 attaches it.
	rows, summary = run("-product", "forms")
	want = "address\tmedia_id\tpurpose\tstatus\tuploader\n" +
		address(cmsImage) + "\t-\t-\t-\t-\n" +
		address(legacy) + "/card.jpg?v=2\t" + legacy.ID.String() + "\tlegacy\tpending\ty\n" +
		address(orphan) + "\t-\t-\t-\t-\n" +
		address(cover) + "\t-\t-\t-\t-\n" +
		fullAddress + "\t-\t-\t-\t-\n" +
		"https://example.com/logo.png with a tab\t-\t-\t-\t-\n"
	if rows != want || !strings.Contains(summary, "product: forms") || !strings.Contains(summary, "named a Media: 1\n") {
		t.Fatalf("rows:\n%s\nsummary:\n%s", rows, summary)
	}
}

func TestMediaLookupCommandRefusesWhatItCannotRun(t *testing.T) {
	getenv := func(name string) string {
		if name == "DATABASE_URL" {
			return "postgres://core:s3cret@db.example.test/core"
		}
		return ""
	}
	for name, args := range map[string][]string{
		"core is no product":   {"-product", "core"},
		"an unknown product":   {"-product", "skyforms"},
		"an argument too many": {"addresses.txt"},
	} {
		var out, summary bytes.Buffer
		if code := runMediaLookup(args, getenv, strings.NewReader(""), &out, &summary); code != 2 || out.Len() != 0 {
			t.Errorf("%s: exit %d, rows %q", name, code, out.String())
		}
		if strings.Contains(summary.String(), "s3cret") {
			t.Fatalf("%s echoes the database address", name)
		}
	}
	var out, summary bytes.Buffer
	if code := runMediaLookup(nil, func(string) string { return "" }, strings.NewReader(""), &out, &summary); code != 2 ||
		!strings.Contains(summary.String(), "DATABASE_URL") {
		t.Fatalf("without a database: exit %d: %s", code, summary.String())
	}
}
