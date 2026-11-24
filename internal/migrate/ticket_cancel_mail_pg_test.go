package migrate_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
)

const (
	cancelMailVersion = "20261124130000"
	cancelMailDown    = cancelMailVersion + "_ticket_cancel_mail_form_response.down.sql"
)

// The cancellation mail's kind and the Ticket's forms answer: the release
// before writes neither (its Ticket keeps no answer); the kind check takes
// cancelled; the up migration runs again over itself; down deletes the
// cancelled rows (the release before would send them as "Biletin hazır"),
// narrows the check and drops the column; up adds them back.
func TestTicketCancelMailMigration(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, readDown(t, cancelMailDown)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+cancelMailVersion); err != nil {
		t.Fatal(err)
	}
	eventID, ticketID := uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO events (id, name, location, owner_team) VALUES ($1, 'Hack', 'YTÜ', 'WEBLAB')`, eventID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO tickets (id, event_id, ticket_type, guest_first_name, guest_last_name, guest_email, guest_phone_number)
		VALUES ($1, $2, 'GUEST', 'Ada', 'Lovelace', 'ada@example.com', '')`, ticketID, eventID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO ticket_notifications (ticket_id) VALUES ($1)`, ticketID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE ticket_notifications SET kind = 'cancelled' WHERE ticket_id = $1`, ticketID); err == nil {
		t.Fatal("the release before took cancelled")
	}

	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	var answer *uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT form_response_id FROM tickets WHERE id = $1`, ticketID).Scan(&answer); err != nil || answer != nil {
		t.Fatalf("an existing Ticket's answer: %v %v", answer, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE ticket_notifications SET kind = 'cancelled' WHERE ticket_id = $1`, ticketID); err != nil {
		t.Fatalf("cancelled: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE ticket_notifications SET kind = 'reminder' WHERE ticket_id = $1`, ticketID); err == nil {
		t.Error("an unknown kind was taken")
	}
	if _, err := pool.Exec(ctx, `UPDATE tickets SET form_response_id = $2 WHERE id = $1`, ticketID, uuid.New()); err != nil {
		t.Fatalf("an answer: %v", err)
	}

	// Again over itself: recorded, nothing changes.
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+cancelMailVersion); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if !recorded(t, pool, cancelMailVersion) {
		t.Fatal("not recorded")
	}

	if _, err := pool.Exec(ctx, readDown(t, cancelMailDown)); err != nil {
		t.Fatal(err)
	}
	var columns, queued int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = 'tickets' AND column_name = 'form_response_id'`).Scan(&columns); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM ticket_notifications WHERE ticket_id = $1`, ticketID).Scan(&queued); err != nil {
		t.Fatal(err)
	}
	if columns != 0 || queued != 0 {
		t.Fatalf("after down: %d columns, %d cancelled rows left to send as created", columns, queued)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+cancelMailVersion); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = 'tickets' AND column_name = 'form_response_id'`).Scan(&columns); err != nil || columns != 1 {
		t.Fatalf("up again: %d %v", columns, err)
	}
}
