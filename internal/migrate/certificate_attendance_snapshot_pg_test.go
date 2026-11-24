package migrate_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
)

const certificateAttendanceSnapshotVersion = "20261124020000"

// Up, down and up again: a certificate issued before keeps no snapshot; the
// check takes three NULLs or three values >= 0 and refuses a mix of the two
// or a negative one.
func TestCertificateAttendanceSnapshotMigration(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	down := readDown(t, certificateAttendanceSnapshotVersion+"_certificate_attendance_snapshot.down.sql")
	forget := func() {
		t.Helper()
		if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+certificateAttendanceSnapshotVersion); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, down); err != nil {
		t.Fatal(err)
	}
	forget()
	eventID, ticketID, certID := uuid.New(), uuid.New(), uuid.New()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO events (id, name, location, owner_team) VALUES ($1, 'ARTLAB', 'YTÜ', 'ARTLAB')`, []any{eventID}},
		{`INSERT INTO tickets (id, event_id, ticket_type, guest_first_name, guest_last_name, guest_email) VALUES ($1, $2, 'GUEST', 'Ayşe', 'Yılmaz', 'ayse@example.com')`, []any{ticketID, eventID}},
		{`INSERT INTO certificates (id, event_id, ticket_id, serial, recipient_name, recipient_email, event_name, owner_team, verify_url)
			VALUES ($1, $2, $3, 'ESKI', 'Ayşe Yılmaz', 'ayse@example.com', 'ARTLAB', 'ARTLAB', '')`, []any{certID, eventID, ticketID}},
	} {
		if _, err := pool.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}

	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if !recorded(t, pool, certificateAttendanceSnapshotVersion) {
		t.Fatal("not recorded")
	}
	var attended, total, minutes *int
	if err := pool.QueryRow(ctx, `SELECT attended_count, counted_total, program_minutes FROM certificates WHERE id = $1`, certID).Scan(&attended, &total, &minutes); err != nil {
		t.Fatal(err)
	}
	if attended != nil || total != nil || minutes != nil {
		t.Fatalf("certificate issued before has a snapshot: %v %v %v", attended, total, minutes)
	}
	set := func(attended, total, minutes any) error {
		_, err := pool.Exec(ctx, `UPDATE certificates SET attended_count = $2, counted_total = $3, program_minutes = $4 WHERE id = $1`, certID, attended, total, minutes)
		return err
	}
	for _, mixed := range [][3]any{{6, nil, nil}, {nil, 8, nil}, {nil, nil, 540}, {6, 8, nil}, {6, nil, 540}, {nil, 8, 540}} {
		if err := set(mixed[0], mixed[1], mixed[2]); err == nil {
			t.Fatalf("mixed NULLs written: %v", mixed)
		}
	}
	for _, negative := range [][3]any{{-1, 8, 540}, {6, -1, 540}, {6, 8, -1}} {
		if err := set(negative[0], negative[1], negative[2]); err == nil {
			t.Fatalf("negative written: %v", negative)
		}
	}
	if err := set(6, 8, 540); err != nil {
		t.Fatalf("6/8/540: %v", err)
	}
	if err := set(0, 0, 0); err != nil {
		t.Fatalf("0/0/0: %v", err)
	}
	if err := set(nil, nil, nil); err != nil {
		t.Fatalf("three NULLs: %v", err)
	}

	if _, err := pool.Exec(ctx, down); err != nil {
		t.Fatal(err)
	}
	forget()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if !recorded(t, pool, certificateAttendanceSnapshotVersion) {
		t.Fatal("not recorded after up again")
	}
	// The fingerprint records a schema that already has the columns.
	forget()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if !recorded(t, pool, certificateAttendanceSnapshotVersion) {
		t.Fatal("not recorded by its fingerprint")
	}
}
