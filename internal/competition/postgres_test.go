package competition_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/internal/competition"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// postgresFixture is the same AGC Event in PostgreSQL, with the organizers
// the tests act as written as users (actor columns reference them).
func postgresFixture(t *testing.T) (fixture, *pgxpool.Pool) {
	t.Helper()
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	users := user.NewPostgresStore(pool)
	for _, id := range []string{agcLeader.ID, agcMember.ID, ykMember.ID} {
		if _, _, err := users.Upsert(ctx, user.User{ID: uuid.MustParse(id), Email: id + "@example.test", FirstName: "Org"}); err != nil {
			t.Fatal(err)
		}
	}
	f := newFixtureWith(t, event.NewPostgresStore(pool), ticket.NewPostgresStore(pool), users,
		func(ticket.Store, userStore) competition.Store { return competition.NewPostgresStore(pool) })
	return f, pool
}

// The service's whole scoring scenario, on PostgreSQL.
func TestPostgresManualScoresAndLeaderboard(t *testing.T) {
	f, _ := postgresFixture(t)
	runManualScores(t, f)
}

func TestPostgresEntrantsAreTicketsOfTheEventOnce(t *testing.T) {
	f, pool := postgresFixture(t)
	ctx := t.Context()
	c := f.create(t)
	ada := f.registered(t, "Ada", "Lovelace")
	grace := f.guest(t, "Grace", "Hopper")
	added, err := f.svc.AddEntrants(ctx, agcLeader, c.ID, []uuid.UUID{ada.ID, grace.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(added) != 2 || added[0].DisplayName != "Ada Lovelace" || added[1].DisplayName != "Grace Hopper" ||
		added[1].Members[0].TicketType != ticket.Guest {
		t.Fatalf("added %+v", added)
	}
	// All or none: a taken Ticket in the batch writes nothing.
	linus := f.guest(t, "Linus", "Torvalds")
	if _, err := f.svc.AddEntrants(ctx, agcLeader, c.ID, []uuid.UUID{linus.ID, grace.ID}); !errors.Is(err, competition.ErrTicketTaken) {
		t.Fatalf("taken: %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM competition_entrants WHERE competition_id = $1`, c.ID).Scan(&n); err != nil || n != 2 {
		t.Fatalf("entrants after the refused batch = %d, %v", n, err)
	}
	// The same Ticket may be an Entrant of another Competition of the Event.
	other := f.create(t)
	if _, err := f.svc.AddEntrants(ctx, agcLeader, other.ID, []uuid.UUID{grace.ID}); err != nil {
		t.Fatalf("other competition: %v", err)
	}
	// Two organizers adding the same Ticket at once: one wins.
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = f.svc.AddEntrants(ctx, agcLeader, c.ID, []uuid.UUID{linus.ID})
		}()
	}
	wg.Wait()
	if (errs[0] == nil) == (errs[1] == nil) || !(errors.Is(errs[0], competition.ErrTicketTaken) || errors.Is(errs[1], competition.ErrTicketTaken)) {
		t.Fatalf("concurrent adds: %v, %v", errs[0], errs[1])
	}
	// A deleted account's name is no longer shown.
	if _, err := f.users.RequestDeletion(ctx, *ada.OwnerID, nil); err != nil {
		t.Fatal(err)
	}
	list, err := f.svc.Entrants(ctx, agcLeader, c.ID)
	if err != nil || list[0].DisplayName != user.DeletedDisplayName || list[1].DisplayName != "Grace Hopper" {
		t.Fatalf("entrants %+v %v", list, err)
	}
}

func TestPostgresCompetitionLifecycle(t *testing.T) {
	f, _ := postgresFixture(t)
	ctx := t.Context()
	c := f.create(t)
	if _, err := f.svc.Open(ctx, agcLeader, c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Open(ctx, agcLeader, c.ID); !errors.Is(err, competition.ErrStatus) {
		t.Fatalf("open twice: %v", err)
	}
	got, err := f.svc.Cancel(ctx, agcLeader, c.ID, "Ertelendi")
	if err != nil || got.Status != competition.StatusCancelled || got.CancelReason != "Ertelendi" || got.OwnerTeam != "AGC" {
		t.Fatalf("cancel %+v %v", got, err)
	}
	if err := f.svc.Archive(ctx, agcLeader, c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Get(ctx, agcLeader, c.ID); !errors.Is(err, competition.ErrNotFound) {
		t.Fatalf("archived get: %v", err)
	}
	restored, err := f.svc.Restore(ctx, agcLeader, c.ID)
	if err != nil || restored.ArchivedAt != nil || len(restored.Stages) != 1 {
		t.Fatalf("restore %+v %v", restored, err)
	}
}
