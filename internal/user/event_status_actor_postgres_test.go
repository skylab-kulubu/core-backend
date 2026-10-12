package user_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/subjectlock"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// An Event's publisher and canceller are actor references like its archiver
// (account-lifecycle.md): account erasure clears them and keeps the Event,
// and once the account is being erased neither can name it.
func TestAnonymizationClearsTheEventPublisherAndCanceller(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	users := user.NewPostgresStore(pool)
	actor := uuid.New()
	if _, _, err := user.NewService(users).Ensure(ctx, actor, user.Profile{Email: "organizer@example.test"}); err != nil {
		t.Fatal(err)
	}
	events := event.NewPostgresStore(pool)
	published, err := events.Create(ctx, event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB", PublishedBy: &actor})
	if err != nil {
		t.Fatal(err)
	}
	if published.PublishedBy == nil || *published.PublishedBy != actor {
		t.Fatalf("publishedBy %v", published.PublishedBy)
	}
	// No cancel endpoint yet: written as it will write it.
	if _, err := pool.Exec(ctx, `UPDATE events SET status = 'cancelled', cancelled_at = now(), cancelled_by = $2 WHERE id = $1`, published.ID, actor); err != nil {
		t.Fatal(err)
	}
	other, err := events.Create(ctx, event.Event{Name: "CTF", Location: "YTÜ", OwnerTeam: "WEBLAB"})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := users.RequestDeletion(ctx, actor, nil); err != nil {
		t.Fatal(err)
	}
	// Being erased: the guard refuses the person as a canceller.
	_, err = pool.Exec(ctx, `UPDATE events SET cancelled_by = $2 WHERE id = $1`, other.ID, actor)
	if !subjectlock.IsInactiveAccountReference(err) {
		t.Fatalf("canceller being erased: %v", err)
	}
	if err := users.AnonymizeAccount(ctx, actor, time.Now().UTC(), nil); err != nil {
		t.Fatal(err)
	}
	var publishedBy, cancelledBy *uuid.UUID
	var status string
	var publishedAt, cancelledAt *time.Time
	if err := pool.QueryRow(ctx, `SELECT status, published_at, published_by, cancelled_at, cancelled_by FROM events WHERE id = $1`, published.ID).Scan(&status, &publishedAt, &publishedBy, &cancelledAt, &cancelledBy); err != nil {
		t.Fatal(err)
	}
	if publishedBy != nil || cancelledBy != nil {
		t.Fatalf("erasure kept publisher=%v canceller=%v", publishedBy, cancelledBy)
	}
	if status != "cancelled" || publishedAt == nil || cancelledAt == nil {
		t.Fatalf("erasure changed the Event: status=%q published=%v cancelled=%v", status, publishedAt, cancelledAt)
	}
}
