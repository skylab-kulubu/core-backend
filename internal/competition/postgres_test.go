package competition_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

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

// Account erasure detaches an organizer from every competition record it
// acted on, as from every other audit actor (docs/account-lifecycle.md),
// through the real path: deletion request, then anonymization. The scores
// stay.
func TestPostgresAnonymizationDetachesCompetitionActors(t *testing.T) {
	f, pool := postgresFixture(t)
	ctx := t.Context()
	users := user.NewPostgresStore(pool)
	organizerID := uuid.New()
	if _, _, err := users.Upsert(ctx, user.User{ID: organizerID, Email: "organizer@example.test", FirstName: "Org"}); err != nil {
		t.Fatal(err)
	}
	organizer := agcLeader
	organizer.ID = organizerID.String()

	c, err := f.svc.Create(ctx, organizer, f.event.ID, competition.CreateInput{Name: "AGC Hafta 4"})
	if err != nil {
		t.Fatal(err)
	}
	archived, err := f.svc.Create(ctx, organizer, f.event.ID, competition.CreateInput{Name: "Yanlış açıldı"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.svc.Archive(ctx, organizer, archived.ID); err != nil {
		t.Fatal(err)
	}
	added, err := f.svc.AddEntrants(ctx, organizer, c.ID, []uuid.UUID{f.guest(t, "Ada", "Lovelace").ID})
	if err != nil {
		t.Fatal(err)
	}
	entrant := added[0].ID
	first, err := f.svc.SetScore(ctx, organizer, c.ID, competition.ScoreInput{EntrantID: entrant, Points: pts(30)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.SetScore(ctx, organizer, c.ID, competition.ScoreInput{EntrantID: entrant, ReplacesID: &first.ID, Points: pts(60), Reason: "Bonus"}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO competition_adjustments (id, competition_id, stage_id, entrant_id, kind, value, reason, recorded_by, revoked_at, revoked_by, revoke_reason)
		VALUES ($1, $2, $3, $4, 'bonus', 1000, 'Bonus', $5, now(), $5, 'Yanlış')`, uuid.New(), c.ID, c.Stages[0].ID, entrant, organizerID); err != nil {
		t.Fatal(err)
	}
	const references = `SELECT
		(SELECT count(*) FROM competitions WHERE created_by = $1 OR archived_by = $1)
		+ (SELECT count(*) FROM competition_entrants WHERE created_by = $1)
		+ (SELECT count(*) FROM competition_submissions WHERE recorded_by = $1 OR revoked_by = $1)
		+ (SELECT count(*) FROM competition_adjustments WHERE recorded_by = $1 OR revoked_by = $1)`
	var before int
	if err := pool.QueryRow(ctx, references, organizerID).Scan(&before); err != nil || before != 6 {
		t.Fatalf("rows naming the organizer before = %d, %v", before, err)
	}

	if _, err := users.RequestDeletion(ctx, organizerID, nil); err != nil {
		t.Fatal(err)
	}
	if err := users.AnonymizeAccount(ctx, organizerID, time.Now().UTC(), nil); err != nil {
		t.Fatalf("anonymize: %v", err)
	}
	var after int
	if err := pool.QueryRow(ctx, references, organizerID).Scan(&after); err != nil || after != 0 {
		t.Fatalf("rows naming the organizer after anonymization = %d, %v", after, err)
	}
	entries, err := f.svc.Entries(ctx, ykMember, c.ID)
	if err != nil || len(entries) != 2 {
		t.Fatalf("entries %+v %v", entries, err)
	}
	for _, e := range entries {
		if e.RecordedBy != nil || e.RevokedBy != nil {
			t.Errorf("entry still names an actor: %+v", e)
		}
	}
	board, err := f.svc.Leaderboard(ctx, ykMember, c.ID)
	if err != nil || len(board.Rows) != 1 || board.Rows[0].Score == nil || *board.Rows[0].Score != pts(60) {
		t.Fatalf("board %+v %v", board, err)
	}
}

// Two organizers writing the same cell: exactly one write lands, the others
// learn the score changed under them; none asks for a reason and overwrites
// a score its writer never saw.
func TestPostgresConcurrentScoreWritesOneWins(t *testing.T) {
	f, pool := postgresFixture(t)
	ctx := t.Context()
	c := f.create(t)
	added, err := f.svc.AddEntrants(ctx, agcLeader, c.ID, []uuid.UUID{f.guest(t, "Ada", "Lovelace").ID})
	if err != nil {
		t.Fatal(err)
	}
	entrant := added[0].ID
	race := func(in competition.ScoreInput) (ok, changed int, written competition.Entry) {
		t.Helper()
		const writers = 12
		var wg sync.WaitGroup
		var mu sync.Mutex
		for i := range writers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				w := in
				w.Points = pts(float64(30 + i))
				e, err := f.svc.SetScore(ctx, agcLeader, c.ID, w)
				mu.Lock()
				defer mu.Unlock()
				switch {
				case err == nil:
					ok++
					written = e
				case errors.Is(err, competition.ErrScoreChanged):
					changed++
				default:
					t.Errorf("writer %d: %v", i, err)
				}
			}()
		}
		wg.Wait()
		return ok, changed, written
	}
	live := func() int {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM competition_submissions WHERE entrant_id = $1 AND revoked_at IS NULL`, entrant).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	ok, changed, first := race(competition.ScoreInput{EntrantID: entrant})
	if ok != 1 || changed != 11 || live() != 1 {
		t.Fatalf("first writes: ok=%d changed=%d live=%d", ok, changed, live())
	}
	ok, changed, _ = race(competition.ScoreInput{EntrantID: entrant, ReplacesID: &first.ID, Reason: "Düzeltme"})
	if ok != 1 || changed != 11 || live() != 1 {
		t.Fatalf("corrections: ok=%d changed=%d live=%d", ok, changed, live())
	}
}
