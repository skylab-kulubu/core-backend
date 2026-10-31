package migrate_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
)

const teamPageObjectionVersion = "20261031200000"

// The team page objection migration takes the new purpose and carries the
// old consent's withdrawals over: an active member whose team_page grants
// have all ended gets one open objection, from the last withdrawal, on the
// text they withdrew; a member with an open consent, one being erased and
// one who already has an objection row get none. The team_page rows stay as
// they were. Running it again adds nothing; down keeps the rows.
func TestTeamPageObjectionMigrationCarriesWithdrawalsOver(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	withdrew, open, erased, lifted := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	for i, id := range []uuid.UUID{withdrew, open, erased, lifted} {
		exec(`INSERT INTO users (id, email) VALUES ($1, $2)`, id, "u"+string(rune('a'+i))+"@example.com")
	}
	first := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)
	last := time.Date(2026, 10, 30, 18, 30, 0, 0, time.UTC)
	consentRow := func(id uuid.UUID, granted time.Time, ended *time.Time) {
		t.Helper()
		var reason, via *string
		if ended != nil {
			r, v := "withdrawn", "self"
			reason, via = &r, &v
		}
		exec(`INSERT INTO contact_consents (id, purpose, channel, user_id, source, client_id, text_version,
			granted_at, confirmed_at, confirmed_via, ended_at, ended_reason, ended_via)
			VALUES ($1, 'team_page', 'web', $2, 'self', 'account-center', 'ekip-sayfasi-v1', $3, $3, 'account', $4, $5, $6)`,
			uuid.New(), id, granted, ended, reason, via)
	}
	firstEnd, lastEnd := first.Add(time.Hour), last
	consentRow(withdrew, first, &firstEnd)
	consentRow(withdrew, last.Add(-time.Hour), &lastEnd)
	consentRow(open, first, &firstEnd)
	consentRow(open, last, nil)
	consentRow(erased, first, &firstEnd)
	consentRow(lifted, first, &firstEnd)
	exec(`INSERT INTO contact_consents (id, purpose, channel, user_id, source, client_id, text_version,
		granted_at, confirmed_at, confirmed_via, ended_at, ended_reason, ended_via)
		VALUES ($1, 'team_page_objection', 'web', $2, 'self', 'account-center', 'ekip-sayfasi-itiraz-v1', $3, $3, 'account', $4, 'withdrawn', 'self')`,
		uuid.New(), lifted, last, last.Add(time.Minute))
	exec(`INSERT INTO account_deletion_requests (id, subject_id) VALUES (gen_random_uuid(), $1)`, erased)

	// The database before this migration: run it again from there.
	exec(readDown(t, teamPageObjectionVersion+"_contact_consents_team_page_objection.down.sql"))
	exec(`DELETE FROM schema_migrations WHERE version = ` + teamPageObjectionVersion)
	for range 2 {
		if err := migrate.Apply(ctx, pool); err != nil || !recorded(t, pool, teamPageObjectionVersion) {
			t.Fatalf("apply: %v", err)
		}
		exec(`DELETE FROM schema_migrations WHERE version = ` + teamPageObjectionVersion)
	}

	objections := func(id uuid.UUID) (n int, granted time.Time) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT count(*), COALESCE(max(granted_at), 'epoch') FROM contact_consents
			WHERE user_id = $1 AND purpose = 'team_page_objection' AND ended_at IS NULL AND confirmed_at = granted_at
			  AND confirmed_via = 'account' AND channel = 'web' AND source = 'self' AND text_version = 'ekip-sayfasi-v1'`, id).Scan(&n, &granted); err != nil {
			t.Fatal(err)
		}
		return n, granted
	}
	if n, granted := objections(withdrew); n != 1 || !granted.Equal(last) {
		t.Fatalf("withdrew: %d objections from %v", n, granted)
	}
	for name, id := range map[string]uuid.UUID{"open consent": open, "being erased": erased, "objection lifted": lifted} {
		if n, _ := objections(id); n != 0 {
			t.Errorf("%s: %d objections", name, n)
		}
	}
	var consents int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM contact_consents WHERE purpose = 'team_page'`).Scan(&consents); err != nil || consents != 6 {
		t.Fatalf("team_page rows %d %v", consents, err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO contact_consents (id, purpose, channel, user_id, source, text_version, granted_at)
		VALUES ($1, 'team_page_objectionx', 'web', $2, 'self', 'x', now())`, uuid.New(), open); err == nil {
		t.Fatal("the purpose check takes any purpose")
	}
}
