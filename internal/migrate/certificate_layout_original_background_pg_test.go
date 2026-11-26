package migrate_test

import (
	"context"
	"sort"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
)

const (
	originalBackgroundVersion = "20261127131500"
	originalBackgroundDown    = originalBackgroundVersion + "_certificate_layout_original_background.down.sql"
)

// A converted background's original is one of the Media a layout links:
// up counts it, down goes back to the old definition, up again counts it.
func TestCertificateLayoutOriginalBackgroundMigration(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	background, original, image := uuid.New(), uuid.New(), uuid.New()
	layout := `{"backgroundMediaId":"` + background.String() + `","backgroundOriginalMediaId":"` + original.String() +
		`","elements":[{"id":"logo","kind":"image","mediaId":"` + image.String() + `"}]}`
	linked := func() []string {
		t.Helper()
		var ids []string
		if err := pool.QueryRow(ctx, `SELECT certificate_layout_media_ids($1::jsonb, '{}'::jsonb)::text[]`, layout).Scan(&ids); err != nil {
			t.Fatal(err)
		}
		sort.Strings(ids)
		return ids
	}
	want := func(ids ...uuid.UUID) []string {
		out := make([]string, len(ids))
		for i, id := range ids {
			out[i] = id.String()
		}
		sort.Strings(out)
		return out
	}
	same := func(a, b []string) bool {
		if len(a) != len(b) {
			return false
		}
		for i := range a {
			if a[i] != b[i] {
				return false
			}
		}
		return true
	}
	if got := linked(); !same(got, want(background, original, image)) {
		t.Fatalf("up: %v", got)
	}
	if !recorded(t, pool, originalBackgroundVersion) {
		t.Fatal("not recorded")
	}
	if _, err := pool.Exec(ctx, readDown(t, originalBackgroundDown)); err != nil {
		t.Fatal(err)
	}
	if got := linked(); !same(got, want(background, image)) {
		t.Fatalf("down: %v", got)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+originalBackgroundVersion); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if got := linked(); !same(got, want(background, original, image)) {
		t.Fatalf("up again: %v", got)
	}
}
