package user_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

func TestPostgresServiceErasureCheckpointKeepsCountsUnderTheLeaseFence(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := user.NewPostgresStore(pool)
	subjectID := uuid.New()
	if _, _, err := user.NewService(store).Ensure(ctx, subjectID, user.Profile{Email: "counts@example.test"}); err != nil {
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
	first, ok, err := store.ClaimDeletionRequest(ctx, now, time.Second)
	if err != nil || !ok {
		t.Fatalf("first claim ok=%v err=%v", ok, err)
	}
	second, ok, err := store.ClaimDeletionRequest(ctx, now.Add(2*time.Second), time.Minute)
	if err != nil || !ok {
		t.Fatalf("second claim ok=%v err=%v", ok, err)
	}

	counts := map[string]int64{"recipients_deleted": 1, "queue_rows_cleared": 12}
	if err := store.CompleteServiceErasureStep(ctx, request.ID, *first.LeaseToken, user.DeletionStepEraseSkyMail, now, counts); !errors.Is(err, user.ErrLeaseLost) {
		t.Fatalf("stale checkpoint error = %v", err)
	}
	if records, err := store.DeletionStepRecords(ctx, request.ID); err != nil || len(records) != 0 {
		t.Fatalf("stale worker wrote proof: %+v err=%v", records, err)
	}

	at := now.Add(3 * time.Second).Truncate(time.Microsecond)
	if err := store.CompleteServiceErasureStep(ctx, request.ID, *second.LeaseToken, user.DeletionStepEraseSkyMail, at, counts); err != nil {
		t.Fatal(err)
	}
	// A repeated checkpoint (a replayed 200) keeps the first proof.
	if err := store.CompleteServiceErasureStep(ctx, request.ID, *second.LeaseToken, user.DeletionStepEraseSkyMail, at.Add(time.Minute), map[string]int64{"recipients_deleted": 0}); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteServiceErasureStep(ctx, request.ID, *second.LeaseToken, user.DeletionStepEraseCMS, at, map[string]int64{}); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteDeletionStep(ctx, request.ID, *second.LeaseToken, user.DeletionStepDisableIdentity, at); err != nil {
		t.Fatal(err)
	}
	records, err := store.DeletionStepRecords(ctx, request.ID)
	if err != nil {
		t.Fatal(err)
	}
	byStep := map[user.DeletionStep]user.DeletionStepRecord{}
	for _, record := range records {
		byStep[record.Step] = record
	}
	skymail := byStep[user.DeletionStepEraseSkyMail]
	if !skymail.CompletedAt.Equal(at) || len(skymail.Counts) != 2 || skymail.Counts["recipients_deleted"] != 1 || skymail.Counts["queue_rows_cleared"] != 12 {
		t.Fatalf("skymail proof = %+v", skymail)
	}
	if cms := byStep[user.DeletionStepEraseCMS]; cms.Counts == nil || len(cms.Counts) != 0 {
		t.Fatalf("empty counts must stay an empty object: %+v", cms)
	}
	if disable := byStep[user.DeletionStepDisableIdentity]; disable.Counts != nil {
		t.Fatalf("a core step has no counts: %+v", disable)
	}
	completed, err := store.CompletedDeletionSteps(ctx, request.ID, *second.LeaseToken)
	if err != nil || !completed[user.DeletionStepEraseSkyMail] || !completed[user.DeletionStepEraseCMS] || completed[user.DeletionStepEraseForms] {
		t.Fatalf("completed = %v err=%v", completed, err)
	}

	for name, statement := range map[string]string{
		"counts must be an object": `INSERT INTO account_deletion_steps (request_id, step, counts) VALUES ($1, 'erase_forms', '[1]')`,
		"unknown step":             `INSERT INTO account_deletion_steps (request_id, step) VALUES ($1, 'erase_elsewhere')`,
	} {
		if _, err := pool.Exec(ctx, statement, request.ID); err == nil || !strings.Contains(err.Error(), "23514") {
			t.Fatalf("%s: err = %v", name, err)
		}
	}
}

func TestPostgresOpenDeletionRequestsListsEveryUnfinishedRequest(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := user.NewPostgresStore(pool)
	statuses := []user.DeletionRequestStatus{
		user.DeletionRequestPending, user.DeletionRequestProcessing,
		user.DeletionRequestManualIntervention, user.DeletionRequestCompleted,
	}
	ids := map[user.DeletionRequestStatus]uuid.UUID{}
	for i, status := range statuses {
		subjectID := uuid.New()
		if _, _, err := user.NewService(store).Ensure(ctx, subjectID, user.Profile{Email: "open" + string(rune('a'+i)) + "@example.test"}); err != nil {
			t.Fatal(err)
		}
		request, err := store.RequestDeletion(ctx, subjectID, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `UPDATE account_deletion_requests SET status=$2, last_error_code=$3 WHERE id=$1`, request.ID, status, "erase_cms_failed"); err != nil {
			t.Fatal(err)
		}
		ids[status] = request.ID
	}
	open, err := store.OpenDeletionRequests(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 3 {
		t.Fatalf("open requests = %+v", open)
	}
	for _, request := range open {
		if request.Status == user.DeletionRequestCompleted || request.ID == ids[user.DeletionRequestCompleted] {
			t.Fatalf("completed request listed as open: %+v", request)
		}
		if request.LastErrorCode != "erase_cms_failed" || request.CreatedAt.IsZero() {
			t.Fatalf("open request = %+v", request)
		}
	}
}
