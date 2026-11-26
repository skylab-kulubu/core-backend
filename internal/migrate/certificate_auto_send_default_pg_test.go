package migrate_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
)

const certificateAutoSendDefaultVersion = "20261127201000"

// Up, down and up again. A new Event's switch is off; the Events already
// there keep theirs, on or off, both ways. The fingerprint records a schema
// whose default is already off.
func TestCertificateAutoSendDefaultMigration(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	event := func(name string) uuid.UUID {
		t.Helper()
		id := uuid.New()
		if _, err := pool.Exec(ctx, `INSERT INTO events (id, name, location, owner_team, attendance_rule)
			VALUES ($1, $2, 'YTÜ', 'ARTLAB', 'once')`, id, name); err != nil {
			t.Fatal(err)
		}
		return id
	}
	auto := func(id uuid.UUID) bool {
		t.Helper()
		var on bool
		if err := pool.QueryRow(ctx, `SELECT attendance_auto_finalize FROM events WHERE id = $1`, id).Scan(&on); err != nil {
			t.Fatal(err)
		}
		return on
	}
	if auto(event("Yeni")) {
		t.Fatal("a new Event's switch is on")
	}
	turnedOn := event("Açılmış")
	if _, err := pool.Exec(ctx, `UPDATE events SET attendance_auto_finalize = true WHERE id = $1`, turnedOn); err != nil {
		t.Fatal(err)
	}
	off := event("Kapalı")

	down := readDown(t, certificateAutoSendDefaultVersion+"_certificate_auto_send_default_off.down.sql")
	if _, err := pool.Exec(ctx, down); err != nil {
		t.Fatal(err)
	}
	if !auto(event("Eski varsayılan")) {
		t.Fatal("after the down a new Event's switch is off")
	}
	if !auto(turnedOn) || auto(off) {
		t.Fatalf("the down changed an Event: on=%v off=%v", auto(turnedOn), auto(off))
	}

	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+certificateAutoSendDefaultVersion); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if !recorded(t, pool, certificateAutoSendDefaultVersion) {
		t.Fatal("not recorded after up again")
	}
	if auto(event("Yeni 2")) {
		t.Fatal("after up again a new Event's switch is on")
	}
	if !auto(turnedOn) || auto(off) {
		t.Fatalf("the up changed an Event: on=%v off=%v", auto(turnedOn), auto(off))
	}

	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+certificateAutoSendDefaultVersion); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if !recorded(t, pool, certificateAutoSendDefaultVersion) {
		t.Fatal("not recorded by its fingerprint")
	}
}

// A transaction holding a lock on events makes the migration give up after
// its lock_timeout instead of queueing every Event read behind its ALTER.
func TestCertificateAutoSendDefaultMigrationGivesUpOnAHeldLock(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	down := readDown(t, certificateAutoSendDefaultVersion+"_certificate_auto_send_default_off.down.sql")
	if _, err := pool.Exec(ctx, down); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+certificateAutoSendDefaultVersion); err != nil {
		t.Fatal(err)
	}
	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(ctx)
	if _, err := holder.Exec(ctx, `LOCK TABLE events IN ACCESS SHARE MODE`); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	err = migrate.Apply(ctx, pool)
	if err == nil || !strings.Contains(err.Error(), "lock timeout") {
		t.Fatalf("apply under a held lock = %v", err)
	}
	if waited := time.Since(started); waited > 30*time.Second {
		t.Fatalf("waited %v", waited)
	}
	if recorded(t, pool, certificateAutoSendDefaultVersion) {
		t.Fatal("a timed-out migration was recorded")
	}
	if err := holder.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if !recorded(t, pool, certificateAutoSendDefaultVersion) {
		t.Fatal("not applied once the lock was gone")
	}
}
