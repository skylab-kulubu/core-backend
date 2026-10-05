package account_test

import (
	"context"
	"errors"
	"slices"
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
	worker := account.NewWorkerWithConfiguredWaits(store, identity, account.WorkerConfig{
		ContactConsents: consentsErased{},
		Services:        erasedServices(),
		Now:             func() time.Time { return now }, AccessBlocker: blocker,
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
	worker := account.NewWorkerWithConfiguredWaits(store, identity, account.WorkerConfig{
		ContactConsents: consentsErased{},
		Services:        erasedServices(),
		Now:             func() time.Time { return now }, Lease: time.Minute, RetryDelay: 0, MaxAttempts: 3,
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
	worker := account.NewWorkerWithConfiguredWaits(store, failingIdentity{}, account.WorkerConfig{
		ContactConsents: consentsErased{},
		Services:        erasedServices(),
		Now:             func() time.Time { return now }, MaxAttempts: 1, AccessBlocker: &accountBlockWriter{},
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
	worker := account.NewWorkerWithConfiguredWaits(store, successfulIdentity{}, account.WorkerConfig{
		ContactConsents: consentsErased{},
		Services:        erasedServices(),
		Now:             func() time.Time { return now }, RetryDelay: 30 * time.Second,
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
	worker := account.NewWorkerWithConfiguredWaits(store, identity, account.WorkerConfig{
		ContactConsents: consentsErased{},
		Services:        erasedServices(),
		Now:             func() time.Time { return now }, MaxAttempts: 2, DeferredRetryHorizon: 12 * time.Hour,
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
	worker := account.NewWorkerWithConfiguredWaits(store, successfulIdentity{}, account.WorkerConfig{
		ContactConsents: consentsErased{},
		Services:        erasedServices(),
		Now:             func() time.Time { return now }, MaxAttempts: 1, DeferredRetryHorizon: 48 * time.Hour,
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
	worker := account.NewWorkerWithConfiguredWaits(store, successfulIdentity{}, account.WorkerConfig{
		ContactConsents: consentsErased{},
		Services:        erasedServices(),
		Now:             func() time.Time { return now }, MaxAttempts: 1, DeferredRetryHorizon: 48 * time.Hour,
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

// fewPerPass is a person's recorded uploads for erase_profile_media and the
// eraser that erases only perPass of them in each pass, as a pass the step's
// time cuts short. The broken one is never erased.
type fewPerPass struct {
	*user.MemoryStore
	left    []uuid.UUID
	perPass int
	broken  uuid.UUID
	erased  map[time.Time]int
}

// newFewPerPass requests the erasure of a person with n recorded uploads.
func newFewPerPass(t *testing.T, n, perPass int) (*fewPerPass, user.DeletionRequest) {
	t.Helper()
	ctx := context.Background()
	store := &fewPerPass{MemoryStore: user.NewMemoryStore(), perPass: perPass, erased: map[time.Time]int{}}
	for range n {
		store.left = append(store.left, uuid.New())
	}
	subjectID := uuid.New()
	if _, _, err := user.NewService(store.MemoryStore).Ensure(ctx, subjectID, user.Profile{Email: "many-files@example.com"}); err != nil {
		t.Fatal(err)
	}
	request, err := store.RequestDeletion(ctx, subjectID, nil)
	if err != nil {
		t.Fatal(err)
	}
	return store, request
}

func (m *fewPerPass) MediaForDeletion(context.Context, uuid.UUID) ([]uuid.UUID, error) {
	return slices.Clone(m.left), nil
}

// EnsureErased counts a pass by its time, which the worker hands every call
// of that pass.
func (m *fewPerPass) EnsureErased(_ context.Context, id uuid.UUID, at time.Time) error {
	if id == m.broken || m.erased[at] >= m.perPass {
		return errors.New("media not erased")
	}
	m.erased[at]++
	m.left = slices.DeleteFunc(m.left, func(left uuid.UUID) bool { return left == id })
	return nil
}

func (m *fewPerPass) EnsureSubjectUploadsErased(context.Context, uuid.UUID, time.Time) error {
	return nil
}

// A person with fifty recorded uploads whose erasure reaches
// erase_profile_media past the deferral horizon (a service kept it waiting):
// every pass erases a few and gives its attempt back, even with a budget of
// one, until the pass that erases the last completes the request.
func TestWorkerGivesBackAPassThatErasedSomePastTheHorizon(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, request := newFewPerPass(t, 50, 7)
	now := request.CreatedAt.Add(49 * time.Hour)
	confirmDeletionProjection(t, store, request, now)
	worker := account.NewWorkerWithConfiguredWaits(store, successfulIdentity{}, account.WorkerConfig{
		ContactConsents: consentsErased{},
		Services:        erasedServices(),
		Now:             func() time.Time { return now }, MaxAttempts: 1, DeferredRetryHorizon: 48 * time.Hour,
		AccessBlocker: &accountBlockWriter{},
	}, store)

	passes := 0
	for {
		if passes++; passes > 10 {
			t.Fatal("no pass finished the step")
		}
		worked, err := worker.RunOnce(ctx)
		if !worked {
			t.Fatalf("pass %d was not claimed", passes)
		}
		if err == nil {
			break
		}
		pending, err := store.DeletionRequest(ctx, request.SubjectID)
		if err != nil {
			t.Fatal(err)
		}
		if pending.Status != user.DeletionRequestPending || pending.AttemptCount != 0 || !pending.NextAttemptAt.Equal(now.Add(30*time.Second)) {
			t.Fatalf("pass %d past the horizon: %+v; want pending, 0 attempts, in 30 s", passes, pending)
		}
		now = now.Add(30 * time.Second)
	}
	completed, err := store.DeletionRequest(ctx, request.SubjectID)
	if err != nil {
		t.Fatal(err)
	}
	if completed.Status != user.DeletionRequestCompleted || completed.AttemptCount != 1 || passes != 8 || len(store.left) != 0 {
		t.Fatalf("after %d passes: %+v, %d left; want completed in 8 passes, 1 attempt", passes, completed, len(store.left))
	}
}

// Past the horizon, the passes that erase some give their attempts back, and
// once only a Media that never goes is left, each pass erases nothing and
// spends its attempt: the eighth sends the request to manual intervention.
func TestWorkerSpendsTheBudgetOnPassesThatEraseNothing(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, request := newFewPerPass(t, 50, 7)
	store.broken = store.left[0]
	now := request.CreatedAt.Add(49 * time.Hour)
	confirmDeletionProjection(t, store, request, now)
	worker := account.NewWorkerWithConfiguredWaits(store, successfulIdentity{}, account.WorkerConfig{
		ContactConsents: consentsErased{},
		Services:        erasedServices(),
		Now:             func() time.Time { return now }, MaxAttempts: 8, DeferredRetryHorizon: 48 * time.Hour,
		AccessBlocker: &accountBlockWriter{},
	}, store)

	for pass := 1; pass <= 7; pass++ {
		if worked, err := worker.RunOnce(ctx); !worked || err == nil {
			t.Fatalf("pass %d worked=%v err=%v", pass, worked, err)
		}
		pending, err := store.DeletionRequest(ctx, request.SubjectID)
		if err != nil {
			t.Fatal(err)
		}
		if pending.AttemptCount != 0 {
			t.Fatalf("pass %d erased some and spent %d attempts", pass, pending.AttemptCount)
		}
		now = now.Add(30 * time.Second)
	}
	if !slices.Equal(store.left, []uuid.UUID{store.broken}) {
		t.Fatalf("%d left after the passes that erased some; want only the broken one", len(store.left))
	}
	for attempt := 1; attempt <= 8; attempt++ {
		if worked, err := worker.RunOnce(ctx); !worked || err == nil {
			t.Fatalf("attempt %d worked=%v err=%v", attempt, worked, err)
		}
		state, err := store.DeletionRequest(ctx, request.SubjectID)
		if err != nil {
			t.Fatal(err)
		}
		want := user.DeletionRequestPending
		if attempt == 8 {
			want = user.DeletionRequestManualIntervention
		}
		if state.Status != want || state.AttemptCount != attempt || state.LastErrorCode != "erase_profile_media_failed" {
			t.Fatalf("attempt %d: %+v; want %s", attempt, state, want)
		}
	}
	if worked, err := worker.RunOnce(ctx); worked || err != nil {
		t.Fatalf("manual request was reclaimed: worked=%v err=%v", worked, err)
	}
}

// Inside the horizon a pass that erased some is the deferral it always was:
// its attempt comes back and its next claim is no later than the horizon.
// The pass at the horizon is given back all the same.
func TestWorkerKeepsAPassThatErasedSomeInsideTheHorizonClampedToIt(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, request := newFewPerPass(t, 20, 7)
	horizon := request.CreatedAt.Add(48 * time.Hour)
	now := horizon.Add(-10 * time.Second)
	confirmDeletionProjection(t, store, request, now)
	worker := account.NewWorkerWithConfiguredWaits(store, successfulIdentity{}, account.WorkerConfig{
		ContactConsents: consentsErased{},
		Services:        erasedServices(),
		Now:             func() time.Time { return now }, MaxAttempts: 1, DeferredRetryHorizon: 48 * time.Hour,
		AccessBlocker: &accountBlockWriter{},
	}, store)

	for _, next := range []time.Time{horizon, horizon.Add(30 * time.Second)} {
		if worked, err := worker.RunOnce(ctx); !worked || err == nil {
			t.Fatalf("pass at %v worked=%v err=%v", now, worked, err)
		}
		pending, err := store.DeletionRequest(ctx, request.SubjectID)
		if err != nil {
			t.Fatal(err)
		}
		if pending.Status != user.DeletionRequestPending || pending.AttemptCount != 0 || !pending.NextAttemptAt.Equal(next) {
			t.Fatalf("pass at %v: %+v; want pending, 0 attempts, next at %v", now, pending, next)
		}
		now = next
	}
	if worked, err := worker.RunOnce(ctx); !worked || err != nil {
		t.Fatalf("last pass worked=%v err=%v", worked, err)
	}
	if len(store.left) != 0 {
		t.Fatalf("%d left", len(store.left))
	}
}
