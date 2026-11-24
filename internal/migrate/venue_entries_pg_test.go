package migrate_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
)

const (
	venueEntriesVersion = "20261124150000"
	venueEntriesDown    = venueEntriesVersion + "_venue_entries.down.sql"
)

// The door's mode and Venue entry: an Event the release before wrote is
// automatic (NULL); the mode's check refuses an unknown one; one entry a
// Ticket a day, of a known source; undoing the Check-in an entry names keeps
// the entry, unnamed; the writer must be an active account; the up migration
// runs again over itself; down drops both, up adds them back.
func TestVenueEntriesMigration(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, readDown(t, venueEntriesDown)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+venueEntriesVersion); err != nil {
		t.Fatal(err)
	}
	eventID, ticketID, dayID, sessionID, checkInID := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	for _, statement := range []string{
		`INSERT INTO events (id, name, location, owner_team) VALUES ('` + eventID.String() + `', 'Hack', 'YTÜ', 'WEBLAB')`,
		`INSERT INTO tickets (id, event_id, ticket_type, guest_first_name, guest_last_name, guest_email, guest_phone_number)
			VALUES ('` + ticketID.String() + `', '` + eventID.String() + `', 'GUEST', 'Ada', 'Lovelace', 'ada@example.com', '')`,
		`INSERT INTO event_days (id, event_id, name) VALUES ('` + dayID.String() + `', '` + eventID.String() + `', '1. gün')`,
		`INSERT INTO sessions (id, event_day_id, title, speaker_name, session_type) VALUES ('` + sessionID.String() + `', '` + dayID.String() + `', 'Açılış', '', 'talk')`,
		`INSERT INTO ticket_checkins (id, ticket_id, event_day_id, session_id) VALUES ('` + checkInID.String() + `', '` + ticketID.String() + `', '` + dayID.String() + `', '` + sessionID.String() + `')`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}

	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	var mode *string
	if err := pool.QueryRow(ctx, `SELECT entry_mode FROM events WHERE id = $1`, eventID).Scan(&mode); err != nil || mode != nil {
		t.Fatalf("the release before's Event: %v %v", mode, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE events SET entry_mode = 'door' WHERE id = $1`, eventID); err == nil {
		t.Error("an unknown entry mode was taken")
	}
	if _, err := pool.Exec(ctx, `UPDATE events SET entry_mode = 'venue' WHERE id = $1`, eventID); err != nil {
		t.Fatalf("venue: %v", err)
	}
	insert := `INSERT INTO venue_entries (id, ticket_id, event_id, entry_date, event_day_id, source, check_in_id)
		VALUES (gen_random_uuid(), $1, $2, $3::date, $4, $5, $6)`
	if _, err := pool.Exec(ctx, insert, ticketID, eventID, "2026-12-12", dayID, "attendance", checkInID); err != nil {
		t.Fatalf("an attendance entry: %v", err)
	}
	if _, err := pool.Exec(ctx, insert, ticketID, eventID, "2026-12-12", nil, "scan", nil); err == nil {
		t.Error("a second entry the same day was taken")
	}
	if _, err := pool.Exec(ctx, insert, ticketID, eventID, "2026-12-13", nil, "walk_in", nil); err == nil {
		t.Error("an unknown source was taken")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO venue_entries (id, ticket_id, event_id, entry_date, source, by_user_id)
		VALUES (gen_random_uuid(), $1, $2, '2026-12-13', 'scan', $3)`, ticketID, eventID, uuid.New()); err == nil {
		t.Error("an entry by no account was taken")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM ticket_checkins WHERE id = $1`, checkInID); err != nil {
		t.Fatal(err)
	}
	var named *uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT check_in_id FROM venue_entries WHERE ticket_id = $1`, ticketID).Scan(&named); err != nil || named != nil {
		t.Fatalf("the entry after its Check-in went: %v %v", named, err)
	}

	// Again over itself: recorded, nothing changes.
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+venueEntriesVersion); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if !recorded(t, pool, venueEntriesVersion) {
		t.Fatal("not recorded")
	}

	if _, err := pool.Exec(ctx, readDown(t, venueEntriesDown)); err != nil {
		t.Fatal(err)
	}
	var left int
	if err := pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM information_schema.columns
			WHERE table_schema = 'public' AND table_name = 'events' AND column_name = 'entry_mode')
		+ (CASE WHEN to_regclass('public.venue_entries') IS NULL THEN 0 ELSE 1 END)`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Fatalf("after down: %d of the column and the table left", left)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+venueEntriesVersion); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if !recorded(t, pool, venueEntriesVersion) {
		t.Fatal("up again: not recorded")
	}
}
