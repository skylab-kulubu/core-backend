package user_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// A Venue entry's writer is an actor reference (docs/venue-entry.md):
// erasing the door staff's account forgets who wrote it and keeps the
// entry.
func TestAnonymizationClearsTheVenueEntryWriter(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	users := user.NewPostgresStore(pool)
	staff := uuid.New()
	if _, _, err := user.NewService(users).Ensure(ctx, staff, user.Profile{Email: "kapi@example.test"}); err != nil {
		t.Fatal(err)
	}
	eventID, ticketID, entryID := uuid.New(), uuid.New(), uuid.New()
	for _, statement := range []string{
		`INSERT INTO events (id, name, location, owner_team) VALUES ('` + eventID.String() + `', 'Hack', 'YTÜ', 'WEBLAB')`,
		`INSERT INTO tickets (id, event_id, ticket_type, guest_first_name, guest_last_name, guest_email, guest_phone_number)
			VALUES ('` + ticketID.String() + `', '` + eventID.String() + `', 'GUEST', 'Ada', 'Lovelace', 'ada@example.com', '')`,
		`INSERT INTO venue_entries (id, ticket_id, event_id, entry_date, source, by_user_id)
			VALUES ('` + entryID.String() + `', '` + ticketID.String() + `', '` + eventID.String() + `', '2026-12-12', 'scan', '` + staff.String() + `')`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	if _, err := users.RequestDeletion(ctx, staff, nil); err != nil {
		t.Fatal(err)
	}
	if err := users.AnonymizeAccount(ctx, staff, time.Now().UTC(), nil); err != nil {
		t.Fatal(err)
	}
	var by *uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT by_user_id FROM venue_entries WHERE id = $1`, entryID).Scan(&by); err != nil {
		t.Fatalf("entry gone: %v", err)
	}
	if by != nil {
		t.Fatalf("writer kept: %v", by)
	}
}

// So is who wrote a Check-in, and who wrote or took back a door mark taken
// back (door_undos): erasing the account forgets them, the rows stay.
func TestAnonymizationClearsTheDoorWriters(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	users := user.NewPostgresStore(pool)
	staff := uuid.New()
	if _, _, err := user.NewService(users).Ensure(ctx, staff, user.Profile{Email: "kapi@example.test"}); err != nil {
		t.Fatal(err)
	}
	eventID, dayID, sessionID, ticketID, checkInID, undoID := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	for _, statement := range []string{
		`INSERT INTO events (id, name, location, owner_team) VALUES ('` + eventID.String() + `', 'Hack', 'YTÜ', 'WEBLAB')`,
		`INSERT INTO event_days (id, event_id, name) VALUES ('` + dayID.String() + `', '` + eventID.String() + `', '1. gün')`,
		`INSERT INTO sessions (id, event_day_id, title, session_type) VALUES ('` + sessionID.String() + `', '` + dayID.String() + `', 'Açılış', 'TALK')`,
		`INSERT INTO tickets (id, event_id, ticket_type, guest_first_name, guest_last_name, guest_email, guest_phone_number)
			VALUES ('` + ticketID.String() + `', '` + eventID.String() + `', 'GUEST', 'Ada', 'Lovelace', 'ada@example.com', '')`,
		`INSERT INTO ticket_checkins (id, ticket_id, event_day_id, session_id, source, by_user_id)
			VALUES ('` + checkInID.String() + `', '` + ticketID.String() + `', '` + dayID.String() + `', '` + sessionID.String() + `', 'scan', '` + staff.String() + `')`,
		`INSERT INTO door_undos (id, kind, ticket_id, event_id, entry_date, source, by_user_id, written_at, undone_by)
			VALUES ('` + undoID.String() + `', 'entry', '` + ticketID.String() + `', '` + eventID.String() + `', '2026-12-12', 'scan', '` + staff.String() + `', now(), '` + staff.String() + `')`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	if _, err := users.RequestDeletion(ctx, staff, nil); err != nil {
		t.Fatal(err)
	}
	if err := users.AnonymizeAccount(ctx, staff, time.Now().UTC(), nil); err != nil {
		t.Fatal(err)
	}
	var named int
	if err := pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM ticket_checkins WHERE id = $1 AND by_user_id IS NULL)
			+ (SELECT count(*) FROM door_undos WHERE id = $2 AND by_user_id IS NULL AND undone_by IS NULL)`, checkInID, undoID).Scan(&named); err != nil {
		t.Fatal(err)
	}
	if named != 2 {
		t.Fatalf("rows kept and cleared: %d of 2", named)
	}
}
