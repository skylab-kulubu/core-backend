package account_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/accessgate"
	"github.com/skylab-kulubu/core-backend/internal/account"
	"github.com/skylab-kulubu/core-backend/internal/testredis"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

type projectionWriter struct {
	err                     error
	asserted                []string
	markerAssertions        int
	contractAfterAssertions int
}

func (w *projectionWriter) EnsureBlocked(_ context.Context, subject string) error {
	if w.err != nil {
		return w.err
	}
	w.asserted = append(w.asserted, subject)
	return nil
}

func (w *projectionWriter) AssertMarker(context.Context, string) error {
	w.markerAssertions++
	return nil
}
func (w *projectionWriter) EnsureContract(context.Context) error {
	w.contractAfterAssertions = w.markerAssertions
	return nil
}

func TestProjectorRetriesDurableRequestUntilMarkerIsConfirmed(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := user.NewMemoryStore()
	subjectID := uuid.New()
	if _, _, err := user.NewService(store).Ensure(ctx, subjectID, user.Profile{Email: "projector@example.test"}); err != nil {
		t.Fatal(err)
	}
	request, err := store.RequestDeletion(ctx, subjectID, nil)
	if err != nil {
		t.Fatal(err)
	}
	writer := &projectionWriter{err: errors.New("redis unavailable")}
	projector := account.NewAccessProjector(store, writer, func() time.Time { return time.Now().UTC() })

	if err := projector.Project(ctx, request); err == nil {
		t.Fatal("projection unexpectedly succeeded")
	}
	got, err := store.DeletionRequest(ctx, subjectID)
	if err != nil || got.PlatformBlockedAt != nil {
		t.Fatalf("failed projection advanced request: %+v err=%v", got, err)
	}
	if _, ok, err := store.ClaimDeletionRequest(ctx, time.Now().Add(time.Hour), time.Minute); err != nil || ok {
		t.Fatalf("failed projection became claimable: ok=%v err=%v", ok, err)
	}

	writer.err = nil
	if err := projector.Project(ctx, got); err != nil {
		t.Fatal(err)
	}
	if err := projector.Project(ctx, got); err != nil {
		t.Fatal(err)
	}
	got, err = store.DeletionRequest(ctx, subjectID)
	if err != nil || got.PlatformBlockedAt == nil || len(writer.asserted) != 2 {
		t.Fatalf("retry projection request=%+v writes=%v err=%v", got, writer.asserted, err)
	}
}

func TestReconcilerRestoresAllPermanentMarkersBeforeContract(t *testing.T) {
	client := testredis.Start(t)
	ctx := context.Background()
	store := user.NewMemoryStore()
	subjects := []uuid.UUID{uuid.New(), uuid.New()}
	for index, subjectID := range subjects {
		if _, _, err := user.NewService(store).Ensure(ctx, subjectID, user.Profile{Email: uuid.NewString() + "@example.test"}); err != nil {
			t.Fatal(err)
		}
		request, err := store.RequestDeletion(ctx, subjectID, nil)
		if err != nil {
			t.Fatal(err)
		}
		if index == 1 {
			if err := store.MarkDeletionPlatformBlocked(ctx, request.ID, time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
		}
	}
	metrics := accessgate.NewMetrics()
	gate := accessgate.NewRedisGate(client, time.Second, 0, 0, metrics)
	if gate.Check(ctx, subjects[0].String()) != accessgate.Unavailable {
		t.Fatal("missing contract did not fail closed")
	}

	reconciler := account.NewAccessReconciler(store, gate, func() time.Time { return time.Now().UTC() })
	if err := reconciler.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	for _, subjectID := range subjects {
		if got := gate.Check(ctx, subjectID.String()); got != accessgate.Blocked {
			t.Fatalf("subject decision after empty-Redis recovery = %q", got)
		}
		ttl, err := client.PTTL(ctx, accessgate.MarkerKey(subjectID.String())).Result()
		if err != nil || ttl != -1 {
			t.Fatalf("marker ttl=%s err=%v", ttl, err)
		}
	}
	unprojected, err := store.UnprojectedDeletionRequests(ctx, 10)
	if err != nil || len(unprojected) != 0 {
		t.Fatalf("unprojected=%+v err=%v", unprojected, err)
	}
	if got := metrics.Prometheus(); !strings.Contains(got, "skylab_account_access_reconciliation_drift_total 2\n") {
		t.Fatalf("recovery did not surface both missing markers:\n%s", got)
	}
}

func TestReconcilerTraversesEveryPageBeforeInstallingContract(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := user.NewMemoryStore()
	const requestCount = 251
	for index := 0; index < requestCount; index++ {
		subjectID := uuid.New()
		if _, _, err := user.NewService(store).Ensure(ctx, subjectID, user.Profile{Email: uuid.NewString() + "@example.test"}); err != nil {
			t.Fatal(err)
		}
		if _, err := store.RequestDeletion(ctx, subjectID, nil); err != nil {
			t.Fatal(err)
		}
	}
	writer := &projectionWriter{}
	reconciler := account.NewAccessReconciler(store, writer, func() time.Time { return time.Now().UTC() })
	if err := reconciler.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if writer.markerAssertions != requestCount || writer.contractAfterAssertions != requestCount {
		t.Fatalf("assertions=%d contract-after=%d", writer.markerAssertions, writer.contractAfterAssertions)
	}
	unprojected, err := store.UnprojectedDeletionRequests(ctx, requestCount)
	if err != nil || len(unprojected) != 0 {
		t.Fatalf("unprojected=%d err=%v", len(unprojected), err)
	}
}
