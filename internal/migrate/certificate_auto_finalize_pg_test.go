package migrate_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
)

const certificateAutoFinalizeVersion = "20261124010000"

// Up, down and up again. An Event that has ended when the migration runs and
// whose attendance is not closed gets the switch off (S3): by its end date,
// or by its last live Session's end although its end date is still to come.
// A coming Event, an undated one and one already closed keep it on. A
// finalization written before is manual; the checks take 'auto' and
// 'rescan' and refuse anything else; the down turns a rescan batch into a
// manual one.
func TestCertificateAutoFinalizeMigration(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	down := readDown(t, certificateAutoFinalizeVersion+"_certificate_auto_finalize.down.sql")
	if _, err := pool.Exec(ctx, down); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+certificateAutoFinalizeVersion); err != nil {
		t.Fatal(err)
	}
	event := func(name, dates string) uuid.UUID {
		t.Helper()
		id := uuid.New()
		if _, err := pool.Exec(ctx, `INSERT INTO events (id, name, location, owner_team, attendance_rule, start_date, end_date)
			SELECT $1, $2, 'YTÜ', 'ARTLAB', 'once', d.s, d.e FROM (`+dates+`) d(s, e)`, id, name); err != nil {
			t.Fatal(err)
		}
		return id
	}
	session := func(eventID uuid.UUID, ends string) {
		t.Helper()
		dayID := uuid.New()
		if _, err := pool.Exec(ctx, `INSERT INTO event_days (id, event_id) VALUES ($1, $2)`, dayID, eventID); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO sessions (id, event_day_id, title, speaker_name, session_type, end_time)
			VALUES ($1, $2, 'Açılış', 'Ada', 'TALK', `+ends+`)`, uuid.New(), dayID); err != nil {
			t.Fatal(err)
		}
	}
	ended := event("Bitti", `SELECT now() - interval '3 days', now() - interval '2 days'`)
	coming := event("Gelecek", `SELECT now() + interval '1 day', now() + interval '2 days'`)
	sessionsOver := event("Oturumlar bitti", `SELECT now() - interval '1 day', now() + interval '1 day'`)
	session(sessionsOver, `now() - interval '2 hours'`)
	sessionsOn := event("Oturum sürüyor", `SELECT now() - interval '1 day', now() + interval '1 day'`)
	session(sessionsOn, `now() + interval '2 hours'`)
	undated := event("Tarihsiz", `SELECT NULL::timestamptz, NULL::timestamptz`)
	closed := event("Kapandı", `SELECT now() - interval '3 days', now() - interval '2 days'`)
	if _, err := pool.Exec(ctx, `INSERT INTO certificate_event_state (event_id, attendance_finalized_at) VALUES ($1, now())`, closed); err != nil {
		t.Fatal(err)
	}

	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if !recorded(t, pool, certificateAutoFinalizeVersion) {
		t.Fatal("not recorded")
	}
	for id, want := range map[uuid.UUID]bool{ended: false, coming: true, sessionsOver: false, sessionsOn: true, undated: true, closed: true} {
		var auto bool
		var name string
		if err := pool.QueryRow(ctx, `SELECT name, attendance_auto_finalize FROM events WHERE id = $1`, id).Scan(&name, &auto); err != nil {
			t.Fatal(err)
		}
		if auto != want {
			t.Errorf("%s: attendance_auto_finalize=%v, want %v", name, auto, want)
		}
	}
	var source string
	if err := pool.QueryRow(ctx, `SELECT finalized_source FROM certificate_event_state WHERE event_id = $1`, closed).Scan(&source); err != nil {
		t.Fatal(err)
	}
	if source != "manual" {
		t.Fatalf("existing finalization source %q", source)
	}
	if _, err := pool.Exec(ctx, `UPDATE certificate_event_state SET finalized_source = 'clock' WHERE event_id = $1`, closed); err == nil {
		t.Fatal("source 'clock' was written")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO certificate_event_state (event_id, attendance_finalized_at, finalized_source) VALUES ($1, now(), 'auto')`, ended); err != nil {
		t.Fatalf("auto finalization: %v", err)
	}
	batch := func(reason string) (uuid.UUID, error) {
		id := uuid.New()
		_, err := pool.Exec(ctx, `INSERT INTO certificate_batches (id, event_id, template_version_id, template_source, reason)
			VALUES ($1, $2, '89c0bfe1-5be5-4ddb-8686-4865f51e3401', 'clubDefault', $3)`, id, closed, reason)
		return id, err
	}
	rescan, err := batch("rescan")
	if err != nil {
		t.Fatalf("rescan batch: %v", err)
	}
	if _, err := batch("sweep"); err == nil {
		t.Fatal("reason 'sweep' was written")
	}
	// A new Event gets the switch on.
	fresh := event("Yeni", `SELECT now() + interval '7 days', now() + interval '8 days'`)
	var auto bool
	if err := pool.QueryRow(ctx, `SELECT attendance_auto_finalize FROM events WHERE id = $1`, fresh).Scan(&auto); err != nil || !auto {
		t.Fatalf("new Event auto=%v err=%v", auto, err)
	}

	if _, err := pool.Exec(ctx, down); err != nil {
		t.Fatal(err)
	}
	var reason string
	if err := pool.QueryRow(ctx, `SELECT reason FROM certificate_batches WHERE id = $1`, rescan).Scan(&reason); err != nil {
		t.Fatal(err)
	}
	if reason != "manual" {
		t.Fatalf("rescan batch after the down: %q", reason)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+certificateAutoFinalizeVersion); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if !recorded(t, pool, certificateAutoFinalizeVersion) {
		t.Fatal("not recorded after up again")
	}
	// The fingerprint records a schema that already has all three.
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+certificateAutoFinalizeVersion); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if !recorded(t, pool, certificateAutoFinalizeVersion) {
		t.Fatal("not recorded by its fingerprint")
	}
}
