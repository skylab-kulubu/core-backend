package migrate_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
)

const (
	attendanceMailsVersion = "20261125203000"
	attendanceMailsDown    = attendanceMailsVersion + "_attendance_mails.down.sql"
)

// The door's mails: an Event the release before wrote has both switches
// on; the kind check pairs a welcome with no Session and a receipt with
// one; one welcome a Ticket a day and one receipt a Ticket a Session; down
// drops the queue and the switches, up adds them back.
func TestAttendanceMailsMigration(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, readDown(t, attendanceMailsDown)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+attendanceMailsVersion); err != nil {
		t.Fatal(err)
	}
	eventID, ticketID, dayID, sessionID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	for _, statement := range []string{
		`INSERT INTO events (id, name, location, owner_team) VALUES ('` + eventID.String() + `', 'Hack', 'YTÜ', 'WEBLAB')`,
		`INSERT INTO tickets (id, event_id, ticket_type, guest_first_name, guest_last_name, guest_email, guest_phone_number)
			VALUES ('` + ticketID.String() + `', '` + eventID.String() + `', 'GUEST', 'Ada', 'Lovelace', 'ada@example.com', '')`,
		`INSERT INTO event_days (id, event_id, name) VALUES ('` + dayID.String() + `', '` + eventID.String() + `', '1. gün')`,
		`INSERT INTO sessions (id, event_day_id, title, speaker_name, session_type) VALUES ('` + sessionID.String() + `', '` + dayID.String() + `', 'Açılış', '', 'talk')`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	var welcome, recorded bool
	if err := pool.QueryRow(ctx, `SELECT attendance_welcome_mail, attendance_recorded_mail FROM events WHERE id = $1`, eventID).Scan(&welcome, &recorded); err != nil || !welcome || !recorded {
		t.Fatalf("the release before's Event: %v %v %v", welcome, recorded, err)
	}
	var note string
	if err := pool.QueryRow(ctx, `SELECT attendee_note FROM events WHERE id = $1`, eventID).Scan(&note); err != nil || note != "" {
		t.Fatalf("the release before's note: %q %v", note, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE events SET attendee_note = repeat('a', 1001) WHERE id = $1`, eventID); err == nil {
		t.Error("a note of 1001 characters was taken")
	}
	insert := `INSERT INTO attendance_mails (id, kind, ticket_id, event_id, entry_date, session_id, next_attempt_at) VALUES (gen_random_uuid(), $1, $2, $3, '2031-03-14', $4, now())`
	if _, err := pool.Exec(ctx, insert, "welcome", ticketID, eventID, sessionID); err == nil {
		t.Error("a welcome with a Session was taken")
	}
	if _, err := pool.Exec(ctx, insert, "recorded", ticketID, eventID, nil); err == nil {
		t.Error("a receipt with no Session was taken")
	}
	if _, err := pool.Exec(ctx, insert, "welcome", ticketID, eventID, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, insert, "welcome", ticketID, eventID, nil); err == nil {
		t.Error("a second welcome the same day was taken")
	}
	if _, err := pool.Exec(ctx, insert, "recorded", ticketID, eventID, sessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, insert, "recorded", ticketID, eventID, sessionID); err == nil {
		t.Error("a second receipt of the same Session was taken")
	}
	if _, err := pool.Exec(ctx, readDown(t, attendanceMailsDown)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+attendanceMailsVersion); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatalf("up again: %v", err)
	}
}
