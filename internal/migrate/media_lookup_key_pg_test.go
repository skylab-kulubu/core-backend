package migrate_test

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
)

const mediaLookupKeyVersion = "20260929180000"

// lookupKeyIndexOID is the address lookup's index, 0 when there is none.
func lookupKeyIndexOID(t *testing.T, pool *pgxpool.Pool) uint32 {
	t.Helper()
	var oid uint32
	if err := pool.QueryRow(context.Background(),
		`SELECT COALESCE(to_regclass('public.media_lookup_key_idx')::oid, 0)`).Scan(&oid); err != nil {
		t.Fatal(err)
	}
	return oid
}

// lookupKeyHolds checks that media_lookup_key maps a faststart copy's key
// to its original's and leaves every other key alone, and that the index
// is built on it.
func lookupKeyHolds(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	const video = "videos/0f2a4c1e-6b7d-4e8f-9a0b-1c2d3e4f5a6b"
	for key, want := range map[string]string{
		video + ".fs.0123456789abcdef0123456789abcdef.mp4": video + ".mp4",
		video + ".mp4": video + ".mp4",
		"images/0f2a4c1e-6b7d-4e8f-9a0b-1c2d3e4f5a6b": "images/0f2a4c1e-6b7d-4e8f-9a0b-1c2d3e4f5a6b",
	} {
		var got string
		if err := pool.QueryRow(context.Background(), `SELECT media_lookup_key($1)`, key).Scan(&got); err != nil || got != want {
			t.Fatalf("media_lookup_key(%q) = %q, err %v; want %q", key, got, err, want)
		}
	}
	var def string
	if err := pool.QueryRow(context.Background(),
		`SELECT indexdef FROM pg_indexes WHERE schemaname = 'public' AND indexname = 'media_lookup_key_idx'`).Scan(&def); err != nil ||
		!strings.HasSuffix(def, "ON public.media USING btree (media_lookup_key(file_url))") {
		t.Fatalf("index %q, err %v", def, err)
	}
}

// A database that already has the lookup key is recorded without the index
// being built again; one that lost part of it is not taken for migrated:
// the migration runs again and puts it back.
func TestApplyRepairsTheMediaLookupKey(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	lookupKeyHolds(t, pool)

	built := lookupKeyIndexOID(t, pool)
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+mediaLookupKeyVersion); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if again := lookupKeyIndexOID(t, pool); again != built {
		t.Fatalf("the index was built again (%d, then %d): the fingerprint did not recognize it", built, again)
	}

	for name, damage := range map[string]string{
		"the index":    `DROP INDEX media_lookup_key_idx`,
		"the function": `DROP FUNCTION media_lookup_key(TEXT) CASCADE`,
		"another function": `CREATE OR REPLACE FUNCTION media_lookup_key(key TEXT) RETURNS TEXT
			LANGUAGE sql IMMUTABLE STRICT AS $$ SELECT key $$`,
		"a volatile function": `ALTER FUNCTION media_lookup_key(TEXT) VOLATILE`,
		"an index on the key alone": `DROP INDEX media_lookup_key_idx;
			CREATE INDEX media_lookup_key_idx ON media (file_url)`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+mediaLookupKeyVersion+`;`+damage); err != nil {
				t.Fatal(err)
			}
			if err := migrate.Apply(ctx, pool); err != nil {
				t.Fatal(err)
			}
			lookupKeyHolds(t, pool)
		})
	}
}
