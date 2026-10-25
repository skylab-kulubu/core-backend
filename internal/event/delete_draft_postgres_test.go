package event_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
)

// ArchiveDraft archives a draft, refuses a published or cancelled Event in
// the same statement (ErrNotDraft), leaves an archived one as it is, and
// tells a missing Event apart.
func TestPostgresStoreArchiveDraft(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := event.NewPostgresStore(pool)
	create := func(status event.Status) event.Event {
		created, err := store.Create(ctx, event.Event{Name: string(status), Location: "YTÜ", OwnerTeam: "WEBLAB", Status: event.StatusDraft})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `UPDATE events SET status = $2 WHERE id = $1`, created.ID, string(status)); err != nil {
			t.Fatal(err)
		}
		return created
	}
	draft, published, cancelled := create(event.StatusDraft), create(event.StatusPublished), create(event.StatusCancelled)
	if err := store.ArchiveDraft(ctx, draft.ID, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.ArchiveDraft(ctx, draft.ID, nil); err != nil {
		t.Fatalf("again: %v", err)
	}
	for _, ev := range []event.Event{published, cancelled} {
		if err := store.ArchiveDraft(ctx, ev.ID, nil); !errors.Is(err, event.ErrNotDraft) {
			t.Fatalf("%s: %v", ev.Name, err)
		}
		if got, err := store.GetIncludingArchived(ctx, ev.ID); err != nil || got.ArchivedAt != nil {
			t.Fatalf("%s archived: %+v %v", ev.Name, got.ArchivedAt, err)
		}
	}
	if err := store.Archive(ctx, published.ID, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.ArchiveDraft(ctx, published.ID, nil); err != nil {
		t.Fatalf("archived published: %v", err)
	}
	if err := store.ArchiveDraft(ctx, uuid.New(), nil); !errors.Is(err, event.ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
}
