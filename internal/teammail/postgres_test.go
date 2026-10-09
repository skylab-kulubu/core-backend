package teammail_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/teammail"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

func migrated(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := testpostgres.Start(t)
	if err := migrate.Apply(context.Background(), pool); err != nil {
		t.Fatal(err)
	}
	return pool
}

func TestPostgresQueue(t *testing.T) {
	pool := migrated(t)
	ctx := context.Background()
	q := teammail.NewPostgresQueue(pool)
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	actor := uuid.New()
	first := teammail.Change{
		ID: uuid.New(), SubjectID: uuid.New(), ActorID: &actor, GroupPath: "/UYELER/ARGE/WEBLAB",
		Action: teammail.ActionAdded, OccurredAt: now.Add(-time.Minute),
	}
	second := teammail.Change{
		ID: uuid.New(), SubjectID: uuid.New(), GroupPath: "/UYELER/ARGE/WEBLAB/LIDERLER",
		Action: teammail.ActionRemoved, OccurredAt: now,
	}
	for _, c := range []teammail.Change{second, first} {
		if err := q.Enqueue(ctx, c); err != nil {
			t.Fatal(err)
		}
	}

	claimed, err := q.Claim(ctx, now, 2*time.Minute, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 1 || claimed[0].ID != first.ID || claimed[0].ActorID == nil || *claimed[0].ActorID != actor ||
		claimed[0].Action != teammail.ActionAdded || claimed[0].GroupPath != first.GroupPath || !claimed[0].OccurredAt.Equal(first.OccurredAt) {
		t.Fatalf("claimed %+v", claimed)
	}
	// Leased: the next claim takes the other one only.
	again, err := q.Claim(ctx, now, 2*time.Minute, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 1 || again[0].ID != second.ID || again[0].ActorID != nil || again[0].Action != teammail.ActionRemoved {
		t.Fatalf("second claim %+v", again)
	}

	if err := q.Retry(ctx, claimed[0], now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	reclaimed, _ := q.Claim(ctx, now.Add(5*time.Minute), time.Minute, 10)
	if len(reclaimed) != 1 || reclaimed[0].ID != second.ID {
		t.Fatalf("after lease %+v", reclaimed)
	}
	count, oldest, err := q.Backlog(ctx, now)
	if err != nil || count != 2 || oldest != time.Minute {
		t.Fatalf("backlog %d %s %v", count, oldest, err)
	}
	if err := q.Complete(ctx, reclaimed[0]); err != nil {
		t.Fatal(err)
	}
	if got, _ := q.Claim(ctx, now.Add(time.Hour), time.Minute, 10); len(got) != 1 || got[0].ID != first.ID || got[0].Attempts != 1 {
		t.Fatalf("after retry %+v", got)
	}
	dropped, err := q.DropBefore(ctx, now)
	if err != nil || dropped != 1 {
		t.Fatalf("dropped %d %v", dropped, err)
	}
	if count, _, _ := q.Backlog(ctx, now); count != 0 {
		t.Fatalf("left %d", count)
	}

	if _, err := pool.Exec(ctx, `INSERT INTO team_membership_mails (id, subject_id, group_path, action, occurred_at, next_attempt_at) VALUES ($1, $2, '/x', 'renamed', now(), now())`, uuid.New(), uuid.New()); err == nil {
		t.Fatal("unknown action accepted")
	}
}

func TestAccountErasureForgetsQueuedTeamMails(t *testing.T) {
	pool := migrated(t)
	ctx := context.Background()
	users := user.NewPostgresStore(pool)
	subject, other := uuid.New(), uuid.New()
	for _, id := range []uuid.UUID{subject, other} {
		if _, _, err := user.NewService(users).Ensure(ctx, id, user.Profile{Email: id.String() + "@example.test"}); err != nil {
			t.Fatal(err)
		}
	}
	q := teammail.NewPostgresQueue(pool)
	now := time.Now().UTC()
	toSubject := teammail.Change{ID: uuid.New(), SubjectID: subject, ActorID: &other, GroupPath: "/UYELER/ARGE/WEBLAB", Action: teammail.ActionAdded, OccurredAt: now}
	bySubject := teammail.Change{ID: uuid.New(), SubjectID: other, ActorID: &subject, GroupPath: "/UYELER/ARGE/WEBLAB", Action: teammail.ActionRemoved, OccurredAt: now}
	for _, c := range []teammail.Change{toSubject, bySubject} {
		if err := q.Enqueue(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := users.RequestDeletion(ctx, subject, nil); err != nil {
		t.Fatal(err)
	}
	if err := users.AnonymizeAccount(ctx, subject, now, nil); err != nil {
		t.Fatal(err)
	}
	claimed, err := q.Claim(ctx, now, time.Minute, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 1 || claimed[0].ID != bySubject.ID || claimed[0].ActorID != nil {
		t.Fatalf("after erasure %+v", claimed)
	}
}

// Two cores: A's lease ran out while it was still sending, and B took the
// row. Whatever A then writes (done, or failed) must not touch B's claim.
func TestPostgresQueueFencesAnotherCoresClaim(t *testing.T) {
	pool := migrated(t)
	ctx := context.Background()
	q := teammail.NewPostgresQueue(pool)
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	change := teammail.Change{ID: uuid.New(), SubjectID: uuid.New(), GroupPath: "/UYELER/ARGE/WEBLAB", Action: teammail.ActionAdded, OccurredAt: now}
	if err := q.Enqueue(ctx, change); err != nil {
		t.Fatal(err)
	}
	a, err := q.Claim(ctx, now, 2*time.Minute, 1)
	if err != nil || len(a) != 1 {
		t.Fatalf("A %v %v", a, err)
	}
	b, err := q.Claim(ctx, now.Add(3*time.Minute), 2*time.Minute, 1)
	if err != nil || len(b) != 1 || b[0].ClaimedAt.Equal(a[0].ClaimedAt) {
		t.Fatalf("B %v %v", b, err)
	}
	if err := q.Retry(ctx, a[0], now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := q.Complete(ctx, a[0]); err != nil {
		t.Fatal(err)
	}
	var attempts int
	var next time.Time
	if err := pool.QueryRow(ctx, `SELECT attempts, next_attempt_at FROM team_membership_mails WHERE id = $1`, change.ID).Scan(&attempts, &next); err != nil {
		t.Fatalf("A's write removed B's row: %v", err)
	}
	if attempts != 0 || !next.Equal(now.Add(5*time.Minute)) {
		t.Fatalf("A's retry changed B's claim: attempts %d next %s", attempts, next)
	}
	if err := q.Complete(ctx, b[0]); err != nil {
		t.Fatal(err)
	}
	if count, _, _ := q.Backlog(ctx, now); count != 0 {
		t.Fatalf("B's complete left %d", count)
	}
}

func TestPostgresQueueKeepsOnePendingRowPerChange(t *testing.T) {
	pool := migrated(t)
	ctx := context.Background()
	q := teammail.NewPostgresQueue(pool)
	now := time.Now().UTC()
	subject := uuid.New()
	for i, action := range []teammail.Action{teammail.ActionAdded, teammail.ActionAdded, teammail.ActionRemoved} {
		if err := q.Enqueue(ctx, teammail.Change{
			ID: uuid.New(), SubjectID: subject, GroupPath: "/UYELER/ARGE/WEBLAB", Action: action, OccurredAt: now.Add(time.Duration(i) * time.Second),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if count, _, _ := q.Backlog(ctx, now); count != 2 {
		t.Fatalf("pending %d, want 2", count)
	}
}
