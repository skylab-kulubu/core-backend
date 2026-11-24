package migrate_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
)

const certificateMailsVersion = "20261125101500"

// Up, down and up again: the queue table comes and goes; its checks refuse
// an unknown status and a sent row without its time, and its indexes one
// second issued mail and one second queued mail per certificate.
func TestCertificateMailsMigration(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	down := readDown(t, certificateMailsVersion+"_certificate_mails.down.sql")
	if _, err := pool.Exec(ctx, down); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+certificateMailsVersion); err != nil {
		t.Fatal(err)
	}
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('certificate_mails') IS NOT NULL`).Scan(&exists); err != nil || exists {
		t.Fatalf("after down: exists=%v %v", exists, err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if !recorded(t, pool, certificateMailsVersion) {
		t.Fatal("not recorded")
	}

	eventID, ticketID, certID := uuid.New(), uuid.New(), uuid.New()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO events (id, name, location, owner_team) VALUES ($1, 'ARTLAB', 'YTÜ', 'ARTLAB')`, []any{eventID}},
		{`INSERT INTO tickets (id, event_id, ticket_type, guest_first_name, guest_last_name, guest_email) VALUES ($1, $2, 'GUEST', 'Ada', 'Lovelace', 'ada@example.com')`, []any{ticketID, eventID}},
		{`INSERT INTO certificates (id, event_id, ticket_id, serial, recipient_name, recipient_email, event_name, owner_team, verify_url)
			VALUES ($1, $2, $3, 'SERIAL1', 'Ada Lovelace', 'ada@example.com', 'ARTLAB', 'ARTLAB', 'https://skyl.app/c/SERIAL1')`, []any{certID, eventID, ticketID}},
	} {
		if _, err := pool.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	insert := func(reason, status string, sent bool) error {
		_, err := pool.Exec(ctx, `INSERT INTO certificate_mails (id, certificate_id, event_id, reason, status, sent_at)
			VALUES ($1, $2, $3, $4, $5, CASE WHEN $6 THEN now() END)`, uuid.New(), certID, eventID, reason, status, sent)
		return err
	}
	if err := insert("issued", "lost", false); err == nil {
		t.Fatal("an unknown status was written")
	}
	if err := insert("issued", "sent", false); err == nil {
		t.Fatal("a sent row without sent_at was written")
	}
	if err := insert("issued", "queued", false); err != nil {
		t.Fatal(err)
	}
	if err := insert("resend", "queued", false); err == nil {
		t.Fatal("a second queued mail was written")
	}
	if err := insert("issued", "failed", false); err == nil {
		t.Fatal("a second issued mail was written")
	}
	if err := insert("resend", "failed", false); err != nil {
		t.Fatalf("a settled resend: %v", err)
	}
}
