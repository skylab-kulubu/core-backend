package account

import (
	"time"

	"github.com/skylab-kulubu/core-backend/internal/user"
)

// NewServiceErasureWorker returns a worker whose saga is the service erasure
// group alone, with the claim, marker, lease and retry rules of every worker.
// The production saga does not hold the group yet; it goes after
// logout_sessions and before anonymize_core (ADR-0051). Until then only these
// tests run it.
func NewServiceErasureWorker(store Store, services ServiceErasure, config WorkerConfig) *Worker {
	worker := NewWorker(store, nil, config)
	worker.saga = func(user.DeletionRequest, time.Time) []sagaStep {
		return []sagaStep{{services: &services}}
	}
	return worker
}
