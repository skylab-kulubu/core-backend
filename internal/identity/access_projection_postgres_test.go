package identity_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/accessgate"
	"github.com/skylab-kulubu/core-backend/internal/account"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
	"github.com/skylab-kulubu/core-backend/internal/testredis"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

type pausedProjectionWriter struct {
	gate    *accessgate.RedisGate
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (w *pausedProjectionWriter) EnsureBlocked(ctx context.Context, subject string) error {
	w.once.Do(func() { close(w.started) })
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-w.release:
	}
	return w.gate.EnsureBlocked(ctx, subject)
}

func (w *pausedProjectionWriter) AssertMarker(ctx context.Context, subject string) error {
	return w.gate.AssertMarker(ctx, subject)
}

func (w *pausedProjectionWriter) EnsureContract(ctx context.Context) error {
	return w.gate.EnsureContract(ctx)
}

func TestCommittedDeletionCannotReachWorkerOrOutboxBeforeRedisProjection(t *testing.T) {
	pool := testpostgres.Start(t)
	redisClient := testredis.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	gate := accessgate.NewRedisGate(redisClient, time.Second, 0, 0)
	if err := gate.EnsureContract(ctx); err != nil {
		t.Fatal(err)
	}
	store := user.NewPostgresStore(pool)
	directory := identity.NewMemory()
	subjectID := uuid.New()
	directory.PutUser(identity.Person{ID: subjectID, Email: "commit-gap@example.test"})
	if _, _, err := user.NewService(store).Ensure(ctx, subjectID, user.Profile{Email: "commit-gap@example.test"}); err != nil {
		t.Fatal(err)
	}
	writer := &pausedProjectionWriter{
		gate: gate, started: make(chan struct{}), release: make(chan struct{}),
	}
	projector := account.NewAccessProjector(store, writer, func() time.Time {
		return time.Date(2026, 9, 20, 21, 0, 0, 0, time.UTC)
	})
	service := identity.NewServiceWithOptions(directory, store, authz.NewAuthorizer(authz.DefaultPolicy()), identity.Options{
		AccountErasureEnabled: true,
		AccessProjector:       projector,
	})

	deleteDone := make(chan error, 1)
	go func() { deleteDone <- service.DeleteUser(ctx, privileged(), subjectID) }()
	select {
	case <-writer.started:
	case <-time.After(5 * time.Second):
		t.Fatal("delete did not reach the post-commit projection seam")
	}

	request, err := store.DeletionRequest(ctx, subjectID)
	if err != nil || request.PlatformBlockedAt != nil {
		t.Fatalf("committed request=%+v err=%v", request, err)
	}
	if claimed, ok, err := store.ClaimDeletionRequest(ctx, time.Now().Add(time.Hour), time.Minute); err != nil || ok {
		t.Fatalf("pre-projection worker claim=%+v ok=%v err=%v", claimed, ok, err)
	}
	var outboxAvailable bool
	if err := pool.QueryRow(ctx, `SELECT available_at <= now() FROM account_deletion_outbox WHERE request_id=$1`, request.ID).Scan(&outboxAvailable); err != nil {
		t.Fatal(err)
	}
	if outboxAvailable {
		t.Fatal("outbox became visible during the post-commit/pre-marker interval")
	}

	close(writer.release)
	if err := <-deleteDone; err != nil {
		t.Fatal(err)
	}
	if decision := gate.Check(ctx, subjectID.String()); decision != accessgate.Blocked {
		t.Fatalf("decision after delete response = %q", decision)
	}
	request, err = store.DeletionRequest(ctx, subjectID)
	if err != nil || request.PlatformBlockedAt == nil {
		t.Fatalf("confirmed request=%+v err=%v", request, err)
	}
	if err := pool.QueryRow(ctx, `SELECT available_at <= now() FROM account_deletion_outbox WHERE request_id=$1`, request.ID).Scan(&outboxAvailable); err != nil {
		t.Fatal(err)
	}
	if !outboxAvailable {
		t.Fatal("outbox remained fenced after projection confirmation")
	}
}
