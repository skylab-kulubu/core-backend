package migrate_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
)

const (
	doorWalkInVersion = "20261127220000"
	doorWalkInDown    = doorWalkInVersion + "_door_walk_in.down.sql"
)

// Walk-in at the door (docs/walk-in.md): a guest Ticket takes a university,
// a department and a grade of the fixed keys; an entry takes the source
// walk_in; the audit record takes ticket.walk_in, by door_staff through a
// Group and for that action only. The up migration runs again over itself;
// down keeps the columns with their values and the rows written, and refuses
// a new walk_in entry and ticket.walk_in row; up again takes them.
func TestDoorWalkInMigration(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	eventID, ticketID := uuid.New(), uuid.New()
	for _, statement := range []string{
		`INSERT INTO events (id, name, location, owner_team) VALUES ('` + eventID.String() + `', 'ARTLAB', 'Tarihi Hamam', 'ARTLAB')`,
		`INSERT INTO tickets (id, event_id, ticket_type, guest_first_name, guest_last_name, guest_email, guest_phone_number,
			guest_university, guest_department, guest_grade)
			VALUES ('` + ticketID.String() + `', '` + eventID.String() + `', 'GUEST', 'Ayşe', 'Yılmaz', 'ayse@example.com', '+905321234567',
			'Yıldız Teknik Üniversitesi', 'Bilgisayar Mühendisliği', 'prep')`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	for name, statement := range map[string]string{
		"a grade not of the keys": `UPDATE tickets SET guest_grade = '5' WHERE id = $1`,
		"a university of 201":     `UPDATE tickets SET guest_university = repeat('a', 201) WHERE id = $1`,
		"a department of 201":     `UPDATE tickets SET guest_department = repeat('a', 201) WHERE id = $1`,
	} {
		if _, err := pool.Exec(ctx, statement, ticketID); err == nil {
			t.Errorf("%s was taken", name)
		}
	}
	entry := `INSERT INTO venue_entries (id, ticket_id, event_id, entry_date, source) VALUES (gen_random_uuid(), $1, $2, $3::date, 'walk_in')`
	if _, err := pool.Exec(ctx, entry, ticketID, eventID, "2026-11-27"); err != nil {
		t.Fatalf("a walk_in entry: %v", err)
	}
	row := `INSERT INTO team_audit_records (id, occurred_at, actor_id, actor_basis, actor_via_group_id, action, group_id, group_path, outcome)
		VALUES (gen_random_uuid(), now(), gen_random_uuid(), 'door_staff', gen_random_uuid(), 'ticket.walk_in', gen_random_uuid(), '/UYELER/SANAT/ARTLAB', 'ok')`
	if _, err := pool.Exec(ctx, row); err != nil {
		t.Fatalf("a door staff's walk-in row: %v", err)
	}
	for name, statement := range map[string]string{
		"door_staff on another action": strings.Replace(row, "'ticket.walk_in'", "'ticket.cancel'", 1),
		"door_staff without a Group":   strings.Replace(row, "'door_staff', gen_random_uuid()", "'door_staff', NULL", 1),
	} {
		if _, err := pool.Exec(ctx, statement); err == nil {
			t.Errorf("%s was taken", name)
		}
	}
	if _, err := pool.Exec(ctx, strings.Replace(row, "'door_staff', gen_random_uuid()", "'privileged', NULL", 1)); err != nil {
		t.Fatalf("a privileged walk-in row: %v", err)
	}

	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+doorWalkInVersion); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if !recorded(t, pool, doorWalkInVersion) {
		t.Fatal("not recorded")
	}

	if _, err := pool.Exec(ctx, readDown(t, doorWalkInDown)); err != nil {
		t.Fatal(err)
	}
	var grade, university string
	if err := pool.QueryRow(ctx, `SELECT guest_grade, guest_university FROM tickets WHERE id = $1`, ticketID).Scan(&grade, &university); err != nil ||
		grade != "prep" || university != "Yıldız Teknik Üniversitesi" {
		t.Fatalf("after down: %q %q %v", grade, university, err)
	}
	if _, err := pool.Exec(ctx, entry, ticketID, eventID, "2026-11-28"); err == nil {
		t.Error("after down a walk_in entry was taken")
	}
	if _, err := pool.Exec(ctx, row); err == nil {
		t.Error("after down a ticket.walk_in row was taken")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO tickets (id, event_id, ticket_type, guest_email) VALUES (gen_random_uuid(), $1, 'GUEST', 'eski@example.com')`, eventID); err != nil {
		t.Fatalf("the release before writes a Ticket: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+doorWalkInVersion); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatalf("up again: %v", err)
	}
	if _, err := pool.Exec(ctx, entry, ticketID, eventID, "2026-11-28"); err != nil {
		t.Fatalf("up again, a walk_in entry: %v", err)
	}
}
