package teammail_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/teammail"
)

// A change that may already have reached the person (it is being sent, its
// send failed after SkyMail may have taken it, or its core died mid-send)
// is not cancelled by the opposite change: the opposite one is queued, is
// not sent while the first is being sent, and once the first is no longer
// being sent the later change wins and the earlier one is dropped. So the
// last mail always tells the person's current state.
func TestQueueLaterOppositeChangeWins(t *testing.T) {
	queues := map[string]func(t *testing.T) teammail.Queue{
		"memory":   func(*testing.T) teammail.Queue { return teammail.NewMemoryQueue() },
		"postgres": func(t *testing.T) teammail.Queue { return teammail.NewPostgresQueue(migrated(t)) },
	}
	for name, open := range queues {
		t.Run(name, func(t *testing.T) {
			t.Run("send failed while the removal came", func(t *testing.T) {
				q, now, change := supersedeFixture(t, open)
				add := claimOne(t, q, now, change(teammail.ActionAdded, 0))
				removal := change(teammail.ActionRemoved, time.Second)
				if got, err := q.Enqueue(context.Background(), removal); err != nil || got != teammail.EnqueuedQueued {
					t.Fatalf("removal: %v %v", got, err)
				}
				// The add is being sent: the removal waits behind it.
				if got := claim(t, q, now.Add(2*time.Second)); len(got) != 0 {
					t.Fatalf("removal taken while the add is sent: %+v", got)
				}
				if err := q.Retry(context.Background(), add, now.Add(30*time.Second)); err != nil {
					t.Fatal(err)
				}
				wantOnly(t, q, now.Add(3*time.Second), removal.ID)
			})
			t.Run("core died mid-send", func(t *testing.T) {
				q, now, change := supersedeFixture(t, open)
				claimOne(t, q, now, change(teammail.ActionAdded, 0))
				removal := change(teammail.ActionRemoved, time.Second)
				if got, err := q.Enqueue(context.Background(), removal); err != nil || got != teammail.EnqueuedQueued {
					t.Fatalf("removal: %v %v", got, err)
				}
				if got := claim(t, q, now.Add(time.Minute)); len(got) != 0 {
					t.Fatalf("removal taken within the add's lease: %+v", got)
				}
				// The lease ran out: the add is not sent again after the removal.
				wantOnly(t, q, now.Add(3*time.Minute), removal.ID)
			})
			t.Run("a retried send may have been delivered", func(t *testing.T) {
				q, now, change := supersedeFixture(t, open)
				add := claimOne(t, q, now, change(teammail.ActionAdded, 0))
				if err := q.Retry(context.Background(), add, now.Add(time.Hour)); err != nil {
					t.Fatal(err)
				}
				removal := change(teammail.ActionRemoved, time.Second)
				if got, err := q.Enqueue(context.Background(), removal); err != nil || got != teammail.EnqueuedQueued {
					t.Fatalf("removal after a failed send: %v %v, want it queued", got, err)
				}
				wantOnly(t, q, now.Add(2*time.Second), removal.ID)
			})
		})
	}
}

func supersedeFixture(t *testing.T, open func(*testing.T) teammail.Queue) (teammail.Queue, time.Time, func(teammail.Action, time.Duration) teammail.Change) {
	t.Helper()
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	subject := uuid.New()
	return open(t), now, func(action teammail.Action, at time.Duration) teammail.Change {
		return teammail.Change{ID: uuid.New(), SubjectID: subject, GroupPath: "/UYELER/ARGE/WEBLAB", Action: action, OccurredAt: now.Add(at)}
	}
}

func claim(t *testing.T, q teammail.Queue, now time.Time) []teammail.Change {
	t.Helper()
	got, err := q.Claim(context.Background(), now, 2*time.Minute, 10)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func claimOne(t *testing.T, q teammail.Queue, now time.Time, c teammail.Change) teammail.Change {
	t.Helper()
	if _, err := q.Enqueue(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	got := claim(t, q, now)
	if len(got) != 1 || got[0].ID != c.ID {
		t.Fatalf("claimed %+v", got)
	}
	return got[0]
}

// wantOnly claims at now and wants the change id alone, and nothing left
// once it is done.
func wantOnly(t *testing.T, q teammail.Queue, now time.Time, id uuid.UUID) {
	t.Helper()
	got := claim(t, q, now)
	if len(got) != 1 || got[0].ID != id {
		t.Fatalf("claimed %+v, want %s alone", got, id)
	}
	if err := q.Complete(context.Background(), got[0]); err != nil {
		t.Fatal(err)
	}
	if count, _, _ := q.Backlog(context.Background(), now); count != 0 {
		t.Fatalf("%d left", count)
	}
}
