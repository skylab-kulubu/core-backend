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
	RetryDeletionRequest(ctx context.Context, requestID, leaseToken uuid.UUID, at, next time.Time, code string, manual, refundAttempt bool) error
	CompleteDeletionRequest(context.Context, uuid.UUID, uuid.UUID, time.Time) error
	AnonymizeAccount(context.Context, uuid.UUID, time.Time, []string) error
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
	// Services is the service erasure step group (ADR-0051), built by
	// NewServiceErasure. The saga holds a step for every registry entry
	// whether or not Services has a sender for it: an entry without one sends
	// the request to manual intervention, so no service is ever passed over
	// as erased.
	Services ServiceErasure
}

type Worker struct {
	store    Store
	identity Identity
	media    MediaEraser
	config   WorkerConfig
	// services holds one step per registry entry, in registry order.
	services ServiceErasure
	// saga lists the steps of one pass in order.
	saga func(user.DeletionRequest, time.Time) []sagaStep
}

// sagaStep is one checkpointed step, or, when services is set, the service
// erasure group whose steps are checkpointed one by one. A step with
// withAddresses runs with the person's addresses of the pass instead of run.
type sagaStep struct {
	name          user.DeletionStep
	run           func(context.Context, uuid.UUID) error
	withAddresses func(context.Context, uuid.UUID, []string) error
	services      *ServiceErasure
}

// erasurePass is what one pass reads once and keeps for that pass only: the
// person's addresses, for the service steps and anonymize_core alike.
type erasurePass struct {
	emails []string
	read   bool
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
	worker := &Worker{store: store, identity: identity, config: config, services: registryServices(config.Services)}
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
	// Steps run in order and a failure ends the pass, so a step runs only
	// after every step before it is checkpointed: anonymize_core waits for
	// every service, delete_identity for everything.
	pass := &erasurePass{}
	for _, step := range w.saga(request, now) {
		if step.services != nil {
			if err := w.runServiceErasure(ctx, request, leaseToken, completed, now, step.services, pass); err != nil {
				return true, err
			}
			continue
		}
		if completed[step.name] {
			continue
		}
		run := step.run
		if step.withAddresses != nil {
			emails, err := w.passAddresses(ctx, request, pass, w.services.Addresses)
			if err != nil {
				return true, w.retry(ctx, request, now, "erasure_addresses_failed", err, false)
			}
			run = func(ctx context.Context, id uuid.UUID) error { return step.withAddresses(ctx, id, emails) }
		}
		stepCtx, cancel := context.WithTimeout(ctx, w.config.StepTimeout)
		err := run(stepCtx, request.SubjectID)
		cancel()
		if err != nil {
			return true, w.retry(ctx, request, now, string(step.name)+"_failed", err, false)
		}
		// A checkpoint carries the time its step finished, not the start of
		// the pass: the completion proof lists when each step was done.
		if err := w.store.CompleteDeletionStep(ctx, request.ID, leaseToken, step.name, w.config.Now()); err != nil {
			return true, w.retry(ctx, request, now, string(step.name)+"_checkpoint_failed", err, false)
		}
	}
	if err := w.store.CompleteDeletionRequest(ctx, request.ID, leaseToken, w.config.Now()); err != nil {
		return true, w.retry(ctx, request, now, "complete_request_failed", err, false)
	}
	return true, nil
}

// coreSaga is the erasure order (ADR-0051, spec §4): block the identity, have
// every service erase the person, erase core and its guest data, then media,
// and delete the identity last.
func (w *Worker) coreSaga(request user.DeletionRequest, now time.Time) []sagaStep {
	return []sagaStep{
		{name: user.DeletionStepDisableIdentity, run: w.identity.EnsureDisabled},
		{name: user.DeletionStepLogoutSessions, run: w.identity.EnsureLoggedOut},
		{services: &w.services},
		{name: user.DeletionStepAnonymizeCore, withAddresses: func(ctx context.Context, id uuid.UUID, emails []string) error {
			return w.store.AnonymizeAccount(ctx, id, now, emails)
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

// passAddresses reads the person's addresses the first time a pass needs
// them and hands the same ones to every later step of that pass.
func (w *Worker) passAddresses(ctx context.Context, request user.DeletionRequest, pass *erasurePass, source ErasureAddressSource) ([]string, error) {
	if pass.read {
		return pass.emails, nil
	}
	if source == nil {
		return nil, errors.New("erasure address source unavailable")
	}
	addressCtx, cancel := context.WithTimeout(ctx, w.config.StepTimeout)
	emails, err := source.ErasureAddresses(addressCtx, request.SubjectID)
	cancel()
	if err != nil {
		return nil, err
	}
	pass.emails, pass.read = emails, true
	return emails, nil
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
	// updated_at is the moment of this change, read now rather than at the
	// start of the pass; the next attempt's time goes only to next_attempt_at.
	if err := w.store.RetryDeletionRequest(ctx, request.ID, *request.LeaseToken, w.config.Now(), next, code, manual, refundAttempt); err != nil {
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
