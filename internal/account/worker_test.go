package account_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/account"
	"github.com/skylab-kulubu/core-backend/internal/erasure"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

type uncertainIdentity struct {
	disableCalls int
	logoutCalls  int
	deleteCalls  int
}

type failingIdentity struct{}

type deleteFailingIdentity struct {
	deleteCalls int
}

type noAccountMedia struct{}

// erasedServices is a service erasure group whose every service confirms at
// once, for tests about the core steps around the group.
func erasedServices() account.ServiceErasure {
	services := account.ServiceErasure{Addresses: &fixedAddresses{}}
	for _, service := range erasure.Registry() {
		services.Steps = append(services.Steps, account.ServiceStep{Step: service.Step, Sender: erasedService{}})
	}
	return services
}

type erasedService struct{}

func (erasedService) Erase(context.Context, erasure.Command) (erasure.Result, error) {
	return erasure.Result{Counts: map[string]int64{}}, nil
}

type accountBlockWriter struct {
	err   error
	calls int
}

type deletionProjectionMarker interface {
	MarkDeletionPlatformBlocked(context.Context, uuid.UUID, time.Time) error
}

func confirmDeletionProjection(t *testing.T, store deletionProjectionMarker, request user.DeletionRequest, at time.Time) {
	t.Helper()
	if err := store.MarkDeletionPlatformBlocked(context.Background(), request.ID, at); err != nil {
		t.Fatal(err)
	}
}

type countingIdentity struct {
	disableCalls int
	logoutCalls  int
	deleteCalls  int
}

func (i *countingIdentity) EnsureDisabled(context.Context, uuid.UUID) error {
	i.disableCalls++
	return nil
}

func (i *countingIdentity) EnsureLoggedOut(context.Context, uuid.UUID) error {
	i.logoutCalls++
	return nil
}

func (i *countingIdentity) EnsureDeleted(context.Context, uuid.UUID) error {
	i.deleteCalls++
	return nil
}

func (w *accountBlockWriter) EnsureBlocked(context.Context, string) error {
	w.calls++
	return w.err
}

type retryAtError struct {
	at time.Time
}

func (e retryAtError) Error() string      { return "durable cleanup deferred" }
func (e retryAtError) RetryAt() time.Time { return e.at }

type deferredAccountMedia struct {
	retryAt []time.Time
	calls   int
}

func (m *deferredAccountMedia) EnsureErased(context.Context, uuid.UUID, time.Time) error { return nil }
func (m *deferredAccountMedia) EnsureSubjectUploadsErased(context.Context, uuid.UUID, time.Time) error {
	if m.calls >= len(m.retryAt) {
		return nil
	}
	retryAt := m.retryAt[m.calls]
	m.calls++
	return retryAtError{at: retryAt}
}

func (noAccountMedia) EnsureErased(context.Context, uuid.UUID, time.Time) error { return nil }
func (noAccountMedia) EnsureSubjectUploadsErased(context.Context, uuid.UUID, time.Time) error {
	return nil
}

func (failingIdentity) EnsureDisabled(context.Context, uuid.UUID) error {
	return errors.New("keycloak unavailable")
}
func (failingIdentity) EnsureLoggedOut(context.Context, uuid.UUID) error { return nil }
func (failingIdentity) EnsureDeleted(context.Context, uuid.UUID) error   { return nil }

func (*deleteFailingIdentity) EnsureDisabled(context.Context, uuid.UUID) error  { return nil }
func (*deleteFailingIdentity) EnsureLoggedOut(context.Context, uuid.UUID) error { return nil }
func (i *deleteFailingIdentity) EnsureDeleted(context.Context, uuid.UUID) error {
	i.deleteCalls++
	return errors.New("keycloak delete unavailable")
}

func (i *uncertainIdentity) EnsureDisabled(context.Context, uuid.UUID) error {
	i.disableCalls++
	return nil
}

func (i *uncertainIdentity) EnsureLoggedOut(context.Context, uuid.UUID) error {
	i.logoutCalls++
	if i.logoutCalls == 1 {
		return errors.New("response lost after logout")
	}
	return nil
}

func (i *uncertainIdentity) EnsureDeleted(context.Context, uuid.UUID) error {
	i.deleteCalls++
	return nil
}

func TestWorkerReassertsPlatformMarkerBeforeFirstErasureSideEffect(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := user.NewMemoryStore()
	subjectID := uuid.New()
	if _, _, err := user.NewService(store).Ensure(ctx, subjectID, user.Profile{Email: "worker-gate@example.test"}); err != nil {
		t.Fatal(err)
	}
	request, err := store.RequestDeletion(ctx, subjectID, nil)
	if err != nil {
		t.Fatal(err)
	}
	now := request.NextAttemptAt
	if err := store.MarkDeletionPlatformBlocked(ctx, request.ID, now); err != nil {
		t.Fatal(err)
	}
	blocker := &accountBlockWriter{err: errors.New("redis unavailable")}
	identity := &countingIdentity{}
	worker := account.NewWorker(store, identity, account.WorkerConfig{
		Services: erasedServices(),
		Now:      func() time.Time { return now }, AccessBlocker: blocker,
	}, noAccountMedia{})

	if worked, err := worker.RunOnce(ctx); !worked || err == nil {
		t.Fatalf("failed reassert worked=%v err=%v", worked, err)
	}
	if identity.disableCalls != 0 || identity.logoutCalls != 0 || identity.deleteCalls != 0 {
		t.Fatalf("identity side effect ran: %+v", identity)
	}
	blocker.err = nil
	if worked, err := worker.RunOnce(ctx); !worked || err != nil {
		t.Fatalf("retry worked=%v err=%v", worked, err)
	}
	if blocker.calls != 2 || identity.disableCalls != 1 {
		t.Fatalf("block calls=%d identity=%+v", blocker.calls, identity)
	}
}

func TestWorkerRetriesUncertainExternalEffectWithoutRepeatingCompletedSteps(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := user.NewMemoryStore()
	users := user.NewService(store)
	subjectID := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	if _, _, err := users.Ensure(ctx, subjectID, user.Profile{Email: "ada@example.com", FirstName: "Ada"}); err != nil {
		t.Fatal(err)
	}
	request, err := store.RequestDeletion(ctx, subjectID, nil)
	if err != nil {
		t.Fatal(err)
	}

	now := request.NextAttemptAt.Add(time.Minute)
	confirmDeletionProjection(t, store, request, now)
	identity := &uncertainIdentity{}
	worker := account.NewWorker(store, identity, account.WorkerConfig{
		Services: erasedServices(),
		Now:      func() time.Time { return now }, Lease: time.Minute, RetryDelay: 0, MaxAttempts: 3,
		AccessBlocker: &accountBlockWriter{},
	}, noAccountMedia{})

	worked, err := worker.RunOnce(ctx)
	if !worked || err == nil {
		t.Fatalf("first run worked=%v err=%v", worked, err)
	}
	pending, err := store.DeletionRequest(ctx, subjectID)
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != user.DeletionRequestPending || pending.LastErrorCode != "logout_sessions_failed" {
		t.Fatalf("retry state: %+v", pending)
	}

	worked, err = worker.RunOnce(ctx)
	if err != nil || !worked {
		t.Fatalf("retry worked=%v err=%v", worked, err)
	}
	completed, err := store.DeletionRequest(ctx, subjectID)
	if err != nil {
		t.Fatal(err)
	}
	if completed.Status != user.DeletionRequestCompleted || completed.CompletedAt == nil {
		t.Fatalf("completion state: %+v", completed)
	}
	tombstone, err := store.Get(ctx, subjectID)
	if err != nil {
		t.Fatal(err)
	}
	if tombstone.AccountState != user.AccountAnonymized || tombstone.Email != "" || tombstone.FirstName != "" {
		t.Fatalf("tombstone: %+v", tombstone)
	}
	if identity.disableCalls != 1 || identity.logoutCalls != 2 || identity.deleteCalls != 1 {
		t.Fatalf("identity calls disable=%d logout=%d delete=%d", identity.disableCalls, identity.logoutCalls, identity.deleteCalls)
	}

	worked, err = worker.RunOnce(ctx)
	if err != nil || worked {
		t.Fatalf("empty queue worked=%v err=%v", worked, err)
	}
}

func TestWorkerSurfacesManualInterventionAfterRetryBudget(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := user.NewMemoryStore()
	subjectID := uuid.MustParse("99999999-8888-7777-6666-555555555555")
	if _, _, err := user.NewService(store).Ensure(ctx, subjectID, user.Profile{Email: "blocked@example.com"}); err != nil {
		t.Fatal(err)
	}
	request, err := store.RequestDeletion(ctx, subjectID, nil)
	if err != nil {
		t.Fatal(err)
	}
	now := request.NextAttemptAt
	confirmDeletionProjection(t, store, request, now)
	worker := account.NewWorker(store, failingIdentity{}, account.WorkerConfig{
		Services: erasedServices(),
		Now:      func() time.Time { return now }, MaxAttempts: 1, AccessBlocker: &accountBlockWriter{},
	}, noAccountMedia{})
	if worked, err := worker.RunOnce(ctx); !worked || err == nil {
		t.Fatalf("worked=%v err=%v", worked, err)
	}
	request, err = store.DeletionRequest(ctx, subjectID)
	if err != nil {
		t.Fatal(err)
	}
	if request.Status != user.DeletionRequestManualIntervention || request.LastErrorCode != "disable_identity_failed" {
		t.Fatalf("manual intervention state: %+v", request)
	}
	if worked, err := worker.RunOnce(ctx); worked || err != nil {
		t.Fatalf("manual request was reclaimed: worked=%v err=%v", worked, err)
	}
}

func TestWorkerDefersStagedCleanupWithoutExhaustingAttemptBudget(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := user.NewMemoryStore()
	subjectID := uuid.MustParse("11111111-aaaa-bbbb-cccc-555555555555")
	if _, _, err := user.NewService(store).Ensure(ctx, subjectID, user.Profile{Email: "deferred@example.com"}); err != nil {
		t.Fatal(err)
	}
	request, err := store.RequestDeletion(ctx, subjectID, nil)
	if err != nil {
		t.Fatal(err)
	}
	now := request.CreatedAt
	confirmDeletionProjection(t, store, request, now)
	firstRetry := now.Add(24 * time.Hour)
	secondRetry := now.Add(25 * time.Hour)
	media := &deferredAccountMedia{retryAt: []time.Time{firstRetry, secondRetry}}
	worker := account.NewWorker(store, successfulIdentity{}, account.WorkerConfig{
		Services: erasedServices(),
		Now:      func() time.Time { return now }, RetryDelay: 30 * time.Second,
		MaxAttempts: 1, DeferredRetryHorizon: 48 * time.Hour, AccessBlocker: &accountBlockWriter{},
	}, media)

	if worked, err := worker.RunOnce(ctx); !worked || err == nil {
		t.Fatalf("first deferred run worked=%v err=%v", worked, err)
	}
	pending, err := store.DeletionRequest(ctx, subjectID)
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != user.DeletionRequestPending || !pending.NextAttemptAt.Equal(firstRetry) {
		t.Fatalf("first deferral: %+v", pending)
	}
	now = firstRetry.Add(-time.Second)
	if worked, err := worker.RunOnce(ctx); worked || err != nil {
		t.Fatalf("claimed before staged retry: worked=%v err=%v", worked, err)
	}
	now = firstRetry
	if worked, err := worker.RunOnce(ctx); !worked || err == nil {
		t.Fatalf("second deferred run worked=%v err=%v", worked, err)
	}
	pending, err = store.DeletionRequest(ctx, subjectID)
	if err != nil {
		t.Fatal(err)
	}
	if pending.AttemptCount != 0 || pending.Status != user.DeletionRequestPending || !pending.NextAttemptAt.Equal(secondRetry) {
		t.Fatalf("second deferral exhausted budget: %+v", pending)
	}
	now = secondRetry
	if worked, err := worker.RunOnce(ctx); !worked || err != nil {
		t.Fatalf("cleanup completion worked=%v err=%v", worked, err)
	}
	completed, err := store.DeletionRequest(ctx, subjectID)
	if err != nil {
		t.Fatal(err)
	}
	if completed.Status != user.DeletionRequestCompleted || completed.CompletedAt == nil {
		t.Fatalf("completion after durable deferrals: %+v", completed)
	}
	if completed.AttemptCount != 1 {
		t.Fatalf("deferrals leaked into durable attempt count: %+v", completed)
	}
}

func TestWorkerPreservesFullFailureBudgetAfterManyDeferrals(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := user.NewMemoryStore()
	subjectID := uuid.MustParse("33333333-aaaa-bbbb-cccc-555555555555")
	if _, _, err := user.NewService(store).Ensure(ctx, subjectID, user.Profile{Email: "budget-after-deferrals@example.com"}); err != nil {
		t.Fatal(err)
	}
	request, err := store.RequestDeletion(ctx, subjectID, nil)
	if err != nil {
		t.Fatal(err)
	}
	now := request.CreatedAt
	confirmDeletionProjection(t, store, request, now)
	retries := make([]time.Time, 6)
	for i := range retries {
		retries[i] = now.Add(time.Duration(i+1) * time.Hour)
	}
	media := &deferredAccountMedia{retryAt: retries}
	identity := &deleteFailingIdentity{}
	worker := account.NewWorker(store, identity, account.WorkerConfig{
		Services: erasedServices(),
		Now:      func() time.Time { return now }, MaxAttempts: 2, DeferredRetryHorizon: 12 * time.Hour,
		AccessBlocker: &accountBlockWriter{},
	}, media)
	for i, retryAt := range retries {
		if worked, err := worker.RunOnce(ctx); !worked || err == nil {
			t.Fatalf("deferral %d worked=%v err=%v", i, worked, err)
		}
		pending, err := store.DeletionRequest(ctx, subjectID)
		if err != nil {
			t.Fatal(err)
		}
		if pending.Status != user.DeletionRequestPending || pending.AttemptCount != 0 || !pending.NextAttemptAt.Equal(retryAt) {
			t.Fatalf("deferral %d poisoned failure budget: %+v", i, pending)
		}
		now = retryAt
	}

	if worked, err := worker.RunOnce(ctx); !worked || err == nil {
		t.Fatalf("first genuine failure worked=%v err=%v", worked, err)
	}
	pending, err := store.DeletionRequest(ctx, subjectID)
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != user.DeletionRequestPending || pending.AttemptCount != 1 {
		t.Fatalf("first genuine failure lost its retry: %+v", pending)
	}
	if worked, err := worker.RunOnce(ctx); !worked || err == nil {
		t.Fatalf("second genuine failure worked=%v err=%v", worked, err)
	}
	manual, err := store.DeletionRequest(ctx, subjectID)
	if err != nil {
		t.Fatal(err)
	}
	if manual.Status != user.DeletionRequestManualIntervention || manual.AttemptCount != 2 || identity.deleteCalls != 2 {
		t.Fatalf("genuine failure budget: request=%+v deleteCalls=%d", manual, identity.deleteCalls)
	}
}

func TestWorkerClampsDeferredRetryToPolicyHorizon(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := user.NewMemoryStore()
	subjectID := uuid.MustParse("44444444-aaaa-bbbb-cccc-555555555555")
	if _, _, err := user.NewService(store).Ensure(ctx, subjectID, user.Profile{Email: "clamped-deferral@example.com"}); err != nil {
		t.Fatal(err)
	}
	request, err := store.RequestDeletion(ctx, subjectID, nil)
	if err != nil {
		t.Fatal(err)
	}
	now := request.CreatedAt
	confirmDeletionProjection(t, store, request, now)
	horizon := now.Add(48 * time.Hour)
	media := &deferredAccountMedia{retryAt: []time.Time{now.Add(7 * 24 * time.Hour)}}
	worker := account.NewWorker(store, successfulIdentity{}, account.WorkerConfig{
		Services: erasedServices(),
		Now:      func() time.Time { return now }, MaxAttempts: 1, DeferredRetryHorizon: 48 * time.Hour,
		AccessBlocker: &accountBlockWriter{},
	}, media)
	if worked, err := worker.RunOnce(ctx); !worked || err == nil {
		t.Fatalf("clamped deferred run worked=%v err=%v", worked, err)
	}
	pending, err := store.DeletionRequest(ctx, subjectID)
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != user.DeletionRequestPending || pending.AttemptCount != 0 || !pending.NextAttemptAt.Equal(horizon) {
		t.Fatalf("deferred retry escaped policy horizon: %+v horizon=%s", pending, horizon)
	}
}

func TestWorkerAllowsDeferredCleanupToBecomeManualAfterPolicyHorizon(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := user.NewMemoryStore()
	subjectID := uuid.MustParse("22222222-aaaa-bbbb-cccc-555555555555")
	if _, _, err := user.NewService(store).Ensure(ctx, subjectID, user.Profile{Email: "expired-deferral@example.com"}); err != nil {
		t.Fatal(err)
	}
	request, err := store.RequestDeletion(ctx, subjectID, nil)
	if err != nil {
		t.Fatal(err)
	}
	now := request.CreatedAt.Add(49 * time.Hour)
	confirmDeletionProjection(t, store, request, now)
	media := &deferredAccountMedia{retryAt: []time.Time{now.Add(time.Hour)}}
	worker := account.NewWorker(store, successfulIdentity{}, account.WorkerConfig{
		Services: erasedServices(),
		Now:      func() time.Time { return now }, MaxAttempts: 1, DeferredRetryHorizon: 48 * time.Hour,
		AccessBlocker: &accountBlockWriter{},
	}, media)

	if worked, err := worker.RunOnce(ctx); !worked || err == nil {
		t.Fatalf("expired deferred run worked=%v err=%v", worked, err)
	}
	manual, err := store.DeletionRequest(ctx, subjectID)
	if err != nil {
		t.Fatal(err)
	}
	if manual.Status != user.DeletionRequestManualIntervention || manual.LastErrorCode != "erase_staged_uploads_failed" {
		t.Fatalf("expired deferral did not enter manual intervention: %+v", manual)
	}
}
