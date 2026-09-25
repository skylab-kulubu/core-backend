package account

import (
	"time"

	"github.com/skylab-kulubu/core-backend/internal/user"
)

// NewServiceErasureWorker returns a worker whose saga is the service erasure
// group alone, with the claim, marker, lease and retry rules of every worker.
// Ticket 07 places the group in the production saga (after logout_sessions,
// before anonymize_core); until then only these tests run it.
func NewServiceErasureWorker(store Store, services ServiceErasure, config WorkerConfig) *Worker {
	worker := NewWorker(store, nil, config)
	worker.saga = func(user.DeletionRequest, time.Time) []sagaStep {
		return []sagaStep{{services: &services}}
	}
	return worker
}
