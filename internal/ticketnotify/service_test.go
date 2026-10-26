package ticketnotify_test

import (
	"context"
	"io"
	"log"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/mail"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/ticketnotify"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// memQueue is the Queue in memory.
type memQueue struct {
	mu   sync.Mutex
	rows []ticketnotify.Queued
	due  map[uuid.UUID]time.Time
}

func (q *memQueue) add(id uuid.UUID, at time.Time) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.due == nil {
		q.due = map[uuid.UUID]time.Time{}
	}
	q.rows = append(q.rows, ticketnotify.Queued{TicketID: id, CreatedAt: at})
	q.due[id] = at
}

func (q *memQueue) Claim(_ context.Context, now time.Time, lease time.Duration, limit int) ([]ticketnotify.Queued, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	var out []ticketnotify.Queued
	for i := range q.rows {
		if len(out) == limit || q.due[q.rows[i].TicketID].After(now) {
			continue
		}
		q.rows[i].ClaimedAt = now
		q.due[q.rows[i].TicketID] = now.Add(lease)
		out = append(out, q.rows[i])
	}
	return out, nil
}

func (q *memQueue) Complete(_ context.Context, c ticketnotify.Queued) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.rows = slices.DeleteFunc(q.rows, func(r ticketnotify.Queued) bool { return r.TicketID == c.TicketID && r.ClaimedAt.Equal(c.ClaimedAt) })
	return nil
}

func (q *memQueue) Retry(_ context.Context, c ticketnotify.Queued, at time.Time) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i := range q.rows {
		if q.rows[i].TicketID == c.TicketID && q.rows[i].ClaimedAt.Equal(c.ClaimedAt) {
			q.rows[i].Attempts++
			q.rows[i].ClaimedAt = time.Time{}
			q.due[c.TicketID] = at
		}
	}
	return nil
}

func (q *memQueue) len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.rows)
}

type call struct {
	key    string
	holder mail.TicketHolder
	vars   map[string]string
	source mail.NotificationSource
	expiry *time.Time
}

type fakeMail struct {
	calls []call
	err   error
}

func (m *fakeMail) TicketCreated(_ context.Context, key string, holder mail.TicketHolder, vars map[string]string, source mail.NotificationSource, expiresAt *time.Time) error {
	m.calls = append(m.calls, call{key, holder, vars, source, expiresAt})
	return m.err
}

type tickets map[uuid.UUID]ticket.Ticket

func (t tickets) Get(_ context.Context, id uuid.UUID) (ticket.Ticket, error) {
	if got, ok := t[id]; ok {
		return got, nil
	}
	return ticket.Ticket{}, ticket.ErrNotFound
}

type events map[uuid.UUID]event.Event

func (e events) GetIncludingArchived(_ context.Context, id uuid.UUID) (event.Event, error) {
	if got, ok := e[id]; ok {
		return got, nil
	}
	return event.Event{}, event.ErrNotFound
}

type users map[uuid.UUID]user.User

func (u users) Get(_ context.Context, id uuid.UUID) (user.User, error) {
	if got, ok := u[id]; ok {
		return got, nil
	}
	return user.User{}, user.ErrNotFound
}

type env struct {
	now     time.Time
	queue   *memQueue
	mail    *fakeMail
	tickets tickets
	events  events
	users   users
	worker  *ticketnotify.Service
	ev      event.Event
}

func newEnv(t *testing.T) *env {
	t.Helper()
	now := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	start := time.Date(2026, 11, 10, 15, 30, 0, 0, time.UTC)
	end := start.Add(3 * time.Hour)
	ev := event.Event{ID: uuid.New(), Name: "Kodlama Gecesi", Location: "D-Blok", OwnerTeam: "WEBLAB", Status: event.StatusPublished, StartDate: &start, EndDate: &end}
	e := &env{now: now, queue: &memQueue{}, mail: &fakeMail{}, tickets: tickets{}, events: events{ev.ID: ev}, users: users{}, ev: ev}
	worker, err := ticketnotify.NewService(ticketnotify.Config{
		Queue: e.queue, Tickets: e.tickets, Events: e.events, Users: e.users, Mail: e.mail,
		Now:    func() time.Time { return e.now },
		Logger: log.New(io.Discard, "", 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	e.worker = worker
	return e
}

func (e *env) member(state user.AccountState) ticket.Ticket {
	id := uuid.New()
	e.users[id] = user.User{ID: id, Email: "ayse@example.com", FirstName: "Ayşe", AccountState: state}
	t := ticket.Ticket{ID: uuid.New(), EventID: e.ev.ID, TicketType: ticket.Registered, OwnerID: &id}
	e.tickets[t.ID] = t
	e.queue.add(t.ID, e.now)
	return t
}

func (e *env) guest(email string) ticket.Ticket {
	t := ticket.Ticket{ID: uuid.New(), EventID: e.ev.ID, TicketType: ticket.Guest, GuestFirstName: " Can ", GuestLastName: "Er", GuestEmail: email}
	e.tickets[t.ID] = t
	e.queue.add(t.ID, e.now)
	return t
}

func (e *env) pass(t *testing.T) ticketnotify.Report {
	t.Helper()
	report, err := e.worker.Pass(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return report
}

func TestMemberGetsOneNotificationBySubject(t *testing.T) {
	e := newEnv(t)
	tk := e.member(user.AccountActive)
	if r := e.pass(t); r.Sent != 1 {
		t.Fatalf("report %+v, want one sent", r)
	}
	if len(e.mail.calls) != 1 {
		t.Fatalf("calls %d, want 1", len(e.mail.calls))
	}
	c := e.mail.calls[0]
	if c.holder != (mail.TicketHolder{Sub: *tk.OwnerID}) {
		t.Fatalf("holder %+v, want the owner's subject alone", c.holder)
	}
	if c.key != ticketnotify.IdempotencyKey(tk.ID) || c.key != "core:event.ticket-created:"+tk.ID.String()+":v1" {
		t.Fatalf("key %q", c.key)
	}
	if c.source != (mail.NotificationSource{Kind: "ticket", Ref: tk.ID.String()}) {
		t.Fatalf("source %+v", c.source)
	}
	want := map[string]string{"EventId": e.ev.ID.String(), "EventName": "Kodlama Gecesi", "Venue": "D-Blok", "OwnerTeam": "WEBLAB", "EventDate": "10.11.2026 18:30"}
	for k, v := range want {
		if c.vars[k] != v {
			t.Fatalf("vars[%s] = %q, want %q (%v)", k, c.vars[k], v, c.vars)
		}
	}
	if len(c.vars) != len(want) {
		t.Fatalf("vars %v, want exactly %v", c.vars, want)
	}
	if c.expiry == nil || !c.expiry.Equal(*e.ev.EndDate) {
		t.Fatalf("expiry %v, want the Event's end", c.expiry)
	}
	if e.queue.len() != 0 {
		t.Fatal("the row stays after the send")
	}
}

func TestGuestGetsTheMailByAddress(t *testing.T) {
	e := newEnv(t)
	e.guest(" Can@Example.com ")
	e.pass(t)
	if len(e.mail.calls) != 1 {
		t.Fatalf("calls %d", len(e.mail.calls))
	}
	if h := e.mail.calls[0].holder; h != (mail.TicketHolder{Email: "can@example.com", FullName: "Can Er"}) {
		t.Fatalf("holder %+v", h)
	}
}

// SkyMail renders the inbox text with missingkey=error: an Event without a
// start still sends EventDate, empty.
func TestEventWithoutAStartSendsAnEmptyDate(t *testing.T) {
	e := newEnv(t)
	ev := e.ev
	ev.StartDate = nil
	e.events[ev.ID] = ev
	e.member(user.AccountActive)
	e.pass(t)
	if got, ok := e.mail.calls[0].vars["EventDate"]; !ok || got != "" {
		t.Fatalf("EventDate %q (present %v), want present and empty", got, ok)
	}
}

func TestNobodyToTellIsDropped(t *testing.T) {
	e := newEnv(t)
	e.member(user.AccountDeletionPending)
	e.guest("")
	gone := e.member(user.AccountActive)
	delete(e.users, *gone.OwnerID)
	missing := uuid.New()
	e.queue.add(missing, e.now)
	if r := e.pass(t); r.Dropped != 4 || r.Sent != 0 {
		t.Fatalf("report %+v, want 4 dropped", r)
	}
	if len(e.mail.calls) != 0 || e.queue.len() != 0 {
		t.Fatalf("calls %d, rows %d", len(e.mail.calls), e.queue.len())
	}
}

func TestOnlyAPublishedEventNotOverIsToldOf(t *testing.T) {
	for name, change := range map[string]func(*event.Event){
		"draft":     func(ev *event.Event) { ev.Status = event.StatusDraft },
		"cancelled": func(ev *event.Event) { ev.Status = event.StatusCancelled },
		"archived":  func(ev *event.Event) { at := time.Now(); ev.ArchivedAt = &at },
		"over":      func(ev *event.Event) { past := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC); ev.EndDate = &past },
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			ev := e.ev
			change(&ev)
			e.events[ev.ID] = ev
			e.member(user.AccountActive)
			if r := e.pass(t); r.Dropped != 1 || len(e.mail.calls) != 0 {
				t.Fatalf("report %+v, calls %d", r, len(e.mail.calls))
			}
		})
	}
}

func TestFailureIsRetriedUnderTheSameKey(t *testing.T) {
	e := newEnv(t)
	tk := e.member(user.AccountActive)
	e.mail.err = &mail.SendError{Status: 503}
	if r := e.pass(t); r.Retried != 1 {
		t.Fatalf("report %+v, want retried", r)
	}
	if e.queue.len() != 1 {
		t.Fatal("a failed row must stay")
	}
	// Not due before its backoff.
	if r := e.pass(t); r != (ticketnotify.Report{}) {
		t.Fatalf("report %+v before the backoff", r)
	}
	e.now = e.now.Add(ticketnotify.RetryFirst)
	e.mail.err = nil
	if r := e.pass(t); r.Sent != 1 {
		t.Fatalf("report %+v, want sent", r)
	}
	if len(e.mail.calls) != 2 || e.mail.calls[0].key != e.mail.calls[1].key || e.mail.calls[1].key != ticketnotify.IdempotencyKey(tk.ID) {
		t.Fatalf("calls %+v, want the same key twice", e.mail.calls)
	}
}

func TestRefusalIsPermanentAndOldFailureGivesUp(t *testing.T) {
	e := newEnv(t)
	e.member(user.AccountActive)
	e.mail.err = &mail.SendError{Status: 422}
	if r := e.pass(t); r.Rejected != 1 || e.queue.len() != 0 {
		t.Fatalf("report %+v, rows %d: a 422 is not tried again", r, e.queue.len())
	}

	e = newEnv(t)
	e.member(user.AccountActive)
	e.mail.err = &mail.SendError{Status: 409}
	e.now = e.now.Add(ticketnotify.GiveUpAfter + time.Minute)
	if r := e.pass(t); r.Expired != 1 || e.queue.len() != 0 || len(e.mail.calls) != 0 {
		t.Fatalf("report %+v, rows %d, calls %d: a day-old row is given up unsent", r, e.queue.len(), len(e.mail.calls))
	}
}

// A guest's mail that went out without an answer is not tried again: /single
// reads no idempotency key, and a second try could mail the guest twice.
func TestUnansweredGuestMailIsNotTriedAgain(t *testing.T) {
	e := newEnv(t)
	e.guest("can@example.com")
	e.mail.err = mail.ErrMaybeDelivered
	if r := e.pass(t); r.Uncertain != 1 || r.Retried != 0 || e.queue.len() != 0 {
		t.Fatalf("report %+v, rows %d", r, e.queue.len())
	}
	e.now = e.now.Add(time.Hour)
	e.pass(t)
	if len(e.mail.calls) != 1 {
		t.Fatalf("calls %d, want one", len(e.mail.calls))
	}
}
