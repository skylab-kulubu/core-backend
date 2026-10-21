package events_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/internal/events"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
)

// migrated is a database with core's schema and a table for a handler's
// effects.
func migrated(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `CREATE TABLE event_effects (consumer TEXT NOT NULL, event_id TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	return pool
}

func effect(consumer, id string) func(pgx.Tx) error {
	return func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), `INSERT INTO event_effects (consumer, event_id) VALUES ($1, $2)`, consumer, id)
		return err
	}
}

func count(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// The record and the handler's writes commit together or not at all, and a
// second consume of the same event runs nothing.
func TestPostgresConsume(t *testing.T) {
	pool := migrated(t)
	store := events.NewPostgres(pool)
	ctx := context.Background()

	dup, err := store.Consume(ctx, "core.forms", "/forms", "e1", effect("core.forms", "e1"))
	if err != nil || dup {
		t.Fatalf("first: %v %v", dup, err)
	}
	dup, err = store.Consume(ctx, "core.forms", "/forms", "e1", func(pgx.Tx) error {
		t.Fatal("ran the handler for a duplicate")
		return nil
	})
	if err != nil || !dup {
		t.Fatalf("second: %v %v", dup, err)
	}
	// Another consumer, or another source, is another event.
	for _, key := range [][2]string{{"core.keycloak", "/forms"}, {"core.forms", "/skymail"}} {
		if dup, err := store.Consume(ctx, key[0], key[1], "e1", effect(key[0], "e1")); err != nil || dup {
			t.Fatalf("%v: %v %v", key, dup, err)
		}
	}
	// A failed handler leaves neither its writes nor the record.
	boom := errors.New("boom")
	_, err = store.Consume(ctx, "core.forms", "/forms", "e2", func(tx pgx.Tx) error {
		if err := effect("core.forms", "e2")(tx); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if n := count(t, pool, `SELECT count(*) FROM consumed_events WHERE event_id = 'e2'`); n != 0 {
		t.Fatalf("records of e2 = %d", n)
	}
	if n := count(t, pool, `SELECT count(*) FROM event_effects`); n != 3 {
		t.Fatalf("effects = %d", n)
	}
	// ... so it is handled when it comes again.
	if dup, err := store.Consume(ctx, "core.forms", "/forms", "e2", effect("core.forms", "e2")); err != nil || dup {
		t.Fatalf("e2 again: %v %v", dup, err)
	}
}

// Two replicas handed the same event at once: the second waits for the
// first's transaction and finds it a duplicate.
func TestPostgresConsumeConcurrent(t *testing.T) {
	pool := migrated(t)
	store := events.NewPostgres(pool)
	ctx := context.Background()
	inside := make(chan struct{})
	release := make(chan struct{})
	first := make(chan error, 1)
	go func() {
		_, err := store.Consume(ctx, "core.forms", "/forms", "same", func(tx pgx.Tx) error {
			close(inside)
			<-release
			return effect("core.forms", "same")(tx)
		})
		first <- err
	}()
	<-inside
	second := make(chan bool, 1)
	go func() {
		dup, err := store.Consume(ctx, "core.forms", "/forms", "same", effect("core.forms", "same"))
		if err != nil {
			t.Error(err)
		}
		second <- dup
	}()
	testpostgres.WaitForBlockedQuery(t, pool, "INSERT INTO consumed_events")
	close(release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if !<-second {
		t.Fatal("the second consume was not a duplicate")
	}
	if n := count(t, pool, `SELECT count(*) FROM event_effects`); n != 1 {
		t.Fatalf("effects = %d", n)
	}
}

// The cleanup deletes the records past 30 days, in batches, and no others.
func TestPostgresRetention(t *testing.T) {
	pool := migrated(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		INSERT INTO consumed_events (consumer, source, event_id, processed_at)
		SELECT 'core.forms', '/forms', 'old-' || i, now() - interval '31 days' FROM generate_series(1, 2500) i
		UNION ALL
		SELECT 'core.forms', '/forms', 'new-' || i, now() - interval '29 days' FROM generate_series(1, 10) i
	`); err != nil {
		t.Fatal(err)
	}
	retention := events.NewRetention(events.NewPostgres(pool), nil)
	retention.Once(ctx)
	if n := count(t, pool, `SELECT count(*) FROM consumed_events`); n != 10 {
		t.Fatalf("left %d records", n)
	}
	if n := count(t, pool, `SELECT count(*) FROM consumed_events WHERE event_id LIKE 'new-%'`); n != 10 {
		t.Fatalf("recent records left = %d", n)
	}
	if got := retention.Prometheus(); got != "# TYPE skylab_events_records_deleted_total counter\nskylab_events_records_deleted_total 2500\n"+
		"# TYPE skylab_events_record_cleanup_failures_total counter\nskylab_events_record_cleanup_failures_total 0\n" {
		t.Fatal(got)
	}
	// Run cleans at once and stops with its context.
	runCtx, cancel := context.WithCancel(ctx)
	done := retention.Run(runCtx)
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not stop")
	}
}

// The table refuses what the consumer never writes: an empty or overlong
// key part.
func TestPostgresConsumedEventsChecks(t *testing.T) {
	pool := migrated(t)
	ctx := context.Background()
	for _, row := range [][3]string{{"", "/s", "1"}, {"c", "", "1"}, {"c", "/s", ""}, {"c", "/s", strings.Repeat("x", 256)}} {
		if _, err := pool.Exec(ctx, `INSERT INTO consumed_events (consumer, source, event_id) VALUES ($1, $2, $3)`, row[0], row[1], row[2]); err == nil {
			t.Fatalf("accepted %q", row)
		}
	}
}
