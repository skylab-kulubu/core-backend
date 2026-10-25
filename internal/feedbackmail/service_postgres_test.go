package feedbackmail_test

import (
	"context"
	"errors"
	"net/url"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/internal/feedback"
	"github.com/skylab-kulubu/core-backend/internal/feedbackmail"
	"github.com/skylab-kulubu/core-backend/internal/mail"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

type sent struct {
	to        string
	firstName string
	eventName string
	items     []mail.FeedbackItem
	kind      mail.FeedbackKind
}

// mailer records the mails; fail makes the next sends fail with it; off
// turns a kind's template off.
type mailer struct {
	mu   sync.Mutex
	sent []sent
	fail []error
	off  map[mail.FeedbackKind]bool
}

func (m *mailer) FeedbackConfigured(kind mail.FeedbackKind) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return !m.off[kind]
}

func (m *mailer) Feedback(_ context.Context, kind mail.FeedbackKind, to, _, first, eventName string, items []mail.FeedbackItem) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.fail) > 0 {
		err := m.fail[0]
		m.fail = m.fail[1:]
		return err
	}
	m.sent = append(m.sent, sent{to: to, firstName: first, eventName: eventName, items: items, kind: kind})
	return nil
}

func (m *mailer) take() []sent {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := m.sent
	m.sent = nil
	return out
}

// day is 2026-11-07 in Istanbul: Session A 10:00–11:00, B 14:00–15:00; the
// Event ends 15:00. Ada (member) checked in to A and B, Grace (guest) to A.
var (
	istanbul = time.FixedZone("TRT", 3*3600)
	at       = func(h, m int) time.Time { return time.Date(2026, 11, 7, h, m, 0, 0, istanbul) }
)

type fixture struct {
	pool                        *pgxpool.Pool
	store                       *feedback.PostgresStore
	svc                         *feedbackmail.Service
	mail                        *mailer
	now                         time.Time
	event, sessionA, sessionB   uuid.UUID
	formEvent, formA, formB     uuid.UUID
	ada, adaTicket, graceTicket uuid.UUID
}

func formURL(id uuid.UUID) string { return "https://forms.yildizskylab.com/" + id.String() }

func newFixture(t *testing.T) *fixture {
	t.Helper()
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	f := &fixture{pool: pool, store: feedback.NewPostgresStore(pool), mail: &mailer{},
		event: uuid.New(), sessionA: uuid.New(), sessionB: uuid.New(),
		formEvent: uuid.New(), formA: uuid.New(), formB: uuid.New(),
		ada: uuid.New(), adaTicket: uuid.New(), graceTicket: uuid.New()}
	users := user.NewPostgresStore(pool)
	if _, _, err := user.NewService(users).Ensure(ctx, f.ada, user.Profile{Email: "Ada@Example.test", FirstName: "Ada", LastName: "Lovelace"}); err != nil {
		t.Fatal(err)
	}
	day := uuid.New()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO events (id, name, location, owner_team, status, start_date, end_date, feedback_mode, feedback_form_url)
		VALUES ($1, 'Gecekodu', 'YTÜ', 'WEBLAB', 'published', $2, $3, 'per_session', $4)`, f.event, at(10, 0), at(15, 0), formURL(f.formEvent))
	exec(`INSERT INTO event_days (id, event_id, name, start_date, end_date) VALUES ($1, $2, 'Cumartesi', $3, $4)`, day, f.event, at(10, 0), at(15, 0))
	exec(`INSERT INTO sessions (id, event_day_id, title, session_type, start_time, end_time, order_index, feedback_form_url)
		VALUES ($1, $2, 'Açılış', 'talk', $3, $4, 1, $5), ($6, $2, 'Go atölyesi', 'talk', $7, $8, 2, $9)`,
		f.sessionA, day, at(10, 0), at(11, 0), formURL(f.formA), f.sessionB, at(14, 0), at(15, 0), formURL(f.formB))
	exec(`INSERT INTO tickets (id, event_id, ticket_type, owner_id) VALUES ($1, $2, 'REGISTERED', $3)`, f.adaTicket, f.event, f.ada)
	exec(`INSERT INTO tickets (id, event_id, ticket_type, guest_first_name, guest_last_name, guest_email) VALUES ($1, $2, 'GUEST', 'Grace', 'Hopper', 'grace@example.test')`, f.graceTicket, f.event)
	exec(`INSERT INTO ticket_checkins (id, ticket_id, event_day_id, session_id) VALUES ($1, $2, $3, $4), ($5, $2, $3, $6), ($7, $8, $3, $4)`,
		uuid.New(), f.adaTicket, day, f.sessionA, uuid.New(), f.sessionB, uuid.New(), f.graceTicket)
	svc, err := feedbackmail.NewService(feedbackmail.Config{
		Queue: f.store, Tickets: ticket.NewPostgresStore(pool), Users: users, Mail: f.mail,
		Now: func() time.Time { return f.now },
	})
	if err != nil {
		t.Fatal(err)
	}
	f.svc = svc
	return f
}

func (f *fixture) pass(t *testing.T, now time.Time) feedbackmail.Report {
	t.Helper()
	f.now = now
	report, err := f.svc.Pass(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return report
}

func titles(items []mail.FeedbackItem) []string {
	out := []string{}
	for _, it := range items {
		out = append(out, it.Title)
	}
	return out
}

// codeOf is the invite code of a link, checking the link is the form's own
// address with the code alone.
func codeOf(t *testing.T, link string, form uuid.UUID) string {
	t.Helper()
	parsed, err := url.Parse(link)
	if err != nil || parsed.Host != "forms.yildizskylab.com" || parsed.Path != "/"+form.String() || len(parsed.Query()) != 1 {
		t.Fatalf("link %q", link)
	}
	return parsed.Query().Get(feedback.InviteParam)
}

// Two hours after each target ends; one mail a person a day, after the last
// Session they checked in to that day; invites written, counted, matched.
func TestScheduleOneMailAPersonADay(t *testing.T) {
	f := newFixture(t)
	if r := f.pass(t, at(12, 59)); r.Planned != 0 || len(f.mail.take()) != 0 {
		t.Fatalf("before A is due %+v", r)
	}
	// A is due at 13:00: Grace's day ends with it, Ada's with B.
	if r := f.pass(t, at(13, 1)); r.Planned != 1 || r.Sent != 0 {
		t.Fatalf("A planned %+v", r)
	}
	if r := f.pass(t, at(13, 6)); r.Sent != 1 {
		t.Fatalf("Grace's mail %+v", r)
	}
	grace := f.mail.take()
	if len(grace) != 1 || grace[0].to != "grace@example.test" || grace[0].firstName != "Grace" || grace[0].eventName != "Gecekodu" ||
		!slices.Equal(titles(grace[0].items), []string{"Açılış"}) {
		t.Fatalf("Grace %+v", grace)
	}
	codeOf(t, grace[0].items[0].URL, f.formA)
	// B and the Event's form are due at 17:00; Ada's mail at 17:05 carries
	// all three.
	if r := f.pass(t, at(17, 1)); r.Planned != 2 || r.Sent != 0 {
		t.Fatalf("B and Event planned %+v", r)
	}
	if r := f.pass(t, at(17, 6)); r.Sent != 2 {
		t.Fatalf("17:06 %+v", r)
	}
	var ada []sent
	for _, m := range f.mail.take() {
		if m.to == "ada@example.test" {
			ada = append(ada, m)
		}
	}
	if len(ada) != 1 || !slices.Equal(titles(ada[0].items), []string{"Açılış", "Go atölyesi", "Gecekodu"}) {
		t.Fatalf("Ada %+v", ada)
	}
	code := codeOf(t, ada[0].items[1].URL, f.formB)
	// The answer by Ada's code marks her B invite.
	if ok, err := f.store.MarkAnswered(context.Background(), f.formB, code, nil); err != nil || !ok {
		t.Fatalf("mark %v %v", ok, err)
	}
	counts, err := f.store.Counts(context.Background(), f.event)
	if err != nil || counts.Event.Sent != 2 || len(counts.Sessions) != 2 {
		t.Fatalf("counts %+v %v", counts, err)
	}
	// Nothing goes twice.
	if r := f.pass(t, at(23, 0)); r.Planned != 0 || r.Sent != 0 {
		t.Fatalf("again %+v", r)
	}
}

// "Şimdi gönder" mails the target at once, once; the schedule never sends
// it again. A draft or a target without a form is refused.
func TestSendNow(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	// At noon, before any target is due.
	sentNow, recipients, err := f.store.SendNow(ctx, feedback.Target{EventID: f.event, SessionID: &f.sessionB}, at(12, 10))
	if err != nil || !sentNow || recipients != 1 {
		t.Fatalf("send now %v %d %v", sentNow, recipients, err)
	}
	if again, _, err := f.store.SendNow(ctx, feedback.Target{EventID: f.event, SessionID: &f.sessionB}, at(12, 11)); err != nil || again {
		t.Fatalf("again %v %v", again, err)
	}
	if r := f.pass(t, at(12, 11)); r.Sent != 1 {
		t.Fatalf("pass %+v", r)
	}
	if m := f.mail.take(); len(m) != 1 || !slices.Equal(titles(m[0].items), []string{"Go atölyesi"}) {
		t.Fatalf("mail %+v", m)
	}
	// At 17:00 the schedule sends A (due) and the Event's form, not B.
	f.pass(t, at(17, 1))
	f.pass(t, at(17, 6))
	for _, m := range f.mail.take() {
		if slices.Contains(titles(m.items), "Go atölyesi") {
			t.Fatalf("B again %+v", m)
		}
	}
	if _, err := f.pool.Exec(ctx, `UPDATE sessions SET feedback_form_url = '' WHERE id = $1`, f.sessionA); err != nil {
		t.Fatal(err)
	}
	other := uuid.New()
	if _, err := f.pool.Exec(ctx, `INSERT INTO events (id, name, location, owner_team, status, feedback_form_url) VALUES ($1, 'Taslak', 'YTÜ', 'WEBLAB', 'draft', $2)`, other, formURL(uuid.New())); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.store.SendNow(ctx, feedback.Target{EventID: other}, at(18, 0)); !errors.Is(err, feedback.ErrNotSendable) {
		t.Fatalf("draft %v", err)
	}
	if _, _, err := f.store.SendNow(ctx, feedback.Target{EventID: f.event, SessionID: &f.sessionA}, at(18, 0)); !errors.Is(err, feedback.ErrNoForm) && !errors.Is(err, feedback.ErrNotSendable) {
		t.Fatalf("no form %v", err)
	}
}

// Nothing goes with the box off, after the Event is cancelled, or for a
// target that ended longer ago than the window.
func TestScheduleSkips(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := f.pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`UPDATE sessions SET feedback_auto_send = false WHERE id = $1`, f.sessionA)
	if r := f.pass(t, at(13, 30)); r.Planned != 0 {
		t.Fatalf("box off %+v", r)
	}
	// B planned at 17:00, then the Event is cancelled before Ada's mail goes.
	f.pass(t, at(17, 1))
	exec(`UPDATE events SET status = 'cancelled', cancelled_at = now() WHERE id = $1`, f.event)
	if r := f.pass(t, at(17, 6)); r.Sent != 0 || r.Dropped == 0 {
		t.Fatalf("cancelled %+v", r)
	}
	// Eight days late: not by itself.
	f2 := newFixture(t)
	if r := f2.pass(t, at(17, 1).Add(8*24*time.Hour)); r.Planned != 0 {
		t.Fatalf("window %+v", r)
	}
}

// A failed send is tried again with the same link; a refusal is done with.
func TestRetrySendsTheSameLink(t *testing.T) {
	f := newFixture(t)
	f.mail.fail = []error{&mail.SendError{Status: 503}}
	f.pass(t, at(13, 1))
	if r := f.pass(t, at(13, 6)); r.Retried != 1 || r.Sent != 0 {
		t.Fatalf("first try %+v", r)
	}
	var invites int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM feedback_invites WHERE ticket_id = $1`, f.graceTicket).Scan(&invites); err != nil || invites != 1 {
		t.Fatalf("invites after a failure %d %v", invites, err)
	}
	if r := f.pass(t, at(13, 7)); r.Sent != 1 {
		t.Fatalf("retry %+v", r)
	}
	m := f.mail.take()
	var code string
	if err := f.pool.QueryRow(context.Background(), `SELECT code FROM feedback_invites WHERE ticket_id = $1`, f.graceTicket).Scan(&code); err != nil {
		t.Fatal(err)
	}
	if len(m) != 1 || codeOf(t, m[0].items[0].URL, f.formA) != code {
		t.Fatalf("retried mail %+v", m)
	}
	// A refusal (422) is done with, not tried again.
	f2 := newFixture(t)
	f2.mail.fail = []error{&mail.SendError{Status: 422}}
	f2.pass(t, at(13, 1))
	if r := f2.pass(t, at(13, 6)); r.Rejected != 1 {
		t.Fatalf("refused %+v", r)
	}
	if r := f2.pass(t, at(13, 30)); r.Sent != 0 {
		t.Fatalf("after refusal %+v", r)
	}
}

// adaAsGuest gives Ada a guest Ticket too, under the same address in
// another spelling, checked in to Session A.
func (f *fixture) adaAsGuest(t *testing.T) uuid.UUID {
	t.Helper()
	id := uuid.New()
	ctx := context.Background()
	if _, err := f.pool.Exec(ctx, `INSERT INTO tickets (id, event_id, ticket_type, guest_first_name, guest_last_name, guest_email) VALUES ($1, $2, 'GUEST', 'Ada', 'L.', ' ADA@example.test ')`, id, f.event); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO ticket_checkins (id, ticket_id, event_day_id, session_id) SELECT $1, $2, event_day_id, id FROM sessions WHERE id = $3`, uuid.New(), id, f.sessionA); err != nil {
		t.Fatal(err)
	}
	return id
}

func to(mails []sent, address string) []sent {
	var out []sent
	for _, m := range mails {
		if m.to == address {
			out = append(out, m)
		}
	}
	return out
}

// One address, one mail a day (decision of 2026-10-20): Ada's guest Ticket
// waits for the last Session her member Ticket checked in to, and both go
// in one mail, each Ticket with its own invite links.
func TestOneMailAnAddressADay(t *testing.T) {
	f := newFixture(t)
	guest := f.adaAsGuest(t)
	f.pass(t, at(13, 1))
	if r := f.pass(t, at(13, 6)); r.Sent != 1 {
		t.Fatalf("13:06 %+v", r)
	}
	if m := f.mail.take(); len(m) != 1 || m[0].to != "grace@example.test" {
		t.Fatalf("only Grace at 13:06: %+v", m)
	}
	f.pass(t, at(17, 1))
	f.pass(t, at(17, 6))
	all := f.mail.take()
	ada := to(all, "ada@example.test")
	if len(ada) != 1 {
		t.Fatalf("Ada's mails %+v", all)
	}
	got := titles(ada[0].items)
	slices.Sort(got)
	if !slices.Equal(got, []string{"Açılış", "Açılış", "Gecekodu", "Gecekodu", "Go atölyesi"}) {
		t.Fatalf("Ada's items %v", got)
	}
	codes := map[string]bool{}
	for _, it := range ada[0].items {
		codes[it.URL] = true
	}
	var invites int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM feedback_invites WHERE ticket_id = ANY($1)`, []uuid.UUID{f.adaTicket, guest}).Scan(&invites); err != nil || invites != 5 || len(codes) != 5 {
		t.Fatalf("invites %d links %d %v", invites, len(codes), err)
	}
	var open int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM feedback_mails WHERE done_at IS NULL`).Scan(&open); err != nil || open != 0 {
		t.Fatalf("open mails %d %v", open, err)
	}
	if r := f.pass(t, at(23, 0)); r.Sent != 0 {
		t.Fatalf("again %+v", r)
	}
}

// "Şimdi gönder" too: one mail an address, two links for two Tickets.
func TestSendNowOneMailAnAddress(t *testing.T) {
	f := newFixture(t)
	f.adaAsGuest(t)
	if ok, n, err := f.store.SendNow(context.Background(), feedback.Target{EventID: f.event, SessionID: &f.sessionA}, at(12, 0)); err != nil || !ok || n != 3 {
		t.Fatalf("send now %v %d %v", ok, n, err)
	}
	if r := f.pass(t, at(12, 1)); r.Sent != 2 {
		t.Fatalf("pass %+v", r)
	}
	all := f.mail.take()
	ada := to(all, "ada@example.test")
	if len(all) != 2 || len(ada) != 1 || !slices.Equal(titles(ada[0].items), []string{"Açılış", "Açılış"}) || ada[0].items[0].URL == ada[0].items[1].URL {
		t.Fatalf("mails %+v", all)
	}
}
