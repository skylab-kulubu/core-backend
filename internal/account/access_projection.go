package account

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/accessgate"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

type AccessProjectionStore interface {
	MarkDeletionPlatformBlocked(context.Context, uuid.UUID, time.Time) error
	UnprojectedDeletionRequests(context.Context, int) ([]user.DeletionRequest, error)
	DeletionRequestsPage(context.Context, uuid.UUID, int) ([]user.DeletionRequest, error)
}

type markerReconciler interface {
	ReconcileMarker(context.Context, string) error
}

type AccessProjector struct {
	store  AccessProjectionStore
	writer accessgate.ProjectionWriter
	now    func() time.Time
}

func NewAccessProjector(store AccessProjectionStore, writer accessgate.ProjectionWriter, now func() time.Time) *AccessProjector {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &AccessProjector{store: store, writer: writer, now: now}
}

func (p *AccessProjector) Project(ctx context.Context, request user.DeletionRequest) error {
	if p == nil || p.store == nil || p.writer == nil {
		return errors.New("account access projector unavailable")
	}
	if err := p.writer.EnsureBlocked(ctx, request.SubjectID.String()); err != nil {
		return err
	}
	return p.store.MarkDeletionPlatformBlocked(ctx, request.ID, p.now())
}

func (p *AccessProjector) RunOnce(ctx context.Context, limit int) (int, error) {
	requests, err := p.store.UnprojectedDeletionRequests(ctx, limit)
	if err != nil {
		return 0, err
	}
	for index, request := range requests {
		if err := p.Project(ctx, request); err != nil {
			return index, err
		}
	}
	return len(requests), nil
}

type AccessReconciler struct {
	store  AccessProjectionStore
	writer accessgate.ProjectionWriter
	now    func() time.Time
}

func NewAccessReconciler(store AccessProjectionStore, writer accessgate.ProjectionWriter, now func() time.Time) *AccessReconciler {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &AccessReconciler{store: store, writer: writer, now: now}
}

// RunOnce re-asserts every durable marker, including completed requests, and
// installs the contract sentinel last. This is safe for an empty replacement
// Redis: readers remain fail-closed until every permanent marker is restored.
func (r *AccessReconciler) RunOnce(ctx context.Context) error {
	if r == nil || r.store == nil || r.writer == nil {
		return errors.New("account access reconciler unavailable")
	}
	const batchSize = 250
	after := uuid.Nil
	for {
		page, err := r.store.DeletionRequestsPage(ctx, after, batchSize)
		if err != nil {
			return err
		}
		for _, request := range page {
			var err error
			if reconciler, ok := r.writer.(markerReconciler); ok {
				err = reconciler.ReconcileMarker(ctx, request.SubjectID.String())
			} else {
				err = r.writer.AssertMarker(ctx, request.SubjectID.String())
			}
			if err != nil {
				return err
			}
		}
		if len(page) < batchSize {
			break
		}
		after = page[len(page)-1].ID
	}
	if err := r.writer.EnsureContract(ctx); err != nil {
		return err
	}
	confirmedAt := r.now()
	after = uuid.Nil
	for {
		page, err := r.store.DeletionRequestsPage(ctx, after, batchSize)
		if err != nil {
			return err
		}
		for _, request := range page {
			if request.PlatformBlockedAt != nil {
				continue
			}
			if err := r.store.MarkDeletionPlatformBlocked(ctx, request.ID, confirmedAt); err != nil {
				return err
			}
		}
		if len(page) < batchSize {
			break
		}
		after = page[len(page)-1].ID
	}
	return nil
}

func MaintainAccessProjection(ctx context.Context, projector *AccessProjector, reconciler *AccessReconciler, interval time.Duration, onError func(error)) {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			if _, err := projector.RunOnce(ctx, 100); err != nil && onError != nil {
				onError(err)
			}
			if err := reconciler.RunOnce(ctx); err != nil && onError != nil {
				onError(err)
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}
