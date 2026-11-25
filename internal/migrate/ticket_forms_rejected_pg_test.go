package migrate_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
)

const (
	ticketFormsRejectedVersion = "20261127110000"
	ticketFormsRejectedDown    = ticketFormsRejectedVersion + "_ticket_forms_rejected.down.sql"
)

// The mark of Forms' decline: up runs again over itself, and down keeps the
// column and its marks.
func TestTicketFormsRejectedMigration(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	eventID, ticketID := uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO events (id, name, location, owner_team) VALUES ($1, 'Hack', 'YTÜ', 'WEBLAB')`, eventID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO tickets (id, event_id, ticket_type, guest_first_name, guest_last_name, guest_email, forms_rejected_at)
		VALUES ($1, $2, 'GUEST', 'Ada', 'Lovelace', 'ada@example.com', now())`, ticketID, eventID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+ticketFormsRejectedVersion); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if !recorded(t, pool, ticketFormsRejectedVersion) {
		t.Fatal("not recorded")
	}
	if _, err := pool.Exec(ctx, readDown(t, ticketFormsRejectedDown)); err != nil {
		t.Fatal(err)
	}
	var marked bool
	if err := pool.QueryRow(ctx, `SELECT forms_rejected_at IS NOT NULL FROM tickets WHERE id = $1`, ticketID).Scan(&marked); err != nil || !marked {
		t.Fatalf("after down: marked %t, %v", marked, err)
	}
}
