package event_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/doorstaffmail"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// A person new to an Event's door staff gets one queued mail, two minutes
// later; one already on it and every change while the mail is off queue
// nothing. The queue's claim, wait and done.
func TestAddingDoorStaffQueuesTheirMail(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := event.NewPostgresStore(pool)
	people := user.NewService(user.NewPostgresStore(pool))
	ada, cem := uuid.New(), uuid.New()
	for _, id := range []uuid.UUID{ada, cem} {
		if _, _, err := people.Ensure(ctx, id, user.Profile{Email: id.String() + "@example.com"}); err != nil {
			t.Fatal(err)
		}
	}
	queued := func(eventID uuid.UUID) map[uuid.UUID]time.Duration {
		t.Helper()
		rows, err := pool.Query(ctx, `SELECT user_id, EXTRACT(EPOCH FROM next_attempt_at - created_at)::bigint FROM door_staff_notifications WHERE event_id = $1`, eventID)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[uuid.UUID]time.Duration{}
		for rows.Next() {
			var id uuid.UUID
			var seconds int64
			if err := rows.Scan(&id, &seconds); err != nil {
				t.Fatal(err)
			}
			out[id] = time.Duration(seconds) * time.Second
		}
		return out
	}
	start := time.Now().Add(72 * time.Hour)
	ev, err := store.Create(ctx, event.Event{Name: "Gece Kodu", Location: "D-Blok", OwnerTeam: "WEBLAB", Status: event.StatusPublished, StartDate: &start, DoorStaffIDs: []uuid.UUID{ada}})
	if err != nil {
		t.Fatal(err)
	}
	if got := queued(ev.ID); len(got) != 0 {
		t.Fatalf("queued %v while off", got)
	}

	store.NotifyDoorStaff(true)
	ev.DoorStaffIDs = []uuid.UUID{ada, cem}
	if ev, err = store.Update(ctx, ev); err != nil {
		t.Fatal(err)
	}
	got := queued(ev.ID)
	if len(got) != 1 || got[cem] != event.DoorStaffMailDelay {
		t.Fatalf("queued %v, want Cem alone after %s", got, event.DoorStaffMailDelay)
	}
	// Saved again with the same list: nothing new.
	if _, err = store.Update(ctx, ev); err != nil {
		t.Fatal(err)
	}
	if got := queued(ev.ID); len(got) != 1 {
		t.Fatalf("queued %v after a save with the same list", got)
	}
	// A new Event with door staff queues each of them.
	other, err := store.Create(ctx, event.Event{Name: "Taslak", Location: "D-Blok", OwnerTeam: "WEBLAB", Status: event.StatusDraft, StartDate: &start, DoorStaffIDs: []uuid.UUID{ada}})
	if err != nil {
		t.Fatal(err)
	}
	if got := queued(other.ID); len(got) != 1 {
		t.Fatalf("queued %v for a new Event's door staff", got)
	}

	q := doorstaffmail.NewPostgresQueue(pool)
	if _, ok, err := q.Claim(ctx, time.Now().UTC(), time.Minute); err != nil || ok {
		t.Fatalf("claimed before the delay: %v %v", ok, err)
	}
	later := time.Now().UTC().Add(event.DoorStaffMailDelay + time.Minute)
	claimed, ok, err := q.Claim(ctx, later, time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim %v %v", ok, err)
	}
	if err := q.Retry(ctx, claimed, later.Add(time.Hour), false); err != nil {
		t.Fatal(err)
	}
	var attempts int
	if err := pool.QueryRow(ctx, `SELECT attempts FROM door_staff_notifications WHERE id = $1`, claimed.ID).Scan(&attempts); err != nil || attempts != 0 {
		t.Fatalf("a draft's wait counted: %d %v", attempts, err)
	}
	second, ok, err := q.Claim(ctx, later, time.Minute)
	if err != nil || !ok || second.ID == claimed.ID {
		t.Fatalf("second claim %+v %v %v", second, ok, err)
	}
	if err := q.Complete(ctx, second); err != nil {
		t.Fatal(err)
	}
	var left int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM door_staff_notifications`).Scan(&left); err != nil || left != 1 {
		t.Fatalf("left %d %v", left, err)
	}
}
