package migrate_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
)

const actorBasisVersion = "20261031120000"

// A ledger row keeps its writer's basis, and its revoker's with the
// revocation, once: neither changes afterwards, and a basis outside the
// list is refused. Adjustments, which have no basis, still revoke. Down
// takes the columns off and the rule back; Apply brings both again (the
// fingerprint knows them).
func TestCompetitionActorBasisMigration(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	r := seedCompetition(t, ctx, pool)
	exec := func(sql string, args ...any) error {
		_, err := pool.Exec(ctx, sql, args...)
		return err
	}
	entry := uuid.New()
	if err := exec(`INSERT INTO competition_submissions (id, competition_id, stage_id, entrant_id, source, value, recorded_by, actor_basis)
		VALUES ($1, $2, $3, $4, 'manual', 30000, $5, 'member_setting')`, entry, r.competition, r.stage, r.entrant, r.user); err != nil {
		t.Fatal(err)
	}
	if err := exec(`INSERT INTO competition_submissions (id, competition_id, stage_id, entrant_id, source, value, actor_basis)
		VALUES ($1, $2, $3, $4, 'native', 1, 'everyone')`, uuid.New(), r.competition, r.stage, r.entrant); err == nil {
		t.Error("an unknown basis was written")
	}
	if err := exec(`UPDATE competition_submissions SET actor_basis = 'leader' WHERE id = $1`, entry); err == nil {
		t.Error("the writer's basis changed")
	}
	if err := exec(`UPDATE competition_submissions SET revoked_basis = 'leader' WHERE id = $1`, entry); err == nil {
		t.Error("a revoker's basis without a revocation")
	}
	if err := exec(`UPDATE competition_submissions SET revoked_at = now(), revoked_by = $2, revoke_reason = 'itiraz', revoked_basis = 'privileged'
		WHERE id = $1`, entry, r.user); err != nil {
		t.Fatalf("revoke with a basis: %v", err)
	}
	if err := exec(`UPDATE competition_submissions SET revoked_basis = 'leader' WHERE id = $1`, entry); err == nil {
		t.Error("the revoker's basis changed after the revocation")
	}
	if code, constraint := pgCode(exec(`UPDATE competition_submissions SET revoked_basis = NULL WHERE id = $1`, entry)); code != "23514" || constraint != "competition_ledger_append_only" {
		t.Errorf("clearing the revoker's basis: %s %s", code, constraint)
	}
	adjustment := uuid.New()
	if err := exec(`INSERT INTO competition_adjustments (id, competition_id, stage_id, entrant_id, kind, value, reason)
		VALUES ($1, $2, $3, $4, 'bonus', 5000, 'Bonus soru')`, adjustment, r.competition, r.stage, r.entrant); err != nil {
		t.Fatal(err)
	}
	if err := exec(`UPDATE competition_adjustments SET revoked_at = now(), revoke_reason = 'yanlış' WHERE id = $1`, adjustment); err != nil {
		t.Errorf("an adjustment's revocation: %v", err)
	}

	if err := exec(readDown(t, actorBasisVersion+"_competition_actor_basis.down.sql")); err != nil {
		t.Fatalf("down: %v", err)
	}
	if err := exec(`DELETE FROM schema_migrations WHERE version = ` + actorBasisVersion); err != nil {
		t.Fatal(err)
	}
	var columns int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns
		WHERE table_name = 'competition_submissions' AND column_name IN ('actor_basis', 'revoked_basis')`).Scan(&columns); err != nil || columns != 0 {
		t.Fatalf("after down: %d columns, %v", columns, err)
	}
	if err := migrate.Apply(ctx, pool); err != nil || !recorded(t, pool, actorBasisVersion) {
		t.Fatalf("again: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns
		WHERE table_name = 'competition_submissions' AND column_name IN ('actor_basis', 'revoked_basis')`).Scan(&columns); err != nil || columns != 2 {
		t.Fatalf("again: %d columns, %v", columns, err)
	}
}
