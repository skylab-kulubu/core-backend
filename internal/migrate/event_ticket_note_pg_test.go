package migrate_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
)

const (
	eventTicketNoteVersion = "20261127210000"
	eventTicketNoteDown    = eventTicketNoteVersion + "_event_ticket_note.down.sql"
)

// An Event's ticket note (docs/ticket-note.md): the check takes 500
// characters, up runs again over itself, and down keeps the column with its
// values.
func TestEventTicketNoteMigration(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	eventID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO events (id, name, location, owner_team, ticket_note)
		VALUES ($1, 'ARTLAB', 'YTÜ', 'ARTLAB', 'https://chat.whatsapp.com/Ornek123')`, eventID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE events SET ticket_note = repeat('a', 501) WHERE id = $1`, eventID); err == nil {
		t.Error("501 characters were stored")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+eventTicketNoteVersion); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if !recorded(t, pool, eventTicketNoteVersion) {
		t.Fatal("not recorded")
	}
	if _, err := pool.Exec(ctx, readDown(t, eventTicketNoteDown)); err != nil {
		t.Fatal(err)
	}
	var note string
	if err := pool.QueryRow(ctx, `SELECT ticket_note FROM events WHERE id = $1`, eventID).Scan(&note); err != nil || note != "https://chat.whatsapp.com/Ornek123" {
		t.Fatalf("after down: %q %v", note, err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO events (id, name, location, owner_team) VALUES ($1, 'Eski sürüm', 'YTÜ', 'WEBLAB')`, uuid.New()); err != nil {
		t.Fatalf("the release before writes an Event: %v", err)
	}
}
