package ticket_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// staleTicketReads answers the first two Ticket lookups as if no Ticket
// existed, and holds each until both have looked: two reports of one answer
// at the same moment both find nothing and both write.
type staleTicketReads struct {
	*ticket.PostgresStore
	reads   atomic.Int32
	arrived sync.WaitGroup
}

func newStaleTicketReads(store *ticket.PostgresStore) *staleTicketReads {
	s := &staleTicketReads{PostgresStore: store}
	s.arrived.Add(2)
	return s
}

func (s *staleTicketReads) stale() bool {
	if s.reads.Add(1) > 2 {
		return false
	}
	s.arrived.Done()
	s.arrived.Wait()
	return true
}

func (s *staleTicketReads) ExistsOwnerEvent(ctx context.Context, ownerID, eventID uuid.UUID) (bool, error) {
	if s.stale() {
		return false, nil
	}
	return s.PostgresStore.ExistsOwnerEvent(ctx, ownerID, eventID)
}

func (s *staleTicketReads) GetByGuestEvent(ctx context.Context, email string, eventID uuid.UUID) (ticket.Ticket, error) {
	if s.stale() {
		return ticket.Ticket{}, ticket.ErrNotFound
	}
	return s.PostgresStore.GetByGuestEvent(ctx, email, eventID)
}

type formResponsePostgres struct {
	pool   *pgxpool.Pool
	events event.Store
	users  *user.PostgresStore
	dir    *identity.Memory
}

func startFormResponsePostgres(t *testing.T) formResponsePostgres {
	t.Helper()
	pool := testpostgres.Start(t)
	if err := migrate.Apply(context.Background(), pool); err != nil {
		t.Fatal(err)
	}
	return formResponsePostgres{
		pool: pool, events: event.NewPostgresStore(pool), users: user.NewPostgresStore(pool), dir: identity.NewMemory(),
	}
}

func (f formResponsePostgres) service(tickets ticket.Store) ticket.Service {
	return ticket.NewService(tickets, f.events, authz.NewAuthorizer(authz.DefaultPolicy()), f.users, f.dir)
}

func (f formResponsePostgres) person(t *testing.T, email string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, _, err := user.NewService(f.users).Ensure(context.Background(), id, user.Profile{
		Email: email, FirstName: "Ada", LastName: "Lovelace",
	}); err != nil {
		t.Fatal(err)
	}
	return id
}

func (f formResponsePostgres) ticketCount(t *testing.T, eventID uuid.UUID) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM tickets WHERE event_id = $1`, eventID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Two reports of one answer at the same moment leave one Ticket, and both
// are answered as done: the one that loses the race meets the unique index
// (ErrConflict), which the memory store never reaches.
func TestRecordFormResponseTwiceAtOnceIsOneTicket(t *testing.T) {
	f := startFormResponsePostgres(t)
	ctx := context.Background()
	member := f.person(t, "member@example.com")

	for name, report := range map[string]func(formID uuid.UUID) ticket.FormResponse{
		"signed in": func(formID uuid.UUID) ticket.FormResponse {
			return ticket.FormResponse{FormID: formID, ResponseID: uuid.New(), Status: ticket.FormResponseAccepted, UserID: &member}
		},
		"guest": func(formID uuid.UUID) ticket.FormResponse {
			return acceptedGuest(formID, "Ada", "Lovelace", "ada@example.com")
		},
	} {
		formID := uuid.New()
		ev := seedFormEvent(t, f.events, formAddress(formID))
		store := newStaleTicketReads(ticket.NewPostgresStore(f.pool))
		svc := f.service(store)
		sent := report(formID)

		var wg sync.WaitGroup
		results := make([]ticket.FormResponseResult, 2)
		errs := make([]error, 2)
		for i := range 2 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				results[i], errs[i] = svc.RecordFormResponse(ctx, formsService, sent)
			}()
		}
		wg.Wait()

		for i, err := range errs {
			if err != nil {
				t.Fatalf("%s: report %d: %v", name, i, err)
			}
		}
		if written := results[0].TicketsWritten + results[1].TicketsWritten; written != 1 {
			t.Errorf("%s: results %+v, want one Ticket written", name, results)
		}
		if n := f.ticketCount(t, ev.ID); n != 1 {
			t.Errorf("%s: %d Tickets, want 1", name, n)
		}
	}
}

// A person being erased, or whose account is no longer active, gets no
// Ticket, and the report is answered as done so the forms outbox stops
// sending it. The database refuses the Ticket (ErrNotFound) or the person's
// core row (user.ErrAccountBlocked), which the memory store never does.
func TestRecordFormResponseGivesNoTicketToAPersonCoreBlocks(t *testing.T) {
	f := startFormResponsePostgres(t)
	ctx := context.Background()

	marked := f.person(t, "marked@example.com")
	if _, err := f.users.RequestDeletion(ctx, marked, &marked); err != nil {
		t.Fatal(err)
	}
	markedInDirectory := uuid.New()
	f.dir.PutUser(identity.Person{ID: markedInDirectory, Email: "directory@example.com", FirstName: "Ada", LastName: "Lovelace"})
	if _, err := f.pool.Exec(ctx, `INSERT INTO account_deletion_requests (id, subject_id) VALUES ($1, $2)`, uuid.New(), markedInDirectory); err != nil {
		t.Fatal(err)
	}
	inactive := f.person(t, "inactive@example.com")
	if _, err := f.pool.Exec(ctx, `UPDATE users SET account_state = 'anonymized' WHERE id = $1`, inactive); err != nil {
		t.Fatal(err)
	}

	for name, person := range map[string]uuid.UUID{
		"deletion marker":                 marked,
		"deletion marker, directory only": markedInDirectory,
		"account no longer active":        inactive,
	} {
		formID := uuid.New()
		ev := seedFormEvent(t, f.events, formAddress(formID))
		got, err := f.service(ticket.NewPostgresStore(f.pool)).RecordFormResponse(ctx, formsService, ticket.FormResponse{
			FormID: formID, ResponseID: uuid.New(), Status: ticket.FormResponseAccepted, UserID: &person,
		})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got.TicketsWritten != 0 {
			t.Errorf("%s: result %+v", name, got)
		}
		if n := f.ticketCount(t, ev.ID); n != 0 {
			t.Errorf("%s: %d Tickets, want 0", name, n)
		}
	}
	if _, err := f.users.Get(ctx, markedInDirectory); err == nil {
		t.Fatal("a core row was written for a person with a deletion marker")
	}
}
