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

func TestPostgresSelfDeletionIntakePersistsOneHashOnlyCapabilityAndNeverReactivates(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := user.NewPostgresStore(pool)
	subject := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	idempotency := [32]byte{1, 2, 3}
	lookup := [32]byte{4, 5, 6}
	proof := [32]byte{7, 8, 9}
	createdAt := time.Date(2026, 9, 20, 8, 0, 0, 0, time.UTC)
	intake := user.SelfDeletionIntake{
		IdempotencyHash: idempotency, ReceiptLookupHash: lookup, ReceiptHash: proof,
		ReceiptExpiresAt: createdAt.Add(90 * 24 * time.Hour), CreatedAt: createdAt,
	}

	type result struct {
		record user.SelfDeletionRecord
		err    error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	for range 2 {
		go func() {
			<-start
			record, err := store.RequestSelfDeletion(ctx, subject, intake)
			results <- result{record: record, err: err}
		}()
	}
	close(start)
	firstResult, secondResult := <-results, <-results
	if firstResult.err != nil || secondResult.err != nil {
		t.Fatalf("concurrent intake errors: first=%v second=%v", firstResult.err, secondResult.err)
	}
	first, second := firstResult.record, secondResult.record
	if first.Request.ID != second.Request.ID || first.Request.SubjectID != subject {
		t.Fatalf("durable request changed: first=%+v second=%+v", first, second)
	}
	if first.Request.PlatformBlockedAt != nil || first.Request.Status != user.DeletionRequestPending {
		t.Fatalf("new intake bypassed projection fence: %+v", first.Request)
	}
	if !first.Request.CreatedAt.Equal(createdAt) || !first.Request.UpdatedAt.Equal(createdAt) ||
		!first.Request.NextAttemptAt.Equal(createdAt) || !first.CreatedAt.Equal(createdAt) {
		t.Fatalf("self-delete chronology request=%+v intake_created_at=%v", first.Request, first.CreatedAt)
	}
	if _, _, err := user.NewService(store).Ensure(ctx, subject, user.Profile{Email: "old-token@example.test"}); !errors.Is(err, user.ErrAccountBlocked) {
		t.Fatalf("old token recreated account: %v", err)
	}

	other := idempotency
	other[0] = 99
	conflicting := intake
	conflicting.IdempotencyHash = other
	if _, err := store.RequestSelfDeletion(ctx, subject, conflicting); !errors.Is(err, user.ErrSelfDeletionIdempotencyConflict) {
		t.Fatalf("different idempotency error = %v", err)
	}
	byReceipt, err := store.SelfDeletionByReceiptLookup(ctx, lookup)
	if err != nil {
		t.Fatal(err)
	}
	if byReceipt.Request.ID != first.Request.ID || byReceipt.ReceiptHash != proof || byReceipt.IdempotencyHash != idempotency {
		t.Fatalf("receipt lookup = %+v", byReceipt)
	}

	var requestCount, outboxCount, intakeCount int
	if err := pool.QueryRow(ctx, `
		SELECT
			(SELECT count(*) FROM account_deletion_requests WHERE subject_id=$1),
			(SELECT count(*) FROM account_deletion_outbox WHERE subject_id=$1),
			(SELECT count(*) FROM account_deletion_self_intakes intake JOIN account_deletion_requests request ON request.id=intake.request_id WHERE request.subject_id=$1)
	`, subject).Scan(&requestCount, &outboxCount, &intakeCount); err != nil {
		t.Fatal(err)
	}
	if requestCount != 1 || outboxCount != 1 || intakeCount != 1 {
		t.Fatalf("request=%d outbox=%d intake=%d", requestCount, outboxCount, intakeCount)
	}
}

func TestPostgresSelfDeletionRetryOnlyRequeuesManualIntervention(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := user.NewPostgresStore(pool)
	subject := uuid.New()
	idempotency := [32]byte{1}
	lookup := [32]byte{2}
	proof := [32]byte{3}
	createdAt := time.Now().UTC().Truncate(time.Microsecond)
	record, err := store.RequestSelfDeletion(ctx, subject, user.SelfDeletionIntake{
		IdempotencyHash: idempotency, ReceiptLookupHash: lookup, ReceiptHash: proof,
		ReceiptExpiresAt: createdAt.Add(time.Hour), CreatedAt: createdAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	blockedAt := time.Now().UTC()
	if err := store.MarkDeletionPlatformBlocked(ctx, record.Request.ID, blockedAt); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE account_deletion_requests
		SET status='manual_intervention', attempt_count=8, last_error_code='private_internal_code'
		WHERE id=$1
	`, record.Request.ID); err != nil {
		t.Fatal(err)
	}

	retried, err := store.RetrySelfDeletion(ctx, lookup, blockedAt.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if retried.Request.Status != user.DeletionRequestPending || retried.Request.AttemptCount != 0 || retried.Request.LastErrorCode != "" {
		t.Fatalf("retry = %+v", retried.Request)
	}
	if retried.Request.PlatformBlockedAt == nil || !retried.Request.PlatformBlockedAt.Equal(blockedAt) {
		t.Fatalf("retry removed platform block: %+v", retried.Request)
	}
}

func TestPostgresSelfDeletionReceiptRevocationIsIdempotent(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := user.NewPostgresStore(pool)
	createdAt := time.Now().UTC().Truncate(time.Microsecond)
	request, err := store.RequestSelfDeletion(ctx, uuid.New(), user.SelfDeletionIntake{
		IdempotencyHash: [32]byte{1}, ReceiptLookupHash: [32]byte{2}, ReceiptHash: [32]byte{3},
		ReceiptExpiresAt: createdAt.Add(time.Hour), CreatedAt: createdAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	revokedAt := time.Now().UTC().Truncate(time.Microsecond)
	if err := store.RevokeSelfDeletionReceipt(ctx, request.Request.ID, revokedAt); err != nil {
		t.Fatal(err)
	}
	if err := store.RevokeSelfDeletionReceipt(ctx, request.Request.ID, revokedAt.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	revoked, err := store.SelfDeletionByReceiptLookup(ctx, request.ReceiptLookupHash)
	if err != nil {
		t.Fatal(err)
	}
	if revoked.ReceiptRevokedAt == nil || !revoked.ReceiptRevokedAt.Equal(revokedAt) {
		t.Fatalf("revocation timestamp = %v, want %v", revoked.ReceiptRevokedAt, revokedAt)
	}
	if err := store.RevokeSelfDeletionReceipt(ctx, uuid.New(), revokedAt); !errors.Is(err, user.ErrNotFound) {
		t.Fatalf("unknown request revocation error = %v", err)
	}
}
