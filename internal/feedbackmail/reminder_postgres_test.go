package feedbackmail_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/feedback"
	"github.com/skylab-kulubu/core-backend/internal/mail"
)

const days3 = 3 * 24 * time.Hour

// sendDay runs the day of the fixture: Grace's mail (A) at 13:06, Ada's
// (A, B, the Event) at 17:06.
func (f *fixture) sendDay(t *testing.T) (grace, ada sent) {
	t.Helper()
	f.pass(t, at(13, 1))
	f.pass(t, at(13, 6))
	f.pass(t, at(17, 1))
	f.pass(t, at(17, 6))
	for _, m := range f.mail.take() {
		switch m.to {
		case "grace@example.test":
			grace = m
		case "ada@example.test":
			ada = m
		}
	}
	if len(grace.items) != 1 || len(ada.items) != 3 {
		t.Fatalf("the day's mails: Grace %+v, Ada %+v", grace, ada)
	}
	return grace, ada
}

func (f *fixture) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatal(err)
	}
}

// Three days after each mail, one reminder with the same links, of the
// forms not answered; never twice.
func TestReminderOnceWithTheSameLinks(t *testing.T) {
	f := newFixture(t)
	grace, ada := f.sendDay(t)
	// Ada answers B with her link.
	if ok, err := f.store.MarkAnswered(context.Background(), f.formB, codeOf(t, ada.items[1].URL, f.formB), nil); err != nil || !ok {
		t.Fatalf("mark %v %v", ok, err)
	}
	if r := f.pass(t, at(13, 5).Add(days3)); r.Reminders != 0 || len(f.mail.take()) != 0 {
		t.Fatalf("before three days %+v", r)
	}
	r := f.pass(t, at(13, 7).Add(days3))
	m := f.mail.take()
	if r.Reminders != 1 || r.Reminded != 1 || len(m) != 1 || m[0].kind != mail.FeedbackReminder || m[0].to != "grace@example.test" ||
		!slices.Equal(titles(m[0].items), []string{"Açılış"}) || m[0].items[0].URL != grace.items[0].URL {
		t.Fatalf("Grace's reminder %+v %+v", r, m)
	}
	f.pass(t, at(17, 7).Add(days3))
	m = f.mail.take()
	if len(m) != 1 || m[0].to != "ada@example.test" || !slices.Equal(titles(m[0].items), []string{"Açılış", "Gecekodu"}) ||
		m[0].items[0].URL != ada.items[0].URL || m[0].items[1].URL != ada.items[2].URL {
		t.Fatalf("Ada's reminder %+v", m)
	}
	var reminded int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM feedback_invites WHERE reminded_at IS NOT NULL`).Scan(&reminded); err != nil || reminded != 3 {
		t.Fatalf("reminded %d %v", reminded, err)
	}
	if r := f.pass(t, at(23, 0).Add(days3+24*time.Hour)); r.Reminders != 0 || r.Sent != 0 {
		t.Fatalf("again %+v", r)
	}
}

// Who answered every form gets no reminder; nor does a mail SkyMail
// refused, nor one whose reminder is more than a week late.
func TestReminderSkips(t *testing.T) {
	f := newFixture(t)
	f.mail.fail = []error{&mail.SendError{Status: 422}} // Grace's mail
	_ = f.pass(t, at(13, 1))
	_ = f.pass(t, at(13, 6))
	f.pass(t, at(17, 1))
	f.pass(t, at(17, 6))
	for _, m := range f.mail.take() {
		for _, it := range m.items {
			form := f.formEvent
			switch it.Title {
			case "Açılış":
				form = f.formA
			case "Go atölyesi":
				form = f.formB
			}
			if _, err := f.store.MarkAnswered(context.Background(), form, codeOf(t, it.URL, form), nil); err != nil {
				t.Fatal(err)
			}
		}
	}
	if r := f.pass(t, at(23, 0).Add(days3)); r.Reminders != 0 || r.Sent != 0 {
		t.Fatalf("all answered, Grace refused %+v", r)
	}
	late := newFixture(t)
	late.sendDay(t)
	if r := late.pass(t, at(18, 0).Add(days3+8*24*time.Hour)); r.Reminders != 0 {
		t.Fatalf("eleven days on %+v", r)
	}
}

// The form closed in the panel or in Forms, at once or at its closing time:
// no reminder, even after it opens again. The box off, or the Event
// cancelled: none either.
func TestReminderCancelled(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.sendDay(t)
	// The panel closes the Event's form; Forms closes B, then opens it again.
	if n, err := f.store.CancelReminders(ctx, feedback.Target{EventID: f.event}, at(18, 0)); err != nil || n != 1 {
		t.Fatalf("panel %d %v", n, err)
	}
	if _, err := f.store.ReportFormState(ctx, f.formB, false, nil, at(18, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.ReportFormState(ctx, f.formB, true, nil, at(19, 0)); err != nil {
		t.Fatal(err)
	}
	// A closes the day before Grace's reminder falls due.
	closes := at(12, 0).Add(2 * 24 * time.Hour)
	if n, err := f.store.ReportFormState(ctx, f.formA, true, &closes, at(19, 0)); err != nil || n != 0 {
		t.Fatalf("A closing later %d %v", n, err)
	}
	f.pass(t, closes.Add(time.Minute))
	var cancelled int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM feedback_invites WHERE reminder_cancelled_at IS NOT NULL`).Scan(&cancelled); err != nil || cancelled != 4 {
		t.Fatalf("cancelled at A's closing: %d %v", cancelled, err)
	}
	if r := f.pass(t, at(23, 0).Add(days3)); r.Reminders != 0 || len(f.mail.take()) != 0 {
		t.Fatalf("every form closed %+v", r)
	}

	// The box turned off after the mail.
	g := newFixture(t)
	g.sendDay(t)
	g.exec(t, `UPDATE sessions SET feedback_auto_send = false WHERE id = $1`, g.sessionA)
	g.exec(t, `UPDATE events SET feedback_auto_send = false WHERE id = $1`, g.event)
	g.pass(t, at(13, 7).Add(days3))
	g.pass(t, at(17, 7).Add(days3))
	if m := g.mail.take(); len(m) != 1 || !slices.Equal(titles(m[0].items), []string{"Go atölyesi"}) {
		t.Fatalf("box off %+v", m)
	}

	// The Event cancelled.
	h := newFixture(t)
	h.sendDay(t)
	h.exec(t, `UPDATE events SET status = 'cancelled', cancelled_at = now() WHERE id = $1`, h.event)
	h.pass(t, at(13, 7).Add(days3))
	if r := h.pass(t, at(17, 7).Add(days3)); r.Sent != 0 || len(h.mail.take()) != 0 {
		t.Fatalf("cancelled %+v", r)
	}
}

// "Şimdi gönder" with the box off sends the form, but no reminder follows:
// the reminder goes on the schedule's terms. With the box on it does.
func TestReminderOfSendNowNeedsTheBox(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.exec(t, `UPDATE events SET feedback_form_url = '' WHERE id = $1`, f.event)
	f.exec(t, `UPDATE sessions SET feedback_auto_send = false WHERE id = $1`, f.sessionB)
	for _, s := range []uuid.UUID{f.sessionB, f.sessionA} {
		if ok, _, err := f.store.SendNow(ctx, feedback.Target{EventID: f.event, SessionID: &s}, at(12, 0)); err != nil || !ok {
			t.Fatalf("send now %v %v", ok, err)
		}
	}
	if r := f.pass(t, at(12, 1)); r.Sent != 2 {
		t.Fatalf("send %+v", r)
	}
	f.mail.take()
	f.pass(t, at(12, 2).Add(days3))
	m := f.mail.take()
	if len(m) != 2 {
		t.Fatalf("reminders %+v", m)
	}
	for _, r := range m {
		if r.kind != mail.FeedbackReminder || !slices.Equal(titles(r.items), []string{"Açılış"}) {
			t.Fatalf("reminder %+v", r)
		}
	}
}

// One address, one reminder: Ada's member and guest Tickets' reminders go
// in one mail.
func TestReminderOneMailAnAddress(t *testing.T) {
	f := newFixture(t)
	f.adaAsGuest(t)
	f.pass(t, at(13, 1))
	f.pass(t, at(13, 6))
	f.pass(t, at(17, 1))
	f.pass(t, at(17, 6))
	f.mail.take()
	f.pass(t, at(17, 7).Add(days3))
	ada := to(f.mail.take(), "ada@example.test")
	if len(ada) != 1 || len(ada[0].items) != 5 || ada[0].kind != mail.FeedbackReminder {
		t.Fatalf("Ada's reminders %+v", ada)
	}
}

// With the reminder's template off the reminders wait; they go once it is
// set, and the requests go meanwhile.
func TestReminderTemplateOff(t *testing.T) {
	f := newFixture(t)
	f.mail.off = map[mail.FeedbackKind]bool{mail.FeedbackReminder: true}
	f.sendDay(t)
	if r := f.pass(t, at(17, 7).Add(days3)); r.Reminders != 2 || r.Sent != 0 {
		t.Fatalf("off %+v", r)
	}
	f.mail.mu.Lock()
	f.mail.off = nil
	f.mail.mu.Unlock()
	if r := f.pass(t, at(17, 8).Add(days3)); r.Reminded != 2 {
		t.Fatalf("on %+v", r)
	}
}

// Forms' word on a form no Event or Session takes as its feedback form is
// not kept.
func TestFormStateOfAnotherForm(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	if n, err := f.store.ReportFormState(ctx, uuid.New(), false, nil, at(18, 0)); err != nil || n != 0 {
		t.Fatalf("other form %d %v", n, err)
	}
	if _, err := f.store.ReportFormState(ctx, f.formEvent, false, nil, at(12, 0)); err != nil {
		t.Fatal(err)
	}
	var rows int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM feedback_form_closings`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("closings %d %v", rows, err)
	}
	// Closed before the mail went: its invites written later get no
	// reminder.
	f.sendDay(t)
	f.pass(t, at(17, 7).Add(days3))
	for _, m := range f.mail.take() {
		if slices.Contains(titles(m.items), "Gecekodu") {
			t.Fatalf("closed form reminded %+v", m)
		}
	}
}
