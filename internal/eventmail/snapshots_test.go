package eventmail

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

type blockingDeleteLists struct {
	*memoryLists
}

func (l *blockingDeleteLists) DeleteList(ctx context.Context, _ uuid.UUID) error {
	<-ctx.Done()
	return ctx.Err()
}

func TestPruneSnapshotsDeletesOnlyExpiredLists(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	now := time.Date(2026, time.September, 19, 12, 0, 0, 0, time.UTC)
	store := NewMemorySnapshotStore()
	lists := newMemoryLists()
	eventID := uuid.New()
	expiredID, err := lists.CreateList(ctx, "expired")
	if err != nil {
		t.Fatal(err)
	}
	freshID, err := lists.CreateList(ctx, "fresh")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Track(ctx, Snapshot{MailListID: expiredID, EventID: eventID, ExpiresAt: now.Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if err := store.Track(ctx, Snapshot{MailListID: freshID, EventID: eventID, ExpiresAt: now.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}

	if err := PruneSnapshots(ctx, store, lists, now, 100); err != nil {
		t.Fatal(err)
	}
	if err := lists.GetList(ctx, expiredID); err == nil {
		t.Fatal("expired list still exists")
	}
	if err := lists.GetList(ctx, freshID); err != nil {
		t.Fatalf("fresh list: %v", err)
	}
	remaining, err := store.Expired(ctx, now.Add(2*time.Minute), 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 1 || remaining[0].MailListID != freshID {
		t.Fatalf("remaining %+v", remaining)
	}
}

func TestMaintainSnapshotRetentionDoesNotBlockStartup(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	store := NewMemorySnapshotStore()
	lists := &blockingDeleteLists{memoryLists: newMemoryLists()}
	listID, err := lists.CreateList(ctx, "expired")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Track(ctx, Snapshot{
		MailListID: listID,
		EventID:    uuid.New(),
		ExpiresAt:  time.Now().UTC().Add(-time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		MaintainSnapshotRetention(ctx, store, lists, time.Hour, nil)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
		cancel()
		<-done
		t.Fatal("retention startup blocked on external delete")
	}
}
