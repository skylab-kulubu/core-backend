package migrate_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
)

const (
	formRespondentsVersion = "20261127170000"
	formRespondentsDown    = formRespondentsVersion + "_form_respondents.down.sql"
)

// The Unticketed respondents: down keeps every row and parks a waiting
// collect job (the release before would run it as a backfill); up again
// puts it back and keeps the rows.
func TestFormRespondentsMigration(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	eventID, formID := uuid.New(), uuid.New()
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO events (id, name, location, owner_team) VALUES ($1, 'Hack', 'YTÜ', 'WEBLAB')`, eventID)
	exec(`INSERT INTO event_form_respondents (id, event_id, form_id, form_response_id, first_name, last_name, email, forms_status, source)
		VALUES ($1, $2, $3, $4, 'Ada', 'Lovelace', 'ada@example.com', 'accepted', 'collect')`, uuid.New(), eventID, formID, uuid.New())
	exec(`INSERT INTO form_pulls (id, form_id, kind, event_id) VALUES ($1, $2, 'collect', $3)`, uuid.New(), formID, eventID)
	var auto bool
	if err := pool.QueryRow(ctx, `SELECT form_responses_auto_ticket FROM events WHERE id = $1`, eventID).Scan(&auto); err != nil || !auto {
		t.Fatalf("auto ticket %t %v", auto, err)
	}
	// A guest row needs its name and address; a person's keeps none.
	if _, err := pool.Exec(ctx, `INSERT INTO event_form_respondents (id, event_id, form_id, form_response_id, user_id, email, forms_status, source)
		VALUES ($1, $2, $3, $4, $5, 'x@example.com', 'accepted', 'new')`, uuid.New(), eventID, formID, uuid.New(), uuid.New()); err == nil {
		t.Fatal("a signed-in row kept an address")
	}

	exec(readDown(t, formRespondentsDown))
	var parked bool
	if err := pool.QueryRow(ctx, `SELECT next_attempt_at = 'infinity' FROM form_pulls WHERE kind = 'collect'`).Scan(&parked); err != nil || !parked {
		t.Fatalf("parked %t %v", parked, err)
	}
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM event_form_respondents`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("rows after down %d %v", rows, err)
	}

	exec(`DELETE FROM schema_migrations WHERE version = ` + formRespondentsVersion)
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if !recorded(t, pool, formRespondentsVersion) {
		t.Fatal("not recorded")
	}
	if err := pool.QueryRow(ctx, `SELECT next_attempt_at = 'infinity' FROM form_pulls WHERE kind = 'collect'`).Scan(&parked); err != nil || parked {
		t.Fatalf("still parked %t %v", parked, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM event_form_respondents`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("rows after up %d %v", rows, err)
	}
}
