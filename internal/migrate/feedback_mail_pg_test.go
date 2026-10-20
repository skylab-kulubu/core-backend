package migrate_test

import (
	"context"
	"slices"
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/migrate"
)

const feedbackMailVersion = "20261020190300"

func TestFeedbackMailMigrationComesLast(t *testing.T) {
	t.Parallel()
	versions, err := migrate.Versions()
	if err != nil {
		t.Fatal(err)
	}
	before := slices.Index(versions, 20261020190200)
	mails := slices.Index(versions, 20261020190300)
	if before < 0 || mails < 0 || before >= mails {
		t.Fatalf("versions %v", versions)
	}
}

// Down and up again work, and the fingerprint records a schema that has
// the tables already.
func TestFeedbackMailMigration(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, readDown(t, feedbackMailVersion+"_feedback_mail.down.sql")); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+feedbackMailVersion); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+feedbackMailVersion); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if !recorded(t, pool, feedbackMailVersion) {
		t.Fatal("not recorded by its fingerprint")
	}
}
