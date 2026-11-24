package migrate_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
)

const (
	externalRegistrationVersion = "20261125201700"
	externalRegistrationDown    = externalRegistrationVersion + "_external_registration_and_attendee_import.down.sql"
)

// The external registration link and the ticket.import audit action
// (docs/attendee-import.md): an Event of the release before has none; the
// check takes an https link and refuses any other; an Event's editor may
// write a ticket.import row; the up migration runs again over itself; down
// drops the column, up adds it back.
func TestExternalRegistrationImportMigration(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, readDown(t, externalRegistrationDown)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+externalRegistrationVersion); err != nil {
		t.Fatal(err)
	}
	eventID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO events (id, name, location, owner_team) VALUES ($1, 'Hack', 'YTÜ', 'WEBLAB')`, eventID); err != nil {
		t.Fatal(err)
	}

	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	var link string
	if err := pool.QueryRow(ctx, `SELECT external_registration_url FROM events WHERE id = $1`, eventID).Scan(&link); err != nil || link != "" {
		t.Fatalf("the release before's Event: %q %v", link, err)
	}
	for value, ok := range map[string]bool{
		"https://forms.office.com/r/AbC": true,
		"":                               true,
		"http://forms.office.com/r/AbC":  false,
		"https://":                       false,
		"javascript:alert(1)":            false,
		"https://x.example.com/" + strings.Repeat("a", 2048): false,
	} {
		_, err := pool.Exec(ctx, `UPDATE events SET external_registration_url = $2 WHERE id = $1`, eventID, value)
		if (err == nil) != ok {
			t.Errorf("%.40q: %v", value, err)
		}
	}
	editorRow := `INSERT INTO team_audit_records (id, occurred_at, actor_id, actor_basis, actor_via_group_id, action, group_id, group_path, outcome)
		VALUES (gen_random_uuid(), now(), gen_random_uuid(), 'event_editor', $1, 'ticket.import', gen_random_uuid(), '/UYELER/ORGANIZASYON/GECEKODU', 'ok')`
	if _, err := pool.Exec(ctx, editorRow, uuid.New()); err != nil {
		t.Fatalf("an event_editor ticket.import row: %v", err)
	}

	// Again over itself: recorded, nothing changes.
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+externalRegistrationVersion); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if !recorded(t, pool, externalRegistrationVersion) {
		t.Fatal("not recorded")
	}

	if _, err := pool.Exec(ctx, readDown(t, externalRegistrationDown)); err != nil {
		t.Fatal(err)
	}
	var columns int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = 'events' AND column_name = 'external_registration_url'`).Scan(&columns); err != nil || columns != 0 {
		t.Fatalf("down left the column: %d %v", columns, err)
	}
	// The row the import wrote stays.
	var kept int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM team_audit_records WHERE action = 'ticket.import'`).Scan(&kept); err != nil || kept != 1 {
		t.Fatalf("audit rows after down: %d %v", kept, err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+externalRegistrationVersion); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
}
