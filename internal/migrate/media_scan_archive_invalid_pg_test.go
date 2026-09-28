package migrate_test

import (
	"context"
	"io/fs"
	"strings"
	"testing"

	"github.com/skylab-kulubu/core-backend/db"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
)

const mediaScanArchiveInvalidVersion = "20260928180000"

// A ZIP the scan's ZIP check finds malformed, or holding what clamd cannot
// read, is rejected as archive_invalid and recorded so; a Media that is not
// rejected cannot claim it.
func TestMediaScanArchiveInvalid(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	id, err := scanMedia(t, pool, "rejected", "archive_invalid")
	if err != nil {
		t.Fatalf("a Media rejected as archive_invalid: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO media_scan_rejections (media_id, result, signature, rejected_at) VALUES ($1, 'archive_invalid', '', now())`, id); err != nil {
		t.Fatalf("its rejection: %v", err)
	}
	for _, status := range []string{"pending", "attached", "scanning", "detached"} {
		if _, err := scanMedia(t, pool, status, "archive_invalid"); err == nil {
			t.Errorf("a %s Media claimed archive_invalid", status)
		}
	}
}

// A database whose result checks lack archive_invalid (a rerun of
// 20260928140000 puts them back so) is not taken for migrated: the
// migration runs again. The malware scan's own fingerprint takes the checks
// with archive_invalid for its own, so it is not run again for them.
func TestApplyRepairsTheArchiveInvalidResult(t *testing.T) {
	scan, err := fs.ReadFile(db.UpSQL, "migrations/"+mediaMalwareScanVersion+"_media_malware_scan.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	for name, damage := range map[string]string{
		"a rerun of the malware scan migration": string(scan) + `; DROP TABLE schema_migrations`,
		"the media check without it": `DELETE FROM schema_migrations WHERE version = ` + mediaScanArchiveInvalidVersion + `;
			ALTER TABLE media DROP CONSTRAINT media_scan_result_check`,
		"the rejection check without it": `DELETE FROM schema_migrations WHERE version = ` + mediaScanArchiveInvalidVersion + `;
			ALTER TABLE media_scan_rejections DROP CONSTRAINT media_scan_rejections_result_check,
				ADD CONSTRAINT media_scan_rejections_result_check CHECK (result IN ('infected', 'too_large_to_scan', 'lost', 'scan_timeout', 'integrity'))`,
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
			id, err := scanMedia(t, pool, "rejected", "archive_invalid")
			if err != nil {
				t.Fatalf("a Media rejected as archive_invalid: %v", err)
			}
			if _, err := pool.Exec(ctx, `INSERT INTO media_scan_rejections (media_id, result, signature, rejected_at) VALUES ($1, 'archive_invalid', '', now())`, id); err != nil {
				t.Fatalf("its rejection: %v", err)
			}
			if _, err := scanMedia(t, pool, "pending", "archive_invalid"); err == nil {
				t.Error("a pending Media claimed archive_invalid")
			}
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
	if _, err := scanMedia(t, pool, "rejected", "archive_invalid"); err != nil {
		t.Fatalf("the malware scan's migration ran again over archive_invalid: %v", err)
	}
}

// Rolling archive_invalid back would lose why a Media was rejected: the
// down migration refuses while a Media or a rejection holds it.
func TestArchiveInvalidDownRefusesWhileAMediaHoldsIt(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	id, err := scanMedia(t, pool, "rejected", "archive_invalid")
	if err != nil {
		t.Fatal(err)
	}
	down, err := fs.ReadFile(db.DownSQL, "migrations/"+mediaScanArchiveInvalidVersion+"_media_scan_archive_invalid.down.sql")
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
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+mediaScanArchiveInvalidVersion); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatalf("up again: %v", err)
	}
}
