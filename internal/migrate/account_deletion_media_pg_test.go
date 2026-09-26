package migrate_test

import (
	"context"
	"io/fs"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/db"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

const accountDeletionMediaVersion = "20260927100000"

// deletionRequestWithRecord is a deletion request with one recorded
// personal Media, as anonymize_core leaves it.
func deletionRequestWithRecord(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	store := user.NewPostgresStore(pool)
	subjectID := uuid.New()
	if _, _, err := user.NewService(store).Ensure(ctx, subjectID, user.Profile{Email: subjectID.String() + "@example.test"}); err != nil {
		t.Fatal(err)
	}
	request, err := store.RequestDeletion(ctx, subjectID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO account_deletion_media (request_id, media_id) VALUES ($1, $2)`, request.ID, uuid.New()); err != nil {
		t.Fatal(err)
	}
	return request.ID
}

func mediaRecords(t *testing.T, pool *pgxpool.Pool, requestID uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM account_deletion_media WHERE request_id = $1`, requestID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// A record left when its request is completed is a personal Media that was
// never purged, and the only way back to it: completion is refused while one
// is left (no id in the error), so the worker retries and, failing, ends in
// manual intervention. Without records the request completes.
func TestDeletionRequestCannotCompleteWithMediaRecordsLeft(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	requestID := deletionRequestWithRecord(t, pool)
	var mediaID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT media_id FROM account_deletion_media WHERE request_id = $1`, requestID).Scan(&mediaID); err != nil {
		t.Fatal(err)
	}
	complete := `UPDATE account_deletion_requests SET status = 'completed', completed_at = now() WHERE id = $1`

	_, err := pool.Exec(ctx, complete, requestID)
	if err == nil || !strings.Contains(err.Error(), "account erasure has personal Media left to purge") {
		t.Fatalf("completion with a record left: %v", err)
	}
	if strings.Contains(err.Error(), mediaID.String()) || strings.Contains(err.Error(), requestID.String()) {
		t.Fatalf("the refusal names an id: %v", err)
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM account_deletion_requests WHERE id = $1`, requestID).Scan(&status); err != nil || status == "completed" {
		t.Fatalf("status %q err %v after the refused completion", status, err)
	}
	if got := mediaRecords(t, pool, requestID); got != 1 {
		t.Fatalf("records after the refused completion = %d, want 1", got)
	}

	if _, err := pool.Exec(ctx, `DELETE FROM account_deletion_media WHERE request_id = $1`, requestID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, complete, requestID); err != nil {
		t.Fatalf("completion without records: %v", err)
	}
}

// A record is a personal Media the erasure still has to purge and the only
// way back to it, so the rollback refuses while one is left. Without one it
// is clean, and the up runs again.
func TestAccountDeletionMediaDownRefusesWhileAPurgeIsLeft(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	requestID := deletionRequestWithRecord(t, pool)
	down, err := fs.ReadFile(db.DownSQL, "migrations/"+accountDeletionMediaVersion+"_account_deletion_media.down.sql")
	if err != nil {
		t.Fatal(err)
	}
	up, err := fs.ReadFile(db.UpSQL, "migrations/"+accountDeletionMediaVersion+"_account_deletion_media.up.sql")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := pool.Exec(ctx, string(down)); err == nil || !strings.Contains(err.Error(), "personal Media left to purge") {
		t.Fatalf("down with a record left: %v", err)
	}
	if got := mediaRecords(t, pool, requestID); got != 1 {
		t.Fatalf("records after the refused down = %d", got)
	}

	if _, err := pool.Exec(ctx, `DELETE FROM account_deletion_media WHERE request_id = $1`, requestID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(down)); err != nil {
		t.Fatalf("down without records: %v", err)
	}
	var table bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('public.account_deletion_media') IS NOT NULL`).Scan(&table); err != nil || table {
		t.Fatalf("table after down = %v err %v", table, err)
	}
	for range 2 {
		if _, err := pool.Exec(ctx, string(up)); err != nil {
			t.Fatalf("up after down: %v", err)
		}
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+accountDeletionMediaVersion); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatalf("apply records the rerun schema: %v", err)
	}
}

func TestApplyRepairsAMissingDeletionMediaCompletionGuard(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		DELETE FROM schema_migrations WHERE version = `+accountDeletionMediaVersion+`;
		DROP TRIGGER account_deletion_requests_require_media_erased ON account_deletion_requests;`); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	var restored bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_trigger
		WHERE tgrelid = 'account_deletion_requests'::regclass AND tgname = 'account_deletion_requests_require_media_erased')`).Scan(&restored); err != nil || !restored {
		t.Fatalf("completion guard restored %v, err %v", restored, err)
	}
}
