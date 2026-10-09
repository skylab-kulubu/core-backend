package account_test

import (
	"context"
	"testing"
	"time"

	"github.com/skylab-kulubu/core-backend/internal/account"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// claimingStore holds the pass that claims until the test lets it go.
type claimingStore struct {
	account.Store
	claiming chan struct{}
	release  chan struct{}
}

func (s claimingStore) ClaimDeletionRequest(ctx context.Context, _ time.Time, _ time.Duration) (user.DeletionRequest, bool, error) {
	select {
	case s.claiming <- struct{}{}:
	default:
	}
	<-s.release
	return user.DeletionRequest{}, false, ctx.Err()
}

// Shutdown waits on Maintain's channel: it closes once ctx is cancelled and
// the pass in flight has returned, not before.
func TestMaintainDoneClosesAfterThePassInFlight(t *testing.T) {
	t.Parallel()
	store := claimingStore{claiming: make(chan struct{}, 1), release: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	done := account.Maintain(ctx, account.NewWorker(store, nil, account.WorkerConfig{}), time.Hour, nil)
	<-store.claiming
	cancel()
	select {
	case <-done:
		t.Fatal("done closed while a pass was in flight")
	case <-time.After(50 * time.Millisecond):
	}
	close(store.release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("done did not close after cancel")
	}
}
