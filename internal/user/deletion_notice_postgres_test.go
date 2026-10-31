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

// A deletion notice is claimed once per request and only under the current
// lease: a worker whose lease ran out claims nothing, so a notice never goes
// twice (docs/account-lifecycle.md, "Deletion notices").
func TestDeletionNoticeIsClaimedOnceUnderTheLease(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := user.NewPostgresStore(pool)
	subjectID := uuid.New()
	if _, _, err := user.NewService(store).Ensure(ctx, subjectID, user.Profile{Email: "notice@example.test"}); err != nil {
		t.Fatal(err)
	}
	request, err := store.RequestDeletion(ctx, subjectID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkDeletionPlatformBlocked(ctx, request.ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	now := request.NextAttemptAt
	if claimed, err := store.ClaimDeletionNotice(ctx, request.ID, uuid.New(), user.DeletionNoticeReceived, now); err != nil || claimed {
		t.Fatalf("claim without a lease = %v, %v", claimed, err)
	}
	first, ok, err := store.ClaimDeletionRequest(ctx, now, time.Second)
	if err != nil || !ok {
		t.Fatalf("claim = %v, %v", ok, err)
	}
	second, ok, err := store.ClaimDeletionRequest(ctx, now.Add(2*time.Second), time.Minute)
	if err != nil || !ok {
		t.Fatalf("second claim = %v, %v", ok, err)
	}
	if claimed, err := store.ClaimDeletionNotice(ctx, request.ID, *first.LeaseToken, user.DeletionNoticeReceived, now); err != nil || claimed {
		t.Fatalf("claim under an expired lease = %v, %v", claimed, err)
	}
	for _, c := range []struct {
		notice user.DeletionNotice
		want   bool
	}{{user.DeletionNoticeReceived, true}, {user.DeletionNoticeReceived, false}, {user.DeletionNoticeCompleted, true}, {user.DeletionNoticeCompleted, false}} {
		claimed, err := store.ClaimDeletionNotice(ctx, request.ID, *second.LeaseToken, c.notice, now)
		if err != nil || claimed != c.want {
			t.Fatalf("claim %s = %v, %v; want %v", c.notice, claimed, err, c.want)
		}
	}
	var received, completed *time.Time
	if err := pool.QueryRow(ctx, `SELECT received_notice_at, completed_notice_at FROM account_deletion_requests WHERE id = $1`, request.ID).Scan(&received, &completed); err != nil {
		t.Fatal(err)
	}
	if received == nil || completed == nil {
		t.Fatalf("received %v, completed %v", received, completed)
	}
	if _, err := store.ClaimDeletionNotice(ctx, request.ID, *second.LeaseToken, user.DeletionNotice("other"), now); err == nil {
		t.Fatal("an unknown notice was claimed")
	}
}
