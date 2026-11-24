package migrate_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
)

const (
	capacityWaitlistVersion = "20261124110000"
	capacityWaitlistDown    = capacityWaitlistVersion + "_event_capacity_waitlist.down.sql"
)

// The waiting list's switch and the queued mail's kind: the release before
// writes neither (its Event has the list on, its queued row is "created");
// the kind check refuses an unknown one; ticket.promote is an audit action
// with the event_editor basis; the up migration runs again over itself;
// down drops the columns (and the rows of the new kinds first), up adds
// them back.
func TestEventCapacityWaitlistMigration(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, readDown(t, capacityWaitlistDown)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+capacityWaitlistVersion); err != nil {
		t.Fatal(err)
	}
	eventID, ticketID := uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO events (id, name, location, owner_team, capacity) VALUES ($1, 'Hack', 'YTÜ', 'WEBLAB', 1)`, eventID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO tickets (id, event_id, ticket_type, guest_first_name, guest_last_name, guest_email, guest_phone_number)
		VALUES ($1, $2, 'GUEST', 'Ada', 'Lovelace', 'ada@example.com', '')`, ticketID, eventID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO ticket_notifications (ticket_id) VALUES ($1)`, ticketID); err != nil {
		t.Fatal(err)
	}

	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	var waitlist bool
	var kind string
	if err := pool.QueryRow(ctx, `SELECT waitlist_enabled FROM events WHERE id = $1`, eventID).Scan(&waitlist); err != nil || !waitlist {
		t.Fatalf("the Event's waiting list: %v %v", waitlist, err)
	}
	if err := pool.QueryRow(ctx, `SELECT kind FROM ticket_notifications WHERE ticket_id = $1`, ticketID).Scan(&kind); err != nil || kind != "created" {
		t.Fatalf("the queued row's kind: %q %v", kind, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE ticket_notifications SET kind = 'reminder' WHERE ticket_id = $1`, ticketID); err == nil {
		t.Error("an unknown kind was taken")
	}
	if _, err := pool.Exec(ctx, `UPDATE ticket_notifications SET kind = 'promoted' WHERE ticket_id = $1`, ticketID); err != nil {
		t.Fatalf("promoted: %v", err)
	}
	editorRow := `INSERT INTO team_audit_records (id, occurred_at, actor_id, actor_basis, actor_via_group_id, action, group_id, group_path, outcome)
		VALUES (gen_random_uuid(), now(), gen_random_uuid(), 'event_editor', $1, 'ticket.promote', gen_random_uuid(), '/UYELER/ORGANIZASYON/GECEKODU', 'ok')`
	if _, err := pool.Exec(ctx, editorRow, uuid.New()); err != nil {
		t.Fatalf("an event_editor ticket.promote row: %v", err)
	}

	// Again over itself: recorded, nothing changes.
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+capacityWaitlistVersion); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if !recorded(t, pool, capacityWaitlistVersion) {
		t.Fatal("not recorded")
	}

	if _, err := pool.Exec(ctx, readDown(t, capacityWaitlistDown)); err != nil {
		t.Fatal(err)
	}
	var columns, queued int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.columns
		WHERE table_schema = 'public' AND (table_name, column_name) IN (('events', 'waitlist_enabled'), ('ticket_notifications', 'kind'))`).Scan(&columns); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM ticket_notifications WHERE ticket_id = $1`, ticketID).Scan(&queued); err != nil {
		t.Fatal(err)
	}
	if columns != 0 || queued != 0 {
		t.Fatalf("after down: %d columns, %d promoted rows left to send as created", columns, queued)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+capacityWaitlistVersion); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT waitlist_enabled FROM events WHERE id = $1`, eventID).Scan(&waitlist); err != nil || !waitlist {
		t.Fatalf("up again: %v %v", waitlist, err)
	}
}
