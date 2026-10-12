package migrate_test

import (
	"context"
	"errors"
	"io/fs"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/db"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
)

const competitionsDown = "migrations/20261012140000_competitions.down.sql"

type competitionRows struct {
	user, event, otherEvent, ticket, otherTicket, competition, stage, entrant uuid.UUID
}

// seedCompetition writes an Event with a guest Ticket, a Competition with
// its Stage and one Entrant for the Ticket, by hand.
func seedCompetition(t *testing.T, ctx context.Context, pool *pgxpool.Pool) competitionRows {
	t.Helper()
	r := competitionRows{
		user: uuid.New(), event: uuid.New(), otherEvent: uuid.New(), ticket: uuid.New(), otherTicket: uuid.New(),
		competition: uuid.New(), stage: uuid.New(), entrant: uuid.New(),
	}
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO users (id, email) VALUES ($1, $2)`, []any{r.user, r.user.String() + "@example.test"}},
		{`INSERT INTO events (id, name, location, owner_team) VALUES ($1, 'AGC', 'YTÜ', 'AGC'), ($2, 'Başka', 'YTÜ', 'AGC')`, []any{r.event, r.otherEvent}},
		{`INSERT INTO tickets (id, event_id, ticket_type, guest_first_name, guest_email) VALUES ($1, $2, 'GUEST', 'Ada', 'ada@example.test'), ($3, $4, 'GUEST', 'Bob', 'bob@example.test')`,
			[]any{r.ticket, r.event, r.otherTicket, r.otherEvent}},
		{`INSERT INTO competitions (id, event_id, name, created_by) VALUES ($1, $2, 'AGC Hafta 1', $3)`, []any{r.competition, r.event, r.user}},
		{`INSERT INTO competition_stages (id, competition_id, position, strategy) VALUES ($1, $2, 1, 'manual')`, []any{r.stage, r.competition}},
		{`INSERT INTO competition_entrants (id, competition_id, created_by) VALUES ($1, $2, $3)`, []any{r.entrant, r.competition, r.user}},
		{`INSERT INTO competition_entrant_members (entrant_id, competition_id, ticket_id) VALUES ($1, $2, $3)`, []any{r.entrant, r.competition, r.ticket}},
	} {
		if _, err := pool.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatalf("%s: %v", q.sql, err)
		}
	}
	return r
}

func pgCode(err error) (code, constraint string) {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		return pg.Code, pg.ConstraintName
	}
	return "", ""
}

// The ledger only grows: a row is never edited or deleted, it is revoked
// once; clearing an actor when the account is purged is the one other
// change.
func TestCompetitionLedgerIsAppendOnly(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	r := seedCompetition(t, ctx, pool)
	entry := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO competition_submissions (id, competition_id, stage_id, entrant_id, source, value, recorded_by)
		VALUES ($1, $2, $3, $4, 'manual', 30000, $5)`, entry, r.competition, r.stage, r.entrant, r.user); err != nil {
		t.Fatal(err)
	}
	adjustment := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO competition_adjustments (id, competition_id, stage_id, entrant_id, kind, value, reason, recorded_by)
		VALUES ($1, $2, $3, $4, 'bonus', 5000, 'Bonus soru', $5)`, adjustment, r.competition, r.stage, r.entrant, r.user); err != nil {
		t.Fatal(err)
	}
	for name, q := range map[string]string{
		"edit a value":          `UPDATE competition_submissions SET value = 60000 WHERE id = $1`,
		"move the contest time": `UPDATE competition_submissions SET occurred_at = occurred_at - interval '1 day' WHERE id = $1`,
		"delete":                `DELETE FROM competition_submissions WHERE id = $1`,
		"delete an adjustment":  `DELETE FROM competition_adjustments WHERE id = $1`,
		"edit an adjustment":    `UPDATE competition_adjustments SET reason = 'other' WHERE id = $1`,
	} {
		target := entry
		if strings.Contains(name, "adjustment") {
			target = adjustment
		}
		if _, err := pool.Exec(ctx, q, target); func() bool { _, c := pgCode(err); return c != "competition_ledger_append_only" }() {
			t.Errorf("%s: %v, want the append-only refusal", name, err)
		}
	}
	// A revocation needs its reason, then holds.
	if _, err := pool.Exec(ctx, `UPDATE competition_submissions SET revoked_at = now(), revoked_by = $2 WHERE id = $1`, entry, r.user); err == nil {
		t.Error("revoked without a reason")
	}
	if _, err := pool.Exec(ctx, `UPDATE competition_submissions SET revoked_at = now(), revoked_by = $2, revoke_reason = 'Yanlış' WHERE id = $1`, entry, r.user); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE competition_submissions SET revoke_reason = 'Başka' WHERE id = $1`, entry); func() bool { _, c := pgCode(err); return c != "competition_ledger_append_only" }() {
		t.Errorf("re-revoke: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE competition_submissions SET revoked_at = NULL, revoke_reason = NULL, revoked_by = NULL WHERE id = $1`, entry); err == nil {
		t.Error("revocation undone")
	}
	// Purging the account clears the actor columns and keeps the rows.
	if _, err := pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, r.user); err != nil {
		t.Fatalf("purge the actor: %v", err)
	}
	var recordedBy, revokedBy *uuid.UUID
	var reason string
	if err := pool.QueryRow(ctx, `SELECT recorded_by, revoked_by, revoke_reason FROM competition_submissions WHERE id = $1`, entry).Scan(&recordedBy, &revokedBy, &reason); err != nil {
		t.Fatal(err)
	}
	if recordedBy != nil || revokedBy != nil || reason != "Yanlış" {
		t.Fatalf("after purge recorded_by=%v revoked_by=%v reason=%q", recordedBy, revokedBy, reason)
	}
}

func TestCompetitionSchemaKeepsItsRules(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	r := seedCompetition(t, ctx, pool)

	// A Ticket is in one Entrant of a Competition.
	second := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO competition_entrants (id, competition_id) VALUES ($1, $2)`, second, r.competition); err != nil {
		t.Fatal(err)
	}
	_, err := pool.Exec(ctx, `INSERT INTO competition_entrant_members (entrant_id, competition_id, ticket_id) VALUES ($1, $2, $3)`, second, r.competition, r.ticket)
	if code, c := pgCode(err); code != "23505" || c != "competition_entrant_members_ticket_key" {
		t.Errorf("second entrant for a ticket: %v", err)
	}
	// A member is a Ticket of the Competition's Event.
	_, err = pool.Exec(ctx, `INSERT INTO competition_entrant_members (entrant_id, competition_id, ticket_id) VALUES ($1, $2, $3)`, second, r.competition, r.otherTicket)
	if _, c := pgCode(err); c != "competition_entrant_member_same_event" {
		t.Errorf("another event's ticket: %v", err)
	}
	// A member names its Entrant's own Competition.
	other := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO competitions (id, event_id, name) VALUES ($1, $2, 'Diğer')`, other, r.event); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO competition_entrant_members (entrant_id, competition_id, ticket_id) VALUES ($1, $2, $3)`, second, other, r.ticket); err == nil {
		t.Error("member crossed competitions")
	}
	// One live manual score per Entrant and Stage.
	if _, err := pool.Exec(ctx, `INSERT INTO competition_submissions (id, competition_id, stage_id, entrant_id, source, value) VALUES ($1, $2, $3, $4, 'manual', 30000)`,
		uuid.New(), r.competition, r.stage, r.entrant); err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO competition_submissions (id, competition_id, stage_id, entrant_id, source, value) VALUES ($1, $2, $3, $4, 'manual', 50000)`,
		uuid.New(), r.competition, r.stage, r.entrant)
	if code, _ := pgCode(err); code != "23505" {
		t.Errorf("second live manual score: %v", err)
	}
	// Shape checks: a row of another Competition's Stage, a value past the
	// bound, an unknown source, a cancellation without its reason.
	for name, q := range map[string]string{
		"stage of another competition": `INSERT INTO competition_submissions (id, competition_id, stage_id, entrant_id, source, value)
			SELECT gen_random_uuid(), '` + other.String() + `', '` + r.stage.String() + `', '` + r.entrant.String() + `', 'manual', 1`,
		"value past the bound": `INSERT INTO competition_submissions (id, competition_id, stage_id, entrant_id, source, value)
			VALUES (gen_random_uuid(), '` + r.competition.String() + `', '` + r.stage.String() + `', '` + second.String() + `', 'manual', 1000000000001)`,
		"unknown source": `INSERT INTO competition_submissions (id, competition_id, stage_id, entrant_id, source, value)
			VALUES (gen_random_uuid(), '` + r.competition.String() + `', '` + r.stage.String() + `', '` + second.String() + `', 'typed', 1)`,
		"manual without a value": `INSERT INTO competition_submissions (id, competition_id, stage_id, entrant_id, source)
			VALUES (gen_random_uuid(), '` + r.competition.String() + `', '` + r.stage.String() + `', '` + second.String() + `', 'manual')`,
		"cancelled without a reason": `UPDATE competitions SET status = 'cancelled' WHERE id = '` + r.competition.String() + `'`,
		"reason without cancelling":  `UPDATE competitions SET cancel_reason = 'x' WHERE id = '` + r.competition.String() + `'`,
		"unknown status":             `UPDATE competitions SET status = 'published' WHERE id = '` + r.competition.String() + `'`,
	} {
		if _, err := pool.Exec(ctx, q); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

// Rolling back refuses while a Competition exists, and drops everything
// once there is none.
func TestCompetitionsDownRefusesWhileCompetitionsExist(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	down, err := fs.ReadFile(db.DownSQL, competitionsDown)
	if err != nil {
		t.Fatal(err)
	}
	seedCompetition(t, ctx, pool)
	if _, err := pool.Exec(ctx, string(down)); err == nil || !strings.Contains(err.Error(), "competitions exist") {
		t.Fatalf("down with a competition: %v", err)
	}
	for _, q := range []string{`DELETE FROM competition_entrant_members`, `DELETE FROM competition_entrants`, `DELETE FROM competition_stages`, `DELETE FROM competitions`} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, string(down)); err != nil {
		t.Fatalf("down on an empty module: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = 20261012140000`); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatalf("up again: %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_trigger WHERE NOT tgisinternal AND tgname LIKE 'competition%'`).Scan(&n); err != nil || n != 10 {
		t.Fatalf("triggers after up again = %d, %v", n, err)
	}
}
