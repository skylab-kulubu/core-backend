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

func TestDeletionRequestCannotAdvanceBeforePlatformBlockConfirmation(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := user.NewPostgresStore(pool)
	subjectID := uuid.New()
	if _, _, err := user.NewService(store).Ensure(ctx, subjectID, user.Profile{Email: "projection-fence@example.test"}); err != nil {
		t.Fatal(err)
	}
	request, err := store.RequestDeletion(ctx, subjectID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if request.PlatformBlockedAt != nil {
		t.Fatalf("new request already projected: %+v", request)
	}

	// Anchor the worker clock on the DB-assigned schedule so claims never depend on the calendar.
	now := request.NextAttemptAt
	if claimed, ok, err := store.ClaimDeletionRequest(ctx, now.Add(time.Hour), time.Minute); err != nil || ok {
		t.Fatalf("unprojected claim=%+v ok=%v err=%v", claimed, ok, err)
	}
	var available bool
	if err := pool.QueryRow(ctx, `
		SELECT available_at <= $2 AND request.platform_blocked_at IS NOT NULL
		FROM account_deletion_outbox outbox
		JOIN account_deletion_requests request ON request.id=outbox.request_id
		WHERE outbox.request_id=$1
	`, request.ID, now.Add(time.Hour)).Scan(&available); err != nil {
		t.Fatal(err)
	}
	if available {
		t.Fatal("outbox became available before the platform marker")
	}

	if err := store.MarkDeletionPlatformBlocked(ctx, request.ID, now); err != nil {
		t.Fatal(err)
	}
	projected, err := store.DeletionRequest(ctx, subjectID)
	if err != nil || projected.PlatformBlockedAt == nil || !projected.PlatformBlockedAt.Equal(now) {
		t.Fatalf("projected request=%+v err=%v", projected, err)
	}
	if err := store.MarkDeletionPlatformBlocked(ctx, request.ID, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	projected, err = store.DeletionRequest(ctx, subjectID)
	if err != nil || projected.PlatformBlockedAt == nil || !projected.PlatformBlockedAt.Equal(now) {
		t.Fatalf("idempotent projection changed timestamp: %+v err=%v", projected, err)
	}
	claimed, ok, err := store.ClaimDeletionRequest(ctx, now, time.Minute)
	if err != nil || !ok || claimed.ID != request.ID {
		t.Fatalf("projected claim=%+v ok=%v err=%v", claimed, ok, err)
	}
}

func TestPlatformBlockConfirmationDoesNotAdvanceWithoutItsOutboxFence(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := user.NewPostgresStore(pool)
	subjectID := uuid.New()
	if _, _, err := user.NewService(store).Ensure(ctx, subjectID, user.Profile{Email: "missing-outbox@example.test"}); err != nil {
		t.Fatal(err)
	}
	request, err := store.RequestDeletion(ctx, subjectID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM account_deletion_outbox WHERE request_id=$1`, request.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkDeletionPlatformBlocked(ctx, request.ID, time.Now().UTC()); err == nil {
		t.Fatal("projection confirmation succeeded without its outbox row")
	}
	request, err = store.DeletionRequest(ctx, subjectID)
	if err != nil {
		t.Fatal(err)
	}
	if request.PlatformBlockedAt != nil {
		t.Fatal("failed atomic confirmation still advanced platform_blocked_at")
	}
}

func TestProjectionQueriesIncludeUnconfirmedAndCompletedDeletionRequests(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := user.NewPostgresStore(pool)
	for index := 0; index < 2; index++ {
		subjectID := uuid.New()
		if _, _, err := user.NewService(store).Ensure(ctx, subjectID, user.Profile{Email: uuid.NewString() + "@example.test"}); err != nil {
			t.Fatal(err)
		}
		request, err := store.RequestDeletion(ctx, subjectID, nil)
		if err != nil {
			t.Fatal(err)
		}
		if index == 1 {
			now := time.Now().UTC()
			if err := store.MarkDeletionPlatformBlocked(ctx, request.ID, now); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, `UPDATE account_deletion_requests SET status='completed', completed_at=$2 WHERE id=$1`, request.ID, now); err != nil {
				t.Fatal(err)
			}
		}
	}

	unconfirmed, err := store.UnprojectedDeletionRequests(ctx, 10)
	if err != nil || len(unconfirmed) != 1 || unconfirmed[0].PlatformBlockedAt != nil {
		t.Fatalf("unconfirmed=%+v err=%v", unconfirmed, err)
	}
	all, err := store.DeletionRequestsPage(ctx, uuid.Nil, 10)
	if err != nil || len(all) != 2 {
		t.Fatalf("all=%+v err=%v", all, err)
	}
	completed := false
	for _, request := range all {
		if request.Status == user.DeletionRequestCompleted {
			completed = true
		}
	}
	if !completed {
		t.Fatal("completed deletion marker was omitted from reconciliation input")
	}
}
