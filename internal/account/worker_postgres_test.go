package account_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/account"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

func TestPostgresWorkerPersistsProgressAcrossRestart(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := user.NewPostgresStore(pool)
	subjectID := uuid.MustParse("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
	if _, _, err := user.NewService(store).Ensure(ctx, subjectID, user.Profile{Email: "worker@example.com", FirstName: "Worker"}); err != nil {
		t.Fatal(err)
	}
	request, err := store.RequestDeletion(ctx, subjectID, nil)
	if err != nil {
		t.Fatal(err)
	}

	now := request.NextAttemptAt.Add(time.Minute)
	confirmDeletionProjection(t, store, request, now)
	identity := &uncertainIdentity{}
	config := account.WorkerConfig{
		Now: func() time.Time { return now }, Lease: time.Minute, MaxAttempts: 3,
		AccessBlocker: &accountBlockWriter{},
	}
	if worked, err := account.NewWorker(store, identity, config, noAccountMedia{}).RunOnce(ctx); !worked || err == nil {
		t.Fatalf("first worker worked=%v err=%v", worked, err)
	}

	if worked, err := account.NewWorker(store, identity, config, noAccountMedia{}).RunOnce(ctx); !worked || err != nil {
		t.Fatalf("restarted worker worked=%v err=%v", worked, err)
	}
	completed, err := store.DeletionRequest(ctx, subjectID)
	if err != nil {
		t.Fatal(err)
	}
	if completed.ID != request.ID || completed.Status != user.DeletionRequestCompleted || completed.AttemptCount != 2 {
		t.Fatalf("request after restart: %+v", completed)
	}
	var stepCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM account_deletion_steps WHERE request_id = $1`, request.ID).Scan(&stepCount); err != nil {
		t.Fatal(err)
	}
	if stepCount != 6 {
		t.Fatalf("step count = %d, want 6", stepCount)
	}
}
