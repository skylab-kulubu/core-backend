package migrate_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
)

const (
	eventThemeVersion = "20261127200000"
	eventThemeDown    = eventThemeVersion + "_event_theme.down.sql"
)

// An Event's theme and ticket colour (docs/event-theme.md): the checks take
// a key's shape and a #RRGGBB in capitals, up runs again over itself, and
// down keeps both columns with their values.
func TestEventThemeMigration(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	eventID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO events (id, name, location, owner_team, theme, ticket_accent_color)
		VALUES ($1, 'ARTLAB', 'YTÜ', 'ARTLAB', 'artlab', '#FF8A1F')`, eventID); err != nil {
		t.Fatal(err)
	}
	for _, refused := range []string{`theme = 'ARTLAB'`, `theme = 'art lab'`, `theme = ''`, `ticket_accent_color = '#ff8a1f'`, `ticket_accent_color = 'FF8A1F'`} {
		if _, err := pool.Exec(ctx, `UPDATE events SET `+refused+` WHERE id = $1`, eventID); err == nil {
			t.Errorf("%s was stored", refused)
		}
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+eventThemeVersion); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if !recorded(t, pool, eventThemeVersion) {
		t.Fatal("not recorded")
	}
	if _, err := pool.Exec(ctx, readDown(t, eventThemeDown)); err != nil {
		t.Fatal(err)
	}
	var key, color string
	if err := pool.QueryRow(ctx, `SELECT theme, ticket_accent_color FROM events WHERE id = $1`, eventID).Scan(&key, &color); err != nil || key != "artlab" || color != "#FF8A1F" {
		t.Fatalf("after down: %q %q %v", key, color, err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO events (id, name, location, owner_team) VALUES ($1, 'Eski sürüm', 'YTÜ', 'WEBLAB')`, uuid.New()); err != nil {
		t.Fatalf("the release before writes an Event: %v", err)
	}
}
