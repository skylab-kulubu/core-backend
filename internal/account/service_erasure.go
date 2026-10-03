package account

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/erasure"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// ErasureSender sends one service its Erasure command.
type ErasureSender interface {
	Erase(context.Context, erasure.Command) (erasure.Result, error)
}

// ErasureAddressSource reads the person's addresses fresh for one pass. They
// are held in memory for that pass only. An error it returns must not carry an
// address.
type ErasureAddressSource interface {
	ErasureAddresses(context.Context, uuid.UUID) ([]string, error)
}

// ServiceStep is one service erasure saga step.
type ServiceStep struct {
	Step   user.DeletionStep
	Sender ErasureSender
	// WaitAfterIdentityClosed holds the command back until this long after
	// the identity was closed (erasure.Service). NewServiceErasure takes it
	// from the registry.
	WaitAfterIdentityClosed time.Duration
}

// ServiceErasure is the service erasure step group. Every pass attempts each
// step that is not checkpointed yet, even when another fails; the request
// advances only when all of them are checkpointed.
type ServiceErasure struct {
	Steps     []ServiceStep
	Addresses ErasureAddressSource
}

// NewServiceErasure builds the group from the registry configuration: one
// client per service, each with its own token cache for its own erase scope.
func NewServiceErasure(config erasure.Config, tokenURL string, addresses ErasureAddressSource) ServiceErasure {
	services := ServiceErasure{Addresses: addresses}
	for _, endpoint := range config.Endpoints {
		services.Steps = append(services.Steps, ServiceStep{
			Step:                    endpoint.Service.Step,
			Sender:                  erasure.NewClient(endpoint, tokenURL, config.ClientID, config.ClientSecret),
			WaitAfterIdentityClosed: endpoint.Service.WaitAfterIdentityClosed,
		})
	}
	return services
}

// permanentFailure is a rejection no retry fixes; the request goes to manual
// intervention under its code at once.
type permanentFailure interface {
	PermanentCode() string
}

type deferredFailure interface {
	RetryAt() time.Time
}

type serviceFailure struct {
	step user.DeletionStep
	err  error
}

// registryServices puts the configured steps in registry order and gives an
// entry nobody configured a sender that refuses. Startup already refuses a
// worker without every service URL (erasure.ConfigFromEnv); this keeps a
// worker built any other way from treating a service as erased.
func registryServices(configured ServiceErasure) ServiceErasure {
	services := ServiceErasure{Addresses: configured.Addresses}
	for _, entry := range erasure.Registry() {
		step := ServiceStep{Step: entry.Step, Sender: unconfiguredService{step: entry.Step}}
		for _, candidate := range configured.Steps {
			if candidate.Step == entry.Step && candidate.Sender != nil {
				step.Sender = candidate.Sender
				step.WaitAfterIdentityClosed = candidate.WaitAfterIdentityClosed
			}
		}
		services.Steps = append(services.Steps, step)
	}
	return services
}

// unconfiguredService stands for a registry entry the worker has no sender
// for. It never calls anything.
type unconfiguredService struct {
	step user.DeletionStep
}

func (s unconfiguredService) Erase(context.Context, erasure.Command) (erasure.Result, error) {
	return erasure.Result{}, notConfiguredError(s)
}

// notConfiguredError sends the request to manual intervention under
// `erase_<service>_not_configured`.
type notConfiguredError struct {
	step user.DeletionStep
}

func (e notConfiguredError) Error() string         { return string(e.step) + ": service not configured" }
func (e notConfiguredError) PermanentCode() string { return string(e.step) + "_not_configured" }

// notYetDue holds back the command to a service without the access gate
// until a token issued before the identity was closed has expired there. It
// is deferred: the attempt is refunded and the request comes back at At.
type notYetDue struct {
	step user.DeletionStep
	at   time.Time
}

func (e notYetDue) Error() string {
	return string(e.step) + ": waits for access tokens issued before the identity was closed to expire"
}
func (e notYetDue) RetryAt() time.Time { return e.at }

// identityClosedAt is when the person's last token could have been issued:
// the latest of the platform block and the disable_identity and
// logout_sessions checkpoints. Keycloak does not read the account access
// marker, so a session could still mint an access token between the block
// and the disable.
func identityClosedAt(request user.DeletionRequest, checkpoints map[user.DeletionStep]time.Time) time.Time {
	var closed time.Time
	if request.PlatformBlockedAt != nil {
		closed = *request.PlatformBlockedAt
	}
	for _, step := range []user.DeletionStep{user.DeletionStepDisableIdentity, user.DeletionStepLogoutSessions} {
		if at := checkpoints[step]; at.After(closed) {
			closed = at
		}
	}
	return closed
}

// runServiceErasure runs one pass of the group. It returns nil when every
// service step is checkpointed; otherwise it has already scheduled the retry
// and returns that error.
func (w *Worker) runServiceErasure(ctx context.Context, request user.DeletionRequest, leaseToken uuid.UUID, completed map[user.DeletionStep]bool, now time.Time, services *ServiceErasure, pass *erasurePass) error {
	pending := make([]ServiceStep, 0, len(services.Steps))
	for _, step := range services.Steps {
		if !completed[step.Step] {
			pending = append(pending, step)
		}
	}
	if len(pending) == 0 {
		return nil
	}
	// A service without a sender is refused before the addresses are read or
	// any other service is called.
	for _, step := range pending {
		if unconfigured, ok := step.Sender.(unconfiguredService); ok {
			err := notConfiguredError(unconfigured)
			return w.retry(ctx, request, now, err.PermanentCode(), err, true)
		}
	}
	// A service without the access gate is called only once a token issued
	// before the identity was closed has expired, or an edit made with it
	// would bring the person's sub back after the erasure (spec §3.2).
	due := make([]ServiceStep, 0, len(pending))
	var waiting []serviceFailure
	for _, step := range pending {
		if step.WaitAfterIdentityClosed > 0 {
			if at := identityClosedAt(request, pass.checkpoints).Add(step.WaitAfterIdentityClosed); now.Before(at) {
				waiting = append(waiting, serviceFailure{step: step.Step, err: notYetDue{step: step.Step, at: at}})
				continue
			}
		}
		due = append(due, step)
	}
	if len(due) == 0 {
		code, permanent, cause := classifyServiceFailures(waiting)
		return w.retry(ctx, request, now, code, cause, permanent)
	}
	emails, err := w.passAddresses(ctx, request, pass, services.Addresses)
	if err != nil {
		return w.retry(ctx, request, now, "erasure_addresses_failed", err, false)
	}

	var failures []serviceFailure
	for _, step := range due {
		stepCtx, cancel := context.WithTimeout(ctx, w.config.StepTimeout)
		result, err := step.Sender.Erase(stepCtx, erasure.Command{RequestID: request.ID, SubjectID: request.SubjectID, Emails: emails})
		cancel()
		if err != nil {
			failures = append(failures, serviceFailure{step: step.Step, err: err})
			continue
		}
		if err := w.store.CompleteServiceErasureStep(ctx, request.ID, leaseToken, step.Step, w.config.Now(), result.Counts); err != nil {
			// A lost lease means another worker owns the request now: stop
			// before calling anything else under a stale claim.
			return w.retry(ctx, request, now, string(step.Step)+"_checkpoint_failed", err, false)
		}
		completed[step.Step] = true
	}
	// A wait comes last, so the code names a service that failed when there
	// is one.
	failures = append(failures, waiting...)
	if len(failures) == 0 {
		return nil
	}
	code, permanent, cause := classifyServiceFailures(failures)
	return w.retry(ctx, request, now, code, cause, permanent)
}

// classifyServiceFailures turns one pass's failures into one retry decision:
// any rejection sends the request to manual intervention; otherwise any
// ordinary failure spends an attempt; only when every failure is deferred is
// the attempt refunded, until the earliest time a service asked for. When the
// first failure is a step that only waits, every failure is one, and the code
// is `erase_<service>_waiting`.
func classifyServiceFailures(failures []serviceFailure) (code string, permanent bool, cause error) {
	messages := make([]string, 0, len(failures))
	for _, failure := range failures {
		messages = append(messages, failure.err.Error())
	}
	message := strings.Join(messages, "; ")
	for _, failure := range failures {
		var rejected permanentFailure
		if errors.As(failure.err, &rejected) {
			return rejected.PermanentCode(), true, errors.New(message)
		}
	}
	var earliest time.Time
	for _, failure := range failures {
		var deferred deferredFailure
		if !errors.As(failure.err, &deferred) {
			// Flattened on purpose: the joined cause must not expose a
			// deferred member, or the retry would refund this attempt.
			return string(failure.step) + "_failed", false, errors.New(message)
		}
		if at := deferred.RetryAt(); earliest.IsZero() || at.Before(earliest) {
			earliest = at
		}
	}
	code = string(failures[0].step) + "_failed"
	if _, waits := failures[0].err.(notYetDue); waits {
		code = string(failures[0].step) + "_waiting"
	}
	return code, false, deferredServiceErasure{message: message, at: earliest}
}

type deferredServiceErasure struct {
	message string
	at      time.Time
}

func (e deferredServiceErasure) Error() string      { return e.message }
func (e deferredServiceErasure) RetryAt() time.Time { return e.at }
