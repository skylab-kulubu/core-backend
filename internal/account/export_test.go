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
