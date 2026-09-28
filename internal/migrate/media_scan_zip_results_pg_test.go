package migrate_test

import (
	"context"
	"io/fs"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/db"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
)

const mediaScanZIPResultsVersion = "20260928180000"

// zipResults are the results of the scan's ZIP check.
var zipResults = []string{"archive_invalid", "archive_nested"}

// zipResultsHold reports whether a Media can be rejected with each of the
// ZIP check's results, with its rejection recorded, and only a rejected one.
func zipResultsHold(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	for _, result := range zipResults {
		id, err := scanMedia(t, pool, "rejected", result)
		if err != nil {
			t.Fatalf("a Media rejected as %s: %v", result, err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO media_scan_rejections (media_id, result, signature, rejected_at) VALUES ($1, $2, '', now())`, id, result); err != nil {
			t.Fatalf("its rejection as %s: %v", result, err)
		}
		for _, status := range []string{"pending", "attached", "scanning", "detached"} {
			if _, err := scanMedia(t, pool, status, result); err == nil {
				t.Errorf("a %s Media claimed %s", status, result)
			}
		}
	}
}

// A ZIP the scan's ZIP check refuses as malformed, or holding what clamd
// cannot read, is rejected as archive_invalid; one holding an archive core
// cannot check, as archive_nested. Each is recorded so; a Media that is not
// rejected cannot claim either.
func TestMediaScanZIPResults(t *testing.T) {
	pool := postgresPool(t)
	if err := migrate.Apply(context.Background(), pool); err != nil {
		t.Fatal(err)
	}
	zipResultsHold(t, pool)
}

// A database whose result checks lack the ZIP check's results (a rerun of
// 20260928140000 puts them back so) is not taken for migrated: the
// migration runs again. The malware scan's own fingerprint takes the checks
// with them for its own, so it is not run again for them.
func TestApplyRepairsTheZIPResults(t *testing.T) {
	scan, err := fs.ReadFile(db.UpSQL, "migrations/"+mediaMalwareScanVersion+"_media_malware_scan.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	for name, damage := range map[string]string{
		"a rerun of the malware scan migration": string(scan) + `; DROP TABLE schema_migrations`,
		"the media check without them": `DELETE FROM schema_migrations WHERE version = ` + mediaScanZIPResultsVersion + `;
			ALTER TABLE media DROP CONSTRAINT media_scan_result_check`,
		"the rejection check without them": `DELETE FROM schema_migrations WHERE version = ` + mediaScanZIPResultsVersion + `;
			ALTER TABLE media_scan_rejections DROP CONSTRAINT media_scan_rejections_result_check,
				ADD CONSTRAINT media_scan_rejections_result_check CHECK (result IN ('infected', 'too_large_to_scan', 'lost', 'scan_timeout', 'integrity', 'archive_invalid'))`,
	} {
		t.Run(name, func(t *testing.T) {
			pool := postgresPool(t)
			ctx := context.Background()
			if err := migrate.Apply(ctx, pool); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, damage); err != nil {
				t.Fatal(err)
			}
			if err := migrate.Apply(ctx, pool); err != nil {
				t.Fatal(err)
			}
			zipResultsHold(t, pool)
		})
	}

	// Recorded again from scratch, the malware scan's migration is found
	// present with archive_invalid in its checks.
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+mediaMalwareScanVersion); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	for _, result := range zipResults {
		if _, err := scanMedia(t, pool, "rejected", result); err != nil {
			t.Fatalf("the malware scan's migration ran again over %s: %v", result, err)
		}
	}
}

// Rolling the ZIP check's results back would lose why a Media was
// rejected: the down migration refuses while a Media or a rejection holds
// one.
func TestZIPResultsDownRefusesWhileAMediaHoldsOne(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	down, err := fs.ReadFile(db.DownSQL, "migrations/"+mediaScanZIPResultsVersion+"_media_scan_zip_results.down.sql")
	if err != nil {
		t.Fatal(err)
	}
	nested, err := scanMedia(t, pool, "rejected", "archive_nested")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(down)); err == nil || !strings.Contains(err.Error(), "archive_nested") {
		t.Fatalf("down with a Media rejected as archive_nested: err = %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE media SET scan_result = 'too_large_to_scan' WHERE id = $1`, nested); err != nil {
		t.Fatal(err)
	}
	id, err := scanMedia(t, pool, "rejected", "archive_invalid")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(down)); err == nil || !strings.Contains(err.Error(), "archive_invalid") {
		t.Fatalf("down with a Media rejected as archive_invalid: err = %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE media SET scan_result = 'too_large_to_scan' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(down)); err != nil {
		t.Fatalf("down: %v", err)
	}
	if _, err := scanMedia(t, pool, "rejected", "archive_invalid"); err == nil {
		t.Fatal("archive_invalid is still taken after the down migration")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+mediaScanZIPResultsVersion); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatalf("up again: %v", err)
	}
}
