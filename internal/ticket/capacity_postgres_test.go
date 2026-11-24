package ticket_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
)

// capacityPG is a database with the Tickets and Events stores, both mails
// on.
type capacityPG struct {
	pool   *pgxpool.Pool
	store  *ticket.PostgresStore
	events *event.PostgresStore
}

func newCapacityPG(t *testing.T) capacityPG {
	t.Helper()
	pool := testpostgres.Start(t)
	if err := migrate.Apply(context.Background(), pool); err != nil {
		t.Fatal(err)
	}
	e := capacityPG{pool: pool, store: ticket.NewPostgresStore(pool), events: event.NewPostgresStore(pool)}
	e.store.NotifyCreated(true)
	e.store.NotifyStatus(true)
	e.events.NotifyTicketsOnPublish(true)
	e.events.NotifyTicketStatus(true)
	return e
}

func (e capacityPG) event(t *testing.T, capacity int, waitlist bool) event.Event {
	t.Helper()
	start, end := time.Now().Add(24*time.Hour), time.Now().Add(26*time.Hour)
	ev, err := e.events.Create(context.Background(), event.Event{
		Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB", Capacity: capacity,
		WaitlistEnabled: &waitlist, StartDate: &start, EndDate: &end,
	})
	if err != nil {
		t.Fatal(err)
	}
	return ev
}

func guestTicket(eventID uuid.UUID, n int) ticket.Ticket {
	return ticket.Ticket{EventID: eventID, TicketType: ticket.Guest, GuestFirstName: "Misafir", GuestLastName: fmt.Sprint(n),
		GuestEmail: fmt.Sprintf("guest%d@example.com", n)}
}

func (e capacityPG) counts(t *testing.T, eventID uuid.UUID) map[ticket.Status]int {
	t.Helper()
	rows, err := e.pool.Query(context.Background(), `SELECT status, count(*) FROM tickets WHERE event_id = $1 GROUP BY status`, eventID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[ticket.Status]int{}
	for rows.Next() {
		var status ticket.Status
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			t.Fatal(err)
		}
		out[status] = n
	}
	return out
}

func (e capacityPG) mailKinds(t *testing.T, eventID uuid.UUID) map[string]int {
	t.Helper()
	rows, err := e.pool.Query(context.Background(), `
		SELECT n.kind, count(*) FROM ticket_notifications n JOIN tickets t ON t.id = n.ticket_id
		WHERE t.event_id = $1 GROUP BY n.kind`, eventID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var kind string
		var n int
		if err := rows.Scan(&kind, &n); err != nil {
			t.Fatal(err)
		}
		out[kind] = n
	}
	return out
}

func (e capacityPG) mailKind(t *testing.T, id uuid.UUID) string {
	t.Helper()
	var kind string
	if err := e.pool.QueryRow(context.Background(), `SELECT kind FROM ticket_notifications WHERE ticket_id = $1`, id).Scan(&kind); err != nil {
		return ""
	}
	return kind
}

// Eight registrations at once for the one place: the Event's row lock lets
// one through as active; the rest wait on the list (or, without it, are
// refused). Each one's mail is queued with it, by kind.
func TestConcurrentRegistrationsForTheLastPlaceOnPostgres(t *testing.T) {
	e := newCapacityPG(t)
	ctx := context.Background()
	for _, waitlist := range []bool{true, false} {
		t.Run(fmt.Sprintf("waitlist %v", waitlist), func(t *testing.T) {
			ev := e.event(t, 1, waitlist)
			const n = 8
			errs := make([]error, n)
			var wg sync.WaitGroup
			start := make(chan struct{})
			for i := range n {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					_, errs[i] = e.store.CreateAdmitted(ctx, guestTicket(ev.ID, i), ticket.Admission{Waitlist: true})
				}()
			}
			close(start)
			wg.Wait()
			full := 0
			for _, err := range errs {
				switch {
				case errors.Is(err, ticket.ErrEventFull):
					full++
				case err != nil:
					t.Fatal(err)
				}
			}
			got := e.counts(t, ev.ID)
			if waitlist {
				if got[ticket.StatusActive] != 1 || got[ticket.StatusWaitlisted] != n-1 || full != 0 {
					t.Fatalf("statuses %v, refused %d", got, full)
				}
				if kinds := e.mailKinds(t, ev.ID); kinds["created"] != 1 || kinds["waitlisted"] != n-1 {
					t.Fatalf("mails %v", kinds)
				}
			} else if got[ticket.StatusActive] != 1 || got[ticket.StatusWaitlisted] != 0 || full != n-1 {
				t.Fatalf("statuses %v, refused %d", got, full)
			}
		})
	}
}

// Two of the active Tickets cancelled at once: each cancellation promotes
// in its own transaction, under the Event's lock, so two different people
// get the two places, the longest waiting first.
func TestConcurrentCancellationsPromoteTwoPeopleOnPostgres(t *testing.T) {
	e := newCapacityPG(t)
	ctx := context.Background()
	ev := e.event(t, 2, true)
	var active, waiting []ticket.Ticket
	for i := range 5 {
		tk, err := e.store.CreateAdmitted(ctx, guestTicket(ev.ID, i), ticket.Admission{Waitlist: true})
		if err != nil {
			t.Fatal(err)
		}
		if tk.StatusOf() == ticket.StatusActive {
			active = append(active, tk)
		} else {
			waiting = append(waiting, tk)
		}
	}
	if len(active) != 2 || len(waiting) != 3 {
		t.Fatalf("%d active, %d waiting", len(active), len(waiting))
	}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, tk := range active {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, errs[i] = e.store.Cancel(ctx, tk.ID, ticket.CancelledByOperator, "")
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	for i, tk := range waiting {
		got, err := e.store.Get(ctx, tk.ID)
		if err != nil {
			t.Fatal(err)
		}
		want := ticket.StatusActive
		if i == 2 {
			want = ticket.StatusWaitlisted
		}
		if got.StatusOf() != want {
			t.Fatalf("waiting %d is %s, want %s", i, got.Status, want)
		}
		if want == ticket.StatusActive && e.mailKind(t, tk.ID) != "promoted" {
			t.Fatalf("promoted %d's mail %q", i, e.mailKind(t, tk.ID))
		}
	}
	if got := e.counts(t, ev.ID); got[ticket.StatusActive] != 2 {
		t.Fatalf("statuses %v", got)
	}
}

// A larger capacity promotes as many in the Event's update; none (0) takes
// everyone. The Event answers full and, to its editors alone, the counts.
func TestCapacityRaisedPromotesInTheEventUpdateOnPostgres(t *testing.T) {
	e := newCapacityPG(t)
	ctx := context.Background()
	ev := e.event(t, 1, true)
	var waiting []ticket.Ticket
	for i := range 6 {
		tk, err := e.store.CreateAdmitted(ctx, guestTicket(ev.ID, i), ticket.Admission{Waitlist: true})
		if err != nil {
			t.Fatal(err)
		}
		if i > 0 {
			waiting = append(waiting, tk)
		}
	}
	got, err := e.events.Get(ctx, ev.ID)
	if err != nil || !got.Full || !got.Waitlist() {
		t.Fatalf("full %+v %v", got, err)
	}
	svc := event.NewService(e.events, authz.NewAuthorizer(authz.DefaultPolicy()))
	if public := svc.ProjectFor(nil, got); public.TicketCount != nil || public.WaitlistCount != nil || !public.Full {
		t.Fatalf("public %+v", public)
	}
	if editor := svc.ProjectFor(&weblabLead, got); editor.TicketCount == nil || *editor.TicketCount != 1 || *editor.WaitlistCount != 5 {
		t.Fatalf("editor counts %v %v", editor.TicketCount, editor.WaitlistCount)
	}

	got.Capacity = 4
	if _, err := e.events.Update(ctx, got); err != nil {
		t.Fatal(err)
	}
	for i, tk := range waiting {
		after, _ := e.store.Get(ctx, tk.ID)
		want := ticket.StatusWaitlisted
		if i < 3 {
			want = ticket.StatusActive
		}
		if after.StatusOf() != want {
			t.Fatalf("waiting %d is %s, want %s", i, after.Status, want)
		}
	}
	got.Capacity = 0
	if _, err := e.events.Update(ctx, got); err != nil {
		t.Fatal(err)
	}
	if counts := e.counts(t, ev.ID); counts[ticket.StatusActive] != 6 || counts[ticket.StatusWaitlisted] != 0 {
		t.Fatalf("unbounded: %v", counts)
	}
	if kinds := e.mailKinds(t, ev.ID); kinds["promoted"] != 5 {
		t.Fatalf("mails %v", kinds)
	}
}

// With the status mails off, a promoted Ticket gets "Biletin hazır" under a
// new resend number, and the waiting list queues nothing.
func TestPromotionMailWithoutTheStatusMailsOnPostgres(t *testing.T) {
	e := newCapacityPG(t)
	ctx := context.Background()
	e.store.NotifyStatus(false)
	ev := e.event(t, 1, true)
	first, err := e.store.CreateAdmitted(ctx, guestTicket(ev.ID, 0), ticket.Admission{})
	if err != nil {
		t.Fatal(err)
	}
	waiting, err := e.store.CreateAdmitted(ctx, guestTicket(ev.ID, 1), ticket.Admission{Waitlist: true})
	if err != nil || waiting.StatusOf() != ticket.StatusWaitlisted {
		t.Fatalf("%+v %v", waiting, err)
	}
	if kind := e.mailKind(t, waiting.ID); kind != "" {
		t.Fatalf("the waiting list queued %q with the status mails off", kind)
	}
	if _, _, err := e.store.Cancel(ctx, first.ID, ticket.CancelledByOperator, ""); err != nil {
		t.Fatal(err)
	}
	var kind string
	var resend, resends int
	if err := e.pool.QueryRow(ctx, `SELECT kind, resend, (SELECT count(*) FROM ticket_resends WHERE ticket_id = $1) FROM ticket_notifications WHERE ticket_id = $1`, waiting.ID).Scan(&kind, &resend, &resends); err != nil {
		t.Fatal(err)
	}
	if kind != "created" || resend != 1 || resends != 1 {
		t.Fatalf("promoted mail %s r%d (%d resends)", kind, resend, resends)
	}
}

// Once the Event has started, a cancellation frees no place for the list;
// an operator's "Öne al" still moves one, past the capacity.
func TestNoPromotionOnceStartedOnPostgres(t *testing.T) {
	e := newCapacityPG(t)
	ctx := context.Background()
	ev := e.event(t, 1, true)
	first, err := e.store.CreateAdmitted(ctx, guestTicket(ev.ID, 0), ticket.Admission{})
	if err != nil {
		t.Fatal(err)
	}
	waiting, err := e.store.CreateAdmitted(ctx, guestTicket(ev.ID, 1), ticket.Admission{Waitlist: true})
	if err != nil {
		t.Fatal(err)
	}
	second, err := e.store.CreateAdmitted(ctx, guestTicket(ev.ID, 2), ticket.Admission{Waitlist: true})
	if err != nil || second.StatusOf() != ticket.StatusWaitlisted || waiting.StatusOf() != ticket.StatusWaitlisted {
		t.Fatalf("%+v %+v %v", second, waiting, err)
	}
	if _, err := e.pool.Exec(ctx, `UPDATE events SET start_date = now() - interval '1 hour' WHERE id = $1`, ev.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.store.Cancel(ctx, first.ID, ticket.CancelledByOperator, ""); err != nil {
		t.Fatal(err)
	}
	if got := e.counts(t, ev.ID); got[ticket.StatusActive] != 0 || got[ticket.StatusWaitlisted] != 2 {
		t.Fatalf("promoted after the start: %v", got)
	}
	promoted, changed, err := e.store.Promote(ctx, waiting.ID)
	if err != nil || !changed || promoted.StatusOf() != ticket.StatusActive || promoted.OverCapacity {
		t.Fatalf("promote %+v %v %v", promoted, changed, err)
	}
	over, _, err := e.store.Promote(ctx, second.ID)
	if err != nil || !over.OverCapacity {
		t.Fatalf("over %+v %v", over, err)
	}
}
