package migrate_test

import (
	"context"
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/migrate"
)

const (
	certificateEventTemplatesVersion = "20261126173000"
	certificateEventTemplatesDown    = certificateEventTemplatesVersion + "_certificate_event_templates.down.sql"
)

// Event templates' column and index: the up migration runs again over
// itself (its fingerprint records it); down drops both, and up adds them
// back.
func TestCertificateEventTemplatesMigration(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	present := func() (column, index bool) {
		t.Helper()
		if err := pool.QueryRow(ctx, `
			SELECT
				EXISTS (SELECT 1 FROM information_schema.columns
					WHERE table_schema = 'public' AND table_name = 'certificate_templates' AND column_name = 'event_id'),
				to_regclass('public.certificate_templates_one_per_event') IS NOT NULL`).Scan(&column, &index); err != nil {
			t.Fatal(err)
		}
		return column, index
	}
	if column, index := present(); !column || !index {
		t.Fatalf("column %v index %v", column, index)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+certificateEventTemplatesVersion); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if !recorded(t, pool, certificateEventTemplatesVersion) {
		t.Fatal("not recorded")
	}
	if _, err := pool.Exec(ctx, readDown(t, certificateEventTemplatesDown)); err != nil {
		t.Fatal(err)
	}
	if column, index := present(); column || index {
		t.Fatalf("after down: column %v index %v", column, index)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+certificateEventTemplatesVersion); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if column, index := present(); !column || !index {
		t.Fatalf("up again: column %v index %v", column, index)
	}
}
