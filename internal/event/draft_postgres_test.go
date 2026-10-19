package event_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/season"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// The lists leave drafts out in SQL (ADR 0066) unless asked for them; a
// Season's list never carries one.
func TestPostgresStoreListsLeaveDraftsOut(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := event.NewPostgresStore(pool)
	s, err := season.NewPostgresStore(pool).Create(ctx, season.Season{Name: "2026"})
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range []event.Event{
		{Name: "Yayında", Status: event.StatusPublished},
		{Name: "Taslak", Status: event.StatusDraft},
	} {
		in.Location, in.OwnerTeam, in.SeasonID = "YTÜ", "WEBLAB", &s.ID
		if _, err := store.Create(ctx, in); err != nil {
			t.Fatal(err)
		}
	}
	names := func(events []event.Event, err error) []string {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		out := []string{}
		for _, e := range events {
			out = append(out, e.Name)
		}
		slices.Sort(out)
		return out
	}
	if got := names(store.List(ctx, "", false)); !slices.Equal(got, []string{"Yayında"}) {
		t.Fatalf("list %v", got)
	}
	if got := names(store.List(ctx, "WEBLAB", true)); !slices.Equal(got, []string{"Yayında"}) {
		t.Fatalf("active list %v", got)
	}
	if got := names(store.ListIncludingDrafts(ctx, "WEBLAB")); !slices.Equal(got, []string{"Taslak", "Yayında"}) {
		t.Fatalf("with drafts %v", got)
	}
	if got := names(store.ListBySeason(ctx, s.ID)); !slices.Equal(got, []string{"Yayında"}) {
		t.Fatalf("season %v", got)
	}
}

// Publish and Unpublish are one conditional UPDATE each: publish only a
// current draft with a name, a location and a start date; unpublish only a
// published Event without a Ticket. Both write the legacy active column.
func TestPostgresStorePublishAndUnpublish(t *testing.T) {
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
	stored := func(id uuid.UUID) (active bool, status string, by *uuid.UUID) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT active, status, published_by FROM events WHERE id = $1`, id).Scan(&active, &status, &by); err != nil {
			t.Fatal(err)
		}
		return active, status, by
	}
	start := time.Now().Add(time.Hour)
	undated, err := store.Create(ctx, event.Event{Name: "Tarihsiz", Location: "YTÜ", OwnerTeam: "WEBLAB", Status: event.StatusDraft})
	if err != nil {
		t.Fatal(err)
	}
	if changed, err := store.Publish(ctx, undated.ID, &actor); err != nil || changed {
		t.Fatalf("publish undated: %v %v", changed, err)
	}
	draft, err := store.Create(ctx, event.Event{Name: "Taslak", Location: "YTÜ", OwnerTeam: "WEBLAB", Status: event.StatusDraft, StartDate: &start})
	if err != nil {
		t.Fatal(err)
	}
	// Two publishes at once: one changes the row.
	var wg sync.WaitGroup
	results := make([]bool, 2)
	for i := range results {
		wg.Go(func() {
			changed, err := store.Publish(ctx, draft.ID, &actor)
			if err != nil {
				t.Error(err)
			}
			results[i] = changed
		})
	}
	wg.Wait()
	if results[0] == results[1] {
		t.Fatalf("concurrent publishes changed %v", results)
	}
	if active, status, by := stored(draft.ID); !active || status != "published" || by == nil || *by != actor {
		t.Fatalf("published row active=%v status=%q by=%v", active, status, by)
	}
	got, err := store.Get(ctx, draft.ID)
	if err != nil || got.Status != event.StatusPublished || !got.Active || got.PublishedAt == nil {
		t.Fatalf("published %+v %v", got, err)
	}
	if changed, err := store.Unpublish(ctx, draft.ID); err != nil || !changed {
		t.Fatalf("unpublish: %v %v", changed, err)
	}
	if active, status, _ := stored(draft.ID); active || status != "draft" {
		t.Fatalf("unpublished row active=%v status=%q", active, status)
	}
	if changed, err := store.Unpublish(ctx, draft.ID); err != nil || changed {
		t.Fatalf("unpublish a draft: %v %v", changed, err)
	}
	if changed, err := store.Publish(ctx, draft.ID, nil); err != nil || !changed {
		t.Fatalf("publish again: %v %v", changed, err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO tickets (id, event_id, ticket_type, guest_email) VALUES ($1, $2, 'GUEST', 'ada@example.com')`, uuid.New(), draft.ID); err != nil {
		t.Fatal(err)
	}
	if changed, err := store.Unpublish(ctx, draft.ID); !errors.Is(err, event.ErrHasTickets) || changed {
		t.Fatalf("unpublish with a Ticket: %v %v", changed, err)
	}
	if _, status, _ := stored(draft.ID); status != "published" {
		t.Fatalf("status %q", status)
	}
	// An archived draft is not published.
	archived, err := store.Create(ctx, event.Event{Name: "Arşiv", Location: "YTÜ", OwnerTeam: "WEBLAB", Status: event.StatusDraft, StartDate: &start})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Archive(ctx, archived.ID, nil); err != nil {
		t.Fatal(err)
	}
	if changed, err := store.Publish(ctx, archived.ID, nil); err != nil || changed {
		t.Fatalf("publish archived: %v %v", changed, err)
	}
}

// A published Event with a current Competitor stays published: their reads
// would otherwise carry a draft's summary. A withdrawn one does not hold it.
func TestPostgresStoreUnpublishRefusesCompetitors(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := event.NewPostgresStore(pool)
	person := uuid.New()
	if _, _, err := user.NewService(user.NewPostgresStore(pool)).Ensure(ctx, person, user.Profile{Email: "competitor@example.test"}); err != nil {
		t.Fatal(err)
	}
	start := time.Now().Add(time.Hour)
	ev, err := store.Create(ctx, event.Event{Name: "Yarışma Günü", Location: "YTÜ", OwnerTeam: "WEBLAB", Status: event.StatusPublished, StartDate: &start})
	if err != nil {
		t.Fatal(err)
	}
	competitor := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO competitors (id, user_id, event_id) VALUES ($1, $2, $3)`, competitor, person, ev.ID); err != nil {
		t.Fatal(err)
	}
	if changed, err := store.Unpublish(ctx, ev.ID); !errors.Is(err, event.ErrHasCompetitors) || changed {
		t.Fatalf("unpublish with a Competitor: %v %v", changed, err)
	}
	if got, err := store.Get(ctx, ev.ID); err != nil || got.Status != event.StatusPublished {
		t.Fatalf("after refusal %q %v", got.Status, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE competitors SET withdrawn_at = now() WHERE id = $1`, competitor); err != nil {
		t.Fatal(err)
	}
	if changed, err := store.Unpublish(ctx, ev.ID); err != nil || !changed {
		t.Fatalf("unpublish after the withdrawal: %v %v", changed, err)
	}
}
