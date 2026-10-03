package migrate_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
)

const ticketsEventCreatedVersion = "20261003120000"

// ticketsEventCreatedIndex is the index's definition, "" when there is none.
func ticketsEventCreatedIndex(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var def string
	err := pool.QueryRow(context.Background(),
		`SELECT COALESCE((SELECT indexdef FROM pg_indexes WHERE schemaname = 'public' AND indexname = 'tickets_event_created_idx'), '')`).Scan(&def)
	if err != nil {
		t.Fatal(err)
	}
	return def
}

// The dashboard summary reads an Event's Tickets, and those of the last
// days, by this index. A database that has it is recorded without building
// it again; one that lost it, or holds another index under its name, gets
// it back.
func TestApplyRepairsTheTicketsEventCreatedIndex(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	const want = "CREATE INDEX tickets_event_created_idx ON public.tickets USING btree (event_id, created_at)"
	if got := ticketsEventCreatedIndex(t, pool); got != want {
		t.Fatalf("index %q", got)
	}
	for name, damage := range map[string]string{
		"recorded again": ``,
		"the index":      `DROP INDEX tickets_event_created_idx`,
		"another index under its name": `DROP INDEX tickets_event_created_idx;
			CREATE INDEX tickets_event_created_idx ON tickets (event_id)`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+ticketsEventCreatedVersion+`;`+damage); err != nil {
				t.Fatal(err)
			}
			if err := migrate.Apply(ctx, pool); err != nil {
				t.Fatal(err)
			}
			if got := ticketsEventCreatedIndex(t, pool); got != want {
				t.Fatalf("index %q", got)
			}
		})
	}
}
