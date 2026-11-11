package feedback_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/feedback"
)

// A Ticket's feedback links (ticket 12): the mail's link for each invite
// while its target still takes it; answered ones stay, marked.
func TestPostgresTicketLinks(t *testing.T) {
	d := newInviteDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	sessionForm := uuid.New()
	eventURL := "https://forms.example.test/" + d.form.String()
	sessionURL := "https://forms.example.test/" + sessionForm.String() + "?x=1"
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := d.pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`UPDATE events SET feedback_mode = 'per_session', feedback_form_url = $2 WHERE id = $1`, d.event, eventURL)
	exec(`UPDATE sessions SET feedback_form_url = $2 WHERE id = $1`, d.session, sessionURL)
	// mail writes a feedback mail of the Ticket carrying the targets, done,
	// sent or not (SkyMail refused it, or it was dropped).
	mail := func(ticket uuid.UUID, sent bool, sessions ...*uuid.UUID) uuid.UUID {
		t.Helper()
		id := uuid.New()
		exec(`INSERT INTO feedback_mails (id, event_id, ticket_id, kind, next_attempt_at, done_at, sent) VALUES ($1, $2, $3, 'request', $4, $4, $5)`,
			id, d.event, ticket, now.Add(-time.Hour), sent)
		for _, session := range sessions {
			exec(`INSERT INTO feedback_mail_items (mail_id, event_id, session_id) VALUES ($1, $2, $3)`, id, d.event, session)
		}
		return id
	}
	memberEvent := d.invite(t, d.memberTicket, nil, d.form)
	memberSession := d.invite(t, d.memberTicket, &d.session, sessionForm)
	guestEvent := d.invite(t, d.guest, nil, d.form)
	guestSession := d.invite(t, d.guest, &d.session, sessionForm)
	mail(d.memberTicket, true, &d.session)
	mail(d.memberTicket, true, nil)
	// The guest's mail with the Event's form went; the one with the
	// Session's form was refused: its invite was written, no link.
	mail(d.guest, true, nil)
	refused := mail(d.guest, false, &d.session)
	// (The member's sent mail with the Session's form gives the guest
	// nothing: the mail must be the Ticket's own.)
	read := func() map[uuid.UUID][]feedback.TicketLink {
		t.Helper()
		links, err := d.store.TicketLinks(ctx, []uuid.UUID{d.memberTicket, d.guest, uuid.New()}, now)
		if err != nil {
			t.Fatal(err)
		}
		return links
	}
	link := func(formURL, code string) string {
		t.Helper()
		l, err := feedback.Link(formURL, code)
		if err != nil {
			t.Fatal(err)
		}
		return l
	}

	links := read()
	member := links[d.memberTicket]
	if len(links) != 2 || len(member) != 2 || len(links[d.guest]) != 1 {
		t.Fatalf("links %+v", links)
	}
	// The Sessions' forms first, the Event's last: the mail's order.
	if member[0].URL != link(sessionURL, memberSession.Code) || member[0].Title != "Açılış" || member[0].SessionID == nil || *member[0].SessionID != d.session || member[0].Answered {
		t.Fatalf("session link %+v", member[0])
	}
	if member[1].URL != link(eventURL, memberEvent.Code) || member[1].Title != "Hack" || member[1].SessionID != nil {
		t.Fatalf("event link %+v", member[1])
	}
	if links[d.guest][0].URL != link(eventURL, guestEvent.Code) {
		t.Fatalf("guest link %+v", links[d.guest])
	}

	// The refused mail tried again and sent: the guest's Session link comes.
	exec(`UPDATE feedback_mails SET sent = true WHERE id = $1`, refused)
	if guest := read()[d.guest]; len(guest) != 2 || guest[0].URL != link(sessionURL, guestSession.Code) {
		t.Fatalf("guest after sent %+v", guest)
	}
	exec(`UPDATE feedback_mails SET sent = false WHERE id = $1`, refused)

	// Answered: still there, marked.
	if ok, err := d.store.MarkAnswered(ctx, d.form, memberEvent.Code, nil); err != nil || !ok {
		t.Fatalf("answer %v %v", ok, err)
	}
	if member := read()[d.memberTicket]; len(member) != 2 || !member[1].Answered || member[0].Answered {
		t.Fatalf("answered %+v", member)
	}

	// The Event left the per_session mode: its Sessions' forms go.
	exec(`UPDATE events SET feedback_mode = 'event' WHERE id = $1`, d.event)
	if member := read()[d.memberTicket]; len(member) != 1 || member[0].SessionID != nil {
		t.Fatalf("event mode %+v", member)
	}
	exec(`UPDATE events SET feedback_mode = 'per_session' WHERE id = $1`, d.event)

	// A Session cancelled: its form goes.
	exec(`UPDATE sessions SET cancelled = true WHERE id = $1`, d.session)
	if member := read()[d.memberTicket]; len(member) != 1 || member[0].SessionID != nil {
		t.Fatalf("session cancelled %+v", member)
	}
	exec(`UPDATE sessions SET cancelled = false WHERE id = $1`, d.session)

	// The Event's form changed since the mail: the old code is not the new
	// form's.
	exec(`UPDATE events SET feedback_form_url = $2 WHERE id = $1`, d.event, "https://forms.example.test/"+uuid.NewString())
	if links := read(); len(links[d.memberTicket]) != 1 || links[d.memberTicket][0].SessionID == nil || len(links[d.guest]) != 0 {
		t.Fatalf("form changed %+v", links)
	}
	exec(`UPDATE events SET feedback_form_url = $2 WHERE id = $1`, d.event, eventURL)

	// The form closes later: still there. Its closing time came: gone.
	exec(`INSERT INTO feedback_form_closings (form_id, closes_at) VALUES ($1, $2)`, d.form, now.Add(time.Hour))
	if links := read(); len(links[d.memberTicket]) != 2 || len(links[d.guest]) != 1 {
		t.Fatalf("closes later %+v", links)
	}
	exec(`UPDATE feedback_form_closings SET closes_at = $2 WHERE form_id = $1`, d.form, now.Add(-time.Minute))
	if links := read(); len(links[d.memberTicket]) != 1 || len(links[d.guest]) != 0 {
		t.Fatalf("closed %+v", links)
	}
	exec(`DELETE FROM feedback_form_closings`)

	// Closed from the panel (its reminders cancelled): gone, also once it
	// opens again.
	exec(`UPDATE feedback_invites SET reminder_cancelled_at = now() WHERE id = $1`, guestEvent.ID)
	if links := read(); len(links[d.guest]) != 0 || len(links[d.memberTicket]) != 2 {
		t.Fatalf("cancelled %+v", links)
	}

	// The Session's day archived: its form goes. The Event archived:
	// nothing.
	exec(`UPDATE event_days SET archived_at = now() WHERE id = (SELECT event_day_id FROM sessions WHERE id = $1)`, d.session)
	if member := read()[d.memberTicket]; len(member) != 1 || member[0].SessionID != nil {
		t.Fatalf("day archived %+v", member)
	}
	exec(`UPDATE event_days SET archived_at = NULL`)
	exec(`UPDATE events SET archived_at = now() WHERE id = $1`, d.event)
	if links := read(); len(links) != 0 {
		t.Fatalf("event archived %+v", links)
	}
	exec(`UPDATE events SET archived_at = NULL WHERE id = $1`, d.event)

	// The Event cancelled: nothing.
	exec(`UPDATE events SET status = 'cancelled' WHERE id = $1`, d.event)
	if links := read(); len(links) != 0 {
		t.Fatalf("event cancelled %+v", links)
	}

	if links, err := d.store.TicketLinks(ctx, nil, now); err != nil || len(links) != 0 {
		t.Fatalf("none %+v %v", links, err)
	}
}
