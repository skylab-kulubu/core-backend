package migrate_test

import (
	"context"
	"slices"
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/migrate"
)

const (
	eventStatusMailsVersion = "20261020140000"
	eventStatusMailsDown    = eventStatusMailsVersion + "_event_status_mails.down.sql"
)

func TestEventStatusMailsMigrationComesLast(t *testing.T) {
	t.Parallel()
	versions, err := migrate.Versions()
	if err != nil {
		t.Fatal(err)
	}
	before := slices.Index(versions, 20261020130000)
	this := slices.Index(versions, 20261020140000)
	if before < 0 || this < 0 || before >= this {
		t.Fatalf("versions %v", versions)
	}
}

// Down and up again keep working, and the fingerprint records a schema that
// has the queue.
func TestEventStatusMailsMigration(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, readDown(t, eventStatusMailsDown)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+eventStatusMailsVersion); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+eventStatusMailsVersion); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if !recorded(t, pool, eventStatusMailsVersion) {
		t.Fatal("not recorded by its fingerprint")
	}
}
