package migrate_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
)

const certificateRuleSessionsVersion = "20261123230000"

// Up, down and up again: a Session and an Event written before the
// migration are counted, not required and keep no threshold; the down puts
// a count Event back on the ratio rule at 0.75; the checks refuse a
// required Session that is not counted and a threshold below 1.
func TestCertificateRuleSessionsMigration(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	down := readDown(t, certificateRuleSessionsVersion+"_certificate_rule_sessions.down.sql")
	if _, err := pool.Exec(ctx, down); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+certificateRuleSessionsVersion); err != nil {
		t.Fatal(err)
	}
	eventID, dayID, sessionID := uuid.New(), uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO events (id, name, location, owner_team, attendance_rule, attendance_ratio) VALUES ($1, 'ARTLAB', 'YTÜ', 'WEBLAB', 'ratio', 0.75)`, eventID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO event_days (id, event_id) VALUES ($1, $2)`, dayID, eventID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sessions (id, event_day_id, title, speaker_name, session_type) VALUES ($1, $2, 'Açılış', 'Ada', 'TALK')`, sessionID, dayID); err != nil {
		t.Fatal(err)
	}

	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if !recorded(t, pool, certificateRuleSessionsVersion) {
		t.Fatal("not recorded")
	}
	var counted, required bool
	if err := pool.QueryRow(ctx, `SELECT certificate_counted, certificate_required FROM sessions WHERE id = $1`, sessionID).Scan(&counted, &required); err != nil {
		t.Fatal(err)
	}
	if !counted || required {
		t.Fatalf("existing Session counted=%v required=%v", counted, required)
	}
	var minCount *int
	var rule string
	if err := pool.QueryRow(ctx, `SELECT attendance_rule, attendance_min_count FROM events WHERE id = $1`, eventID).Scan(&rule, &minCount); err != nil {
		t.Fatal(err)
	}
	if rule != "ratio" || minCount != nil {
		t.Fatalf("existing Event rule=%q min=%v", rule, minCount)
	}
	if _, err := pool.Exec(ctx, `UPDATE sessions SET certificate_counted = false, certificate_required = true WHERE id = $1`, sessionID); err == nil {
		t.Fatal("a required Session that is not counted was written")
	}
	if _, err := pool.Exec(ctx, `UPDATE events SET attendance_min_count = 0 WHERE id = $1`, eventID); err == nil {
		t.Fatal("a threshold of 0 was written")
	}

	// A count Event goes back to the ratio rule at 0.75.
	if _, err := pool.Exec(ctx, `UPDATE events SET attendance_rule = 'count', attendance_ratio = NULL, attendance_min_count = 6 WHERE id = $1`, eventID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, down); err != nil {
		t.Fatal(err)
	}
	var ratio *float64
	if err := pool.QueryRow(ctx, `SELECT attendance_rule, attendance_ratio FROM events WHERE id = $1`, eventID).Scan(&rule, &ratio); err != nil {
		t.Fatal(err)
	}
	if rule != "ratio" || ratio == nil || *ratio != 0.75 {
		t.Fatalf("after down rule=%q ratio=%v", rule, ratio)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+certificateRuleSessionsVersion); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if !recorded(t, pool, certificateRuleSessionsVersion) {
		t.Fatal("not recorded after up again")
	}
	// The fingerprint records a schema that already has the columns.
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+certificateRuleSessionsVersion); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if !recorded(t, pool, certificateRuleSessionsVersion) {
		t.Fatal("not recorded by its fingerprint")
	}
}
