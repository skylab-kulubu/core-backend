package user_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// Account erasure deletes the person's management membership terms and the
// term mails that go to them or name them, and forgets them as the one who
// granted another's term (docs/management-membership-terms.md).
func TestAnonymizationDeletesTheManagementTerms(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	users := user.NewPostgresStore(pool)
	person, other := uuid.New(), uuid.New()
	for id, email := range map[uuid.UUID]string{person: "kisi@example.test", other: "baska@example.test"} {
		if _, _, err := user.NewService(users).Ensure(ctx, id, user.Profile{Email: email}); err != nil {
			t.Fatal(err)
		}
	}
	own, others, group := uuid.New(), uuid.New(), uuid.New()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO management_membership_terms (id, user_id, group_id, group_path, scope, source, started_at, updated_at)
			VALUES ($1, $2, $3, '/UYELER/YK', 'privileged', 'route', now(), now())`, []any{own, person, group}},
		{`INSERT INTO management_membership_terms (id, user_id, group_id, group_path, scope, source, granted_by, started_at, updated_at)
			VALUES ($1, $2, $3, '/UYELER/YK', 'privileged', 'route', $4, now(), now())`, []any{others, other, group, person}},
		{`INSERT INTO management_term_mails (id, recipient_id, notice, subject_id, group_path, occurred_at, next_attempt_at)
			VALUES (gen_random_uuid(), $1, 'warning_14d', $1, '/UYELER/YK', now(), now()),
			       (gen_random_uuid(), $2, 'warning_14d', $1, '/UYELER/YK', now(), now()),
			       (gen_random_uuid(), $2, 'warning_14d', $2, '/UYELER/YK', now(), now())`, []any{person, other}},
	} {
		if _, err := pool.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := users.RequestDeletion(ctx, person, nil); err != nil {
		t.Fatal(err)
	}
	if err := users.AnonymizeAccount(ctx, person, time.Now().UTC(), nil); err != nil {
		t.Fatal(err)
	}
	var terms, granted, mails int
	if err := pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM management_membership_terms),
		       (SELECT count(*) FROM management_membership_terms WHERE granted_by IS NOT NULL),
		       (SELECT count(*) FROM management_term_mails)`).Scan(&terms, &granted, &mails); err != nil {
		t.Fatal(err)
	}
	if terms != 1 || granted != 0 || mails != 1 {
		t.Fatalf("terms %d granted %d mails %d", terms, granted, mails)
	}
}
