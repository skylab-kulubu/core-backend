package migrate_test

import (
	"context"
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/migrate"
)

const (
	teamMailGroupIDVersion = "20261027190000"
	teamMailGroupIDDown    = teamMailGroupIDVersion + "_team_membership_mail_group_id.down.sql"
)

// The Group id of a queued membership mail (ADR 0065 ticket 07): down and
// up again keep working, and the fingerprint records a schema that has the
// column.
func TestTeamMembershipMailGroupIDMigration(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, readDown(t, teamMailGroupIDDown)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+teamMailGroupIDVersion); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+teamMailGroupIDVersion); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if !recorded(t, pool, teamMailGroupIDVersion) {
		t.Fatal("not recorded by its fingerprint")
	}
}
