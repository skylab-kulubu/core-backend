package migrate_test

import (
	"context"
	"slices"
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/migrate"
)

const feedbackReminderVersion = "20261025180100"

func TestFeedbackReminderMigrationComesLast(t *testing.T) {
	t.Parallel()
	versions, err := migrate.Versions()
	if err != nil {
		t.Fatal(err)
	}
	mails := slices.Index(versions, 20261025180000)
	reminder := slices.Index(versions, 20261025180100)
	if mails < 0 || reminder < 0 || mails >= reminder {
		t.Fatalf("versions %v", versions)
	}
}

// Down and up again work, the fingerprint records a schema that has it
// already, and a reminder row fits the kind's check.
func TestFeedbackReminderMigration(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, readDown(t, feedbackReminderVersion+"_feedback_reminder.down.sql")); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+feedbackReminderVersion); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+feedbackReminderVersion); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if !recorded(t, pool, feedbackReminderVersion) {
		t.Fatal("not recorded by its fingerprint")
	}
	var ok bool
	if err := pool.QueryRow(ctx, `
		SELECT pg_get_constraintdef(oid) LIKE '%reminder%' FROM pg_constraint
		WHERE conrelid = 'feedback_mails'::regclass AND conname = 'feedback_mails_kind_check'`).Scan(&ok); err != nil || !ok {
		t.Fatalf("kind check %v %v", ok, err)
	}
}
