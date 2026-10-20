package migrate_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
)

const userWelcomeSentVersion = "20261020180000"

// The Welcome mark comes as a nullable column. Rows already there count as
// welcomed when they have an e-mail (the mail went out when such a row was
// opened); a row without one stays unmarked for the user sync. The down
// drops the column and Apply brings it back the same way.
func TestUserWelcomeSentMigration(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	down := readDown(t, userWelcomeSentVersion+"_user_welcome_sent.down.sql")
	if _, err := pool.Exec(ctx, down); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+userWelcomeSentVersion); err != nil {
		t.Fatal(err)
	}
	withEmail, withoutEmail := uuid.New(), uuid.New()
	opened := time.Date(2026, 10, 19, 10, 0, 0, 0, time.UTC)
	if _, err := pool.Exec(ctx, `INSERT INTO users (id, email, created_at) VALUES ($1, 'ada@example.com', $3), ($2, '', $3)`,
		withEmail, withoutEmail, opened); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	var marked, unmarked *time.Time
	if err := pool.QueryRow(ctx, `SELECT
		(SELECT welcome_sent_at FROM users WHERE id = $1),
		(SELECT welcome_sent_at FROM users WHERE id = $2)`, withEmail, withoutEmail).Scan(&marked, &unmarked); err != nil {
		t.Fatal(err)
	}
	if marked == nil || !marked.Equal(opened) || unmarked != nil {
		t.Fatalf("marked=%v unmarked=%v", marked, unmarked)
	}
	// A row opened after the migration is unmarked until the sync sends.
	fresh := uuid.New()
	var fresher *time.Time
	if err := pool.QueryRow(ctx, `INSERT INTO users (id, email) VALUES ($1, 'grace@example.com') RETURNING welcome_sent_at`, fresh).Scan(&fresher); err != nil || fresher != nil {
		t.Fatalf("fresh row %v %v", fresher, err)
	}
	// Recorded already: Apply does not run it again.
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT welcome_sent_at FROM users WHERE id = $1`, fresh).Scan(&fresher); err != nil || fresher != nil {
		t.Fatalf("fresh row after a second Apply %v %v", fresher, err)
	}
}
