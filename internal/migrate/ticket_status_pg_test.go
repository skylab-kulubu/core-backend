package migrate_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
)

const (
	ticketStatusVersion = "20261124100000"
	ticketStatusDown    = ticketStatusVersion + "_ticket_status.down.sql"
)

// A Ticket's status: the Tickets already there, and those the release
// before writes (no status in its insert), are active; the checks refuse an
// unknown status and a cancellation without its time and maker; the up
// migration runs again over itself (its fingerprint records it); down drops
// the columns and keeps the Tickets, and up then adds them back.
func TestTicketStatusMigration(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, readDown(t, ticketStatusDown)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+ticketStatusVersion); err != nil {
		t.Fatal(err)
	}
	eventID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO events (id, name, location, owner_team) VALUES ($1, 'Hack', 'YTÜ', 'WEBLAB')`, eventID); err != nil {
		t.Fatal(err)
	}
	// The insert of the release before: no status.
	oldInsert := `INSERT INTO tickets (id, event_id, ticket_type, guest_first_name, guest_last_name, guest_email, guest_phone_number)
		VALUES ($1, $2, 'GUEST', 'Ada', 'Lovelace', $3, '')`
	before := uuid.New()
	if _, err := pool.Exec(ctx, oldInsert, before, eventID, "ada@example.com"); err != nil {
		t.Fatal(err)
	}

	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	after := uuid.New()
	if _, err := pool.Exec(ctx, oldInsert, after, eventID, "grace@example.com"); err != nil {
		t.Fatalf("the release before writes on the new schema: %v", err)
	}
	var active int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM tickets WHERE id = ANY($1) AND status = 'active' AND cancelled_at IS NULL`,
		[]uuid.UUID{before, after}).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != 2 {
		t.Fatalf("%d of 2 Tickets active", active)
	}
	for name, q := range map[string]string{
		"an unknown status":           `UPDATE tickets SET status = 'deleted' WHERE id = $1`,
		"a cancellation with no time": `UPDATE tickets SET status = 'cancelled', cancelled_by = 'operator' WHERE id = $1`,
		"an unknown maker":            `UPDATE tickets SET status = 'cancelled', cancelled_at = now(), cancelled_by = 'robot' WHERE id = $1`,
		"a reason on an active one":   `UPDATE tickets SET cancel_reason = 'x' WHERE id = $1`,
	} {
		if _, err := pool.Exec(ctx, q, after); err == nil {
			t.Errorf("%s was taken", name)
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE tickets SET status = 'cancelled', cancelled_at = now(), cancelled_by = 'operator', cancel_reason = 'x' WHERE id = $1`, after); err != nil {
		t.Fatalf("a cancellation: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO team_audit_records (id, occurred_at, actor_id, actor_basis, action, group_id, group_path, outcome)
		VALUES (gen_random_uuid(), now(), gen_random_uuid(), 'privileged', 'ticket.cancel', gen_random_uuid(), '/UYELER/ARGE/WEBLAB', 'ok')`); err != nil {
		t.Fatalf("a ticket.cancel row: %v", err)
	}

	// Again over itself: recorded, nothing changes.
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+ticketStatusVersion); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if !recorded(t, pool, ticketStatusVersion) {
		t.Fatal("not recorded")
	}

	if _, err := pool.Exec(ctx, readDown(t, ticketStatusDown)); err != nil {
		t.Fatal(err)
	}
	var columns, rows int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = 'tickets'
		  AND column_name IN ('status', 'status_changed_at', 'waitlisted_at', 'cancelled_at', 'cancelled_by', 'cancel_reason')`).Scan(&columns); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM tickets WHERE id = ANY($1)`, []uuid.UUID{before, after}).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if columns != 0 || rows != 2 {
		t.Fatalf("after down: %d status columns, %d Tickets", columns, rows)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+ticketStatusVersion); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM tickets WHERE id = ANY($1) AND status = 'active'`, []uuid.UUID{before, after}).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != 2 {
		t.Fatalf("up again: %d of 2 active", active)
	}
}
