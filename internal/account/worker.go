package account

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/accessgate"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

type Store interface {
	ClaimDeletionRequest(context.Context, time.Time, time.Duration) (user.DeletionRequest, bool, error)
	CompletedDeletionSteps(context.Context, uuid.UUID, uuid.UUID) (map[user.DeletionStep]bool, error)
	CompleteDeletionStep(context.Context, uuid.UUID, uuid.UUID, user.DeletionStep, time.Time) error
	CompleteServiceErasureStep(context.Context, uuid.UUID, uuid.UUID, user.DeletionStep, time.Time, map[string]int64) error
	RetryDeletionRequest(context.Context, uuid.UUID, uuid.UUID, time.Time, string, bool, bool) error
	CompleteDeletionRequest(context.Context, uuid.UUID, uuid.UUID, time.Time) error
	AnonymizeAccount(context.Context, uuid.UUID, time.Time) error
	ProfileMediaForDeletion(context.Context, uuid.UUID) (*uuid.UUID, error)
}

type Identity interface {
	EnsureDisabled(context.Context, uuid.UUID) error
	EnsureLoggedOut(context.Context, uuid.UUID) error
	EnsureDeleted(context.Context, uuid.UUID) error
}

type MediaEraser interface {
	EnsureErased(context.Context, uuid.UUID, time.Time) error
	EnsureSubjectUploadsErased(context.Context, uuid.UUID, time.Time) error
}

type WorkerConfig struct {
	Now                  func() time.Time
	Lease                time.Duration
	RetryDelay           time.Duration
	MaxAttempts          int
	StepTimeout          time.Duration
	DeferredRetryHorizon time.Duration
	AccessBlocker        accessgate.BlockWriter
}

type Worker struct {
	store    Store
	identity Identity
	media    MediaEraser
	config   WorkerConfig
	// saga lists the steps of one pass in order. It is the core saga; the
	// service erasure group joins it in ticket 07.
	saga func(user.DeletionRequest, time.Time) []sagaStep
}

// sagaStep is one checkpointed step, or, when services is set, the service
// erasure group whose steps are checkpointed one by one.
type sagaStep struct {
	name     user.DeletionStep
	run      func(context.Context, uuid.UUID) error
	services *ServiceErasure
}

func NewWorker(store Store, identity Identity, config WorkerConfig, media ...MediaEraser) *Worker {
	if config.Now == nil {
		config.Now = func() time.Time { return time.Now().UTC() }
	}
	if config.Lease <= 0 {
		config.Lease = 5 * time.Minute
	}
	if config.RetryDelay < 0 {
		config.RetryDelay = 0
	}
	if config.MaxAttempts <= 0 {
		config.MaxAttempts = 8
	}
	if config.StepTimeout <= 0 {
		config.StepTimeout = 15 * time.Second
	}
	if config.DeferredRetryHorizon <= 0 {
		config.DeferredRetryHorizon = 48 * time.Hour
	}
	worker := &Worker{store: store, identity: identity, config: config}
	if len(media) > 0 {
		worker.media = media[0]
	}
	worker.saga = worker.coreSaga
	return worker
}

func (w *Worker) RunOnce(ctx context.Context) (bool, error) {
	now := w.config.Now()
	request, ok, err := w.store.ClaimDeletionRequest(ctx, now, w.config.Lease)
	if err != nil || !ok {
		return ok, err
	}
	if request.LeaseToken == nil {
		return true, fmt.Errorf("account erasure claim missing lease token")
	}
	leaseToken := *request.LeaseToken
	if w.config.AccessBlocker == nil {
		return true, w.retry(ctx, request, now, "platform_block_failed", errors.New("account access marker writer unavailable"), false)
	}
	if err := w.config.AccessBlocker.EnsureBlocked(ctx, request.SubjectID.String()); err != nil {
		return true, w.retry(ctx, request, now, "platform_block_failed", err, false)
	}
	completed, err := w.store.CompletedDeletionSteps(ctx, request.ID, leaseToken)
	if err != nil {
		return true, w.retry(ctx, request, now, "read_steps_failed", err, false)
	}
	for _, step := range w.saga(request, now) {
		if step.services != nil {
			if err := w.runServiceErasure(ctx, request, leaseToken, completed, now, step.services); err != nil {
				return true, err
			}
			continue
		}
		if completed[step.name] {
			continue
		}
		stepCtx, cancel := context.WithTimeout(ctx, w.config.StepTimeout)
		err := step.run(stepCtx, request.SubjectID)
		cancel()
		if err != nil {
			return true, w.retry(ctx, request, now, string(step.name)+"_failed", err, false)
		}
		if err := w.store.CompleteDeletionStep(ctx, request.ID, leaseToken, step.name, now); err != nil {
			return true, w.retry(ctx, request, now, string(step.name)+"_checkpoint_failed", err, false)
		}
	}
	if err := w.store.CompleteDeletionRequest(ctx, request.ID, leaseToken, now); err != nil {
		return true, w.retry(ctx, request, now, "complete_request_failed", err, false)
	}
	return true, nil
}

// coreSaga is today's order: block the identity, erase core, delete the
// identity last.
func (w *Worker) coreSaga(request user.DeletionRequest, now time.Time) []sagaStep {
	return []sagaStep{
		{name: user.DeletionStepDisableIdentity, run: w.identity.EnsureDisabled},
		{name: user.DeletionStepLogoutSessions, run: w.identity.EnsureLoggedOut},
		{name: user.DeletionStepAnonymizeCore, run: func(ctx context.Context, id uuid.UUID) error {
			return w.store.AnonymizeAccount(ctx, id, now)
		}},
		{name: user.DeletionStepEraseProfile, run: func(ctx context.Context, _ uuid.UUID) error {
			mediaID, err := w.store.ProfileMediaForDeletion(ctx, request.ID)
			if err != nil || mediaID == nil {
				return err
			}
			if w.media == nil {
				return fmt.Errorf("profile media eraser unavailable")
			}
			return w.media.EnsureErased(ctx, *mediaID, now)
		}},
		{name: user.DeletionStepEraseUploads, run: func(ctx context.Context, id uuid.UUID) error {
			if w.media == nil {
				return fmt.Errorf("staged upload eraser unavailable")
			}
			return w.media.EnsureSubjectUploadsErased(ctx, id, now)
		}},
		{name: user.DeletionStepDeleteIdentity, run: w.identity.EnsureDeleted},
	}
}

// retry releases the claim. A permanent failure goes to manual intervention at
// once; a deferred one (RetryAt) refunds the attempt until the horizon; any
// other failure spends an attempt and goes to manual intervention when the
// budget is spent.
func (w *Worker) retry(ctx context.Context, request user.DeletionRequest, now time.Time, code string, cause error, permanent bool) error {
	if request.LeaseToken == nil {
		return fmt.Errorf("account erasure %s without lease token: %w", code, cause)
	}
	next := now.Add(w.config.RetryDelay)
	manual := permanent || request.AttemptCount >= w.config.MaxAttempts
	refundAttempt := false
	var deferred interface{ RetryAt() time.Time }
	horizon := request.CreatedAt.Add(w.config.DeferredRetryHorizon)
	if !permanent && errors.As(cause, &deferred) && now.Before(horizon) {
		if retryAt := deferred.RetryAt(); retryAt.After(next) {
			next = retryAt
		}
		if next.After(horizon) {
			next = horizon
		}
		// A staged upload can legitimately retain its pre-publication lease for
		// the configured grace period and an R2 delete failure adds another
		// durable retry delay. Atomically refund this claim under the lease fence
		// so any later genuine failure still receives the full attempt budget.
		manual = false
		refundAttempt = true
	}
	if err := w.store.RetryDeletionRequest(ctx, request.ID, *request.LeaseToken, next, code, manual, refundAttempt); err != nil {
		return fmt.Errorf("account erasure %s; checkpoint retry: %w", code, err)
	}
	return fmt.Errorf("account erasure %s: %w", code, cause)
}

func Maintain(ctx context.Context, worker *Worker, interval time.Duration, onError func(error)) {
	if interval <= 0 {
		interval = 2 * time.Second
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			worked, err := worker.RunOnce(ctx)
			if err != nil && onError != nil {
				onError(err)
			}
			if worked {
				continue
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}
