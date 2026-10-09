package certificate_test

import (
	"context"
	"testing"
	"time"

	"github.com/skylab-kulubu/core-backend/internal/certificate"
)

type processingService struct {
	certificate.Service
	processing chan struct{}
	release    chan struct{}
}

func (s processingService) ProcessNext(ctx context.Context, _ int) (int, error) {
	select {
	case s.processing <- struct{}{}:
	default:
	}
	<-s.release
	return 0, ctx.Err()
}

// Shutdown waits on MaintainIssuance's channel: it closes once ctx is
// cancelled and the batch in flight has returned, not before.
func TestMaintainIssuanceDoneClosesAfterTheBatchInFlight(t *testing.T) {
	t.Parallel()
	svc := processingService{processing: make(chan struct{}, 1), release: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	done := certificate.MaintainIssuance(ctx, svc, time.Millisecond, 10, nil)
	<-svc.processing
	cancel()
	select {
	case <-done:
		t.Fatal("done closed while a batch was in flight")
	case <-time.After(50 * time.Millisecond):
	}
	close(svc.release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("done did not close after cancel")
	}
}
