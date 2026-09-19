package lifecycle_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/internal/competitor"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/lifecycle"
	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/season"
	"github.com/skylab-kulubu/core-backend/internal/shorturl"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

func lifecyclePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := testpostgres.Start(t)
	if err := migrate.Apply(context.Background(), pool); err != nil {
		t.Fatal(err)
	}
	return pool
}

func TestEventLifecycleQueryContract(t *testing.T) {
	pool := lifecyclePool(t)
	ctx := context.Background()

	actorID := uuid.New()
	if _, _, err := user.NewService(user.NewPostgresStore(pool)).Ensure(ctx, actorID, user.Profile{
		Email: "operator@example.com", FirstName: "Grace", LastName: "Hopper", Username: "grace",
	}); err != nil {
		t.Fatal(err)
	}
	store := event.NewPostgresStore(pool)
	created, err := store.Create(ctx, event.Event{Name: "Arşivlenecek", Location: "YTÜ", OwnerTeam: "WEBLAB"})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := pool.Exec(ctx, `UPDATE events SET archived_at = now(), archived_by = $2 WHERE id = $1`, created.ID, actorID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(ctx, created.ID); !errors.Is(err, event.ErrNotFound) {
		t.Fatalf("archived event Get error = %v, want ErrNotFound", err)
	}
	listed, err := store.List(ctx, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 0 {
		t.Fatalf("default list returned archived events: %+v", listed)
	}
	archived, err := store.ListLifecycle(ctx, "", lifecycle.InactiveOnly)
	if err != nil {
		t.Fatal(err)
	}
	if len(archived) != 1 || archived[0].ID != created.ID || archived[0].ArchivedAt == nil || archived[0].ArchivedBy == nil || *archived[0].ArchivedBy != actorID {
		t.Fatalf("archived list = %+v", archived)
	}
	includingArchived, err := store.GetIncludingArchived(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if includingArchived.ArchivedAt == nil {
		t.Fatalf("including archived = %+v", includingArchived)
	}
}

func TestEventDayLifecycleQueryContract(t *testing.T) {
	pool := lifecyclePool(t)
	ctx := context.Background()

	actorID := uuid.New()
	if _, _, err := user.NewService(user.NewPostgresStore(pool)).Ensure(ctx, actorID, user.Profile{
		Email: "day-operator@example.com", FirstName: "Katherine", LastName: "Johnson", Username: "katherine",
	}); err != nil {
		t.Fatal(err)
	}
	store := event.NewPostgresStore(pool)
	createdEvent, err := store.Create(ctx, event.Event{Name: "Program", Location: "YTÜ", OwnerTeam: "WEBLAB"})
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.CreateDay(ctx, event.Day{EventID: createdEvent.ID, Name: "Gün 1"})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := pool.Exec(ctx, `UPDATE event_days SET archived_at = now(), archived_by = $2 WHERE id = $1`, created.ID, actorID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetDay(ctx, created.ID); !errors.Is(err, event.ErrNotFound) {
		t.Fatalf("archived day Get error = %v, want ErrNotFound", err)
	}
	listed, err := store.ListDays(ctx, createdEvent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 0 {
		t.Fatalf("default day list returned archived rows: %+v", listed)
	}
	archived, err := store.ListDaysLifecycle(ctx, createdEvent.ID, lifecycle.InactiveOnly)
	if err != nil {
		t.Fatal(err)
	}
	if len(archived) != 1 || archived[0].ArchivedAt == nil || archived[0].ArchivedBy == nil || *archived[0].ArchivedBy != actorID {
		t.Fatalf("archived day list = %+v", archived)
	}
	if _, err := store.GetDayIncludingArchived(ctx, created.ID); err != nil {
		t.Fatal(err)
	}
}

func TestSessionLifecycleQueryContract(t *testing.T) {
	pool := lifecyclePool(t)
	ctx := context.Background()

	actorID := uuid.New()
	if _, _, err := user.NewService(user.NewPostgresStore(pool)).Ensure(ctx, actorID, user.Profile{
		Email: "session-operator@example.com", FirstName: "Margaret", LastName: "Hamilton", Username: "margaret",
	}); err != nil {
		t.Fatal(err)
	}
	store := event.NewPostgresStore(pool)
	createdEvent, err := store.Create(ctx, event.Event{Name: "Program", Location: "YTÜ", OwnerTeam: "WEBLAB"})
	if err != nil {
		t.Fatal(err)
	}
	day, err := store.CreateDay(ctx, event.Day{EventID: createdEvent.ID, Name: "Gün 1"})
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.CreateSession(ctx, event.Session{EventDayID: day.ID, Title: "Taslak oturum", SessionType: "PRESENTATION"})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := pool.Exec(ctx, `UPDATE sessions SET archived_at = now(), archived_by = $2 WHERE id = $1`, created.ID, actorID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetSession(ctx, created.ID); !errors.Is(err, event.ErrNotFound) {
		t.Fatalf("archived session Get error = %v, want ErrNotFound", err)
	}
	listed, err := store.ListSessions(ctx, day.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 0 {
		t.Fatalf("default session list returned archived rows: %+v", listed)
	}
	archived, err := store.ListSessionsLifecycle(ctx, day.ID, lifecycle.InactiveOnly)
	if err != nil {
		t.Fatal(err)
	}
	if len(archived) != 1 || archived[0].ArchivedAt == nil || archived[0].ArchivedBy == nil || *archived[0].ArchivedBy != actorID {
		t.Fatalf("archived session list = %+v", archived)
	}
	if _, err := store.GetSessionIncludingArchived(ctx, created.ID); err != nil {
		t.Fatal(err)
	}
}

func TestArchivedEventAncestorsHideCurrentProgramChildren(t *testing.T) {
	pool := lifecyclePool(t)
	ctx := context.Background()

	store := event.NewPostgresStore(pool)
	createdEvent, err := store.Create(ctx, event.Event{Name: "Arşivli program", Location: "YTÜ", OwnerTeam: "WEBLAB"})
	if err != nil {
		t.Fatal(err)
	}
	day, err := store.CreateDay(ctx, event.Day{EventID: createdEvent.ID, Name: "Gün 1"})
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.CreateSession(ctx, event.Session{EventDayID: day.ID, Title: "Oturum", SessionType: "PRESENTATION"})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := pool.Exec(ctx, `UPDATE events SET archived_at = now() WHERE id = $1`, createdEvent.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetDay(ctx, day.ID); !errors.Is(err, event.ErrNotFound) {
		t.Fatalf("day under archived event Get error = %v, want ErrNotFound", err)
	}
	if got, err := store.ListDays(ctx, createdEvent.ID); err != nil || len(got) != 0 {
		t.Fatalf("days under archived event = %+v, err = %v", got, err)
	}
	if _, err := store.GetSession(ctx, session.ID); !errors.Is(err, event.ErrNotFound) {
		t.Fatalf("session under archived event Get error = %v, want ErrNotFound", err)
	}
	if got, err := store.ListSessions(ctx, day.ID); err != nil || len(got) != 0 {
		t.Fatalf("sessions under archived event = %+v, err = %v", got, err)
	}
	if _, err := store.GetDayIncludingArchived(ctx, day.ID); err != nil {
		t.Fatalf("management day read: %v", err)
	}
	if _, err := store.GetSessionIncludingArchived(ctx, session.ID); err != nil {
		t.Fatalf("management session read: %v", err)
	}
	if got, err := store.ListDaysLifecycle(ctx, createdEvent.ID, lifecycle.CurrentOnly); err != nil || len(got) != 1 {
		t.Fatalf("management current day list = %+v, err = %v", got, err)
	}
	if got, err := store.ListSessionsLifecycle(ctx, day.ID, lifecycle.CurrentOnly); err != nil || len(got) != 1 {
		t.Fatalf("management current session list = %+v, err = %v", got, err)
	}

	if _, err := pool.Exec(ctx, `UPDATE events SET archived_at = NULL WHERE id = $1`, createdEvent.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE event_days SET archived_at = now() WHERE id = $1`, day.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetSession(ctx, session.ID); !errors.Is(err, event.ErrNotFound) {
		t.Fatalf("session under archived day Get error = %v, want ErrNotFound", err)
	}
	if got, err := store.ListSessions(ctx, day.ID); err != nil || len(got) != 0 {
		t.Fatalf("sessions under archived day = %+v, err = %v", got, err)
	}
}

func TestSeasonLifecycleQueryContract(t *testing.T) {
	pool := lifecyclePool(t)
	ctx := context.Background()

	actorID := uuid.New()
	if _, _, err := user.NewService(user.NewPostgresStore(pool)).Ensure(ctx, actorID, user.Profile{
		Email: "season-operator@example.com", FirstName: "Dorothy", LastName: "Vaughan", Username: "dorothy",
	}); err != nil {
		t.Fatal(err)
	}
	store := season.NewPostgresStore(pool)
	created, err := store.Create(ctx, season.Season{Name: "2026-2027", Active: true})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := pool.Exec(ctx, `UPDATE seasons SET archived_at = now(), archived_by = $2 WHERE id = $1`, created.ID, actorID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(ctx, created.ID); !errors.Is(err, season.ErrNotFound) {
		t.Fatalf("archived season Get error = %v, want ErrNotFound", err)
	}
	listed, err := store.List(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 0 {
		t.Fatalf("default season list returned archived rows: %+v", listed)
	}
	archived, err := store.ListLifecycle(ctx, lifecycle.InactiveOnly)
	if err != nil {
		t.Fatal(err)
	}
	if len(archived) != 1 || archived[0].ArchivedAt == nil || archived[0].ArchivedBy == nil || *archived[0].ArchivedBy != actorID {
		t.Fatalf("archived season list = %+v", archived)
	}
	if _, err := store.GetIncludingArchived(ctx, created.ID); err != nil {
		t.Fatal(err)
	}
}

func TestCompetitorLifecycleQueryContract(t *testing.T) {
	pool := lifecyclePool(t)
	ctx := context.Background()

	actorID := uuid.New()
	if _, _, err := user.NewService(user.NewPostgresStore(pool)).Ensure(ctx, actorID, user.Profile{
		Email: "competition-operator@example.com", FirstName: "Mary", LastName: "Jackson", Username: "mary",
	}); err != nil {
		t.Fatal(err)
	}
	participantID := uuid.New()
	if _, _, err := user.NewService(user.NewPostgresStore(pool)).Ensure(ctx, participantID, user.Profile{
		Email: "participant@example.com", FirstName: "Alan", LastName: "Turing", Username: "alan",
	}); err != nil {
		t.Fatal(err)
	}
	eventStore := event.NewPostgresStore(pool)
	createdEvent, err := eventStore.Create(ctx, event.Event{Name: "Yarışma", Location: "YTÜ", OwnerTeam: "WEBLAB", Ranked: true})
	if err != nil {
		t.Fatal(err)
	}
	store := competitor.NewPostgresStore(pool)
	created, err := store.Create(ctx, competitor.Competitor{UserID: participantID, EventID: createdEvent.ID})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := pool.Exec(ctx, `UPDATE competitors SET withdrawn_at = now(), withdrawn_by = $2 WHERE id = $1`, created.ID, actorID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(ctx, created.ID); !errors.Is(err, competitor.ErrNotFound) {
		t.Fatalf("withdrawn competitor Get error = %v, want ErrNotFound", err)
	}
	listed, err := store.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 0 {
		t.Fatalf("default competitor list returned withdrawn rows: %+v", listed)
	}
	withdrawn, err := store.ListLifecycle(ctx, lifecycle.InactiveOnly)
	if err != nil {
		t.Fatal(err)
	}
	if len(withdrawn) != 1 || withdrawn[0].WithdrawnAt == nil || withdrawn[0].WithdrawnBy == nil || *withdrawn[0].WithdrawnBy != actorID {
		t.Fatalf("withdrawn competitor list = %+v", withdrawn)
	}
	if _, err := store.GetIncludingWithdrawn(ctx, created.ID); err != nil {
		t.Fatal(err)
	}
}

func TestMediaLifecycleQueryContract(t *testing.T) {
	pool := lifecyclePool(t)
	ctx := context.Background()

	actorID := uuid.New()
	if _, _, err := user.NewService(user.NewPostgresStore(pool)).Ensure(ctx, actorID, user.Profile{
		Email: "media-operator@example.com", FirstName: "Hedy", LastName: "Lamarr", Username: "hedy",
	}); err != nil {
		t.Fatal(err)
	}
	store := media.NewPostgresStore(pool)
	created, err := store.Create(ctx, media.Media{
		Name: "poster.png", Type: "image/png", Key: "images/poster", Kind: media.KindImage, UploadedBy: actorID,
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := pool.Exec(ctx, `UPDATE media SET deleted_at = now(), deleted_by = $2 WHERE id = $1`, created.ID, actorID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(ctx, created.ID); !errors.Is(err, media.ErrNotFound) {
		t.Fatalf("deleted media Get error = %v, want ErrNotFound", err)
	}
	listed, err := store.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 0 {
		t.Fatalf("default media list returned deleted rows: %+v", listed)
	}
	deleted, err := store.ListLifecycle(ctx, lifecycle.InactiveOnly)
	if err != nil {
		t.Fatal(err)
	}
	if len(deleted) != 1 || deleted[0].DeletedAt == nil || deleted[0].DeletedBy == nil || *deleted[0].DeletedBy != actorID {
		t.Fatalf("deleted media list = %+v", deleted)
	}
	if _, err := store.GetIncludingDeleted(ctx, created.ID); err != nil {
		t.Fatal(err)
	}
}

func TestShortURLLifecycleQueryContract(t *testing.T) {
	pool := lifecyclePool(t)
	ctx := context.Background()

	actorID := uuid.New()
	if _, _, err := user.NewService(user.NewPostgresStore(pool)).Ensure(ctx, actorID, user.Profile{
		Email: "url-operator@example.com", FirstName: "Radia", LastName: "Perlman", Username: "radia",
	}); err != nil {
		t.Fatal(err)
	}
	store := shorturl.NewPostgresStore(pool)
	created, err := store.Create(ctx, shorturl.URL{Alias: "lifecycle", URL: "https://example.com", CreatedBy: &actorID})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := pool.Exec(ctx, `UPDATE urls SET disabled_at = now(), disabled_by = $2 WHERE id = $1`, created.ID, actorID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(ctx, created.ID); !errors.Is(err, shorturl.ErrNotFound) {
		t.Fatalf("disabled URL Get error = %v, want ErrNotFound", err)
	}
	if _, err := store.GetByAlias(ctx, created.Alias); !errors.Is(err, shorturl.ErrNotFound) {
		t.Fatalf("disabled alias error = %v, want ErrNotFound", err)
	}
	if _, err := store.RecordHit(ctx, created.ID, shorturl.Hit{}); !errors.Is(err, shorturl.ErrNotFound) {
		t.Fatalf("disabled hit error = %v, want ErrNotFound", err)
	}
	disabled, err := store.ListLifecycle(ctx, lifecycle.InactiveOnly)
	if err != nil {
		t.Fatal(err)
	}
	if len(disabled) != 1 || disabled[0].DisabledAt == nil || disabled[0].DisabledBy == nil || *disabled[0].DisabledBy != actorID {
		t.Fatalf("disabled URL list = %+v", disabled)
	}
	if _, err := store.GetIncludingDisabled(ctx, created.ID); err != nil {
		t.Fatal(err)
	}
}
