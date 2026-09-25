package user_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

func TestDeletionLeaseTokenFencesExpiredWorker(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := user.NewPostgresStore(pool)
	subjectID := uuid.New()
	if _, _, err := user.NewService(store).Ensure(ctx, subjectID, user.Profile{Email: "lease@example.test"}); err != nil {
		t.Fatal(err)
	}
	request, err := store.RequestDeletion(ctx, subjectID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkDeletionPlatformBlocked(ctx, request.ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	profileMediaID := uuid.New()
	if _, err := pool.Exec(ctx, `UPDATE account_deletion_requests SET profile_media_id=$2 WHERE id=$1`, request.ID, profileMediaID); err != nil {
		t.Fatal(err)
	}

	// Anchor the worker clock on the DB-assigned schedule so claims never depend on the calendar.
	now := request.NextAttemptAt
	first, ok, err := store.ClaimDeletionRequest(ctx, now, time.Second)
	if err != nil || !ok || first.LeaseToken == nil {
		t.Fatalf("first claim=%+v ok=%v err=%v", first, ok, err)
	}
	second, ok, err := store.ClaimDeletionRequest(ctx, now.Add(2*time.Second), time.Minute)
	if err != nil || !ok || second.LeaseToken == nil {
		t.Fatalf("second claim=%+v ok=%v err=%v", second, ok, err)
	}
	if *first.LeaseToken == *second.LeaseToken {
		t.Fatal("lease token was reused across claims")
	}

	if _, err := store.CompletedDeletionSteps(ctx, request.ID, *first.LeaseToken); !errors.Is(err, user.ErrLeaseLost) {
		t.Fatalf("stale read error = %v", err)
	}
	if err := store.CompleteDeletionStep(ctx, request.ID, *first.LeaseToken, user.DeletionStepEraseProfile, now); !errors.Is(err, user.ErrLeaseLost) {
		t.Fatalf("stale step error = %v", err)
	}
	if retained, err := store.ProfileMediaForDeletion(ctx, request.ID); err != nil || retained == nil || *retained != profileMediaID {
		t.Fatalf("stale checkpoint cleared retry media: id=%v err=%v", retained, err)
	}
	if err := store.RetryDeletionRequest(ctx, request.ID, *first.LeaseToken, now, now, "stale", false, true); !errors.Is(err, user.ErrLeaseLost) {
		t.Fatalf("stale retry error = %v", err)
	}
	var attemptsAfterStale int
	if err := pool.QueryRow(ctx, `SELECT attempt_count FROM account_deletion_requests WHERE id=$1`, request.ID).Scan(&attemptsAfterStale); err != nil {
		t.Fatal(err)
	}
	if attemptsAfterStale != 2 {
		t.Fatalf("stale worker refunded replacement claim: attempts=%d", attemptsAfterStale)
	}
	if err := store.CompleteDeletionRequest(ctx, request.ID, *first.LeaseToken, now); !errors.Is(err, user.ErrLeaseLost) {
		t.Fatalf("stale completion error = %v", err)
	}

	if err := store.RetryDeletionRequest(ctx, request.ID, *second.LeaseToken, now, now.Add(2*time.Second), "durable_deferral", false, true); err != nil {
		t.Fatal(err)
	}
	var attemptsAfterRefund int
	if err := pool.QueryRow(ctx, `SELECT attempt_count FROM account_deletion_requests WHERE id=$1`, request.ID).Scan(&attemptsAfterRefund); err != nil {
		t.Fatal(err)
	}
	if attemptsAfterRefund != 1 {
		t.Fatalf("current lease did not refund exactly its own claim: attempts=%d", attemptsAfterRefund)
	}
	third, ok, err := store.ClaimDeletionRequest(ctx, now.Add(2*time.Second), time.Minute)
	if err != nil || !ok || third.LeaseToken == nil {
		t.Fatalf("third claim=%+v ok=%v err=%v", third, ok, err)
	}
	if third.AttemptCount != 2 {
		t.Fatalf("third claim attempt count=%d, want retained expired claim plus current claim", third.AttemptCount)
	}
	if err := store.CompleteDeletionStep(ctx, request.ID, *third.LeaseToken, user.DeletionStepEraseProfile, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if retained, err := store.ProfileMediaForDeletion(ctx, request.ID); err != nil || retained != nil {
		t.Fatalf("successful checkpoint retained profile association: id=%v err=%v", retained, err)
	}
	if err := store.CompleteDeletionRequest(ctx, request.ID, *third.LeaseToken, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
}

func TestDeletionStepCheckpointCannotCommitAfterConcurrentLeaseReplacement(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := user.NewPostgresStore(pool)
	subjectID := uuid.New()
	if _, _, err := user.NewService(store).Ensure(ctx, subjectID, user.Profile{Email: "lease-race@example.test"}); err != nil {
		t.Fatal(err)
	}
	request, err := store.RequestDeletion(ctx, subjectID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkDeletionPlatformBlocked(ctx, request.ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	profileMediaID := uuid.New()
	if _, err := pool.Exec(ctx, `UPDATE account_deletion_requests SET profile_media_id=$2 WHERE id=$1`, request.ID, profileMediaID); err != nil {
		t.Fatal(err)
	}

	now := request.NextAttemptAt
	claim, ok, err := store.ClaimDeletionRequest(ctx, now, time.Second)
	if err != nil || !ok || claim.LeaseToken == nil {
		t.Fatalf("claim=%+v ok=%v err=%v", claim, ok, err)
	}

	// Hold the request row while the old worker starts checkpointing. Replacing
	// the token under that lock models a reclaim at the exact fence boundary.
	blocker, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(ctx)
	var lockedID uuid.UUID
	if err := blocker.QueryRow(ctx, `SELECT id FROM account_deletion_requests WHERE id=$1 FOR UPDATE`, request.ID).Scan(&lockedID); err != nil {
		t.Fatal(err)
	}

	checkpointDone := make(chan error, 1)
	go func() {
		checkpointDone <- store.CompleteDeletionStep(ctx, request.ID, *claim.LeaseToken, user.DeletionStepEraseProfile, now)
	}()
	testpostgres.WaitForBlockedQuery(t, pool, "account_deletion_steps")

	replacementToken := uuid.New()
	if _, err := blocker.Exec(ctx, `UPDATE account_deletion_requests SET lease_token=$2, lease_until=$3 WHERE id=$1`, request.ID, replacementToken, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-checkpointDone; !errors.Is(err, user.ErrLeaseLost) {
		t.Fatalf("stale checkpoint error = %v", err)
	}

	var stepCount int
	var retainedProfileID *uuid.UUID
	if err := pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM account_deletion_steps WHERE request_id=$1), profile_media_id
		FROM account_deletion_requests WHERE id=$1
	`, request.ID).Scan(&stepCount, &retainedProfileID); err != nil {
		t.Fatal(err)
	}
	if stepCount != 0 {
		t.Fatalf("stale worker committed %d checkpoint rows", stepCount)
	}
	if retainedProfileID == nil || *retainedProfileID != profileMediaID {
		t.Fatalf("stale worker cleared retry media: %v", retainedProfileID)
	}
}
