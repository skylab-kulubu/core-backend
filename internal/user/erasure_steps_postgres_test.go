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
	if _, forms := completed[user.DeletionStepEraseForms]; err != nil || !completed[user.DeletionStepEraseSkyMail].Equal(at) ||
		completed[user.DeletionStepEraseCMS].IsZero() || forms {
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

// An operator who carried out a service's erasure by hand, because the
// service has no endpoint yet, records it as that step's checkpoint: only for
// a request in manual intervention under that step's code, with
// operator_recorded in the counts, and the request goes back to pending so
// the saga goes on. Everything else is refused and writes nothing.
func TestPostgresRecordOperatorErasureStep(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := user.NewPostgresStore(pool)
	subjectID := uuid.New()
	if _, _, err := user.NewService(store).Ensure(ctx, subjectID, user.Profile{Email: "operator@example.test"}); err != nil {
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
	at := now.Add(time.Hour).Truncate(time.Microsecond)
	counts := map[string]int64{"actor_columns_replaced": 2}
	refused := func(name string, requestID uuid.UUID, step user.DeletionStep, want error) {
		t.Helper()
		if err := store.RecordOperatorErasureStep(ctx, requestID, step, at, counts); !errors.Is(err, want) {
			t.Fatalf("%s: err = %v, want %v", name, err, want)
		}
		records, err := store.DeletionStepRecords(ctx, request.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, record := range records {
			if record.Step == step || record.Counts[user.OperatorRecordedCount] != 0 {
				t.Fatalf("%s: wrote proof %+v", name, records)
			}
		}
	}

	refused("pending request", request.ID, user.DeletionStepEraseCMS, user.ErrOperatorStepRefused)
	refused("unknown request", uuid.New(), user.DeletionStepEraseCMS, user.ErrNotFound)

	claimed, ok, err := store.ClaimDeletionRequest(ctx, now, time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim ok=%v err=%v", ok, err)
	}
	refused("processing request", request.ID, user.DeletionStepEraseCMS, user.ErrOperatorStepRefused)
	if err := store.CompleteServiceErasureStep(ctx, request.ID, *claimed.LeaseToken, user.DeletionStepEraseForms, now, map[string]int64{"responses_redacted": 1}); err != nil {
		t.Fatal(err)
	}
	if err := store.RetryDeletionRequest(ctx, request.ID, *claimed.LeaseToken, now, now.Add(30*time.Second), "erase_cms_rejected_404", true, false); err != nil {
		t.Fatal(err)
	}

	// The request waits on erase_cms: not on Forms or SkyMail, and a core
	// step is never the operator's to record.
	refused("another service's step", request.ID, user.DeletionStepEraseSkyMail, user.ErrOperatorStepRefused)
	if _, err := pool.Exec(ctx, `UPDATE account_deletion_requests SET last_error_code = 'anonymize_core_failed' WHERE id = $1`, request.ID); err != nil {
		t.Fatal(err)
	}
	refused("a core step", request.ID, user.DeletionStepAnonymizeCore, user.ErrOperatorStepRefused)
	// erase_cms_ is a prefix of no other step's codes; erase_cmsx is not erase_cms.
	if _, err := pool.Exec(ctx, `UPDATE account_deletion_requests SET last_error_code = 'erase_cmsx_failed' WHERE id = $1`, request.ID); err != nil {
		t.Fatal(err)
	}
	refused("a code that only starts like the step", request.ID, user.DeletionStepEraseCMS, user.ErrOperatorStepRefused)
	if _, err := pool.Exec(ctx, `UPDATE account_deletion_requests SET last_error_code = 'erase_cms_rejected_404', attempt_count = 3 WHERE id = $1`, request.ID); err != nil {
		t.Fatal(err)
	}

	if err := store.RecordOperatorErasureStep(ctx, request.ID, user.DeletionStepEraseCMS, at, counts); err != nil {
		t.Fatal(err)
	}
	records, err := store.DeletionStepRecords(ctx, request.ID)
	if err != nil || len(records) != 2 {
		t.Fatalf("proof = %+v err=%v", records, err)
	}
	forms, cms := records[0], records[1]
	if forms.Step != user.DeletionStepEraseForms || forms.Counts[user.OperatorRecordedCount] != 0 {
		t.Fatalf("the service's own proof changed: %+v", forms)
	}
	if cms.Step != user.DeletionStepEraseCMS || !cms.CompletedAt.Equal(at) || len(cms.Counts) != 2 ||
		cms.Counts["actor_columns_replaced"] != 2 || cms.Counts[user.OperatorRecordedCount] != 1 {
		t.Fatalf("operator proof = %+v", cms)
	}
	if counts[user.OperatorRecordedCount] != 0 {
		t.Fatalf("the caller's counts were changed: %v", counts)
	}
	again, err := store.DeletionRequest(ctx, subjectID)
	if err != nil {
		t.Fatal(err)
	}
	if again.Status != user.DeletionRequestPending || again.AttemptCount != 0 || again.LastErrorCode != "" ||
		again.LeaseToken != nil || again.LeaseUntil != nil || !again.NextAttemptAt.Equal(at) || !again.UpdatedAt.Equal(at) {
		t.Fatalf("request after the operator's step = %+v", again)
	}
	// Once more: the request no longer waits on anyone, so nothing changes.
	if err := store.RecordOperatorErasureStep(ctx, request.ID, user.DeletionStepEraseCMS, at.Add(time.Minute), map[string]int64{}); !errors.Is(err, user.ErrOperatorStepRefused) {
		t.Fatalf("second record err = %v", err)
	}
	// The worker claims it again and finds erase_cms checkpointed.
	reclaimed, ok, err := store.ClaimDeletionRequest(ctx, at, time.Minute)
	if err != nil || !ok || reclaimed.ID != request.ID {
		t.Fatalf("reclaim ok=%v err=%v %+v", ok, err, reclaimed)
	}
	done, err := store.CompletedDeletionSteps(ctx, request.ID, *reclaimed.LeaseToken)
	if err != nil || done[user.DeletionStepEraseCMS].IsZero() {
		t.Fatalf("completed steps = %v err=%v", done, err)
	}
}

// A step the request has checkpointed already is never recorded over, even
// when the request waits on that step's code.
func TestPostgresRecordOperatorErasureStepKeepsAnExistingCheckpoint(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := user.NewPostgresStore(pool)
	subjectID := uuid.New()
	if _, _, err := user.NewService(store).Ensure(ctx, subjectID, user.Profile{Email: "kept@example.test"}); err != nil {
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
	claimed, ok, err := store.ClaimDeletionRequest(ctx, now, time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim ok=%v err=%v", ok, err)
	}
	if err := store.CompleteServiceErasureStep(ctx, request.ID, *claimed.LeaseToken, user.DeletionStepEraseCMS, now, map[string]int64{"actor_columns_replaced": 5}); err != nil {
		t.Fatal(err)
	}
	if err := store.RetryDeletionRequest(ctx, request.ID, *claimed.LeaseToken, now, now, "erase_cms_checkpoint_failed", true, false); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordOperatorErasureStep(ctx, request.ID, user.DeletionStepEraseCMS, now.Add(time.Hour), map[string]int64{}); !errors.Is(err, user.ErrOperatorStepRefused) {
		t.Fatalf("err = %v", err)
	}
	records, err := store.DeletionStepRecords(ctx, request.ID)
	if err != nil || len(records) != 1 || records[0].Counts["actor_columns_replaced"] != 5 || records[0].Counts[user.OperatorRecordedCount] != 0 {
		t.Fatalf("proof = %+v err=%v", records, err)
	}
	if again, err := store.DeletionRequest(ctx, subjectID); err != nil || again.Status != user.DeletionRequestManualIntervention {
		t.Fatalf("request = %+v err=%v", again, err)
	}
}
