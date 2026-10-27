package migrate_test

import (
	"context"
	"slices"
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/migrate"
)

const (
	managementTermsVersion     = "20261027140000"
	teamAuditTermActionVersion = "20261027140100"
)

func TestManagementTermMigrationsComeLast(t *testing.T) {
	t.Parallel()
	versions, err := migrate.Versions()
	if err != nil {
		t.Fatal(err)
	}
	// The audit record they extend, and main's newest migration when they
	// were written (2026-10-27).
	audit := slices.Index(versions, 20261023120000)
	head := slices.Index(versions, 20261025180100)
	terms := slices.Index(versions, 20261027140000)
	actions := slices.Index(versions, 20261027140100)
	if audit < 0 || head < 0 || terms < 0 || actions < 0 || audit >= actions || head >= terms || terms >= actions {
		t.Fatalf("versions %v", versions)
	}
}

// Down and up again work, the fingerprints record a schema that has them
// already, and the down of the audit actions keeps term rows written.
func TestManagementTermMigrations(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO team_audit_records (id, occurred_at, actor_id, actor_basis, action, group_id, group_path, target_user_id, outcome)
		VALUES (gen_random_uuid(), now(), '00000000-0000-0000-0000-000000000000', 'system', 'membership.term.expired',
			gen_random_uuid(), '/UYELER/YK', gen_random_uuid(), 'ok')`); err != nil {
		t.Fatal(err)
	}
	// The system basis is the expiry's alone.
	if _, err := pool.Exec(ctx, `
		INSERT INTO team_audit_records (id, occurred_at, actor_id, actor_basis, action, group_id, group_path, target_user_id, outcome)
		VALUES (gen_random_uuid(), now(), '00000000-0000-0000-0000-000000000000', 'system', 'member.add',
			gen_random_uuid(), '/UYELER/YK', gen_random_uuid(), 'ok')`); err == nil {
		t.Fatal("a system member.add was accepted")
	}
	for _, version := range []string{teamAuditTermActionVersion + "_team_audit_membership_terms", managementTermsVersion + "_management_membership_terms"} {
		if _, err := pool.Exec(ctx, readDown(t, version+".down.sql")); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version IN (`+managementTermsVersion+`, `+teamAuditTermActionVersion+`)`); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version IN (`+managementTermsVersion+`, `+teamAuditTermActionVersion+`)`); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	for _, version := range []string{managementTermsVersion, teamAuditTermActionVersion} {
		if !recorded(t, pool, version) {
			t.Fatalf("%s not recorded by its fingerprint", version)
		}
	}
}
