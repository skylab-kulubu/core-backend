package migrate_test

import (
	"context"
	"encoding/json"
	"io/fs"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/db"
	"github.com/skylab-kulubu/core-backend/internal/certificate"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/eventmail"
	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/shorturl"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

func TestAccountLifecycleDownRefusesToDropAntiResurrectionState(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := user.NewPostgresStore(pool)
	subjectID := uuid.New()
	if _, _, err := user.NewService(store).Ensure(ctx, subjectID, user.Profile{Email: "rollback@example.test"}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET account_state='deletion_pending' WHERE id=$1`, subjectID); err != nil {
		t.Fatal(err)
	}
	down, err := fs.ReadFile(db.DownSQL, "migrations/20260920010000_account_lifecycle.down.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(down)); err == nil || !strings.Contains(err.Error(), "anti-resurrection") {
		t.Fatalf("down migration with non-active identity error = %v", err)
	}

	if _, err := pool.Exec(ctx, `UPDATE users SET account_state='active', deletion_requested_at=NULL WHERE id=$1`, subjectID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RequestDeletion(ctx, subjectID, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.AnonymizeAccount(ctx, subjectID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE account_deletion_requests SET status='completed', completed_at=now() WHERE subject_id=$1`, subjectID); err != nil {
		t.Fatal(err)
	}
	if err := store.HardPurgeAccount(ctx, subjectID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(down)); err == nil || !strings.Contains(err.Error(), "anti-resurrection") {
		t.Fatalf("down migration with hard-purge marker error = %v", err)
	}
	var markers int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM account_deletion_requests WHERE subject_id=$1`, subjectID).Scan(&markers); err != nil || markers != 1 {
		t.Fatalf("anti-resurrection markers=%d err=%v", markers, err)
	}
}

func TestAccountAccessProjectionDownRefusesAnyDurableDeletionRequest(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := user.NewPostgresStore(pool)
	subjectID := uuid.New()
	if _, _, err := user.NewService(store).Ensure(ctx, subjectID, user.Profile{Email: "gate-rollback@example.test"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RequestDeletion(ctx, subjectID, nil); err != nil {
		t.Fatal(err)
	}
	down, err := fs.ReadFile(db.DownSQL, "migrations/20260920120000_account_access_projection.down.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(down)); err == nil || !strings.Contains(err.Error(), "durable deletion request") {
		t.Fatalf("down migration error = %v", err)
	}
	var columnExists bool
	if err := pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM information_schema.columns
			WHERE table_schema='public' AND table_name='account_deletion_requests' AND column_name='platform_blocked_at'
		)
	`).Scan(&columnExists); err != nil || !columnExists {
		t.Fatalf("projection column exists=%v err=%v", columnExists, err)
	}
}

func TestApplyRepairsBrownfieldSchema(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()

	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `SELECT extra_form_urls FROM events`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `SELECT cover_colors, cover_colors_computed FROM media`); err != nil {
		t.Fatal(err)
	}
	assertLifecycleSchema(t, pool)

	if _, err := pool.Exec(ctx, `ALTER TABLE events DROP COLUMN extra_form_urls`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DROP INDEX url_hits_url_id_at_idx`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `ALTER TABLE urls DROP COLUMN disabled_at`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `ALTER TABLE media DROP COLUMN deleted_by CASCADE`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DROP INDEX events_current_owner_team_idx`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DROP TABLE schema_migrations`); err != nil {
		t.Fatal(err)
	}

	_, err := pool.Exec(ctx, `SELECT extra_form_urls FROM events`)
	if err == nil {
		t.Fatal("expected missing column")
	}
	msg := err.Error()
	if !strings.Contains(msg, "extra_form_urls") || !strings.Contains(msg, "42703") {
		t.Fatalf("want SQLSTATE 42703 extra_form_urls, got %v", err)
	}

	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `SELECT extra_form_urls FROM events`); err != nil {
		t.Fatal(err)
	}
	var indexCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_indexes WHERE schemaname = 'public' AND indexname = 'url_hits_url_id_at_idx'`).Scan(&indexCount); err != nil {
		t.Fatal(err)
	}
	if indexCount != 1 {
		t.Fatalf("url_hits index count = %d", indexCount)
	}
	assertLifecycleSchema(t, pool)
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
}

func assertLifecycleSchema(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	var count int
	if err := pool.QueryRow(context.Background(), `
		SELECT count(*)
		FROM (VALUES
			('events', 'archived_at'),
			('events', 'archived_by'),
			('event_days', 'archived_at'),
			('event_days', 'archived_by'),
			('sessions', 'archived_at'),
			('sessions', 'archived_by'),
			('seasons', 'archived_at'),
			('seasons', 'archived_by'),
			('competitors', 'withdrawn_at'),
			('competitors', 'withdrawn_by'),
			('media', 'deleted_at'),
			('media', 'deleted_by'),
			('urls', 'disabled_at'),
			('urls', 'disabled_by')
		) AS expected(table_name, column_name)
		JOIN information_schema.columns actual
		  ON actual.table_schema = 'public'
		 AND actual.table_name = expected.table_name
		 AND actual.column_name = expected.column_name
	`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 14 {
		t.Fatalf("lifecycle column count = %d, want 14", count)
	}
	if err := pool.QueryRow(context.Background(), `
		SELECT count(*)
		FROM (VALUES
			('events_current_owner_team_idx'),
			('event_days_current_event_idx'),
			('sessions_current_event_day_idx'),
			('seasons_current_start_date_idx'),
			('competitors_current_event_idx'),
			('competitors_current_user_idx'),
			('media_current_created_at_idx'),
			('urls_current_created_by_idx')
		) AS expected(index_name)
		JOIN pg_indexes actual
		  ON actual.schemaname = 'public'
		 AND actual.indexname = expected.index_name
	`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 8 {
		t.Fatalf("lifecycle index count = %d, want 8", count)
	}
}

func postgresPool(t *testing.T) *pgxpool.Pool {
	return testpostgres.Start(t)
}

func TestApplyFreshThenIdempotent(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("no recorded versions")
	}
	if _, err := pool.Exec(ctx, `SELECT extra_form_urls FROM events`); err != nil {
		t.Fatal(err)
	}
	var templateName, scope, scopeKey string
	var rawLayout []byte
	var system bool
	if err := pool.QueryRow(ctx, `SELECT name,system,draft_layout FROM certificate_templates WHERE source_ref='system-default'`).Scan(&templateName, &system, &rawLayout); err != nil {
		t.Fatal(err)
	}
	if templateName != "SKY LAB Varsayılan Sertifika" || !system {
		t.Fatalf("certificate default = %q system=%v", templateName, system)
	}
	var layout certificate.Layout
	if err := json.Unmarshal(rawLayout, &layout); err != nil {
		t.Fatal(err)
	}
	if err := certificate.ValidateLayout(layout); err != nil {
		t.Fatalf("certificate default layout: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT scope,scope_key FROM certificate_template_bindings WHERE scope='club'`).Scan(&scope, &scopeKey); err != nil {
		t.Fatal(err)
	}
	if scope != "club" || scopeKey != "SKY_LAB" {
		t.Fatalf("certificate binding = %q %q", scope, scopeKey)
	}
	if _, err := pool.Exec(ctx, `SELECT asset_manifest FROM certificate_template_versions`); err != nil {
		t.Fatal(err)
	}
	assertDoorStoreQueries(t, pool)
}

func TestApplyCreatesAccountLifecycleSchemaWithoutCascadeDelete(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}

	var lifecycleColumns int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		FROM (VALUES
			('account_state'),
			('deletion_requested_at'),
			('anonymized_at')
		) AS expected(column_name)
		JOIN information_schema.columns actual
		  ON actual.table_schema = 'public'
		 AND actual.table_name = 'users'
		 AND actual.column_name = expected.column_name
	`).Scan(&lifecycleColumns); err != nil {
		t.Fatal(err)
	}
	if lifecycleColumns != 3 {
		t.Fatalf("user lifecycle column count = %d, want 3", lifecycleColumns)
	}

	for _, table := range []string{"account_deletion_requests", "account_deletion_steps", "account_deletion_outbox"} {
		var exists bool
		if err := pool.QueryRow(ctx, `SELECT to_regclass('public.' || $1) IS NOT NULL`, table).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if !exists {
			t.Fatalf("missing table %s", table)
		}
	}

	for _, fk := range []struct {
		table  string
		column string
	}{
		{table: "competitors", column: "user_id"},
		{table: "media", column: "uploaded_by"},
	} {
		var nullable, deleteAction string
		if err := pool.QueryRow(ctx, `
			SELECT c.is_nullable, CASE con.confdeltype WHEN 'n' THEN 'SET NULL' ELSE con.confdeltype::text END
			FROM information_schema.columns c
			JOIN pg_attribute a
			  ON a.attrelid = to_regclass('public.' || $1::text) AND a.attname = c.column_name
		JOIN pg_constraint con
		  ON con.conrelid = a.attrelid AND a.attnum = ANY(con.conkey) AND con.contype = 'f'
			WHERE c.table_schema = 'public' AND c.table_name = $1::text AND c.column_name = $2
		`, fk.table, fk.column).Scan(&nullable, &deleteAction); err != nil {
			t.Fatal(err)
		}
		if nullable != "YES" || deleteAction != "SET NULL" {
			t.Fatalf("%s.%s nullable=%s on delete=%s", fk.table, fk.column, nullable, deleteAction)
		}
	}
}

func TestApplyRepairsPartialAccountLifecycleFingerprintDrift(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}

	if _, err := pool.Exec(ctx, `
		DELETE FROM schema_migrations WHERE version = 20260920010000;
		ALTER TABLE competitors
			DROP CONSTRAINT competitors_user_id_fkey,
			ALTER COLUMN user_id SET NOT NULL,
			ADD CONSTRAINT competitors_user_id_fkey FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE;
		ALTER TABLE media
			DROP CONSTRAINT media_uploaded_by_fkey,
			ALTER COLUMN uploaded_by SET NOT NULL,
			ADD CONSTRAINT media_uploaded_by_fkey FOREIGN KEY (uploaded_by) REFERENCES users(id) ON DELETE CASCADE;
		ALTER TABLE account_deletion_requests
			DROP COLUMN lease_token,
			DROP COLUMN profile_media_id,
			DROP CONSTRAINT account_deletion_requests_status_check,
			ADD CONSTRAINT account_deletion_requests_status_check CHECK (status <> '');
		DROP INDEX account_deletion_requests_claim_idx;
		CREATE INDEX account_deletion_requests_claim_idx ON account_deletion_requests (id);
		DROP TRIGGER events_require_active_archiver ON events;
	`); err != nil {
		t.Fatal(err)
	}

	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}

	var repairedReferences int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		FROM (VALUES ('competitors', 'user_id'), ('media', 'uploaded_by')) expected(table_name, column_name)
		JOIN pg_attribute attribute
		  ON attribute.attrelid = to_regclass('public.' || expected.table_name)
		 AND attribute.attname = expected.column_name
		 AND NOT attribute.attnotnull
		JOIN pg_constraint foreign_key
		  ON foreign_key.conrelid = attribute.attrelid
		 AND foreign_key.contype = 'f'
		 AND attribute.attnum = ANY(foreign_key.conkey)
		 AND foreign_key.confdeltype = 'n'
	`).Scan(&repairedReferences); err != nil {
		t.Fatal(err)
	}
	if repairedReferences != 2 {
		t.Fatalf("repaired nullable SET NULL references = %d", repairedReferences)
	}
	var repairedObjects int
	if err := pool.QueryRow(ctx, `
		SELECT
			(SELECT count(*) FROM information_schema.columns
			 WHERE table_schema='public' AND table_name='account_deletion_requests'
			   AND column_name IN ('lease_token','profile_media_id'))
			+ (SELECT count(*) FROM pg_constraint
			   WHERE conrelid='account_deletion_requests'::regclass
			     AND conname='account_deletion_requests_status_check')
			+ CASE WHEN to_regclass('public.account_deletion_requests_claim_idx') IS NULL THEN 0 ELSE 1 END
			+ (SELECT count(DISTINCT trigger_name) FROM information_schema.triggers
			   WHERE trigger_schema='public' AND trigger_name='events_require_active_archiver')
	`).Scan(&repairedObjects); err != nil {
		t.Fatal(err)
	}
	if repairedObjects != 5 {
		t.Fatalf("repaired lifecycle objects = %d, want 5", repairedObjects)
	}
	var statusConstraint string
	if err := pool.QueryRow(ctx, `
		SELECT pg_get_constraintdef(oid) FROM pg_constraint
		WHERE conrelid='account_deletion_requests'::regclass
		  AND conname='account_deletion_requests_status_check'
	`).Scan(&statusConstraint); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(statusConstraint, "manual_intervention") {
		t.Fatalf("status constraint was not repaired: %s", statusConstraint)
	}
	var claimIndex string
	if err := pool.QueryRow(ctx, `SELECT indexdef FROM pg_indexes WHERE schemaname='public' AND indexname='account_deletion_requests_claim_idx'`).Scan(&claimIndex); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(claimIndex, "next_attempt_at, created_at") || !strings.Contains(claimIndex, "processing") {
		t.Fatalf("claim index was not repaired: %s", claimIndex)
	}
	var recorded bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version=20260920010000)`).Scan(&recorded); err != nil || !recorded {
		t.Fatalf("migration recorded=%v err=%v", recorded, err)
	}
	var beforeFingerprintOID, afterFingerprintOID uint32
	if err := pool.QueryRow(ctx, `SELECT 'users_account_state_idx'::regclass::oid`).Scan(&beforeFingerprintOID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version=20260920010000`); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT 'users_account_state_idx'::regclass::oid`).Scan(&afterFingerprintOID); err != nil {
		t.Fatal(err)
	}
	if afterFingerprintOID != beforeFingerprintOID {
		t.Fatalf("repaired schema did not satisfy fingerprint; index recreated: %d -> %d", beforeFingerprintOID, afterFingerprintOID)
	}
}

func TestApplyRepairsPermissiveLifecycleCheckBeforeRecording(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		DELETE FROM schema_migrations WHERE version = 20260920010000;
		ALTER TABLE account_deletion_requests
			DROP CONSTRAINT account_deletion_requests_status_check,
			ADD CONSTRAINT account_deletion_requests_status_check
			CHECK (status IN ('pending', 'processing', 'completed', 'manual_intervention') OR TRUE);
	`); err != nil {
		t.Fatal(err)
	}

	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	var definition string
	if err := pool.QueryRow(ctx, `
		SELECT pg_get_constraintdef(oid)
		FROM pg_constraint
		WHERE conrelid='account_deletion_requests'::regclass
		  AND conname='account_deletion_requests_status_check'
	`).Scan(&definition); err != nil {
		t.Fatal(err)
	}
	const expected = "CHECK ((status = ANY (ARRAY['pending'::text, 'processing'::text, 'completed'::text, 'manual_intervention'::text])))"
	if definition != expected {
		t.Fatalf("permissive status constraint survived migration: %s", definition)
	}
	var recorded bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version=20260920010000)`).Scan(&recorded); err != nil || !recorded {
		t.Fatalf("migration recorded=%v err=%v", recorded, err)
	}
}

func TestApplyDoesNotRecordUnrepairablePartialAccountLifecycleSchema(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		DELETE FROM schema_migrations WHERE version=20260920010000;
		ALTER TABLE account_deletion_requests DROP COLUMN status CASCADE;
	`); err != nil {
		t.Fatal(err)
	}

	err := migrate.Apply(ctx, pool)
	if err == nil || !strings.Contains(err.Error(), "migration 20260920010000") || !strings.Contains(err.Error(), "status") {
		t.Fatalf("partial lifecycle error = %v", err)
	}
	var recorded bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version=20260920010000)`).Scan(&recorded); err != nil {
		t.Fatal(err)
	}
	if recorded {
		t.Fatal("unrepairable partial lifecycle schema was recorded as applied")
	}
}

func TestApplyRepairsNoopAccountReferenceGuardBeforeRecording(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		DELETE FROM schema_migrations WHERE version=20260920010000;
		CREATE OR REPLACE FUNCTION public.require_active_account_reference()
		RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			-- TG_ARGV array_agg(DISTINCT value ORDER BY value)
			-- pg_advisory_xact_lock account_state = 'active'
			-- account_deletion_requests ERRCODE = '23514'
			RETURN NEW;
		END
		$$;
	`); err != nil {
		t.Fatal(err)
	}

	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	var source string
	var recorded bool
	if err := pool.QueryRow(ctx, `
		SELECT prosrc FROM pg_proc
		WHERE oid=to_regprocedure('public.require_active_account_reference()')
	`).Scan(&source); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version=20260920010000)`).Scan(&recorded); err != nil {
		t.Fatal(err)
	}
	if !recorded || !strings.Contains(source, "pg_advisory_xact_lock") || !strings.Contains(source, "account_state = 'active'") {
		t.Fatalf("guard recorded=%v source=%s", recorded, source)
	}
}

func TestApplyRepairsTriggerArgumentSubstringBypassBeforeRecording(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		DELETE FROM schema_migrations WHERE version=20260920010000;
		DROP TRIGGER tickets_require_active_owner ON tickets;
		CREATE TRIGGER tickets_require_active_owner
		BEFORE INSERT OR UPDATE OF owner_id ON tickets
		FOR EACH ROW EXECUTE FUNCTION public.require_active_account_reference('not_owner_id');
	`); err != nil {
		t.Fatal(err)
	}

	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	var argsHex string
	if err := pool.QueryRow(ctx, `
		SELECT encode(tgargs, 'hex')
		FROM pg_trigger
		WHERE tgrelid='tickets'::regclass
		  AND tgname='tickets_require_active_owner'
		  AND NOT tgisinternal
	`).Scan(&argsHex); err != nil {
		t.Fatal(err)
	}
	if argsHex != "6f776e65725f696400" {
		t.Fatalf("ticket trigger tgargs hex = %q", argsHex)
	}
}

func TestApplyRepairsMalformedAccountReferenceTriggerBeforeRecording(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		DELETE FROM schema_migrations WHERE version=20260920010000;
		CREATE FUNCTION public.noop_account_reference_guard() RETURNS trigger
		LANGUAGE plpgsql AS $$ BEGIN RETURN NEW; END $$;
		DROP TRIGGER tickets_require_active_owner ON tickets;
		CREATE TRIGGER tickets_require_active_owner
		BEFORE INSERT OR UPDATE OF owner_id ON tickets
		FOR EACH ROW EXECUTE FUNCTION public.noop_account_reference_guard();
	`); err != nil {
		t.Fatal(err)
	}

	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	var functionName string
	if err := pool.QueryRow(ctx, `
		SELECT function.proname
		FROM pg_trigger trigger
		JOIN pg_proc function ON function.oid=trigger.tgfoid
		WHERE trigger.tgrelid='tickets'::regclass
		  AND trigger.tgname='tickets_require_active_owner'
		  AND NOT trigger.tgisinternal
	`).Scan(&functionName); err != nil {
		t.Fatal(err)
	}
	if functionName != "require_active_account_reference" {
		t.Fatalf("ticket trigger function = %q", functionName)
	}
}

func TestApplyDoesNotRecordMalformedLifecycleStructuralContract(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		DELETE FROM schema_migrations WHERE version=20260920010000;
		ALTER TABLE account_deletion_steps
			DROP CONSTRAINT account_deletion_steps_request_id_fkey;
		ALTER TABLE account_deletion_outbox
			DROP CONSTRAINT account_deletion_outbox_request_id_fkey,
			DROP CONSTRAINT account_deletion_outbox_request_id_key;
		ALTER TABLE account_deletion_requests
			DROP CONSTRAINT account_deletion_requests_pkey;
	`); err != nil {
		t.Fatal(err)
	}

	err := migrate.Apply(ctx, pool)
	if err == nil || !strings.Contains(err.Error(), "migration 20260920010000 postcondition") {
		t.Fatalf("malformed structural contract error = %v", err)
	}
	var recorded bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version=20260920010000)`).Scan(&recorded); err != nil {
		t.Fatal(err)
	}
	if recorded {
		t.Fatal("migration version recorded despite missing primary/foreign/unique contract")
	}
}

func TestAccountLifecycleDownSerializesWithDeletionRequest(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	users := user.NewPostgresStore(pool)
	subjectID := uuid.New()
	if _, _, err := user.NewService(users).Ensure(ctx, subjectID, user.Profile{Email: "down-race@example.test"}); err != nil {
		t.Fatal(err)
	}
	down, err := fs.ReadFile(db.DownSQL, "migrations/20260920010000_account_lifecycle.down.sql")
	if err != nil {
		t.Fatal(err)
	}

	blocker, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(ctx)
	if _, err := blocker.Exec(ctx, `LOCK TABLE account_deletion_requests IN ACCESS SHARE MODE`); err != nil {
		t.Fatal(err)
	}
	downDone := make(chan error, 1)
	go func() {
		_, err := pool.Exec(ctx, string(down))
		downDone <- err
	}()
	testpostgres.WaitForBlockedQuery(t, pool, "LOCK TABLE tickets, competitors")

	deletionDone := make(chan error, 1)
	go func() {
		_, err := users.RequestDeletion(ctx, subjectID, nil)
		deletionDone <- err
	}()
	testpostgres.WaitForBlockedQuery(t, pool, "SELECT id, subject_id, requested_by")

	if err := blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-downDone; err != nil {
		t.Fatalf("down migration: %v", err)
	}
	if err := <-deletionDone; err == nil {
		t.Fatal("deletion request committed after lifecycle schema was removed")
	}
	var lifecycleTablePresent bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('public.account_deletion_requests') IS NOT NULL`).Scan(&lifecycleTablePresent); err != nil {
		t.Fatal(err)
	}
	if lifecycleTablePresent {
		t.Fatal("account lifecycle table remained after successful down migration")
	}
}

func TestAccountLifecycleDownLocksGuardedLeafBeforeDeletionMarker(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	users := user.NewPostgresStore(pool)
	ownerID := uuid.New()
	if _, _, err := user.NewService(users).Ensure(ctx, ownerID, user.Profile{Email: "down-writer@example.test"}); err != nil {
		t.Fatal(err)
	}
	eventRow, err := event.NewPostgresStore(pool).Create(ctx, event.Event{Name: "Down writer", Location: "YTÜ", OwnerTeam: "WEBLAB"})
	if err != nil {
		t.Fatal(err)
	}
	down, err := fs.ReadFile(db.DownSQL, "migrations/20260920010000_account_lifecycle.down.sql")
	if err != nil {
		t.Fatal(err)
	}

	blocker, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(ctx)
	if _, err := blocker.Exec(ctx, `LOCK TABLE events IN ACCESS SHARE MODE`); err != nil {
		t.Fatal(err)
	}
	downDone := make(chan error, 1)
	go func() {
		_, err := pool.Exec(ctx, string(down))
		downDone <- err
	}()
	testpostgres.WaitForBlockedQuery(t, pool, "LOCK TABLE tickets, competitors")

	writerDone := make(chan error, 1)
	go func() {
		_, err := pool.Exec(ctx, `
			INSERT INTO tickets (id,event_id,ticket_type,owner_id)
			VALUES ($1,$2,'REGISTERED',$3)
		`, uuid.New(), eventRow.ID, ownerID)
		writerDone <- err
	}()
	testpostgres.WaitForBlockedQuery(t, pool, "INSERT INTO tickets")
	if err := blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-downDone; err != nil {
		t.Fatalf("down migration: %v", err)
	}
	if err := <-writerDone; err != nil {
		t.Fatalf("guarded writer after down: %v", err)
	}
}

func assertDoorStoreQueries(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	ownerID := uuid.New()
	if _, _, err := user.NewService(user.NewPostgresStore(pool)).Ensure(ctx, ownerID, user.Profile{
		Email: "ada@example.com", FirstName: "Ada", LastName: "Lovelace", Username: "ada",
	}); err != nil {
		t.Fatal(err)
	}
	eventStore := event.NewPostgresStore(pool)
	cover, err := media.NewPostgresStore(pool).Create(ctx, media.Media{
		Name:                "cover.png",
		Type:                "image/png",
		Key:                 "images/cover",
		Kind:                media.KindImage,
		UploadedBy:          ownerID,
		CoverColors:         []string{"#3c82be", "#8a642f"},
		CoverColorsComputed: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	ev, err := eventStore.Create(ctx, event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB", CoverImageID: &cover.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(ev.CoverColors) != 2 || ev.CoverColors[0] != "#3c82be" {
		t.Fatalf("event cover colors %#v", ev.CoverColors)
	}
	day, err := eventStore.CreateDay(ctx, event.Day{EventID: ev.ID, Name: "Gün 1"})
	if err != nil {
		t.Fatal(err)
	}
	session, err := eventStore.CreateSession(ctx, event.Session{
		EventDayID: day.ID, Title: "Açılış", SessionType: "PRESENTATION",
	})
	if err != nil {
		t.Fatal(err)
	}
	ticketStore := ticket.NewPostgresStore(pool)
	created, err := ticketStore.Create(ctx, ticket.Ticket{
		EventID: ev.ID, TicketType: ticket.Registered, OwnerID: &ownerID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ticketStore.AddCheckIn(ctx, ticket.CheckIn{
		TicketID: created.ID, EventDayID: day.ID, SessionID: session.ID,
	}); err != nil {
		t.Fatal(err)
	}
	search, err := ticketStore.SearchDoorTickets(ctx, ev.ID, "ada", false, 20)
	if err != nil || len(search) != 1 || search[0].Name != "Ada Lovelace" {
		t.Fatalf("search=%+v err=%v", search, err)
	}
	exact, err := ticketStore.SearchDoorTickets(ctx, ev.ID, "ADA@EXAMPLE.COM", true, 2)
	if err != nil || len(exact) != 1 || exact[0].Ticket.ID != created.ID {
		t.Fatalf("exact=%+v err=%v", exact, err)
	}
	owners, err := ticketStore.DoorTicketsByOwners(ctx, ev.ID, []uuid.UUID{ownerID})
	if err != nil || len(owners) != 1 || owners[0].Email != "ada@example.com" {
		t.Fatalf("owners=%+v err=%v", owners, err)
	}
	activity, err := ticketStore.DoorSessionActivity(ctx, session.ID, 20)
	if err != nil || activity.Total != 1 || len(activity.Items) != 1 || activity.Items[0].PersonName != "Ada Lovelace" {
		t.Fatalf("activity=%+v err=%v", activity, err)
	}
	listed, err := ticketStore.ListByEvent(ctx, ev.ID)
	if err != nil || len(listed) != 1 || len(listed[0].CheckIns) != 1 {
		t.Fatalf("listed=%+v err=%v", listed, err)
	}
}

func TestHitRetentionPhysicallyDeletesExpiredPII(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}

	store := shorturl.NewPostgresStore(pool)
	created, err := store.Create(ctx, shorturl.URL{ID: uuid.New(), Alias: "retention-test", URL: "https://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := store.RecordHit(ctx, created.ID, shorturl.Hit{IP: "192.0.2.1", UserAgent: "old-agent", Referer: "https://old.example", CreatedAt: now.Add(-91 * 24 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordHit(ctx, created.ID, shorturl.Hit{IP: "192.0.2.2", UserAgent: "new-agent", Referer: "https://new.example", CreatedAt: now.Add(-89 * 24 * time.Hour)}); err != nil {
		t.Fatal(err)
	}

	if err := store.PruneHits(ctx, now.Add(-shorturl.HitRetention)); err != nil {
		t.Fatal(err)
	}
	var oldRows, newRows, clicks int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM url_hits WHERE ip = '192.0.2.1' OR user_agent = 'old-agent' OR referer = 'https://old.example'`).Scan(&oldRows); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM url_hits WHERE ip = '192.0.2.2'`).Scan(&newRows); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT click_count FROM urls WHERE id = $1`, created.ID).Scan(&clicks); err != nil {
		t.Fatal(err)
	}
	if oldRows != 0 || newRows != 1 || clicks != 1 {
		t.Fatalf("old=%d new=%d clicks=%d", oldRows, newRows, clicks)
	}
}

func TestEventMailSnapshotRetentionStorePersistsExpiry(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}

	store := eventmail.NewPostgresSnapshotStore(pool)
	now := time.Now().UTC().Truncate(time.Microsecond)
	expired := eventmail.Snapshot{MailListID: uuid.New(), EventID: uuid.New(), ExpiresAt: now.Add(-time.Minute)}
	fresh := eventmail.Snapshot{MailListID: uuid.New(), EventID: uuid.New(), ExpiresAt: now.Add(time.Minute)}
	if err := store.Track(ctx, expired); err != nil {
		t.Fatal(err)
	}
	if err := store.Track(ctx, fresh); err != nil {
		t.Fatal(err)
	}

	rows, err := store.Expired(ctx, now, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].MailListID != expired.MailListID || rows[0].EventID != expired.EventID {
		t.Fatalf("expired %+v", rows)
	}
	if err := store.Forget(ctx, expired.MailListID); err != nil {
		t.Fatal(err)
	}
	rows, err = store.Expired(ctx, now.Add(2*time.Minute), 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].MailListID != fresh.MailListID {
		t.Fatalf("remaining %+v", rows)
	}
}
