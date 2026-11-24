package eventreminder_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/eventreminder"
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

// pgFixture is a database whose clock is a fixed evening at 18:05: the
// Events of the next Istanbul day are due.
type pgFixture struct {
	pool    *pgxpool.Pool
	events  *event.PostgresStore
	tickets *ticket.PostgresStore
	queue   *eventreminder.PostgresQueue
	now     time.Time
	window  eventreminder.Window
}

func newPG(t *testing.T) *pgFixture {
	t.Helper()
	pool := migrated(t)
	// A day well ahead, so that the Events are not over for the stores.
	day := time.Now().In(istanbul).AddDate(0, 0, 10)
	now := time.Date(day.Year(), day.Month(), day.Day(), 18, 5, 0, 0, istanbul)
	w, ok := eventreminder.PlanWindow(now, istanbul)
	if !ok {
		t.Fatal("no window at 18:05")
	}
	return &pgFixture{pool: pool, events: event.NewPostgresStore(pool), tickets: ticket.NewPostgresStore(pool),
		queue: eventreminder.NewPostgresQueue(pool), now: now.UTC(), window: w}
}

// event creates an Event of status starting at start (Istanbul time).
func (f *pgFixture) event(t *testing.T, status event.Status, start time.Time) event.Event {
	t.Helper()
	end := start.Add(3 * time.Hour)
	ev, err := f.events.Create(context.Background(), event.Event{Name: "Gece Kodu", Location: "D-Blok", OwnerTeam: "WEBLAB",
		StartDate: &start, EndDate: &end, Status: status})
	if err != nil {
		t.Fatal(err)
	}
	return ev
}

// tomorrowAt is hh:mm the day after the fixture's evening, Istanbul time.
func (f *pgFixture) tomorrowAt(hh, mm int) time.Time {
	d := f.window.From
	return time.Date(d.Year(), d.Month(), d.Day(), hh, mm, 0, 0, istanbul)
}

func (f *pgFixture) guestTicket(t *testing.T, ev event.Event, email string, status ticket.Status) ticket.Ticket {
	t.Helper()
	tk, err := f.tickets.Create(context.Background(), ticket.Ticket{EventID: ev.ID, TicketType: ticket.Guest, GuestFirstName: "Can", GuestEmail: email})
	if err != nil {
		t.Fatal(err)
	}
	f.setStatus(t, tk.ID, status)
	return tk
}

func (f *pgFixture) setStatus(t *testing.T, id uuid.UUID, status ticket.Status) {
	t.Helper()
	switch status {
	case ticket.StatusCancelled:
		if _, err := f.pool.Exec(context.Background(), `UPDATE tickets SET status = 'cancelled', cancelled_at = now(), cancelled_by = 'operator' WHERE id = $1`, id); err != nil {
			t.Fatal(err)
		}
	case ticket.StatusWaitlisted:
		if _, err := f.pool.Exec(context.Background(), `UPDATE tickets SET status = 'waitlisted' WHERE id = $1`, id); err != nil {
			t.Fatal(err)
		}
	}
}

func (f *pgFixture) queued(t *testing.T, eventID uuid.UUID) []uuid.UUID {
	t.Helper()
	rows, err := f.pool.Query(context.Background(), `SELECT ticket_id FROM event_reminder_mails WHERE event_id = $1 ORDER BY created_at, ticket_id`, eventID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		out = append(out, id)
	}
	return out
}

// Two cores planning at once plan once; only a published, current Event of
// the next day is planned, for its active Tickets of that moment; a later
// pass plans nothing more, not even a Ticket written since.
func TestPlanningIsOnceAndOnlyForActiveTicketsOfPublishedEventsTomorrow(t *testing.T) {
	f := newPG(t)
	ctx := context.Background()
	due := f.event(t, event.StatusPublished, f.tomorrowAt(10, 0))
	active := f.guestTicket(t, due, "a@example.com", ticket.StatusActive)
	f.guestTicket(t, due, "c@example.com", ticket.StatusCancelled)
	f.guestTicket(t, due, "w@example.com", ticket.StatusWaitlisted)

	cancelled := f.event(t, event.StatusPublished, f.tomorrowAt(11, 0))
	f.guestTicket(t, cancelled, "a@example.com", ticket.StatusActive)
	if ok, err := f.events.Cancel(ctx, cancelled.ID, nil, "", false); err != nil || !ok {
		t.Fatalf("cancel %v, %v", ok, err)
	}
	draft := f.event(t, event.StatusDraft, f.tomorrowAt(12, 0))
	archived := f.event(t, event.StatusPublished, f.tomorrowAt(13, 0))
	f.guestTicket(t, archived, "a@example.com", ticket.StatusActive)
	if err := f.events.Archive(ctx, archived.ID, nil); err != nil {
		t.Fatal(err)
	}
	dayAfter := f.event(t, event.StatusPublished, f.tomorrowAt(10, 0).AddDate(0, 0, 1))
	f.guestTicket(t, dayAfter, "a@example.com", ticket.StatusActive)
	today := f.event(t, event.StatusPublished, f.now.Add(2*time.Hour))
	f.guestTicket(t, today, "a@example.com", ticket.StatusActive)

	var wg sync.WaitGroup
	results := make([][2]int, 2)
	errs := make([]error, 2)
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i][0], results[i][1], errs[i] = f.queue.Plan(ctx, f.now, f.window)
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if results[0][0]+results[1][0] != 1 || results[0][1]+results[1][1] != 1 {
		t.Fatalf("plans %v, want one Event and one Ticket in all", results)
	}
	if got := f.queued(t, due.ID); len(got) != 1 || got[0] != active.ID {
		t.Fatalf("queued %v, want the active Ticket", got)
	}
	for _, ev := range []event.Event{cancelled, draft, archived, dayAfter, today} {
		if got := f.queued(t, ev.ID); len(got) != 0 {
			t.Fatalf("%s: queued %v", ev.StartDate, got)
		}
	}

	late := f.guestTicket(t, due, "late@example.com", ticket.StatusActive)
	if events, mails, err := f.queue.Plan(ctx, f.now.Add(time.Minute), f.window); err != nil || events != 0 || mails != 0 {
		t.Fatalf("second plan %d, %d, %v", events, mails, err)
	}
	if got := f.queued(t, due.ID); len(got) != 1 {
		t.Fatalf("queued %v; %v was written after planning", got, late.ID)
	}
	var tickets int
	if err := f.pool.QueryRow(ctx, `SELECT tickets FROM event_reminders WHERE event_id = $1`, due.ID).Scan(&tickets); err != nil || tickets != 1 {
		t.Fatalf("event_reminders.tickets %d, %v", tickets, err)
	}
}

// An Event whose start moves to another day is planned again for that day;
// one that starts within MissedPlanCutoff of a late pass is not planned.
func TestAStartMovedToAnotherDayIsPlannedAgainAndALatePlanStopsShort(t *testing.T) {
	f := newPG(t)
	ctx := context.Background()
	ev := f.event(t, event.StatusPublished, f.tomorrowAt(10, 0))
	f.guestTicket(t, ev, "a@example.com", ticket.StatusActive)
	if events, _, err := f.queue.Plan(ctx, f.now, f.window); err != nil || events != 1 {
		t.Fatalf("plan %d, %v", events, err)
	}
	moved := f.tomorrowAt(10, 0).AddDate(0, 0, 1)
	if _, err := f.pool.Exec(ctx, `UPDATE events SET start_date = $2, end_date = $3 WHERE id = $1`, ev.ID, moved, moved.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	next := f.now.AddDate(0, 0, 1)
	w, _ := eventreminder.PlanWindow(next, istanbul)
	if events, mails, err := f.queue.Plan(ctx, next, w); err != nil || events != 1 || mails != 1 {
		t.Fatalf("moved plan %d, %d, %v", events, mails, err)
	}
	if got := f.queued(t, ev.ID); len(got) != 2 {
		t.Fatalf("queued %v, want a row for each day", got)
	}

	// core was down until 23:30: an Event at 01:00 is too close, one at
	// 03:00 is not.
	soon := f.event(t, event.StatusPublished, f.tomorrowAt(1, 0))
	f.guestTicket(t, soon, "a@example.com", ticket.StatusActive)
	later := f.event(t, event.StatusPublished, f.tomorrowAt(3, 0))
	f.guestTicket(t, later, "a@example.com", ticket.StatusActive)
	lateNow := f.window.From.Add(-30 * time.Minute)
	lateWindow, _ := eventreminder.PlanWindow(lateNow, istanbul)
	if events, _, err := f.queue.Plan(ctx, lateNow.UTC(), lateWindow); err != nil || events != 1 {
		t.Fatalf("late plan %d, %v", events, err)
	}
	if len(f.queued(t, soon.ID)) != 0 || len(f.queued(t, later.ID)) != 1 {
		t.Fatalf("soon %v, later %v", f.queued(t, soon.ID), f.queued(t, later.ID))
	}
}

// The per-minute bound holds on the rows that led a mail; a row done with
// no mail gives its place back. A held row is not claimed twice, and a
// stale claim writes nothing.
func TestClaimKeepsThePerMinuteBoundAndHonoursItsClaim(t *testing.T) {
	f := newPG(t)
	ctx := context.Background()
	ev := f.event(t, event.StatusPublished, f.tomorrowAt(10, 0))
	for _, email := range []string{"a@example.com", "b@example.com", "c@example.com"} {
		f.guestTicket(t, ev, email, ticket.StatusActive)
	}
	if _, mails, err := f.queue.Plan(ctx, f.now, f.window); err != nil || mails != 3 {
		t.Fatalf("plan %d, %v", mails, err)
	}
	now := f.now.Add(time.Second)
	first, ok, limited, err := f.queue.Claim(ctx, now, time.Minute, 1)
	if err != nil || !ok || limited {
		t.Fatalf("claim %v, %v, %v", ok, limited, err)
	}
	if _, ok, limited, err := f.queue.Claim(ctx, now, time.Minute, 1); err != nil || ok || !limited {
		t.Fatalf("over the bound: %v, %v, %v", ok, limited, err)
	}
	stale := first
	stale.ClaimedAt = first.ClaimedAt.Add(-time.Second)
	if err := f.queue.Finish(ctx, stale, now, eventreminder.Dropped); err != nil {
		t.Fatal(err)
	}
	var open int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM event_reminder_mails WHERE done_at IS NULL`).Scan(&open); err != nil || open != 3 {
		t.Fatalf("a stale claim finished a row: %d open, %v", open, err)
	}
	if err := f.queue.Finish(ctx, first, now, eventreminder.Dropped); err != nil {
		t.Fatal(err)
	}
	second, ok, limited, err := f.queue.Claim(ctx, now, time.Minute, 1)
	if err != nil || !ok || limited || second.ID == first.ID {
		t.Fatalf("after a dropped row: %v, %v, %v", ok, limited, err)
	}
	if err := f.queue.Finish(ctx, second, now, eventreminder.Sent); err != nil {
		t.Fatal(err)
	}
	if _, ok, limited, err := f.queue.Claim(ctx, now, time.Minute, 1); err != nil || ok || !limited {
		t.Fatalf("a sent row keeps its place: %v, %v, %v", ok, limited, err)
	}
	third, ok, _, err := f.queue.Claim(ctx, now.Add(time.Minute), time.Minute, 1)
	if err != nil || !ok {
		t.Fatalf("the next minute: %v, %v", ok, err)
	}
	if err := f.queue.Retry(ctx, third, now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, ok, _, err := f.queue.Claim(ctx, now.Add(90*time.Second), time.Minute, 0); err != nil || ok {
		t.Fatalf("a retried row before its time: %v, %v", ok, err)
	}
	again, ok, _, err := f.queue.Claim(ctx, now.Add(2*time.Minute), time.Minute, 0)
	if err != nil || !ok || again.ID != third.ID || again.Attempts != 1 {
		t.Fatalf("retry %+v, %v, %v", again, ok, err)
	}
}

// Siblings are the open rows of the same reminder under one address: a
// guest Ticket under it and a member whose primary e-mail it is.
func TestSiblingsAreTheSameAddressesRows(t *testing.T) {
	f := newPG(t)
	ctx := context.Background()
	member := uuid.New()
	if _, err := f.pool.Exec(ctx, `INSERT INTO users (id, email) VALUES ($1, 'Ayse@Example.com')`, member); err != nil {
		t.Fatal(err)
	}
	ev := f.event(t, event.StatusPublished, f.tomorrowAt(10, 0))
	guest := f.guestTicket(t, ev, " ayse@example.com", ticket.StatusActive)
	own, err := f.tickets.Create(ctx, ticket.Ticket{EventID: ev.ID, TicketType: ticket.Registered, OwnerID: &member})
	if err != nil {
		t.Fatal(err)
	}
	f.guestTicket(t, ev, "can@example.com", ticket.StatusActive)
	if _, mails, err := f.queue.Plan(ctx, f.now, f.window); err != nil || mails != 3 {
		t.Fatalf("plan %d, %v", mails, err)
	}
	claimed, ok, _, err := f.queue.Claim(ctx, f.now, time.Minute, 0)
	if err != nil || !ok {
		t.Fatalf("claim %v, %v", ok, err)
	}
	if claimed.TicketID != guest.ID && claimed.TicketID != own.ID {
		// Rows planned together share created_at: whichever goes first,
		// the other of the address is its sibling.
		claimed, ok, _, err = f.queue.Claim(ctx, f.now, time.Minute, 0)
		if err != nil || !ok {
			t.Fatalf("claim %v, %v", ok, err)
		}
	}
	siblings, err := f.queue.Siblings(ctx, claimed, "ayse@example.com", f.now)
	if err != nil || len(siblings) != 1 {
		t.Fatalf("siblings %+v, %v", siblings, err)
	}
	want := own.ID
	if claimed.TicketID == own.ID {
		want = guest.ID
	}
	if siblings[0].TicketID != want {
		t.Fatalf("sibling %v, want %v", siblings[0].TicketID, want)
	}
	taken, ok, err := f.queue.ClaimSibling(ctx, siblings[0].ID, f.now, time.Minute)
	if err != nil || !ok || taken.ID != siblings[0].ID {
		t.Fatalf("claim sibling %v, %v", ok, err)
	}
	if _, ok, err := f.queue.ClaimSibling(ctx, siblings[0].ID, f.now, time.Minute); err != nil || ok {
		t.Fatalf("a held sibling was taken twice: %v, %v", ok, err)
	}
	// A deleted Ticket takes its row with it.
	if _, err := f.pool.Exec(ctx, `DELETE FROM tickets WHERE id = $1`, guest.ID); err != nil {
		t.Fatal(err)
	}
	if got := f.queued(t, ev.ID); len(got) != 2 {
		t.Fatalf("queued %v after the Ticket went", got)
	}
}
