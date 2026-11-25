package doorstaffmail_test

import (
	"context"
	"io"
	"log"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/doorstaffmail"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/mail"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

var now = time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)

type memQueue struct {
	rows      []doorstaffmail.Queued
	due       map[uuid.UUID]time.Time
	completed []uuid.UUID
	retried   map[uuid.UUID]bool
}

func (q *memQueue) Claim(_ context.Context, at time.Time, lease time.Duration) (doorstaffmail.Queued, bool, error) {
	for i, r := range q.rows {
		if !q.due[r.ID].After(at) {
			q.due[r.ID] = at.Add(lease)
			q.rows[i].ClaimedAt = at
			return q.rows[i], true, nil
		}
	}
	return doorstaffmail.Queued{}, false, nil
}

func (q *memQueue) Complete(_ context.Context, c doorstaffmail.Queued) error {
	q.completed = append(q.completed, c.ID)
	q.remove(c.ID)
	return nil
}

func (q *memQueue) Retry(_ context.Context, c doorstaffmail.Queued, at time.Time, failed bool) error {
	q.due[c.ID] = at
	q.retried[c.ID] = failed
	return nil
}

func (q *memQueue) remove(id uuid.UUID) {
	for i, r := range q.rows {
		if r.ID == id {
			q.rows = append(q.rows[:i], q.rows[i+1:]...)
			return
		}
	}
}

type call struct {
	key  string
	sub  uuid.UUID
	vars map[string]string
}

type fakeMail struct {
	calls []call
	err   error
}

func (m *fakeMail) DoorStaffAdded(_ context.Context, key string, sub uuid.UUID, vars map[string]string, _ mail.NotificationSource, _ *time.Time) error {
	m.calls = append(m.calls, call{key, sub, vars})
	return m.err
}

type events map[uuid.UUID]event.Event

func (e events) GetIncludingArchived(_ context.Context, id uuid.UUID) (event.Event, error) {
	ev, ok := e[id]
	if !ok {
		return event.Event{}, event.ErrNotFound
	}
	return ev, nil
}

type users map[uuid.UUID]user.User

func (u users) Get(_ context.Context, id uuid.UUID) (user.User, error) {
	got, ok := u[id]
	if !ok {
		return user.User{}, user.ErrNotFound
	}
	return got, nil
}

func at(t time.Time) *time.Time { return &t }

type fixture struct {
	queue  *memQueue
	mail   *fakeMail
	ev     event.Event
	staff  uuid.UUID
	events events
	users  users
}

func setup() *fixture {
	staff := uuid.New()
	ev := event.Event{
		ID: uuid.New(), Name: "Gece Kodu", Location: " D-Blok ", Status: event.StatusPublished,
		StartDate: at(time.Date(2026, 12, 4, 15, 0, 0, 0, time.UTC)), EndDate: at(time.Date(2026, 12, 4, 18, 0, 0, 0, time.UTC)),
		DoorStaffIDs: []uuid.UUID{staff},
	}
	return &fixture{
		queue: &memQueue{due: map[uuid.UUID]time.Time{}, retried: map[uuid.UUID]bool{}},
		mail:  &fakeMail{}, ev: ev, staff: staff,
		events: events{ev.ID: ev},
		users:  users{staff: {ID: staff, FirstName: " Ada ", AccountState: user.AccountActive}},
	}
}

func (f *fixture) add(created time.Time) doorstaffmail.Queued {
	row := doorstaffmail.Queued{ID: uuid.New(), EventID: f.ev.ID, UserID: f.staff, CreatedAt: created}
	f.queue.rows = append(f.queue.rows, row)
	f.queue.due[row.ID] = now
	return row
}

func (f *fixture) pass(t *testing.T, panel string) doorstaffmail.Report {
	t.Helper()
	svc, err := doorstaffmail.NewService(doorstaffmail.Config{
		Queue: f.queue, Events: f.events, Users: f.users, Mail: f.mail, PanelURL: panel,
		Now: func() time.Time { return now }, Logger: log.New(io.Discard, "", 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	report, err := svc.Pass(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return report
}

func TestTheAddedPersonIsToldOnce(t *testing.T) {
	f := setup()
	row := f.add(now.Add(-2 * time.Minute))
	report := f.pass(t, "https://panel.example/")
	if report.Sent != 1 || len(f.mail.calls) != 1 || len(f.queue.completed) != 1 {
		t.Fatalf("report %+v, calls %d", report, len(f.mail.calls))
	}
	c := f.mail.calls[0]
	want := map[string]string{
		"FirstName": "Ada", "EventId": f.ev.ID.String(), "EventName": "Gece Kodu",
		"StartsAt": "04.12.2026 18:00", "EndsAt": "04.12.2026 21:00", "Venue": "D-Blok",
		"DoorUrl": "https://panel.example/events/" + f.ev.ID.String() + "/door",
	}
	if c.sub != f.staff || c.key != "core:event.door-staff-added:"+row.ID.String()+":v1" || len(c.vars) != len(want) {
		t.Fatalf("call %+v", c)
	}
	for k, v := range want {
		if c.vars[k] != v {
			t.Fatalf("%s = %q, want %q", k, c.vars[k], v)
		}
	}
}

func TestNoPanelURLSendsAnEmptyDoorLink(t *testing.T) {
	f := setup()
	f.add(now)
	f.pass(t, "")
	if got, ok := f.mail.calls[0].vars["DoorUrl"]; !ok || got != "" {
		t.Fatalf("DoorUrl %q %v", got, ok)
	}
}

func TestNobodyIsToldWhenNoLongerNews(t *testing.T) {
	for name, mutate := range map[string]func(*fixture){
		"taken off":    func(f *fixture) { ev := f.ev; ev.DoorStaffIDs = nil; f.events[ev.ID] = ev },
		"cancelled":    func(f *fixture) { ev := f.ev; ev.Status = event.StatusCancelled; f.events[ev.ID] = ev },
		"archived":     func(f *fixture) { ev := f.ev; ev.ArchivedAt = at(now); f.events[ev.ID] = ev },
		"over":         func(f *fixture) { ev := f.ev; ev.EndDate = at(now.Add(-time.Minute)); f.events[ev.ID] = ev },
		"gone":         func(f *fixture) { delete(f.events, f.ev.ID) },
		"erased":       func(f *fixture) { f.users[f.staff] = user.User{ID: f.staff, AccountState: user.AccountAnonymized} },
		"no such user": func(f *fixture) { delete(f.users, f.staff) },
	} {
		t.Run(name, func(t *testing.T) {
			f := setup()
			mutate(f)
			f.add(now)
			report := f.pass(t, "")
			if report.Dropped != 1 || len(f.mail.calls) != 0 || len(f.queue.completed) != 1 {
				t.Fatalf("report %+v, calls %d", report, len(f.mail.calls))
			}
		})
	}
}

func TestADraftsDoorStaffWaitsForThePublication(t *testing.T) {
	f := setup()
	ev := f.ev
	ev.Status = event.StatusDraft
	f.events[ev.ID] = ev
	row := f.add(now)
	report := f.pass(t, "")
	if report.Waiting != 1 || len(f.mail.calls) != 0 || len(f.queue.completed) != 0 {
		t.Fatalf("report %+v", report)
	}
	if !f.queue.due[row.ID].Equal(now.Add(doorstaffmail.DraftWait)) || f.queue.retried[row.ID] {
		t.Fatalf("due %v, counted as failure %v", f.queue.due[row.ID], f.queue.retried[row.ID])
	}
	// Given up 30 days after the person was added.
	g := setup()
	g.events[g.ev.ID] = ev
	g.ev = ev
	g.add(now.Add(-doorstaffmail.GiveUpAfter - time.Minute))
	if report := g.pass(t, ""); report.Expired != 1 || len(g.queue.completed) != 1 {
		t.Fatalf("report %+v", report)
	}
}

func TestAFailureIsTriedAgainAndARefusalIsDone(t *testing.T) {
	f := setup()
	f.mail.err = &mail.SendError{Status: http.StatusServiceUnavailable}
	row := f.add(now)
	if report := f.pass(t, ""); report.Retried != 1 || !f.queue.retried[row.ID] || !f.queue.due[row.ID].Equal(now.Add(doorstaffmail.RetryFirst)) {
		t.Fatalf("report %+v, due %v", report, f.queue.due[row.ID])
	}

	g := setup()
	g.mail.err = &mail.SendError{Status: http.StatusUnprocessableEntity, Code: "notification.type_unknown"}
	g.add(now)
	if report := g.pass(t, ""); report.Retried != 1 || len(g.queue.completed) != 0 {
		t.Fatalf("an unseeded type: report %+v", report)
	}

	h := setup()
	h.mail.err = &mail.SendError{Status: http.StatusBadRequest}
	h.add(now)
	if report := h.pass(t, ""); report.Rejected != 1 || len(h.queue.completed) != 1 {
		t.Fatalf("a refusal: report %+v", report)
	}
}
