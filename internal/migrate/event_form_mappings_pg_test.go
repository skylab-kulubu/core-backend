package migrate_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
)

const (
	formMappingsVersion = "20261127100000"
	formMappingsDown    = formMappingsVersion + "_event_form_mappings.down.sql"
)

// Form mappings: the checks hold, the up migration runs again over itself,
// and down keeps the table and its rows (an organizer's mapping is not lost
// to a rollback).
func TestEventFormMappingsMigration(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	eventID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO events (id, name, location, owner_team) VALUES ($1, 'Hack', 'YTÜ', 'WEBLAB')`, eventID); err != nil {
		t.Fatal(err)
	}
	insert := func(first, last, full, email any) error {
		_, err := pool.Exec(ctx, `
			INSERT INTO event_form_mappings (event_id, form_id, first_name_question, last_name_question, full_name_question, email_question)
			VALUES ($1, $2, $3, $4, $5, $6)`, eventID, uuid.New(), first, last, full, email)
		return err
	}
	if err := insert("q1", "q2", nil, "q3"); err != nil {
		t.Fatal(err)
	}
	if err := insert(nil, nil, "q1", "q2"); err != nil {
		t.Fatal(err)
	}
	for name, bad := range map[string][4]any{
		"no name":      {nil, nil, nil, "q1"},
		"first only":   {"q1", nil, nil, "q2"},
		"blank e-mail": {"q1", "q2", nil, " "},
		"blank full":   {nil, nil, "", "q2"},
		"id too long":  {"q1", "q2", nil, strings.Repeat("q", 101)},
	} {
		if err := insert(bad[0], bad[1], bad[2], bad[3]); err == nil {
			t.Fatalf("%s: taken", name)
		}
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+formMappingsVersion); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if !recorded(t, pool, formMappingsVersion) {
		t.Fatal("not recorded")
	}
	if _, err := pool.Exec(ctx, readDown(t, formMappingsDown)); err != nil {
		t.Fatal(err)
	}
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM event_form_mappings`).Scan(&rows); err != nil || rows != 2 {
		t.Fatalf("after down: %d rows, %v", rows, err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+formMappingsVersion); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM events WHERE id = $1`, eventID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM event_form_mappings`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("the Event's deletion left %d rows, %v", rows, err)
	}
}
