package event_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
)

// events.active is gone (ADR 0066): every write of an Event works without
// it and every read answers the derived active.
func TestPostgresStoreWritesWithoutLegacyActive(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := event.NewPostgresStore(pool)

	published, err := store.Create(ctx, event.Event{Name: "Yayında", Location: "YTÜ", OwnerTeam: "WEBLAB"})
	if err != nil {
		t.Fatalf("create published: %v", err)
	}
	draft, err := store.Create(ctx, event.Event{Name: "Taslak", Location: "YTÜ", OwnerTeam: "WEBLAB", Status: event.StatusDraft, StartDate: stamp("2099-01-01T00:00:00Z")})
	if err != nil {
		t.Fatalf("create draft: %v", err)
	}
	if changed, err := store.Publish(ctx, draft.ID, nil); err != nil || !changed {
		t.Fatalf("publish: %v %v", changed, err)
	}
	if changed, err := store.Unpublish(ctx, draft.ID); err != nil || !changed {
		t.Fatalf("unpublish: %v %v", changed, err)
	}
	if changed, err := store.Cancel(ctx, published.ID, nil, "Ertelendi", false); err != nil || !changed {
		t.Fatalf("cancel: %v %v", changed, err)
	}
	if changed, err := store.Uncancel(ctx, published.ID, false); err != nil || !changed {
		t.Fatalf("uncancel: %v %v", changed, err)
	}
	copied, _, err := store.Copy(ctx, event.CopyPlan{Source: published.ID, Event: event.Event{ID: uuid.New(), Name: "Kopya", OwnerTeam: "WEBLAB", StartDate: stamp("2027-01-01T00:00:00Z")}})
	if err != nil {
		t.Fatalf("copy: %v", err)
	}

	for id, want := range map[uuid.UUID]bool{published.ID: true, draft.ID: false, copied.ID: false} {
		got, err := store.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if got.Active != want {
			t.Errorf("%s: active=%v, want %v", got.Name, got.Active, want)
		}
	}
}
