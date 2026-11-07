package eventupdatenotify_test

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/eventupdatenotify"
	"github.com/skylab-kulubu/core-backend/internal/mail"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// memQueue is the Queue in memory.
type memQueue struct {
	mu    sync.Mutex
	rows  []eventupdatenotify.Queued
	due   map[uuid.UUID]time.Time
	sends map[uuid.UUID][]uuid.UUID
}

func (q *memQueue) add(row eventupdatenotify.Queued, due time.Time) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.due == nil {
		q.due, q.sends = map[uuid.UUID]time.Time{}, map[uuid.UUID][]uuid.UUID{}
	}
	q.rows = append(q.rows, row)
	q.due[row.ID] = due
}

func (q *memQueue) Claim(_ context.Context, now time.Time, lease time.Duration) (eventupdatenotify.Queued, bool, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i := range q.rows {
		if q.due[q.rows[i].ID].After(now) {
			continue
		}
		q.rows[i].ClaimedAt = now
		q.due[q.rows[i].ID] = now.Add(lease)
		return q.rows[i], true, nil
	}
	return eventupdatenotify.Queued{}, false, nil
}

func (q *memQueue) Renew(_ context.Context, c eventupdatenotify.Queued, until time.Time) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.due[c.ID] = until
	return nil
}

func (q *memQueue) Sends(_ context.Context, id uuid.UUID) ([]uuid.UUID, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return slices.Clone(q.sends[id]), nil
}

func (q *memQueue) RecordSend(_ context.Context, id, ticketID uuid.UUID) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if !slices.Contains(q.sends[id], ticketID) {
		q.sends[id] = append(q.sends[id], ticketID)
	}
	return nil
}

func (q *memQueue) Complete(_ context.Context, c eventupdatenotify.Queued) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.rows = slices.DeleteFunc(q.rows, func(r eventupdatenotify.Queued) bool { return r.ID == c.ID && r.ClaimedAt.Equal(c.ClaimedAt) })
	return nil
}

func (q *memQueue) Retry(_ context.Context, c eventupdatenotify.Queued, at time.Time) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i := range q.rows {
		if q.rows[i].ID == c.ID && q.rows[i].ClaimedAt.Equal(c.ClaimedAt) {
			q.rows[i].Attempts++
			q.rows[i].ClaimedAt = time.Time{}
			q.due[c.ID] = at
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

// fakeMail answers err to the holders in fail, nil to the rest.
type fakeMail struct {
	calls []call
	fail  map[string]error
}

func (m *fakeMail) EventUpdated(_ context.Context, key string, holder mail.TicketHolder, vars map[string]string, source mail.NotificationSource, expiresAt *time.Time) error {
	m.calls = append(m.calls, call{key, holder, vars, source, expiresAt})
	name := holder.Email
	if holder.Sub != uuid.Nil {
		name = holder.Sub.String()
	}
	return m.fail[name]
}

type tickets []ticket.Ticket

func (t tickets) ListByEvent(_ context.Context, eventID uuid.UUID) ([]ticket.Ticket, error) {
	var out []ticket.Ticket
	for _, tk := range t {
		if tk.EventID == eventID {
			out = append(out, tk)
		}
	}
	return out, nil
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

var now = time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)

func at(t time.Time) *time.Time { return &t }

type fixture struct {
	queue   *memQueue
	mail    *fakeMail
	svc     *eventupdatenotify.Service
	ev      event.Event
	member  uuid.UUID
	tickets tickets
}

// setup is a published Event moved a day later and to another hall, with a
// member's Ticket, a guest's, a guest Ticket under the member's own address,
// an erased member's and a guest's without an address.
func setup(t *testing.T) *fixture {
	t.Helper()
	ev := event.Event{
		ID: uuid.New(), Name: "Gece Kodu", Location: "Kongre Merkezi", Status: event.StatusPublished,
		StartDate: at(time.Date(2026, 11, 20, 15, 30, 0, 0, time.UTC)), EndDate: at(time.Date(2026, 11, 20, 18, 0, 0, 0, time.UTC)),
	}
	member, erased := uuid.New(), uuid.New()
	tks := tickets{
		{ID: uuid.New(), EventID: ev.ID, TicketType: ticket.Registered, OwnerID: &member},
		{ID: uuid.New(), EventID: ev.ID, TicketType: ticket.Guest, GuestEmail: " Can@Example.com ", GuestFirstName: "Can", GuestLastName: "Er"},
		{ID: uuid.New(), EventID: ev.ID, TicketType: ticket.Guest, GuestEmail: "ayse@example.com", GuestFirstName: "Ayşe"},
		{ID: uuid.New(), EventID: ev.ID, TicketType: ticket.Registered, OwnerID: &erased},
		{ID: uuid.New(), EventID: ev.ID, TicketType: ticket.Guest},
	}
	us := users{
		member: {ID: member, Email: "Ayse@Example.com", AccountState: user.AccountActive},
		erased: {ID: erased, AccountState: user.AccountDeletionPending},
	}
	f := &fixture{queue: &memQueue{}, mail: &fakeMail{}, ev: ev, member: member, tickets: tks}
	svc, err := eventupdatenotify.NewService(eventupdatenotify.Config{
		Queue: f.queue, Tickets: tks, Events: events{ev.ID: ev}, Users: us, Mail: f.mail,
		Now: func() time.Time { return now }, Logger: log.New(io.Discard, "", 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	f.svc = svc
	return f
}

// queue1 queues a row for f's Event whose before is the Event a day earlier
// in another hall, unless mutate changes it.
func (f *fixture) queue1(mutate func(*eventupdatenotify.Queued)) eventupdatenotify.Queued {
	row := eventupdatenotify.Queued{
		ID: uuid.New(), EventID: f.ev.ID, CreatedAt: now.Add(-event.UpdateNotificationWindow),
		BeforeStart: at(f.ev.StartDate.Add(-24 * time.Hour)), BeforeEnd: at(f.ev.EndDate.Add(-24 * time.Hour)), BeforeLocation: "D-Blok",
	}
	if mutate != nil {
		mutate(&row)
	}
	f.queue.add(row, now)
	return row
}

func TestAChangeTellsEachHolderOnceAndIsDoneWith(t *testing.T) {
	f := setup(t)
	row := f.queue1(nil)
	report, err := f.svc.Pass(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// The member (with the guest Ticket under their address) and Can; the
	// erased member and the guest without an address are skipped.
	if report.Changes != 1 || report.Sent != 2 || report.Skipped != 2 || f.queue.len() != 0 {
		t.Fatalf("report %+v, rows %d", report, f.queue.len())
	}
	member, guest := f.mail.calls[0], f.mail.calls[1]
	if member.holder != (mail.TicketHolder{Sub: f.member}) || member.key != "core:event.updated:"+row.ID.String()+":"+f.member.String()+":v1" {
		t.Fatalf("member call %+v", member)
	}
	if guest.holder != (mail.TicketHolder{Email: "can@example.com", FullName: "Can Er"}) {
		t.Fatalf("guest call %+v", guest)
	}
	want := map[string]string{
		"EventId": f.ev.ID.String(), "EventName": "Gece Kodu", "ChangeSummary": "Etkinliğin saati ve yeri değişti.",
		"NewStartsAt": "20.11.2026 18:30", "NewEndsAt": "20.11.2026 21:00", "NewVenue": "Kongre Merkezi",
	}
	for name, value := range want {
		if member.vars[name] != value {
			t.Fatalf("%s = %q, want %q (vars %v)", name, member.vars[name], value, member.vars)
		}
	}
	if len(member.vars) != len(want) {
		t.Fatalf("vars %v, want exactly %v", member.vars, want)
	}
	if member.source != (mail.NotificationSource{Kind: "event", Ref: f.ev.ID.String()}) || member.expiry == nil || !member.expiry.Equal(*f.ev.EndDate) {
		t.Fatalf("source %+v, expiry %v", member.source, member.expiry)
	}
	// Every Ticket a Notification reached is recorded: the member's two
	// and Can's.
	if got := f.queue.sends[row.ID]; len(got) != 3 || !slices.Contains(got, f.tickets[2].ID) {
		t.Fatalf("sends %v", got)
	}
}

func TestTheSummarySaysWhatChanged(t *testing.T) {
	for name, tc := range map[string]struct {
		before func(ev event.Event, r *eventupdatenotify.Queued)
		want   string
	}{
		"place": {func(ev event.Event, r *eventupdatenotify.Queued) {
			r.BeforeStart, r.BeforeEnd = ev.StartDate, ev.EndDate
		}, "Etkinliğin yeri değişti."},
		"time": {func(_ event.Event, r *eventupdatenotify.Queued) { r.BeforeLocation = " Kongre Merkezi " }, "Etkinliğin saati değişti."},
		"end": {func(ev event.Event, r *eventupdatenotify.Queued) {
			r.BeforeStart, r.BeforeLocation = ev.StartDate, ev.Location
		}, "Etkinliğin saati değişti."},
	} {
		t.Run(name, func(t *testing.T) {
			f := setup(t)
			f.queue1(func(r *eventupdatenotify.Queued) { tc.before(f.ev, r) })
			if _, err := f.svc.Pass(context.Background()); err != nil {
				t.Fatal(err)
			}
			if got := f.mail.calls[0].vars["ChangeSummary"]; got != tc.want {
				t.Fatalf("ChangeSummary %q, want %q", got, tc.want)
			}
		})
	}
}

func TestNothingIsToldOfAnEventNoLongerNewsOrBackAsItWas(t *testing.T) {
	for name, mutate := range map[string]func(*event.Event, *eventupdatenotify.Queued){
		"undone within the window": func(ev *event.Event, r *eventupdatenotify.Queued) {
			r.BeforeStart, r.BeforeEnd, r.BeforeLocation = ev.StartDate, ev.EndDate, ev.Location
		},
		"cancelled": func(ev *event.Event, _ *eventupdatenotify.Queued) { ev.Status = event.StatusCancelled },
		"a draft":   func(ev *event.Event, _ *eventupdatenotify.Queued) { ev.Status = event.StatusDraft },
		"archived":  func(ev *event.Event, _ *eventupdatenotify.Queued) { ev.ArchivedAt = at(now) },
		"over":      func(ev *event.Event, _ *eventupdatenotify.Queued) { ev.EndDate = at(now.Add(-time.Minute)) },
		"gone":      func(ev *event.Event, _ *eventupdatenotify.Queued) { ev.ID = uuid.New() },
	} {
		t.Run(name, func(t *testing.T) {
			f := setup(t)
			ev := f.ev
			row := eventupdatenotify.Queued{
				ID: uuid.New(), EventID: f.ev.ID, CreatedAt: now.Add(-event.UpdateNotificationWindow),
				BeforeStart: at(f.ev.StartDate.Add(-time.Hour)), BeforeEnd: f.ev.EndDate, BeforeLocation: f.ev.Location,
			}
			mutate(&ev, &row)
			f.queue.add(row, now)
			svc, err := eventupdatenotify.NewService(eventupdatenotify.Config{
				Queue: f.queue, Tickets: f.tickets, Events: events{ev.ID: ev}, Users: users{}, Mail: f.mail,
				Now: func() time.Time { return now }, Logger: log.New(io.Discard, "", 0),
			})
			if err != nil {
				t.Fatal(err)
			}
			report, err := svc.Pass(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if report.Dropped != 1 || report.Changes != 1 || len(f.mail.calls) != 0 || f.queue.len() != 0 {
				t.Fatalf("report %+v, calls %d, rows %d", report, len(f.mail.calls), f.queue.len())
			}
		})
	}
}

func TestAFailureKeepsTheRowAndTheRetryTellsOnlyWhoWasNotReached(t *testing.T) {
	f := setup(t)
	row := f.queue1(nil)
	f.mail.fail = map[string]error{"can@example.com": &mail.SendError{Status: http.StatusServiceUnavailable}}
	report, err := f.svc.Pass(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Retried != 1 || report.Sent != 1 || f.queue.len() != 1 {
		t.Fatalf("report %+v, rows %d", report, f.queue.len())
	}
	if f.queue.rows[0].Attempts != 1 || !f.queue.due[row.ID].Equal(now.Add(eventupdatenotify.RetryFirst)) {
		t.Fatalf("row %+v due %v", f.queue.rows[0], f.queue.due[row.ID])
	}
	// Due again: only Can is told; the member, reached, is not told twice.
	f.queue.due[row.ID] = now
	f.mail.fail, f.mail.calls = nil, nil
	report, err = f.svc.Pass(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Sent != 1 || len(f.mail.calls) != 1 || f.mail.calls[0].holder.Email != "can@example.com" || f.queue.len() != 0 {
		t.Fatalf("report %+v, calls %+v", report, f.mail.calls)
	}
}

func TestARefusalOrAnUnansweredGuestMailIsNotTriedAgain(t *testing.T) {
	f := setup(t)
	row := f.queue1(nil)
	f.mail.fail = map[string]error{
		f.member.String(): &mail.SendError{Status: http.StatusUnprocessableEntity},
		"can@example.com": mail.ErrMaybeDelivered,
	}
	report, err := f.svc.Pass(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Rejected != 1 || report.Uncertain != 1 || report.Retried != 0 || f.queue.len() != 0 {
		t.Fatalf("report %+v, rows %d", report, f.queue.len())
	}
	if got := f.queue.sends[row.ID]; len(got) != 1 || got[0] != f.tickets[1].ID {
		t.Fatalf("sends %v: the unanswered guest counts as reached, the refused member does not", got)
	}
}

func TestAChangeTriedForADayIsGivenUp(t *testing.T) {
	f := setup(t)
	f.queue1(func(r *eventupdatenotify.Queued) { r.CreatedAt = now.Add(-eventupdatenotify.GiveUpAfter - time.Second) })
	report, err := f.svc.Pass(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Expired != 1 || len(f.mail.calls) != 0 || f.queue.len() != 0 {
		t.Fatalf("report %+v", report)
	}
}

func TestStoppingCoreLeavesTheRowToItsLease(t *testing.T) {
	f := setup(t)
	f.queue1(nil)
	ctx, cancel := context.WithCancel(context.Background())
	f.mail.fail = map[string]error{f.member.String(): errors.New("stopping")}
	cancel()
	if _, err := f.svc.Pass(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v", err)
	}
	if f.queue.len() != 1 || f.queue.rows[0].Attempts != 0 {
		t.Fatalf("row %+v: kept, no failure counted", f.queue.rows)
	}
}
