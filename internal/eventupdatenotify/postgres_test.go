package eventupdatenotify_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/eventupdatenotify"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
)

func migrated(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := testpostgres.Start(t)
	if err := migrate.Apply(context.Background(), pool); err != nil {
		t.Fatal(err)
	}
	return pool
}

type queuedRow struct {
	id             uuid.UUID
	beforeStart    *time.Time
	beforeLocation string
	window         time.Duration
	started        bool
}

func queuedRows(t *testing.T, pool *pgxpool.Pool, eventID uuid.UUID) []queuedRow {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
		SELECT id, before_start_date, before_location, EXTRACT(EPOCH FROM next_attempt_at - created_at)::bigint, started_at IS NOT NULL
		FROM event_update_notifications WHERE event_id = $1 ORDER BY created_at, id`, eventID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []queuedRow
	for rows.Next() {
		var r queuedRow
		var seconds int64
		if err := rows.Scan(&r.id, &r.beforeStart, &r.beforeLocation, &seconds, &r.started); err != nil {
			t.Fatal(err)
		}
		r.window = time.Duration(seconds) * time.Second
		out = append(out, r)
	}
	return out
}

func futureEvent(t *testing.T, store *event.PostgresStore, status event.Status) event.Event {
	t.Helper()
	start := time.Now().Add(72 * time.Hour).UTC().Truncate(time.Second)
	end := start.Add(3 * time.Hour)
	ev, err := store.Create(context.Background(), event.Event{Name: "Gece Kodu", Location: "D-Blok", OwnerTeam: "WEBLAB", StartDate: &start, EndDate: &end, Status: status})
	if err != nil {
		t.Fatal(err)
	}
	return ev
}

func update(t *testing.T, store *event.PostgresStore, ev event.Event, change func(*event.Event)) event.Event {
	t.Helper()
	change(&ev)
	got, err := store.Update(context.Background(), ev)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// A published Event's new time or place queues one row that gathers the
// changes of the window and keeps what the Event said before the first; a
// change of its name, a draft's change and any change while it is off queue
// nothing.
func TestTheUpdateQueuesAChangeOfTimeOrPlaceOnce(t *testing.T) {
	pool := migrated(t)
	store := event.NewPostgresStore(pool)

	ev := futureEvent(t, store, event.StatusPublished)
	ev = update(t, store, ev, func(e *event.Event) { e.Location = "Kongre Merkezi" })
	if got := queuedRows(t, pool, ev.ID); len(got) != 0 {
		t.Fatalf("queued %+v while off", got)
	}

	store.NotifyUpdated(true)
	ev = update(t, store, ev, func(e *event.Event) { e.Name, e.Description = "Gece Kodu 2026", "Yeni açıklama" })
	if got := queuedRows(t, pool, ev.ID); len(got) != 0 {
		t.Fatalf("queued %+v for a new name", got)
	}
	before := *ev.StartDate
	ev = update(t, store, ev, func(e *event.Event) { later := e.StartDate.Add(time.Hour); e.StartDate = &later })
	ev = update(t, store, ev, func(e *event.Event) { e.Location = "Oditoryum" })
	ev = update(t, store, ev, func(e *event.Event) { later := e.EndDate.Add(time.Hour); e.EndDate = &later })
	got := queuedRows(t, pool, ev.ID)
	if len(got) != 1 {
		t.Fatalf("queued %+v, want one row gathering three changes", got)
	}
	if got[0].beforeStart == nil || !got[0].beforeStart.Equal(before) || got[0].beforeLocation != "Kongre Merkezi" || got[0].window != event.UpdateNotificationWindow {
		t.Fatalf("row %+v, want the first change's before and a %s window", got[0], event.UpdateNotificationWindow)
	}

	draft := futureEvent(t, store, event.StatusDraft)
	update(t, store, draft, func(e *event.Event) { e.Location = "Oditoryum" })
	if got := queuedRows(t, pool, draft.ID); len(got) != 0 {
		t.Fatalf("queued %+v for a draft", got)
	}

	// Once the worker has taken the row, a change queues a row of its own,
	// which waits until the first is done.
	q := eventupdatenotify.NewPostgresQueue(pool)
	now := time.Now().UTC().Add(event.UpdateNotificationWindow + time.Minute)
	first, ok, err := q.Claim(context.Background(), now, time.Minute)
	if err != nil || !ok || first.ID != got[0].id || first.BeforeLocation != "Kongre Merkezi" {
		t.Fatalf("claim %+v, %v, %v", first, ok, err)
	}
	update(t, store, ev, func(e *event.Event) { e.Location = "D-Blok" })
	got = queuedRows(t, pool, ev.ID)
	if len(got) != 2 || !got[0].started || got[1].started || got[1].beforeLocation != "Oditoryum" {
		t.Fatalf("queued %+v, want the started row and a new one", got)
	}
	if err := q.Retry(context.Background(), first, now); err != nil {
		t.Fatal(err)
	}
	again, ok, err := q.Claim(context.Background(), now.Add(time.Hour), time.Minute)
	if err != nil || !ok || again.ID != first.ID {
		t.Fatalf("claim %+v, %v, %v: the older row goes first", again, ok, err)
	}
	if err := q.Complete(context.Background(), again); err != nil {
		t.Fatal(err)
	}
	next, ok, err := q.Claim(context.Background(), now.Add(time.Hour), time.Minute)
	if err != nil || !ok || next.ID != got[1].id {
		t.Fatalf("claim %+v, %v, %v: the newer row once the older is done", next, ok, err)
	}
}

// A cancelled or ended Event's change queues nothing.
func TestAChangeOfACancelledOrEndedEventQueuesNothing(t *testing.T) {
	pool := migrated(t)
	ctx := context.Background()
	store := event.NewPostgresStore(pool)
	store.NotifyUpdated(true)

	cancelled := futureEvent(t, store, event.StatusPublished)
	if ok, err := store.Cancel(ctx, cancelled.ID, nil, "", false); err != nil || !ok {
		t.Fatalf("cancel %v, %v", ok, err)
	}
	cancelled, err := store.Get(ctx, cancelled.ID)
	if err != nil {
		t.Fatal(err)
	}
	update(t, store, cancelled, func(e *event.Event) { e.Location = "Oditoryum" })

	start, end := time.Now().Add(-48*time.Hour).UTC(), time.Now().Add(-45*time.Hour).UTC()
	ended, err := store.Create(ctx, event.Event{Name: "Dün", Location: "D-Blok", OwnerTeam: "WEBLAB", StartDate: &start, EndDate: &end})
	if err != nil {
		t.Fatal(err)
	}
	update(t, store, ended, func(e *event.Event) { e.Location = "Oditoryum" })

	for _, id := range []uuid.UUID{cancelled.ID, ended.ID} {
		if got := queuedRows(t, pool, id); len(got) != 0 {
			t.Fatalf("queued %+v", got)
		}
	}
}

// The sends are recorded once, skip a Ticket deleted meanwhile, and go with
// the row when it is done; a stale claim's writes change nothing.
func TestPostgresQueueRecordsSendsAndHonoursItsClaim(t *testing.T) {
	pool := migrated(t)
	ctx := context.Background()
	store := event.NewPostgresStore(pool)
	store.NotifyUpdated(true)
	ev := futureEvent(t, store, event.StatusPublished)
	tickets := ticket.NewPostgresStore(pool)
	tk, err := tickets.Create(ctx, ticket.Ticket{EventID: ev.ID, TicketType: ticket.Guest, GuestFirstName: "A", GuestEmail: "a@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	update(t, store, ev, func(e *event.Event) { e.Location = "Oditoryum" })

	q := eventupdatenotify.NewPostgresQueue(pool)
	now := time.Now().UTC()
	if _, ok, err := q.Claim(ctx, now, time.Minute); err != nil || ok {
		t.Fatalf("claim within the window: %v, %v", ok, err)
	}
	now = now.Add(event.UpdateNotificationWindow + time.Minute)
	claimed, ok, err := q.Claim(ctx, now, time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim %v, %v", ok, err)
	}
	if _, ok, err := q.Claim(ctx, now, time.Minute); err != nil || ok {
		t.Fatalf("a second core took a held row: %v, %v", ok, err)
	}
	for range 2 {
		if err := q.RecordSend(ctx, claimed.ID, tk.ID); err != nil {
			t.Fatal(err)
		}
	}
	if err := q.RecordSend(ctx, claimed.ID, uuid.New()); err != nil {
		t.Fatalf("a deleted Ticket: %v", err)
	}
	sends, err := q.Sends(ctx, claimed.ID)
	if err != nil || len(sends) != 1 || sends[0] != tk.ID {
		t.Fatalf("sends %v, %v", sends, err)
	}
	stale := claimed
	stale.ClaimedAt = claimed.ClaimedAt.Add(-time.Second)
	if err := q.Complete(ctx, stale); err != nil {
		t.Fatal(err)
	}
	if len(queuedRows(t, pool, ev.ID)) != 1 {
		t.Fatal("a stale claim completed the row")
	}
	if err := q.Complete(ctx, claimed); err != nil {
		t.Fatal(err)
	}
	var left int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM event_update_notifications) + (SELECT count(*) FROM event_update_notification_sends)`).Scan(&left); err != nil || left != 0 {
		t.Fatalf("left %d, %v", left, err)
	}
}
