package eventreminder_test

import (
	"context"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/eventreminder"
	"github.com/skylab-kulubu/core-backend/internal/mail"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// memQueue is the Queue in memory. Plan records the windows it was asked
// for and queues the rows in toPlan; Claim keeps the per-minute bound by the
// rows it handed out (sends) in the minute before now.
type memQueue struct {
	mu      sync.Mutex
	windows []eventreminder.Window
	toPlan  []eventreminder.Mail
	rows    []eventreminder.Mail
	done    map[uuid.UUID]eventreminder.Outcome
	sends   []time.Time
}

func (q *memQueue) Plan(_ context.Context, now time.Time, w eventreminder.Window) (int, int, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.windows = append(q.windows, w)
	n := len(q.toPlan)
	for _, m := range q.toPlan {
		m.CreatedAt, m.NextAttemptAt = now, now
		q.rows = append(q.rows, m)
	}
	q.toPlan = nil
	if n == 0 {
		return 0, 0, nil
	}
	return 1, n, nil
}

func (q *memQueue) add(m eventreminder.Mail, due time.Time) {
	q.mu.Lock()
	defer q.mu.Unlock()
	m.NextAttemptAt = due
	q.rows = append(q.rows, m)
}

func (q *memQueue) open(i int, now time.Time) bool {
	_, done := q.done[q.rows[i].ID]
	return !done && !q.rows[i].NextAttemptAt.After(now)
}

func (q *memQueue) Claim(_ context.Context, now time.Time, lease time.Duration, perMinute int) (eventreminder.Mail, bool, bool, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if perMinute > 0 {
		recent := 0
		for _, at := range q.sends {
			if at.After(now.Add(-time.Minute)) && !at.After(now) {
				recent++
			}
		}
		if recent >= perMinute {
			return eventreminder.Mail{}, false, true, nil
		}
	}
	for i := range q.rows {
		if !q.open(i, now) {
			continue
		}
		q.rows[i].ClaimedAt = now
		q.rows[i].NextAttemptAt = now.Add(lease)
		q.sends = append(q.sends, now)
		return q.rows[i], true, false, nil
	}
	return eventreminder.Mail{}, false, false, nil
}

func (q *memQueue) Siblings(_ context.Context, claimed eventreminder.Mail, _ string, now time.Time) ([]eventreminder.Mail, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	var out []eventreminder.Mail
	for i, m := range q.rows {
		if m.ID != claimed.ID && m.EventID == claimed.EventID && m.StartsAt.Equal(claimed.StartsAt) && q.open(i, now) {
			out = append(out, m)
		}
	}
	return out, nil
}

func (q *memQueue) ClaimSibling(_ context.Context, id uuid.UUID, now time.Time, lease time.Duration) (eventreminder.Mail, bool, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i := range q.rows {
		if q.rows[i].ID == id && q.open(i, now) {
			q.rows[i].ClaimedAt = now
			q.rows[i].NextAttemptAt = now.Add(lease)
			return q.rows[i], true, nil
		}
	}
	return eventreminder.Mail{}, false, nil
}

func (q *memQueue) Finish(_ context.Context, claimed eventreminder.Mail, _ time.Time, outcome eventreminder.Outcome) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.done == nil {
		q.done = map[uuid.UUID]eventreminder.Outcome{}
	}
	q.done[claimed.ID] = outcome
	return nil
}

func (q *memQueue) Retry(_ context.Context, claimed eventreminder.Mail, at time.Time) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i := range q.rows {
		if q.rows[i].ID == claimed.ID {
			q.rows[i].Attempts++
			q.rows[i].NextAttemptAt = at
			q.rows[i].ClaimedAt = time.Time{}
		}
	}
	return nil
}

func (q *memQueue) outcome(id uuid.UUID) eventreminder.Outcome {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.done[id]
}

type call struct {
	typ, key string
	holder   mail.TicketHolder
	vars     map[string]string
	source   mail.NotificationSource
	expiry   *time.Time
}

// fakeMail answers err to the holders in fail, nil to the rest.
type fakeMail struct {
	calls []call
	fail  map[string]error
}

func (m *fakeMail) EventReminder(_ context.Context, typ, key string, holder mail.TicketHolder, vars map[string]string, source mail.NotificationSource, expiresAt *time.Time) error {
	m.calls = append(m.calls, call{typ, key, holder, vars, source, expiresAt})
	name := holder.Email
	if holder.Sub != uuid.Nil {
		name = holder.Sub.String()
	}
	return m.fail[name]
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

var istanbul = func() *time.Location {
	loc, err := time.LoadLocation("Europe/Istanbul")
	if err != nil {
		panic(err)
	}
	return loc
}()

func at(t time.Time) *time.Time { return &t }

// fixture is a published Event tomorrow at 10:00 Istanbul time, at 18:05
// the evening before.
type fixture struct {
	clock   time.Time
	queue   *memQueue
	mail    *fakeMail
	events  events
	tickets tickets
	users   users
	ev      event.Event
	page    ticket.PageConfig
	perMin  int
}

func newFixture() *fixture {
	start := time.Date(2026, 11, 25, 10, 0, 0, 0, istanbul)
	ev := event.Event{
		ID: uuid.New(), Name: "Gece Kodu", Location: "Kongre Merkezi", LocationAddress: "Davutpaşa Kampüsü",
		OwnerTeam: "WEBLAB", Status: event.StatusPublished, StartDate: at(start), EndDate: at(start.Add(3 * time.Hour)),
	}
	return &fixture{
		clock: time.Date(2026, 11, 24, 18, 5, 0, 0, istanbul),
		queue: &memQueue{}, mail: &fakeMail{},
		events: events{ev.ID: ev}, tickets: tickets{}, users: users{}, ev: ev,
	}
}

func (f *fixture) service(t *testing.T, typ string) *eventreminder.Service {
	t.Helper()
	svc, err := eventreminder.NewService(eventreminder.Config{
		Queue: f.queue, Tickets: f.tickets, Events: f.events, Users: f.users, Mail: f.mail,
		Type: typ, Page: f.page, APIOrigin: "https://api.example.test/", PerMinute: f.perMin,
		Now: func() time.Time { return f.clock }, Logger: log.New(io.Discard, "", 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

// member adds a member's active Ticket and their account under email.
func (f *fixture) member(email string) ticket.Ticket {
	sub := uuid.New()
	f.users[sub] = user.User{ID: sub, Email: email, AccountState: user.AccountActive}
	tk := ticket.Ticket{ID: uuid.New(), EventID: f.ev.ID, TicketType: ticket.Registered, OwnerID: &sub, Status: ticket.StatusActive,
		Code: "K7M2-QX9F", AccessKey: "7Q2MK4XRWZ3HN5TJVB6PDYCGEA"}
	f.tickets[tk.ID] = tk
	return tk
}

func (f *fixture) guest(email, first string) ticket.Ticket {
	tk := ticket.Ticket{ID: uuid.New(), EventID: f.ev.ID, TicketType: ticket.Guest, GuestEmail: email, GuestFirstName: first,
		GuestLastName: "Er", Status: ticket.StatusActive}
	f.tickets[tk.ID] = tk
	return tk
}

// queue1 queues tk's row for the Event as it starts now, due now.
func (f *fixture) queue1(tk ticket.Ticket) eventreminder.Mail {
	m := eventreminder.Mail{ID: uuid.New(), EventID: f.ev.ID, TicketID: tk.ID, StartsAt: *f.ev.StartDate, CreatedAt: f.clock}
	f.queue.add(m, f.clock)
	return m
}

func TestNothingIsPlannedBefore18AndTheNextDayFrom18(t *testing.T) {
	t.Parallel()
	f := newFixture()
	svc := f.service(t, mail.EventReminderType)
	f.clock = time.Date(2026, 11, 24, 17, 59, 59, 0, istanbul)
	if _, err := svc.Pass(context.Background()); err != nil || len(f.queue.windows) != 0 {
		t.Fatalf("17:59:59 planned %v, %v", f.queue.windows, err)
	}
	f.clock = time.Date(2026, 11, 24, 18, 0, 0, 0, istanbul)
	if _, err := svc.Pass(context.Background()); err != nil || len(f.queue.windows) != 1 {
		t.Fatalf("18:00 planned %v, %v", f.queue.windows, err)
	}
	w := f.queue.windows[0]
	if !w.From.Equal(time.Date(2026, 11, 25, 0, 0, 0, 0, istanbul)) || !w.To.Equal(time.Date(2026, 11, 26, 0, 0, 0, 0, istanbul)) ||
		!w.After.Equal(f.clock.Add(eventreminder.MissedPlanCutoff)) {
		t.Fatalf("window %+v", w)
	}
}

func TestAnEmptyTemplateKeyPlansAndSendsNothing(t *testing.T) {
	t.Parallel()
	f := newFixture()
	f.queue1(f.member("ayse@example.com"))
	if _, err := f.service(t, " ").Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.queue.windows) != 0 || len(f.mail.calls) != 0 {
		t.Fatalf("planned %v, sent %d", f.queue.windows, len(f.mail.calls))
	}
}

// A member gets a Notification (inbox and e-mail) carrying their own Ticket,
// a guest the e-mail; a guest Ticket under the member's address rides the
// member's mail; cancelled and waitlisted Tickets, an erased member and a
// guest without an address get nothing.
func TestEachAddressGetsOneReminderAndOnlyActiveTicketsCount(t *testing.T) {
	t.Parallel()
	f := newFixture()
	f.page = ticket.PageConfig{URL: "https://yildizskylab.com/bilet/" + ticket.KeyPlaceholder}
	member := f.member("Ayse@Example.com")
	f.queue1(f.guest(" ayse@example.com ", "Ayşe"))
	f.queue1(member)
	can := f.guest("can@example.com", "Can")
	f.queue1(can)
	cancelled := f.guest("cancelled@example.com", "İptal")
	cancelled.Status = ticket.StatusCancelled
	f.tickets[cancelled.ID] = cancelled
	waitlisted := f.member("waitlisted@example.com")
	waitlisted.Status = ticket.StatusWaitlisted
	f.tickets[waitlisted.ID] = waitlisted
	erased := f.member("erased@example.com")
	u := f.users[*erased.OwnerID]
	u.AccountState = user.AccountDeletionPending
	f.users[*erased.OwnerID] = u
	noAddress := f.guest("", "Adsız")
	skipped := []eventreminder.Mail{f.queue1(cancelled), f.queue1(waitlisted), f.queue1(erased), f.queue1(noAddress)}

	report, err := f.service(t, mail.EventReminderType).Pass(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Sent != 2 || report.Skipped != 4 || len(f.mail.calls) != 2 {
		t.Fatalf("report %+v, calls %d", report, len(f.mail.calls))
	}
	first, second := f.mail.calls[0], f.mail.calls[1]
	if first.holder.Sub != *member.OwnerID || first.holder.Email != "" || first.typ != mail.EventReminderType {
		t.Fatalf("first mail %+v: the member's, not the guest Ticket's under their address", first.holder)
	}
	if first.key != eventreminder.IdempotencyKey(f.ev.ID, *f.ev.StartDate, member.ID) {
		t.Fatalf("key %q", first.key)
	}
	if first.source != (mail.NotificationSource{Kind: "event", Ref: f.ev.ID.String()}) || first.expiry == nil || !first.expiry.Equal(*f.ev.EndDate) {
		t.Fatalf("source %+v, expiry %v", first.source, first.expiry)
	}
	want := map[string]string{
		"EventId": f.ev.ID.String(), "EventName": "Gece Kodu", "StartsAt": "25.11.2026 10:00", "Venue": "Kongre Merkezi",
		"OwnerTeam": first.vars["OwnerTeam"], "TicketUrl": "https://yildizskylab.com/bilet/7Q2MK4XRWZ3HN5TJVB6PDYCGEA",
		"TicketQrUrl": "https://api.example.test/v1/public/tickets/7Q2MK4XRWZ3HN5TJVB6PDYCGEA/qr.png", "TicketCode": "K7M2-QX9F",
		"MapUrl": first.vars["MapUrl"], "CoOrganizers": "",
	}
	if len(first.vars) != len(want) || first.vars["OwnerTeam"] == "" || !strings.HasPrefix(first.vars["MapUrl"], "https://") {
		t.Fatalf("vars %v", first.vars)
	}
	for k, v := range want {
		if first.vars[k] != v {
			t.Fatalf("%s = %q, want %q", k, first.vars[k], v)
		}
	}
	if second.holder.Sub != uuid.Nil || second.holder.Email != "can@example.com" || second.holder.FullName != "Can Er" {
		t.Fatalf("second mail %+v", second.holder)
	}
	// A guest Ticket has no access key here: no page link, but every key is
	// there.
	for _, k := range []string{"TicketUrl", "TicketQrUrl", "TicketCode"} {
		if v, ok := second.vars[k]; !ok || v != "" {
			t.Fatalf("guest %s = %q, %v", k, v, ok)
		}
	}
	for _, m := range skipped {
		if got := f.queue.outcome(m.ID); got != eventreminder.Skipped {
			t.Fatalf("row %v: %q", m.TicketID, got)
		}
	}
	if len(f.queue.done) != 7 {
		t.Fatalf("done %d rows", len(f.queue.done))
	}
}

// With TICKET_PAGE_URL unset the Ticket's link, QR and code are empty, and
// still sent: SkyNotify renders with missingkey=error.
func TestWithoutTheTicketPageTheTicketVariablesAreEmpty(t *testing.T) {
	t.Parallel()
	f := newFixture()
	f.queue1(f.member("ayse@example.com"))
	if _, err := f.service(t, "event.reminder.v2").Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.mail.calls) != 1 || f.mail.calls[0].typ != "event.reminder.v2" {
		t.Fatalf("calls %+v", f.mail.calls)
	}
	vars := f.mail.calls[0].vars
	for _, k := range []string{"TicketUrl", "TicketQrUrl", "TicketCode"} {
		if v, ok := vars[k]; !ok || v != "" {
			t.Fatalf("%s = %q, %v", k, v, ok)
		}
	}
	if !strings.HasPrefix(f.mail.calls[0].key, "core:event.reminder:") {
		t.Fatalf("the key names the type whatever the template: %q", f.mail.calls[0].key)
	}
}

// The worker reads the Event again: cancelled, unpublished, archived, gone
// or moved to another day, nothing goes; moved within the day, the mail
// says the new time.
func TestTheEventIsReadAgainWhenTheMailGoes(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		mutate func(*event.Event)
		gone   bool
		want   eventreminder.Outcome
		starts string
	}{
		"cancelled":           {mutate: func(e *event.Event) { e.Status = event.StatusCancelled }, want: eventreminder.Dropped},
		"back to draft":       {mutate: func(e *event.Event) { e.Status = event.StatusDraft }, want: eventreminder.Dropped},
		"archived":            {mutate: func(e *event.Event) { e.ArchivedAt = at(time.Now()) }, want: eventreminder.Dropped},
		"deleted":             {gone: true, want: eventreminder.Dropped},
		"moved a day later":   {mutate: func(e *event.Event) { e.StartDate = at(e.StartDate.Add(24 * time.Hour)) }, want: eventreminder.Dropped},
		"moved within a day":  {mutate: func(e *event.Event) { e.StartDate = at(e.StartDate.Add(4 * time.Hour)) }, want: eventreminder.Sent, starts: "25.11.2026 14:00"},
		"unchanged":           {want: eventreminder.Sent, starts: "25.11.2026 10:00"},
		"moved to start soon": {mutate: func(e *event.Event) { e.StartDate = at(time.Date(2026, 11, 24, 18, 0, 0, 0, istanbul)) }, want: eventreminder.Dropped},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFixture()
			row := f.queue1(f.member("ayse@example.com"))
			ev := f.ev
			if tc.mutate != nil {
				tc.mutate(&ev)
			}
			f.events[ev.ID] = ev
			if tc.gone {
				delete(f.events, ev.ID)
			}
			if _, err := f.service(t, mail.EventReminderType).Pass(context.Background()); err != nil {
				t.Fatal(err)
			}
			if got := f.queue.outcome(row.ID); got != tc.want {
				t.Fatalf("outcome %q, want %q", got, tc.want)
			}
			if tc.want == eventreminder.Sent && f.mail.calls[0].vars["StartsAt"] != tc.starts {
				t.Fatalf("StartsAt %q", f.mail.calls[0].vars["StartsAt"])
			}
			if tc.want != eventreminder.Sent && len(f.mail.calls) != 0 {
				t.Fatalf("sent %d", len(f.mail.calls))
			}
		})
	}
}

// A reminder that did not go by the Event's own day is not sent late: the
// mail says "Yarın".
func TestAReminderNotSentByTheEventsDayIsGivenUp(t *testing.T) {
	t.Parallel()
	f := newFixture()
	row := f.queue1(f.member("ayse@example.com"))
	f.clock = time.Date(2026, 11, 25, 0, 0, 0, 0, istanbul)
	report, err := f.service(t, mail.EventReminderType).Pass(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Expired != 1 || len(f.mail.calls) != 0 || f.queue.outcome(row.ID) != eventreminder.Expired {
		t.Fatalf("report %+v, calls %d", report, len(f.mail.calls))
	}
}

// A failure keeps the row for later, with a growing wait; a refusal and a
// guest's mail that went out unanswered are done with.
func TestAFailureIsTriedAgainAndARefusalIsNot(t *testing.T) {
	t.Parallel()
	f := newFixture()
	down := f.member("down@example.com")
	refused := f.member("refused@example.com")
	unanswered := f.guest("unanswered@example.com", "Can")
	rows := []eventreminder.Mail{f.queue1(down), f.queue1(refused), f.queue1(unanswered)}
	f.mail.fail = map[string]error{
		down.OwnerID.String():    &mail.SendError{Status: http.StatusServiceUnavailable},
		refused.OwnerID.String(): &mail.SendError{Status: http.StatusUnprocessableEntity, Code: "validation_error"},
		"unanswered@example.com": mail.ErrMaybeDelivered,
	}
	report, err := f.service(t, mail.EventReminderType).Pass(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Retried != 1 || report.Rejected != 1 || report.Uncertain != 1 {
		t.Fatalf("report %+v", report)
	}
	if f.queue.outcome(rows[0].ID) != "" || f.queue.outcome(rows[1].ID) != eventreminder.Rejected || f.queue.outcome(rows[2].ID) != eventreminder.Uncertain {
		t.Fatalf("outcomes %v", f.queue.done)
	}
	retried := f.queue.rows[0]
	if retried.Attempts != 1 || !retried.NextAttemptAt.Equal(f.clock.Add(eventreminder.RetryFirst)) {
		t.Fatalf("retried row %+v", retried)
	}
	// Not seeded yet is not a refusal: tried again.
	f2 := newFixture()
	m := f2.member("ayse@example.com")
	f2.queue1(m)
	f2.mail.fail = map[string]error{m.OwnerID.String(): &mail.SendError{Status: http.StatusUnprocessableEntity, Code: "notification.type_unknown"}}
	if report, err := f2.service(t, mail.EventReminderType).Pass(context.Background()); err != nil || report.Retried != 1 {
		t.Fatalf("type unknown: %+v, %v", report, err)
	}
}

// At most PerMinute mails a minute: the pass stops at the bound and the next
// minute goes on.
func TestThePerMinuteBoundPacesTheMails(t *testing.T) {
	t.Parallel()
	f := newFixture()
	f.perMin = 2
	for i := range 5 {
		f.queue1(f.guest(string(rune('a'+i))+"@example.com", "Can"))
	}
	svc := f.service(t, mail.EventReminderType)
	report, err := svc.Pass(context.Background())
	if err != nil || report.Sent != 2 || !report.Limited {
		t.Fatalf("first minute %+v, %v", report, err)
	}
	f.clock = f.clock.Add(30 * time.Second)
	if report, err = svc.Pass(context.Background()); err != nil || report.Sent != 0 || !report.Limited {
		t.Fatalf("same minute %+v, %v", report, err)
	}
	f.clock = f.clock.Add(31 * time.Second)
	if report, err = svc.Pass(context.Background()); err != nil || report.Sent != 2 {
		t.Fatalf("next minute %+v, %v", report, err)
	}
	f.clock = f.clock.Add(time.Minute)
	if report, err = svc.Pass(context.Background()); err != nil || report.Sent != 1 || report.Limited {
		t.Fatalf("last %+v, %v", report, err)
	}
	if len(f.mail.calls) != 5 {
		t.Fatalf("calls %d", len(f.mail.calls))
	}
}

func TestNewServiceRefusesANegativeBound(t *testing.T) {
	t.Parallel()
	f := newFixture()
	_, err := eventreminder.NewService(eventreminder.Config{
		Queue: f.queue, Tickets: f.tickets, Events: f.events, Users: f.users, Mail: f.mail, Type: "event.reminder", PerMinute: -1,
	})
	if err == nil {
		t.Fatal("a negative bound must be refused")
	}
	if _, err := eventreminder.NewService(eventreminder.Config{}); err == nil {
		t.Fatal("an empty config must be refused")
	}
}
