package migrate_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
)

const (
	eventCoOrganizersVersion = "20261126120000"
	eventCoOrganizersDown    = eventCoOrganizersVersion + "_event_co_organizers.down.sql"
)

// An Event's Co-organizers: a row is a team with both choices or a person
// with none; the choices take their known values alone; a team and a person
// are each once on an Event; a person and the adder must be active
// accounts; the rows go with their Event; the up migration runs again over
// itself; down drops the table, up adds it back.
func TestEventCoOrganizersMigration(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	eventID, person, adder := uuid.New(), uuid.New(), uuid.New()
	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO events (id, name, location, owner_team) VALUES ($1, 'Hack', 'YTÜ', 'YK')`, []any{eventID}},
		{`INSERT INTO users (id, email) VALUES ($1, 'person@example.test'), ($2, 'adder@example.test')`, []any{person, adder}},
	} {
		if _, err := pool.Exec(ctx, statement.sql, statement.args...); err != nil {
			t.Fatalf("%s: %v", statement.sql, err)
		}
	}
	insert := `INSERT INTO event_co_organizers (event_id, team_code, user_id, management, visibility, added_by) VALUES ($1, $2, $3, $4, $5, $6)`
	ok := []struct {
		name string
		args []any
	}{
		{"a team", []any{eventID, "WEBLAB", nil, "leaders", "co_organizer", adder}},
		{"a helper team of members", []any{eventID, "GECEKODU", nil, "members", "helper", nil}},
		{"a person", []any{eventID, nil, person, nil, nil, adder}},
	}
	for _, c := range ok {
		if _, err := pool.Exec(ctx, insert, c.args...); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
	}
	refused := []struct {
		name string
		args []any
	}{
		{"neither a team nor a person", []any{eventID, nil, nil, nil, nil, nil}},
		{"both a team and a person", []any{eventID, "ARTLAB", uuid.New(), "leaders", "helper", nil}},
		{"a team without its management", []any{eventID, "ARTLAB", nil, nil, "helper", nil}},
		{"a team without its visibility", []any{eventID, "ARTLAB", nil, "leaders", nil, nil}},
		{"a person with a choice", []any{eventID, nil, person, "leaders", nil, nil}},
		{"an unknown management", []any{eventID, "ARTLAB", nil, "everyone", "helper", nil}},
		{"an unknown visibility", []any{eventID, "ARTLAB", nil, "leaders", "public", nil}},
		{"a blank code", []any{eventID, " ", nil, "leaders", "helper", nil}},
		{"the same team twice", []any{eventID, "WEBLAB", nil, "members", "helper", nil}},
		{"the same person twice", []any{eventID, nil, person, nil, nil, nil}},
		{"no account", []any{eventID, nil, uuid.New(), nil, nil, nil}},
		{"an adder of no account", []any{eventID, "ARTLAB", nil, "leaders", "helper", uuid.New()}},
	}
	for _, c := range refused {
		if _, err := pool.Exec(ctx, insert, c.args...); err == nil {
			t.Errorf("%s was taken", c.name)
		}
	}

	// Again over itself: recorded, nothing changes.
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+eventCoOrganizersVersion); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if !recorded(t, pool, eventCoOrganizersVersion) {
		t.Fatal("not recorded")
	}
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM event_co_organizers WHERE event_id = $1`, eventID).Scan(&rows); err != nil || rows != 3 {
		t.Fatalf("rows %d %v", rows, err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM events WHERE id = $1`, eventID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM event_co_organizers`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("after the Event went: %d rows %v", rows, err)
	}

	if _, err := pool.Exec(ctx, readDown(t, eventCoOrganizersDown)); err != nil {
		t.Fatal(err)
	}
	var gone bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('public.event_co_organizers') IS NULL`).Scan(&gone); err != nil || !gone {
		t.Fatalf("after down: gone %v %v", gone, err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+eventCoOrganizersVersion); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if !recorded(t, pool, eventCoOrganizersVersion) {
		t.Fatal("up again: not recorded")
	}
}
