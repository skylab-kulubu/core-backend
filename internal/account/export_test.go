package account

import (
	"time"

	"github.com/skylab-kulubu/core-backend/internal/user"
)

// NewServiceErasureWorker returns a worker whose saga is the service erasure
// group alone, with the claim, marker, lease and retry rules of every worker,
// so the group's own behaviour is tested apart from the core steps around it.
func NewServiceErasureWorker(store Store, services ServiceErasure, config WorkerConfig) *Worker {
	worker := NewWorker(store, nil, config)
	worker.saga = func(user.DeletionRequest, time.Time) []sagaStep {
		return []sagaStep{{services: &services}}
	}
	return worker
}

// NewWorkerWithConfiguredWaits is NewWorker without the registry's floor on
// each service step's wait: saga tests that are not about the CMS token window
// run in one pass.
func NewWorkerWithConfiguredWaits(store Store, identity Identity, config WorkerConfig, media ...MediaEraser) *Worker {
	worker := NewWorker(store, identity, config, media...)
	worker.services = registryServicesWith(config.Services, false)
	return worker
}

// IdentityClosedAt exposes identityClosedAt.
func IdentityClosedAt(request user.DeletionRequest, checkpoints map[user.DeletionStep]time.Time, now time.Time) time.Time {
	return identityClosedAt(request, checkpoints, now)
}

// ClosedAtTolerance exposes closedAtTolerance.
const ClosedAtTolerance = closedAtTolerance
