package migrate_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
)

// secondPool is another process's pool on the same database: a second
// replica starting at the same time.
func secondPool(t *testing.T, pool *pgxpool.Pool) *pgxpool.Pool {
	t.Helper()
	other, err := pgxpool.NewWithConfig(context.Background(), pool.Config().Copy())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(other.Close)
	return other
}

// Apply runs under one database-wide advisory lock: while another session
// holds it (another replica migrating), Apply waits and touches nothing, not
// even schema_migrations; once it is released Apply goes on and finds what
// the other one did.
func TestApplyWaitsWhileAnotherReplicaMigrates(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	holder, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Release()
	if _, err := holder.Exec(ctx, `SELECT pg_advisory_lock(hashtextextended($1, 0))`, migrate.LockName); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() { done <- migrate.Apply(ctx, secondPool(t, pool)) }()

	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND NOT granted`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting == 1 {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("Apply did not wait for the lock: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("Apply never asked for the lock")
		}
		time.Sleep(50 * time.Millisecond)
	}
	var table *string
	if err := pool.QueryRow(ctx, `SELECT to_regclass('public.schema_migrations')::text`).Scan(&table); err != nil {
		t.Fatal(err)
	}
	if table != nil {
		t.Fatal("Apply created schema_migrations before it held the lock")
	}

	if _, err := holder.Exec(ctx, `SELECT pg_advisory_unlock(hashtextextended($1, 0))`, migrate.LockName); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Minute):
		t.Fatal("Apply did not finish after the lock was released")
	}
	assertEveryVersionRecordedOnce(t, pool)
	// The lock goes with Apply's session: nothing holds it afterwards.
	var held int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_locks WHERE locktype = 'advisory'`).Scan(&held); err != nil {
		t.Fatal(err)
	}
	if held != 0 {
		t.Fatalf("%d advisory locks left after Apply", held)
	}
}

// Two replicas starting at once on an empty database both come up: one
// migrates, the other waits and then finds every version recorded, instead
// of racing it into a deadlock or a duplicate object and a restart.
func TestTwoReplicasApplyAtOnce(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	pools := []*pgxpool.Pool{pool, secondPool(t, pool), secondPool(t, pool)}
	errs := make(chan error, len(pools))
	start := make(chan struct{})
	for _, p := range pools {
		go func() {
			<-start
			errs <- migrate.Apply(ctx, p)
		}()
	}
	close(start)
	for range pools {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	assertEveryVersionRecordedOnce(t, pool)
}

func assertEveryVersionRecordedOnce(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	versions, err := migrate.Versions()
	if err != nil {
		t.Fatal(err)
	}
	var recorded, distinct int
	if err := pool.QueryRow(context.Background(), `SELECT count(*), count(DISTINCT version) FROM schema_migrations`).Scan(&recorded, &distinct); err != nil {
		t.Fatal(err)
	}
	if recorded != len(versions) || distinct != len(versions) {
		t.Fatalf("schema_migrations holds %d rows (%d versions), want %d", recorded, distinct, len(versions))
	}
}
