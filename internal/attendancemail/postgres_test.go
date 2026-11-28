package attendancemail_test

import (
	"context"
	"io"
	"log"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/internal/attendancemail"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/mail"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

type pgEnv struct {
	pool    *pgxpool.Pool
	events  *event.PostgresStore
	tickets *ticket.PostgresStore
	users   *user.PostgresStore
	queue   *attendancemail.PostgresQueue
	ev      event.Event
	day     event.Day
	open    event.Session
	talk    event.Session
	guest   ticket.Ticket
	member  ticket.Ticket
}

const pgDay = "2031-03-14"

func newPGEnv(t *testing.T) pgEnv {
	t.Helper()
	ctx := context.Background()
	pool := testpostgres.Start(t)
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	e := pgEnv{pool: pool, events: event.NewPostgresStore(pool), tickets: ticket.NewPostgresStore(pool), users: user.NewPostgresStore(pool), queue: attendancemail.NewPostgresQueue(pool)}
	start := time.Date(2031, 3, 14, 7, 0, 0, 0, time.UTC)
	end := start.Add(10 * time.Hour)
	var err error
	if e.ev, err = e.events.Create(ctx, event.Event{Name: "GECEKODU", Location: "YTÜ", OwnerTeam: "WEBLAB", AttendanceRule: event.RuleCount, Status: event.StatusPublished, StartDate: &start, EndDate: &end}); err != nil {
		t.Fatal(err)
	}
	if e.day, err = e.events.CreateDay(ctx, event.Day{EventID: e.ev.ID, Name: "1. gün", StartDate: &start}); err != nil {
		t.Fatal(err)
	}
	for i, title := range []string{"Açılış", "Konuşma"} {
		s, f := start.Add(time.Duration(i)*time.Hour), start.Add(time.Duration(i)*time.Hour+45*time.Minute)
		sess, err := e.events.CreateSession(ctx, event.Session{EventDayID: e.day.ID, Title: title, SpeakerName: "Ada", SessionType: "TALK", OrderIndex: i, StartTime: &s, EndTime: &f, CertificateCounted: true})
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			e.open = sess
		} else {
			e.talk = sess
		}
	}
	if e.guest, err = e.tickets.Create(ctx, ticket.Ticket{EventID: e.ev.ID, TicketType: ticket.Guest, GuestFirstName: "Deniz", GuestLastName: "Ak", GuestEmail: "deniz@example.com"}); err != nil {
		t.Fatal(err)
	}
	ownerID := uuid.New()
	if _, _, err := e.users.Upsert(ctx, user.User{ID: ownerID, Email: "elif@example.com", FirstName: "Elif", LastName: "Kaya"}); err != nil {
		t.Fatal(err)
	}
	if e.member, err = e.tickets.Create(ctx, ticket.Ticket{EventID: e.ev.ID, TicketType: ticket.Registered, OwnerID: &ownerID}); err != nil {
		t.Fatal(err)
	}
	return e
}

// checkIn writes a Check-in on sess with the day's entry, as the door does.
func (e pgEnv) checkIn(t *testing.T, tk ticket.Ticket, sess event.Session) ticket.CheckIn {
	t.Helper()
	c, err := e.tickets.AddCheckIn(context.Background(), ticket.CheckIn{TicketID: tk.ID, EventDayID: e.day.ID, SessionID: sess.ID,
		Entry: &ticket.VenueEntry{EntryDate: pgDay, Source: ticket.EntryAttendance}})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

type queued struct {
	kind    string
	session *uuid.UUID
	outcome *string
	due     time.Time
}

func (e pgEnv) rows(t *testing.T, tk ticket.Ticket) []queued {
	t.Helper()
	rows, err := e.pool.Query(context.Background(), `SELECT kind, session_id, outcome, next_attempt_at FROM attendance_mails WHERE ticket_id = $1 ORDER BY kind DESC, created_at`, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []queued
	for rows.Next() {
		var q queued
		if err := rows.Scan(&q.kind, &q.session, &q.outcome, &q.due); err != nil {
			t.Fatal(err)
		}
		out = append(out, q)
	}
	return out
}

// The ticket store queues in the statement that writes: off, nothing; on,
// the day's first entry a welcome and each Check-in a receipt, two minutes
// on; a second scan nothing; the Event's switch off, nothing of that kind.
func TestStoreQueuesWithTheScan(t *testing.T) {
	ctx := context.Background()
	e := newPGEnv(t)
	e.checkIn(t, e.guest, e.open)
	if got := e.rows(t, e.guest); len(got) != 0 {
		t.Fatalf("off, queued %+v", got)
	}
	e.tickets.NotifyAttendance(true, true)
	before := time.Now()
	e.checkIn(t, e.member, e.open)
	got := e.rows(t, e.member)
	if len(got) != 2 || got[0].kind != attendancemail.KindWelcome || got[1].kind != attendancemail.KindRecorded || *got[1].session != e.open.ID {
		t.Fatalf("queued %+v", got)
	}
	if due := got[0].due.Sub(before); due < time.Minute || due > 3*time.Minute {
		t.Fatalf("due in %s", due)
	}
	// Another Session the same day: a receipt, no second welcome.
	e.checkIn(t, e.member, e.talk)
	// The same Session again: refused, nothing queued.
	if _, err := e.tickets.AddCheckIn(ctx, ticket.CheckIn{TicketID: e.member.ID, EventDayID: e.day.ID, SessionID: e.talk.ID}); err == nil {
		t.Fatal("a second check-in passed")
	}
	// A door scan at the Event's door the same day: refused, nothing queued.
	if _, err := e.tickets.AddVenueEntry(ctx, ticket.VenueEntry{TicketID: e.member.ID, EventID: e.ev.ID, EntryDate: pgDay, Source: ticket.EntryScan}); err == nil {
		t.Fatal("a second entry passed")
	}
	if got := e.rows(t, e.member); len(got) != 3 {
		t.Fatalf("after the second scans %+v", got)
	}
	// The Event's door the next day: a new welcome.
	if _, err := e.tickets.AddVenueEntry(ctx, ticket.VenueEntry{TicketID: e.member.ID, EventID: e.ev.ID, EntryDate: "2031-03-15", Source: ticket.EntryScan}); err != nil {
		t.Fatal(err)
	}
	if got := e.rows(t, e.member); len(got) != 4 {
		t.Fatalf("next day %+v", got)
	}

	off := false
	e.ev.AttendanceWelcomeMail = &off
	note := "Wi-Fi: SKYLAB\nParola: ornek"
	e.ev.AttendeeNote = &note
	if _, err := e.events.Update(ctx, e.ev); err != nil {
		t.Fatal(err)
	}
	e.checkIn(t, e.guest, e.talk)
	if got := e.rows(t, e.guest); len(got) != 1 || got[0].kind != attendancemail.KindRecorded {
		t.Fatalf("welcome switched off: %+v", got)
	}
	if got, _ := e.events.Get(ctx, e.ev.ID); got.AttendeeNote == nil || *got.AttendeeNote != note {
		t.Fatalf("note %v", got.AttendeeNote)
	}
	// A write that leaves the note keeps it.
	e.ev.AttendeeNote = nil
	if _, err := e.events.Update(ctx, e.ev); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.events.GetIncludingArchived(ctx, e.ev.ID); got.AttendeeNote == nil || *got.AttendeeNote != note {
		t.Fatalf("kept note %v", got.AttendeeNote)
	}
	if got, _ := e.events.Get(ctx, e.ev.ID); got.AttendanceWelcomeMail == nil || *got.AttendanceWelcomeMail || got.AttendanceRecordedMail == nil || !*got.AttendanceRecordedMail {
		t.Fatalf("switches %v %v", got.AttendanceWelcomeMail, got.AttendanceRecordedMail)
	}
}

// fixedMail records the mails and answers nil.
type fixedMail struct{ sent []sent }

func (f *fixedMail) AttendanceMail(_ context.Context, typ, key string, holder mail.TicketHolder, vars map[string]any, _ mail.NotificationSource, _ *time.Time) error {
	f.sent = append(f.sent, sent{typ, key, holder, vars})
	return nil
}

// The worker on the database: nothing before the two minutes; then the
// welcome with its receipt as one mail; an undone Check-in is dropped, and
// scanned again it is queued again and sent; at most PerMinute a minute.
func TestWorkerOnPostgres(t *testing.T) {
	ctx := context.Background()
	e := newPGEnv(t)
	e.tickets.NotifyAttendance(true, true)
	now := time.Now()
	sink := &fixedMail{}
	svc, err := attendancemail.NewService(attendancemail.Config{
		Queue: e.queue, Tickets: e.tickets, Events: e.events, Users: e.users, Mail: sink,
		WelcomeType: mail.AttendanceWelcomeType, RecordedType: mail.AttendanceRecordedType, PerMinute: 1,
		Now: func() time.Time { return now }, Logger: log.New(io.Discard, "", 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	e.checkIn(t, e.guest, e.open)
	undo := e.checkIn(t, e.member, e.open)
	if report, err := svc.Pass(ctx); err != nil || report.Sent != 0 {
		t.Fatalf("before the delay: %v %+v", err, report)
	}
	// The member's Check-in is undone; their welcome stays (an attendance
	// entry goes with its Check-in, docs/venue-entry.md: then it drops too).
	if ok, err := e.tickets.RemoveCheckInByID(ctx, undo.ID, nil); err != nil || !ok {
		t.Fatalf("undo %v %v", ok, err)
	}
	now = now.Add(3 * time.Minute)
	report, err := svc.Pass(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// One a minute: the first welcome goes, with its receipt.
	if report.Sent != 1 || !report.Limited || len(sink.sent) != 1 {
		t.Fatalf("first minute %+v %d", report, len(sink.sent))
	}
	now = now.Add(time.Minute)
	if _, err := svc.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	var guestMail *sent
	for i := range sink.sent {
		if sink.sent[i].holder.Email == "deniz@example.com" {
			guestMail = &sink.sent[i]
		}
	}
	if guestMail == nil || guestMail.typ != mail.AttendanceWelcomeType || asJSON(t, guestMail.vars["Recorded"]) != `[{"Title":"Açılış"}]` ||
		guestMail.vars["DayLabel"] != "14 Mart Cuma" || guestMail.vars["Attended"] != 1 || guestMail.vars["CountedTotal"] != 2 {
		t.Fatalf("guest mail %+v", guestMail)
	}
	for _, q := range e.rows(t, e.member) {
		if q.outcome == nil || *q.outcome != string(attendancemail.Dropped) {
			t.Fatalf("member rows %+v", e.rows(t, e.member))
		}
	}
	for _, s := range sink.sent {
		if s.holder.Sub != uuid.Nil {
			t.Fatalf("the undone member was mailed: %+v", s)
		}
	}
	// Scanned again: the dropped rows are queued again and go.
	e.checkIn(t, e.member, e.open)
	now = now.Add(5 * time.Minute)
	if _, err := svc.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	if last := sink.sent[len(sink.sent)-1]; last.holder.Sub != *e.member.OwnerID || last.vars["FirstName"] != "Elif" {
		t.Fatalf("after the new scan %+v", last)
	}
	// A third scan after it went queues nothing.
	if ok, err := e.tickets.RemoveCheckIn(ctx, e.member.ID, e.open.ID, nil); err != nil || !ok {
		t.Fatal(err)
	}
	e.checkIn(t, e.member, e.open)
	n := len(sink.sent)
	now = now.Add(5 * time.Minute)
	if _, err := svc.Pass(ctx); err != nil || len(sink.sent) != n {
		t.Fatalf("told twice: %v %d", err, len(sink.sent)-n)
	}
	// Done rows go after Keep.
	now = now.Add(attendancemail.Keep + time.Hour)
	if report, err := svc.Pass(ctx); err != nil || report.Purged == 0 {
		t.Fatalf("purge %v %+v", err, report)
	}
}

// Progress reads the programme, the Check-ins and the titles.
func TestProgressOnPostgres(t *testing.T) {
	ctx := context.Background()
	e := newPGEnv(t)
	e.checkIn(t, e.guest, e.talk)
	p, err := e.queue.Progress(ctx, e.ev.ID, e.guest.ID, []uuid.UUID{e.talk.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Days) != 1 || len(p.Sessions) != 2 || p.Sessions[0].Title != "Açılış" || !p.Sessions[1].Counted || !p.Attended[e.talk.ID] || p.Attended[e.open.ID] || p.Titles[e.talk.ID] != "Konuşma" {
		t.Fatalf("progress %+v", p)
	}
}

// A receipt whose Session was cancelled before it went is dropped; the
// welcome goes alone. A row's first try keys it: the key is kept on the row.
func TestCancelledSessionDropsItsReceipt(t *testing.T) {
	ctx := context.Background()
	e := newPGEnv(t)
	e.tickets.NotifyAttendance(true, true)
	now := time.Now()
	sink := &fixedMail{}
	svc, err := attendancemail.NewService(attendancemail.Config{
		Queue: e.queue, Tickets: e.tickets, Events: e.events, Users: e.users, Mail: sink,
		WelcomeType: mail.AttendanceWelcomeType, RecordedType: mail.AttendanceRecordedType,
		Now: func() time.Time { return now }, Logger: log.New(io.Discard, "", 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	e.checkIn(t, e.guest, e.open)
	e.open.Cancelled = true
	if _, err := e.events.UpdateSession(ctx, e.open); err != nil {
		t.Fatal(err)
	}
	now = now.Add(3 * time.Minute)
	if _, err := svc.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	if len(sink.sent) != 1 || sink.sent[0].typ != mail.AttendanceWelcomeType || asJSON(t, sink.sent[0].vars["Recorded"]) != "[]" {
		t.Fatalf("sent %+v", sink.sent)
	}
	var outcome, key string
	if err := e.pool.QueryRow(ctx, `SELECT outcome, COALESCE(mail_key, '') FROM attendance_mails WHERE ticket_id = $1 AND kind = 'recorded'`, e.guest.ID).Scan(&outcome, &key); err != nil || outcome != "dropped" || key != "" {
		t.Fatalf("receipt %q %q %v", outcome, key, err)
	}
	if err := e.pool.QueryRow(ctx, `SELECT COALESCE(mail_key, '') FROM attendance_mails WHERE ticket_id = $1 AND kind = 'welcome'`, e.guest.ID).Scan(&key); err != nil || key != sink.sent[0].key {
		t.Fatalf("welcome key %q %v", key, err)
	}
}
