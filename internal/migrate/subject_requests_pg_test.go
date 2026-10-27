package migrate_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
)

const subjectRequestsVersion = "20261027120000"

// The register computes the due date, refuses what the policy keeps out
// (an unknown channel, type or status, no type at all, an answered request
// without its date or with one before the request) and keeps a request
// when its account goes. Its trail goes with the request. Down refuses
// while a request is held; empty, it drops both tables and Apply makes them
// again.
func TestSubjectRequestsMigration(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	insert := func(channel, types, status, answered string) error {
		_, err := pool.Exec(ctx, `
			INSERT INTO subject_requests (id, received_on, channel, requester_name, requester_contact, request_types,
				status, answered_on, created_at, updated_at)
			VALUES ($1, '2026-10-01', $2, 'Ada Lovelace', 'ada@example.test', $3::text[], $4, NULLIF($5, '')::date, now(), now())`,
			uuid.New(), channel, types, status, answered)
		return err
	}
	if err := insert("email", "{erase,info}", "received", ""); err != nil {
		t.Fatal(err)
	}
	for name, err := range map[string]error{
		"channel":           insert("fax", "{erase}", "received", ""),
		"type":              insert("email", "{forget}", "received", ""),
		"no type":           insert("email", "{}", "received", ""),
		"status":            insert("email", "{erase}", "closed", ""),
		"answered, no date": insert("email", "{erase}", "answered", ""),
		"open with a date":  insert("email", "{erase}", "received", "2026-10-05"),
		"answered before":   insert("post", "{erase}", "rejected", "2026-09-30"),
	} {
		if err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := insert("in_person", "{other}", "answered", "2026-10-01"); err != nil {
		t.Errorf("answered the day it came: %v", err)
	}
	var due string
	if err := pool.QueryRow(ctx, `SELECT to_char(due_on, 'YYYY-MM-DD') FROM subject_requests WHERE status = 'received'`).Scan(&due); err != nil || due != "2026-10-31" {
		t.Fatalf("due %q %v", due, err)
	}

	// An account's request outlives the account; its trail goes with it.
	account := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO users (id, email, first_name, last_name) VALUES ($1, 'b@example.test', 'B', 'C')`, account); err != nil {
		t.Fatal(err)
	}
	request := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO subject_requests (id, received_on, channel, requester_name, requester_contact, user_id, request_types,
			status, created_at, updated_at)
		VALUES ($1, '2026-10-02', 'email', 'B C', 'b@example.test', $2, '{learn}', 'received', now(), now())`, request, account); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO subject_request_events (request_id, occurred_at, action, fields, status) VALUES ($1, now(), 'create', '{status}', 'received')`, request); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, account); err != nil {
		t.Fatal(err)
	}
	var kept bool
	if err := pool.QueryRow(ctx, `SELECT user_id IS NULL FROM subject_requests WHERE id = $1`, request).Scan(&kept); err != nil || !kept {
		t.Fatalf("request after the account went: %v %v", kept, err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM subject_requests WHERE id = $1`, request); err != nil {
		t.Fatal(err)
	}
	var trail int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM subject_request_events`).Scan(&trail); err != nil || trail != 0 {
		t.Fatalf("trail left %d %v", trail, err)
	}

	down := readDown(t, subjectRequestsVersion+"_subject_requests.down.sql")
	if _, err := pool.Exec(ctx, down); err == nil {
		t.Fatal("down dropped a held request")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM subject_requests`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, down); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+subjectRequestsVersion); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if !recorded(t, pool, subjectRequestsVersion) {
		t.Fatal("not recorded")
	}
	// The fingerprint knows the schema: unrecorded, Apply records it.
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+subjectRequestsVersion); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil || !recorded(t, pool, subjectRequestsVersion) {
		t.Fatalf("fingerprint: %v", err)
	}
}
