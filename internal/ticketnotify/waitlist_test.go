package ticketnotify_test

import (
	"context"
	"io"
	"log"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/mail"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/ticketnotify"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

type statusCall struct {
	typ, key string
	holder   mail.TicketHolder
	vars     map[string]string
}

// statusMail is fakeMail that also sends the waiting list's mails.
type statusMail struct {
	fakeMail
	status []statusCall
}

func (m *statusMail) TicketStatus(_ context.Context, typ, key string, holder mail.TicketHolder, vars map[string]string, _ mail.NotificationSource, _ *time.Time, _ []mail.Attachment) error {
	m.status = append(m.status, statusCall{typ, key, holder, vars})
	return nil
}

func newStatusEnv(t *testing.T, m ticketnotify.Mailer) *env {
	t.Helper()
	e := newEnv(t)
	worker, err := ticketnotify.NewService(ticketnotify.Config{
		Queue: e.queue, Tickets: e.tickets, Events: e.events, Users: e.users, Mail: m,
		Page:   ticket.PageConfig{URL: "https://yildizskylab.com/bilet/" + ticket.KeyPlaceholder},
		Now:    func() time.Time { return e.now },
		Logger: log.New(io.Discard, "", 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	e.worker = worker
	return e
}

func (e *env) queueKind(id uuid.UUID, kind string) {
	e.queue.mu.Lock()
	defer e.queue.mu.Unlock()
	for i := range e.queue.rows {
		if e.queue.rows[i].TicketID == id {
			e.queue.rows[i].Kind = kind
		}
	}
}

// "Bekleme listesindesin" goes as event.waitlist-joined with its own key
// and SkyMail's variable set (no QR, no code); "Yer açıldı" as
// event.waitlist-promoted with the ticket's variables. Each goes only while
// the Ticket is in the state it tells of.
func TestWaitlistMails(t *testing.T) {
	m := &statusMail{}
	e := newStatusEnv(t, m)
	key1, _ := ticket.NewAccessKey()
	key2, _ := ticket.NewAccessKey()
	waiting := e.member(user.AccountActive)
	waiting.Status, waiting.AccessKey, waiting.Code, waiting.QRVersion = ticket.StatusWaitlisted, key1, "K7M2-QX9F", 2
	e.tickets[waiting.ID] = waiting
	e.queueKind(waiting.ID, ticketnotify.KindWaitlisted)
	promoted := e.guest("can@example.com")
	promoted.Status, promoted.AccessKey, promoted.Code, promoted.QRVersion = ticket.StatusActive, key2, "AAAA-BBBB", 1
	e.tickets[promoted.ID] = promoted
	e.queueKind(promoted.ID, ticketnotify.KindPromoted)
	// A waiting mail whose Ticket was promoted meanwhile, and a promoted
	// one whose Ticket was cancelled: neither goes.
	stale := e.member(user.AccountActive)
	stale.Status = ticket.StatusActive
	e.tickets[stale.ID] = stale
	e.queueKind(stale.ID, ticketnotify.KindWaitlisted)
	gone := e.guest("gone@example.com")
	gone.Status = ticket.StatusCancelled
	e.tickets[gone.ID] = gone
	e.queueKind(gone.ID, ticketnotify.KindPromoted)

	if r := e.pass(t); r.Sent != 2 || r.Dropped != 2 {
		t.Fatalf("report %+v", r)
	}
	if len(m.calls) != 0 || len(m.status) != 2 {
		t.Fatalf("created %d, status %d", len(m.calls), len(m.status))
	}
	byType := map[string]statusCall{}
	for _, c := range m.status {
		byType[c.typ] = c
	}
	joined := byType[mail.WaitlistJoinedType]
	if joined.key != "core:event.waitlist-joined:"+waiting.ID.String()+":q2" || joined.holder.Sub != *waiting.OwnerID {
		t.Fatalf("joined %+v", joined)
	}
	want := []string{"EventId", "EventName", "EventDate", "OwnerTeam", "Venue", "TicketUrl"}
	if len(joined.vars) != len(want) || joined.vars["TicketUrl"] != "https://yildizskylab.com/bilet/"+key1 {
		t.Fatalf("joined vars %v", joined.vars)
	}
	for _, name := range want {
		if _, ok := joined.vars[name]; !ok {
			t.Fatalf("joined vars %v lack %s", joined.vars, name)
		}
	}
	got := byType[mail.WaitlistPromotedType]
	if got.key != "core:event.waitlist-promoted:"+promoted.ID.String()+":q1" || got.holder.Email != "can@example.com" ||
		got.vars["TicketCode"] != "AAAA-BBBB" || got.vars["TicketUrl"] == "" {
		t.Fatalf("promoted %+v", got)
	}
}

// A mailer that cannot send the waiting list's mails drops those rows.
func TestWaitlistMailsWithoutAStatusMailer(t *testing.T) {
	e := newEnv(t)
	waiting := e.guest("can@example.com")
	waiting.Status = ticket.StatusWaitlisted
	e.tickets[waiting.ID] = waiting
	e.queueKind(waiting.ID, ticketnotify.KindWaitlisted)
	if r := e.pass(t); r.Dropped != 1 || len(e.mail.calls) != 0 {
		t.Fatalf("report %+v, calls %d", r, len(e.mail.calls))
	}
}
