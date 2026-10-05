package migrate_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
)

const urlHitsIndexVersion = "20261005140000"

func urlHitsIndexState(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var state string
	if err := pool.QueryRow(context.Background(), `
		SELECT COALESCE((SELECT CASE WHEN i.indisvalid THEN 'valid' ELSE 'invalid' END || ':' || (i.indpred IS NOT NULL)::text
			FROM pg_index i WHERE i.indexrelid = to_regclass('public.url_hits_personal_at_idx')), 'none')`).Scan(&state); err != nil {
		t.Fatal(err)
	}
	return state
}

// The click index is built with CREATE INDEX CONCURRENTLY, so building it
// on a live url_hits never blocks a click. The statement cannot run in a
// transaction, so its migration is that statement alone. A build that
// stopped midway (a deploy killed it) leaves an invalid index: the next
// start drops it and builds it again, instead of recording or keeping a
// broken one.
func TestApplyBuildsTheClickIndexConcurrentlyAndRepairsAnInvalidOne(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if got := urlHitsIndexState(t, pool); got != "valid:true" {
		t.Fatalf("index %s", got)
	}
	for name, damage := range map[string]string{
		"recorded again": ``,
		"left invalid": `UPDATE pg_index SET indisvalid = false
			WHERE indexrelid = to_regclass('public.url_hits_personal_at_idx')`,
		"gone": `DROP INDEX url_hits_personal_at_idx`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+urlHitsIndexVersion); err != nil {
				t.Fatal(err)
			}
			if damage != "" {
				if _, err := pool.Exec(ctx, damage); err != nil {
					t.Fatal(err)
				}
			}
			if err := migrate.Apply(ctx, pool); err != nil {
				t.Fatal(err)
			}
			if got := urlHitsIndexState(t, pool); got != "valid:true" {
				t.Fatalf("index %s", got)
			}
		})
	}
}
