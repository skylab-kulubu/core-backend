package migrate_test

import (
	"context"
	"slices"
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/migrate"
)

const feedbackInvitesVersion = "20261020190200"

func TestFeedbackInvitesMigrationComesLast(t *testing.T) {
	t.Parallel()
	versions, err := migrate.Versions()
	if err != nil {
		t.Fatal(err)
	}
	before := slices.Index(versions, 20261020190100)
	invites := slices.Index(versions, 20261020190200)
	if before < 0 || invites < 0 || before >= invites {
		t.Fatalf("versions %v", versions)
	}
}

// Down and up again work, and the fingerprint records a schema that has
// the tables already.
func TestFeedbackInvitesMigration(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, readDown(t, feedbackInvitesVersion+"_feedback_invites.down.sql")); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+feedbackInvitesVersion); err != nil {
		t.Fatal(err)
	}
	var gone bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('public.feedback_invites') IS NULL AND to_regclass('public.feedback_invite_counts') IS NULL`).Scan(&gone); err != nil || !gone {
		t.Fatalf("after down: %v %v", gone, err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if !recorded(t, pool, feedbackInvitesVersion) {
		t.Fatal("not recorded")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+feedbackInvitesVersion); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if !recorded(t, pool, feedbackInvitesVersion) {
		t.Fatal("not recorded by its fingerprint")
	}
}
