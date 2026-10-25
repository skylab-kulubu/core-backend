package event_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// The database derives active as the Go code does (status.go), for the reads
// and for the active list's filter, on every row of the derivation table.
func TestPostgresStoreDerivesActive(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := event.NewPostgresStore(pool)
	now := time.Now()
	want := map[uuid.UUID]statusCase{}
	for _, tc := range statusCases(now) {
		created, err := store.Create(ctx, event.Event{Name: tc.name, Location: "YTÜ", OwnerTeam: "WEBLAB", StartDate: tc.start, EndDate: tc.end})
		if err != nil {
			t.Fatal(err)
		}
		// No status endpoint yet: the row gets its status as one will
		// write it.
		if _, err := pool.Exec(ctx, `UPDATE events SET status = $2 WHERE id = $1`, created.ID, string(tc.status)); err != nil {
			t.Fatal(err)
		}
		got, err := store.Get(ctx, created.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != tc.status || got.Active != tc.active {
			t.Errorf("%s: status=%q active=%v, want active=%v", tc.name, got.Status, got.Active, tc.active)
		}
		if res := got.Resource(); res.Status != tc.status || res.Active != tc.active {
			t.Errorf("%s: resource status=%q active=%v", tc.name, res.Status, res.Active)
		}
		want[created.ID] = tc
	}
	listed, err := store.List(ctx, "", true)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, tc := range want {
		if tc.active {
			count++
		}
	}
	if len(listed) != count {
		t.Fatalf("active list %d, want %d", len(listed), count)
	}
	for _, e := range listed {
		if !want[e.ID].active || !e.Active {
			t.Errorf("%s listed as active", e.Name)
		}
	}
	all, err := store.ListIncludingDrafts(ctx, "")
	if err != nil || len(all) != len(want) {
		t.Fatalf("all %d %v", len(all), err)
	}
}

// Create writes the status (published when none is given, with when and by
// whom); Update does not.
func TestPostgresStoreWritesStatusOnCreateOnly(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := event.NewPostgresStore(pool)
	actor := uuid.New()
	if _, _, err := user.NewService(user.NewPostgresStore(pool)).Ensure(ctx, actor, user.Profile{Email: "publisher@example.test"}); err != nil {
		t.Fatal(err)
	}
	stored := func(id uuid.UUID) (status string, publishedBy *uuid.UUID) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT status, published_by FROM events WHERE id = $1`, id).Scan(&status, &publishedBy); err != nil {
			t.Fatal(err)
		}
		return status, publishedBy
	}

	published, err := store.Create(ctx, event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB", PublishedBy: &actor})
	if err != nil {
		t.Fatal(err)
	}
	if published.Status != event.StatusPublished || !published.Active || published.PublishedAt == nil || published.PublishedBy == nil || *published.PublishedBy != actor {
		t.Fatalf("published %+v", published)
	}
	if status, _ := stored(published.ID); status != "published" {
		t.Fatalf("stored status=%q", status)
	}

	draft, err := store.Create(ctx, event.Event{Name: "Taslak", Location: "YTÜ", OwnerTeam: "WEBLAB", Status: event.StatusDraft, PublishedBy: &actor})
	if err != nil {
		t.Fatal(err)
	}
	if draft.Status != event.StatusDraft || draft.Active || draft.PublishedAt != nil {
		t.Fatalf("draft %+v", draft)
	}
	if status, by := stored(draft.ID); status != "draft" || by != nil {
		t.Fatalf("stored draft status=%q by=%v", status, by)
	}

	// An actor with no account row (a client without one) does not fail
	// the create: there is no one to name.
	nobody := uuid.New()
	unnamed, err := store.Create(ctx, event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB", PublishedBy: &nobody})
	if err != nil {
		t.Fatal(err)
	}
	if unnamed.PublishedBy != nil || unnamed.PublishedAt == nil {
		t.Fatalf("unnamed %+v", unnamed)
	}

	// An update, whatever it carries, leaves the status alone.
	published.Name = "Hack 2"
	published.Active = false
	published.Status = event.StatusCancelled
	updated, err := store.Update(ctx, published)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Name != "Hack 2" || updated.Status != event.StatusPublished || !updated.Active {
		t.Fatalf("updated %+v", updated)
	}
	if status, _ := stored(published.ID); status != "published" {
		t.Fatalf("stored after update status=%q", status)
	}
}

// published_by is a current-identity link like archived_by: a person whose
// account is being erased cannot be named, and the create fails whole.
func TestPostgresStoreRefusesAPublisherBeingErased(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	users := user.NewPostgresStore(pool)
	actor := uuid.New()
	if _, _, err := user.NewService(users).Ensure(ctx, actor, user.Profile{Email: "leaving@example.test"}); err != nil {
		t.Fatal(err)
	}
	if _, err := users.RequestDeletion(ctx, actor, nil); err != nil {
		t.Fatal(err)
	}
	id := uuid.New()
	if _, err := event.NewPostgresStore(pool).Create(ctx, event.Event{ID: id, Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB", PublishedBy: &actor}); !errors.Is(err, event.ErrForbidden) {
		t.Fatalf("create = %v, want forbidden", err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE id = $1`, id).Scan(&n); err != nil || n != 0 {
		t.Fatalf("refused create persisted %d %v", n, err)
	}
}
