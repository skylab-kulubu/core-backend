package formsfeed_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/internal/formsfeed"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
)

func migrated(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := testpostgres.Start(t)
	if err := migrate.Apply(context.Background(), pool); err != nil {
		t.Fatal(err)
	}
	return pool
}

func advance(t *testing.T, pool *pgxpool.Pool, id uuid.UUID, version int64, orEqual, commit bool) bool {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	ok, err := formsfeed.PostgresVersions{}.Advance(ctx, tx, formsfeed.KindResponse, id, version, orEqual)
	if err != nil {
		t.Fatal(err)
	}
	if commit {
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	return ok
}

func TestPostgresVersionsKeepTheLatest(t *testing.T) {
	pool := migrated(t)
	id := uuid.New()
	if !advance(t, pool, id, 5, false, true) {
		t.Fatal("first version refused")
	}
	if advance(t, pool, id, 5, false, true) || advance(t, pool, id, 4, false, true) || advance(t, pool, id, 4, true, true) {
		t.Fatal("an older or equal version advanced")
	}
	if !advance(t, pool, id, 5, true, true) {
		t.Fatal("orEqual refused the same version")
	}
	// A rolled back transaction keeps the old version.
	if !advance(t, pool, id, 9, false, false) || !advance(t, pool, id, 6, false, true) {
		t.Fatal("a rolled back version was kept")
	}
	// The same id of another kind is apart.
	ctx := context.Background()
	tx, _ := pool.Begin(ctx)
	ok, err := formsfeed.PostgresVersions{}.Advance(ctx, tx, formsfeed.KindForm, id, 1, false)
	tx.Rollback(ctx)
	if err != nil || !ok {
		t.Fatalf("form kind: %t %v", ok, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE forms_record_versions SET updated_at = now() - interval '31 days'`); err != nil {
		t.Fatal(err)
	}
	n, err := formsfeed.DeleteOldVersions(ctx, pool, time.Now().Add(-formsfeed.VersionRetention))
	if err != nil || n != 1 {
		t.Fatalf("deleted %d %v", n, err)
	}
}
