package attendancemail_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/attendancemail"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/mail"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// fakeQueue is the queue in memory, as PostgresQueue keeps it.
type fakeQueue struct {
	rows      []*row
	present   map[uuid.UUID]bool
	progress  attendancemail.Progress
	attempted []uuid.UUID
}

type row struct {
	m       attendancemail.Mail
	held    bool
	done    attendancemail.Outcome
	retried bool
}

func (q *fakeQueue) add(m attendancemail.Mail) attendancemail.Mail {
	if m.ID == uuid.Nil {
		m.ID = uuid.New()
	}
	q.rows = append(q.rows, &row{m: m})
	if q.present == nil {
		q.present = map[uuid.UUID]bool{}
	}
	q.present[m.ID] = true
	return m
}

func (q *fakeQueue) find(id uuid.UUID) *row {
	for _, r := range q.rows {
		if r.m.ID == id {
			return r
		}
	}
	return nil
}

func (q *fakeQueue) Claim(_ context.Context, now time.Time, _ time.Duration, _ int) (attendancemail.Mail, bool, bool, error) {
	var best *row
	for _, r := range q.rows {
		if r.done != "" || r.held || r.retried || r.m.NextAttemptAt.After(now) {
			continue
		}
		if best == nil || (r.m.Kind == attendancemail.KindWelcome && best.m.Kind != attendancemail.KindWelcome) {
			best = r
		}
	}
	if best == nil {
		return attendancemail.Mail{}, false, false, nil
	}
	best.held = true
	best.m.ClaimedAt = now
	return best.m, true, false, nil
}

func (q *fakeQueue) Siblings(_ context.Context, claimed attendancemail.Mail, now time.Time) ([]attendancemail.Mail, error) {
	var out []attendancemail.Mail
	for _, r := range q.rows {
		if r.done != "" || r.held || r.m.ID == claimed.ID || r.m.TicketID != claimed.TicketID || r.m.EntryDate != claimed.EntryDate || r.m.Kind != attendancemail.KindRecorded {
			continue
		}
		if claimed.Kind != attendancemail.KindWelcome && r.m.NextAttemptAt.After(now) {
			continue
		}
		if r.m.MailKey != claimed.MailKey {
			continue
		}
		out = append(out, r.m)
	}
	return out, nil
}

func (q *fakeQueue) ClaimSibling(_ context.Context, id uuid.UUID, now time.Time, _ time.Duration) (attendancemail.Mail, bool, error) {
	r := q.find(id)
	if r == nil || r.held || r.done != "" {
		return attendancemail.Mail{}, false, nil
	}
	r.held = true
	r.m.ClaimedAt = now
	return r.m, true, nil
}

func (q *fakeQueue) Present(_ context.Context, m attendancemail.Mail) (bool, error) {
	return q.present[m.ID], nil
}

func (q *fakeQueue) Progress(context.Context, uuid.UUID, uuid.UUID, []uuid.UUID) (attendancemail.Progress, error) {
	return q.progress, nil
}

func (q *fakeQueue) Finish(_ context.Context, claimed attendancemail.Mail, _ time.Time, outcome attendancemail.Outcome) error {
	r := q.find(claimed.ID)
	r.done, r.held = outcome, false
	return nil
}

func (q *fakeQueue) Retry(_ context.Context, claimed attendancemail.Mail, _ time.Time) error {
	r := q.find(claimed.ID)
	r.held, r.retried = false, true
	return nil
}

func (q *fakeQueue) Keyed(_ context.Context, rows []attendancemail.Mail, key string) error {
	for _, m := range rows {
		if r := q.find(m.ID); r.m.MailKey == "" {
			r.m.MailKey = key
		}
	}
	return nil
}

func (q *fakeQueue) Attempted(_ context.Context, m attendancemail.Mail, _ time.Time) error {
	q.attempted = append(q.attempted, m.ID)
	return nil
}

func (q *fakeQueue) Purge(context.Context, time.Time) (int, error) { return 0, nil }

// sent is one AttendanceMail call.
type sent struct {
	typ, key string
	holder   mail.TicketHolder
	vars     map[string]any
}

type fakeMail struct {
	sent []sent
	err  error
}

func (f *fakeMail) AttendanceMail(_ context.Context, typ, key string, holder mail.TicketHolder, vars map[string]any, _ mail.NotificationSource, _ *time.Time) error {
	f.sent = append(f.sent, sent{typ, key, holder, vars})
	return f.err
}

type env struct {
	q       *fakeQueue
	mail    *fakeMail
	svc     *attendancemail.Service
	events  *event.MemoryStore
	tickets *ticket.MemoryStore
	users   *user.MemoryStore
	ev      event.Event
	member  ticket.Ticket
	guest   ticket.Ticket
	now     time.Time
}

const day = "2031-03-14"

func newEnv(t *testing.T, rule string) *env {
	t.Helper()
	ctx := context.Background()
	e := &env{q: &fakeQueue{}, mail: &fakeMail{}, events: event.NewMemoryStore(), tickets: ticket.NewMemoryStore(), users: user.NewMemoryStore(),
		now: time.Date(2031, 3, 14, 9, 0, 0, 0, time.UTC)}
	start := e.now.Add(-time.Hour)
	end := e.now.Add(8 * time.Hour)
	ev, err := e.events.Create(ctx, event.Event{Name: "GECEKODU", Location: "YTÜ", OwnerTeam: "WEBLAB", AttendanceRule: rule, Status: event.StatusPublished, StartDate: &start, EndDate: &end})
	if err != nil {
		t.Fatal(err)
	}
	e.ev = ev
	ownerID := uuid.New()
	if _, _, err := e.users.Upsert(ctx, user.User{ID: ownerID, Email: "elif@example.com", FirstName: "Elif", LastName: "Kaya", AccountState: user.AccountActive}); err != nil {
		t.Fatal(err)
	}
	if e.member, err = e.tickets.Create(ctx, ticket.Ticket{ID: uuid.New(), EventID: ev.ID, TicketType: ticket.Registered, OwnerID: &ownerID}); err != nil {
		t.Fatal(err)
	}
	if e.guest, err = e.tickets.Create(ctx, ticket.Ticket{ID: uuid.New(), EventID: ev.ID, TicketType: ticket.Guest, GuestFirstName: "Deniz", GuestLastName: "Ak", GuestEmail: "Deniz@Example.com"}); err != nil {
		t.Fatal(err)
	}
	e.svc, err = attendancemail.NewService(attendancemail.Config{
		Queue: e.q, Tickets: e.tickets, Events: e.events, Users: e.users, Mail: e.mail,
		WelcomeType: mail.AttendanceWelcomeType, RecordedType: mail.AttendanceRecordedType, PerMinute: 20,
		Now: func() time.Time { return e.now }, Logger: log.New(io.Discard, "", 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *env) welcome(tk ticket.Ticket) attendancemail.Mail {
	return e.q.add(attendancemail.Mail{TicketID: tk.ID, EventID: e.ev.ID, Kind: attendancemail.KindWelcome, EntryDate: day, CreatedAt: e.now.Add(-2 * time.Minute), NextAttemptAt: e.now})
}

func (e *env) recorded(tk ticket.Ticket, session uuid.UUID, due time.Time) attendancemail.Mail {
	return e.q.add(attendancemail.Mail{TicketID: tk.ID, EventID: e.ev.ID, Kind: attendancemail.KindRecorded, EntryDate: day, SessionID: &session, CreatedAt: due.Add(-2 * time.Minute), NextAttemptAt: due})
}

// program is 16 live Sessions over one day of e's: counted, the first and
// the last required (the last one still to come), the 16th not counted
// would break N, so all count; plus a NETWORKING one not counted.
func (e *env) program(attended int) (ids []uuid.UUID, networking uuid.UUID) {
	dayID := uuid.New()
	start := time.Date(2031, 3, 14, 7, 0, 0, 0, time.UTC) // 10:00 Istanbul
	p := attendancemail.Progress{Days: []attendancemail.Day{{ID: dayID, Start: &start}}, Attended: map[uuid.UUID]bool{}, Titles: map[uuid.UUID]string{}}
	for i := range 16 {
		id := uuid.New()
		s, f := start.Add(time.Duration(i)*30*time.Minute), start.Add(time.Duration(i)*30*time.Minute+25*time.Minute)
		title := "Oturum " + string(rune('A'+i))
		p.Sessions = append(p.Sessions, attendancemail.Session{ID: id, Title: title, DayID: dayID, Start: &s, End: &f, Ends: &f, Counted: true, Required: i == 0 || i == 15})
		p.Titles[id] = title
		if i >= 1 && i <= attended {
			p.Attended[id] = true
		}
		ids = append(ids, id)
	}
	networking = uuid.New()
	p.Sessions = append(p.Sessions, attendancemail.Session{ID: networking, Title: "Ağ", DayID: dayID, Counted: false})
	p.Titles[networking] = "Ağ"
	e.q.progress = p
	return ids, networking
}

func asJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// A welcome takes the same Ticket's waiting receipt of the day into one
// mail to the member's subject, with every variable of the template; the
// receipt is not sent again.
func TestWelcomeTakesTheReceiptAlong(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, event.RuleCount)
	ids, _ := e.program(4)
	e.q.progress.Attended[ids[5]] = true // the receipt's Session
	e.welcome(e.member)
	e.recorded(e.member, ids[5], e.now.Add(30*time.Second)) // not due yet
	report, err := e.svc.Pass(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if report.Sent != 1 || report.Merged != 1 || len(e.mail.sent) != 1 {
		t.Fatalf("report %+v sent %d", report, len(e.mail.sent))
	}
	got := e.mail.sent[0]
	if got.typ != mail.AttendanceWelcomeType || got.holder.Sub != *e.member.OwnerID || got.holder.Email != "" {
		t.Fatalf("mail %+v", got)
	}
	for _, name := range []string{"FirstName", "EventName", "EventId", "OwnerTeam", "DayLabel", "Program", "AttendeeNote", "CertificateLine", "Recorded", "Attended", "CountedTotal", "MinRequired", "MissingRequired", "MissedRequired"} {
		if _, ok := got.vars[name]; !ok {
			t.Errorf("welcome has no %s", name)
		}
	}
	if got.vars["FirstName"] != "Elif" || got.vars["DayLabel"] != "14 Mart Cuma" || got.vars["Attended"] != 5 || got.vars["CountedTotal"] != 16 || got.vars["MinRequired"] != 12 {
		t.Fatalf("vars %s", asJSON(t, got.vars))
	}
	if line := got.vars["CertificateLine"]; line != "Sertifika için 16 oturumdan en az 12 tanesine katılmalısın. Zorunlu: Oturum A, Oturum P." {
		t.Fatalf("certificate line %q", line)
	}
	if asJSON(t, got.vars["Recorded"]) != `[{"Title":"Oturum F"}]` {
		t.Fatalf("recorded %s", asJSON(t, got.vars["Recorded"]))
	}
	program := asJSON(t, got.vars["Program"])
	if !strings.HasPrefix(program, `[{"Time":"10:00–10:25","Title":"Oturum A"}`) || !strings.Contains(program, `{"Time":"","Title":"Ağ"}`) {
		t.Fatalf("program %s", program)
	}
	// 09:00 UTC is 12:00 Istanbul: Oturum A (10:00–10:25) is over and was
	// missed, Oturum P (17:30) is still to come.
	if asJSON(t, got.vars["MissedRequired"]) != `[{"Title":"Oturum A"}]` || asJSON(t, got.vars["MissingRequired"]) != `[{"Title":"Oturum P"}]` {
		t.Fatalf("required %s %s", asJSON(t, got.vars["MissedRequired"]), asJSON(t, got.vars["MissingRequired"]))
	}
	for _, r := range e.q.rows {
		if r.done != attendancemail.Sent {
			t.Fatalf("row %s left %q", r.m.Kind, r.done)
		}
	}
	if report, _ := e.svc.Pass(ctx); report.Sent != 0 || len(e.mail.sent) != 1 {
		t.Fatalf("second pass sent again: %+v", report)
	}
}

// Two due receipts of the same Ticket go as one mail naming both; a
// NETWORKING Session is marked as not counted. The guest gets it by
// address, lower-cased, with their first name.
func TestReceiptsGoTogether(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, event.RuleCount)
	ids, networking := e.program(4)
	e.recorded(e.guest, ids[5], e.now)
	e.recorded(e.guest, networking, e.now.Add(-time.Second))
	if _, err := e.svc.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	if len(e.mail.sent) != 1 {
		t.Fatalf("mails %d", len(e.mail.sent))
	}
	got := e.mail.sent[0]
	if got.typ != mail.AttendanceRecordedType || got.holder.Email != "deniz@example.com" || got.holder.FullName != "Deniz Ak" || got.vars["FirstName"] != "Deniz" {
		t.Fatalf("mail %+v", got)
	}
	for _, name := range []string{"FirstName", "EventName", "EventId", "OwnerTeam", "Sessions", "HasRule", "Attended", "CountedTotal", "MinRequired", "MissingRequired", "MissedRequired", "NotCounted"} {
		if _, ok := got.vars[name]; !ok {
			t.Errorf("receipt has no %s", name)
		}
	}
	sessions := asJSON(t, got.vars["Sessions"])
	if !strings.Contains(sessions, "Oturum F") || !strings.Contains(sessions, "Ağ") || asJSON(t, got.vars["NotCounted"]) != `[{"Title":"Ağ"}]` || got.vars["HasRule"] != true {
		t.Fatalf("vars %s", asJSON(t, got.vars))
	}
}

// Without a rule the receipt says only that it was taken: HasRule false,
// the counts 0, the lists empty, and the welcome no certificate line.
func TestNoRuleNoCounts(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, event.RuleNone)
	ids, _ := e.program(4)
	e.recorded(e.member, ids[1], e.now)
	e.welcome(e.guest)
	if _, err := e.svc.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	if len(e.mail.sent) != 2 {
		t.Fatalf("mails %d", len(e.mail.sent))
	}
	for _, got := range e.mail.sent {
		if got.vars["Attended"] != 0 || got.vars["MinRequired"] != 0 || asJSON(t, got.vars["MissingRequired"]) != "[]" {
			t.Fatalf("%s %s", got.typ, asJSON(t, got.vars))
		}
		if got.typ == mail.AttendanceWelcomeType && got.vars["CertificateLine"] != "" {
			t.Fatalf("certificate line %q", got.vars["CertificateLine"])
		}
		if got.typ == mail.AttendanceRecordedType && got.vars["HasRule"] != false {
			t.Fatal("HasRule without a rule")
		}
	}
}

// What is no longer news is dropped, not sent: an undone Check-in, a
// cancelled Ticket, an unpublished Event, the organizer's switch off.
func TestNoLongerNewsIsDropped(t *testing.T) {
	ctx := context.Background()
	t.Run("undone", func(t *testing.T) {
		e := newEnv(t, event.RuleCount)
		ids, _ := e.program(1)
		gone := e.recorded(e.member, ids[1], e.now)
		e.q.present[gone.ID] = false
		report, err := e.svc.Pass(ctx)
		if err != nil || report.Dropped != 1 || len(e.mail.sent) != 0 || e.q.find(gone.ID).done != attendancemail.Dropped {
			t.Fatalf("%v %+v", err, report)
		}
	})
	t.Run("one of two undone", func(t *testing.T) {
		e := newEnv(t, event.RuleCount)
		ids, _ := e.program(1)
		e.welcome(e.member)
		gone := e.recorded(e.member, ids[1], e.now)
		e.q.present[gone.ID] = false
		if _, err := e.svc.Pass(ctx); err != nil {
			t.Fatal(err)
		}
		if len(e.mail.sent) != 1 || asJSON(t, e.mail.sent[0].vars["Recorded"]) != "[]" || e.q.find(gone.ID).done != attendancemail.Dropped {
			t.Fatalf("sent %+v", e.mail.sent)
		}
	})
	t.Run("cancelled ticket", func(t *testing.T) {
		e := newEnv(t, event.RuleCount)
		e.program(1)
		e.welcome(e.member)
		if _, _, err := e.tickets.Cancel(ctx, e.member.ID, ticket.CancelledByOperator, ""); err != nil {
			t.Fatal(err)
		}
		if report, err := e.svc.Pass(ctx); err != nil || report.Dropped != 1 || len(e.mail.sent) != 0 {
			t.Fatalf("%v %+v", err, report)
		}
	})
	t.Run("switch off", func(t *testing.T) {
		e := newEnv(t, event.RuleCount)
		ids, _ := e.program(1)
		off := false
		e.ev.AttendanceRecordedMail = &off
		if _, err := e.events.Update(ctx, e.ev); err != nil {
			t.Fatal(err)
		}
		e.welcome(e.member)
		e.recorded(e.member, ids[1], e.now)
		if _, err := e.svc.Pass(ctx); err != nil {
			t.Fatal(err)
		}
		if len(e.mail.sent) != 1 || e.mail.sent[0].typ != mail.AttendanceWelcomeType || asJSON(t, e.mail.sent[0].vars["Recorded"]) != "[]" {
			t.Fatalf("sent %+v", e.mail.sent)
		}
	})
	t.Run("cancelled event", func(t *testing.T) {
		e := newEnv(t, event.RuleCount)
		e.program(1)
		e.welcome(e.member)
		if _, err := e.events.Cancel(ctx, e.ev.ID, nil, "Ertelendi", false); err != nil {
			t.Fatal(err)
		}
		if report, err := e.svc.Pass(ctx); err != nil || report.Dropped != 1 || len(e.mail.sent) != 0 {
			t.Fatalf("%v %+v", err, report)
		}
	})
}

// A failure is tried again under the same key; a refusal is not; a guest
// mail with no answer is not tried again.
func TestFailuresAndKeys(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, event.RuleCount)
	ids, _ := e.program(1)
	m := e.recorded(e.member, ids[1], e.now)
	e.mail.err = &mail.SendError{Status: 503}
	if report, err := e.svc.Pass(ctx); err != nil || report.Retried != 1 || !e.q.find(m.ID).retried {
		t.Fatalf("%v %+v", err, report)
	}
	first := e.mail.sent[0].key
	if first != attendancemail.IdempotencyKey(attendancemail.KindRecorded, []attendancemail.Mail{m}) || !strings.HasPrefix(first, "core:attendance-mail:recorded:") {
		t.Fatalf("key %q", first)
	}
	// The same rows queued again after an undo carry the same key.
	again := m
	again.ID = uuid.New()
	if attendancemail.IdempotencyKey(attendancemail.KindRecorded, []attendancemail.Mail{again}) != first {
		t.Fatal("a re-queued row took a new key")
	}

	e.mail.err = &mail.SendError{Status: 422}
	refused := e.welcome(e.guest)
	if report, err := e.svc.Pass(ctx); err != nil || report.Rejected != 1 || e.q.find(refused.ID).done != attendancemail.Rejected {
		t.Fatalf("%v %+v", err, report)
	}
	e.mail.err = mail.ErrMaybeDelivered
	unsure := e.welcome(e.member)
	if report, err := e.svc.Pass(ctx); err != nil || report.Uncertain != 1 || e.q.find(unsure.ID).done != attendancemail.Uncertain {
		t.Fatalf("%v %+v", err, report)
	}
	if !errors.Is(e.mail.err, mail.ErrMaybeDelivered) {
		t.Fatal("unreachable")
	}
}

// A row a day old is not sent.
func TestADayLateIsExpired(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, event.RuleCount)
	e.program(1)
	m := e.welcome(e.member)
	e.q.find(m.ID).m.CreatedAt = e.now.Add(-25 * time.Hour)
	if report, err := e.svc.Pass(ctx); err != nil || report.Expired != 1 || len(e.mail.sent) != 0 {
		t.Fatalf("%v %+v", err, report)
	}
}

// The welcome carries the organizer's note as it is, and cut at its web
// addresses for the template to link them (trailing punctuation left out);
// without a note both are empty and the section is not in the mail.
func TestWelcomeCarriesTheAttendeeNote(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, event.RuleNone)
	e.program(0)
	e.welcome(e.member)
	if _, err := e.svc.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	if got := e.mail.sent[0].vars; got["AttendeeNote"] != "" || asJSON(t, got["AttendeeNoteParts"]) != "[]" {
		t.Fatalf("no note: %v %s", got["AttendeeNote"], asJSON(t, got["AttendeeNoteParts"]))
	}

	note := "Wi-Fi: SKYLAB\nTarif: https://example.test/yol?a=1. Sonra D-001"
	e.ev.AttendeeNote = &note
	if _, err := e.events.Update(ctx, e.ev); err != nil {
		t.Fatal(err)
	}
	e.welcome(e.guest)
	if _, err := e.svc.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	got := e.mail.sent[1].vars
	if got["AttendeeNote"] != note {
		t.Fatalf("note %q", got["AttendeeNote"])
	}
	want := `[{"Text":"Wi-Fi: SKYLAB\nTarif: ","Url":""},{"Text":"https://example.test/yol?a=1","Url":"https://example.test/yol?a=1"},{"Text":". Sonra D-001","Url":""}]`
	if parts := asJSON(t, got["AttendeeNoteParts"]); parts != want {
		t.Fatalf("parts %s", parts)
	}
}

// A retry sends the rows of the first try under its key; a receipt queued
// since goes in a mail of its own, under its own key.
func TestRetryKeepsTheFirstTrysRowsAndKey(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, event.RuleCount)
	ids, _ := e.program(2)
	e.welcome(e.member)
	first := e.recorded(e.member, ids[1], e.now.Add(time.Second))
	e.mail.err = &mail.SendError{Status: 503}
	if report, err := e.svc.Pass(ctx); err != nil || report.Retried != 1 {
		t.Fatalf("%v %+v", err, report)
	}
	key := e.mail.sent[0].key
	// A check-in after the failed try; then the retry is due.
	later := e.recorded(e.member, ids[2], e.now)
	for _, r := range e.q.rows {
		r.retried = false
	}
	e.mail.err = nil
	if _, err := e.svc.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	if len(e.mail.sent) != 3 {
		t.Fatalf("mails %d", len(e.mail.sent))
	}
	retry, apart := e.mail.sent[1], e.mail.sent[2]
	if retry.key != key || retry.typ != mail.AttendanceWelcomeType || asJSON(t, retry.vars["Recorded"]) != `[{"Title":"Oturum B"}]` {
		t.Fatalf("retry %s %s", retry.key, asJSON(t, retry.vars["Recorded"]))
	}
	if apart.key == key || apart.typ != mail.AttendanceRecordedType || asJSON(t, apart.vars["Sessions"]) != `[{"Title":"Oturum C"}]` {
		t.Fatalf("apart %s %s", apart.typ, asJSON(t, apart.vars["Sessions"]))
	}
	if e.q.find(first.ID).done != attendancemail.Sent || e.q.find(later.ID).done != attendancemail.Sent {
		t.Fatal("rows left")
	}
}

// The claimed row dropped (its entry undone) while a receipt that went
// along was sent: the mail still counts in the minute.
func TestDroppedLeadStillCountsTheMail(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, event.RuleCount)
	ids, _ := e.program(1)
	lead := e.welcome(e.member)
	e.q.present[lead.ID] = false
	kept := e.recorded(e.member, ids[1], e.now.Add(time.Minute))
	if _, err := e.svc.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	if len(e.mail.sent) != 1 || e.mail.sent[0].typ != mail.AttendanceRecordedType {
		t.Fatalf("sent %+v", e.mail.sent)
	}
	if len(e.q.attempted) != 1 || e.q.attempted[0] != kept.ID {
		t.Fatalf("attempted %v", e.q.attempted)
	}
}

// On an Event with no live Session and a rule, the welcome says the entry
// counted for the certificate; with no rule it says nothing.
func TestSessionlessWelcomeCertificateLine(t *testing.T) {
	ctx := context.Background()
	for rule, want := range map[string]string{event.RuleOnce: attendancemail.SessionlessCertificateLine, event.RuleNone: ""} {
		e := newEnv(t, rule)
		e.q.progress = attendancemail.Progress{Attended: map[uuid.UUID]bool{}, Titles: map[uuid.UUID]string{}}
		e.welcome(e.member)
		if _, err := e.svc.Pass(ctx); err != nil {
			t.Fatal(err)
		}
		if got := e.mail.sent[0].vars["CertificateLine"]; got != want {
			t.Fatalf("%s: %q", rule, got)
		}
	}
}

// A retry SkyNotify refuses as a reused key went the first time: sent, not
// rejected.
func TestKeyReusedCountsAsSent(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, event.RuleCount)
	ids, _ := e.program(1)
	m := e.recorded(e.member, ids[1], e.now)
	e.mail.err = &mail.SendError{Status: 422, Code: "notification.idempotency_key_reused"}
	report, err := e.svc.Pass(ctx)
	if err != nil || report.Sent != 1 || report.Rejected != 0 || e.q.find(m.ID).done != attendancemail.Sent {
		t.Fatalf("%v %+v", err, report)
	}
}
