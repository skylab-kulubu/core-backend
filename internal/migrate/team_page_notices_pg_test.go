package migrate_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
)

const teamPageNoticesVersion = "20261030100000"

// The ledger keeps one row per person. Down refuses while it records a
// notice (the notice would go again); empty, it drops the table and Apply
// makes it again. The fingerprint knows the table.
func TestTeamPageNoticesMigration(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	person := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO team_page_notices (user_id, sent_at) VALUES ($1, now())`, person); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO team_page_notices (user_id, sent_at) VALUES ($1, now())`, person); err == nil {
		t.Fatal("a second row for the same person")
	}

	down := readDown(t, teamPageNoticesVersion+"_team_page_notices.down.sql")
	if _, err := pool.Exec(ctx, down); err == nil {
		t.Fatal("down dropped a recorded notice")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM team_page_notices`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, down); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+teamPageNoticesVersion); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil || !recorded(t, pool, teamPageNoticesVersion) {
		t.Fatalf("again: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+teamPageNoticesVersion); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil || !recorded(t, pool, teamPageNoticesVersion) {
		t.Fatalf("fingerprint: %v", err)
	}
}
