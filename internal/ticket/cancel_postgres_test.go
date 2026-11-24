package ticket_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/competition"
	"github.com/skylab-kulubu/core-backend/internal/dashboard"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/teamaudit"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
)

// On the database: a cancellation keeps the row with its status, drops the
// mail still waiting and leaves the readers (the list, the door's search
// and code, the store's check-in, the counts, the Competition's candidates
// and Entrants); a restore gives a new key and QR version and queues the
// mail under the next number. Both are Team audit rows the table takes.
func TestCancelAndRestoreOnPostgres(t *testing.T) {
	e := newResendEnv(t)
	ctx := context.Background()
	dir := identity.NewMemory()
	weblab := identity.Group{ID: uuid.NewString(), Name: "WEBLAB", Path: "/UYELER/ARGE/WEBLAB"}
	dir.PutGroup(weblab)
	audit := teamaudit.NewPostgres(e.pool)
	svc := ticket.NewService(e.store, e.events, authz.NewAuthorizer(authz.DefaultPolicy()), e.users, e.signer, dir, audit)
	lead := authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}
	as := teamaudit.WithActor(ctx, teamaudit.Actor{ID: uuid.MustParse(lead.ID), Client: "core-frontend", CorrelationID: "req-1"})

	// The first mail is still waiting.
	if _, err := e.pool.Exec(ctx, `INSERT INTO ticket_notifications (ticket_id) VALUES ($1)`, e.guest.ID); err != nil {
		t.Fatal(err)
	}
	before, err := e.store.Get(ctx, e.guest.ID)
	if err != nil || before.Status != ticket.StatusActive {
		t.Fatalf("before %+v %v", before, err)
	}
	cancelled, err := svc.CancelTicket(as, lead, e.guest.ID, "Yanlış kayıt")
	if err != nil {
		t.Fatal(err)
	}
	if cancelled.Status != ticket.StatusCancelled || cancelled.CancelledBy != ticket.CancelledByOperator ||
		cancelled.CancelReason != "Yanlış kayıt" || cancelled.CancelledAt == nil {
		t.Fatalf("cancelled %+v", cancelled)
	}
	if _, ok := e.queued(t, e.guest.ID); ok {
		t.Fatal("the waiting mail stayed")
	}
	if listed, _ := e.store.ListByEvent(ctx, e.ev.ID); len(listed) != 0 {
		t.Fatalf("active list %+v", listed)
	}
	if listed, _ := e.store.ListByEventStatus(ctx, e.ev.ID, []ticket.Status{ticket.StatusCancelled}); len(listed) != 1 {
		t.Fatalf("cancelled list %+v", listed)
	}
	if _, err := svc.ResolveAndCheckIn(ctx, lead, e.sess.ID, ticket.DoorCheckInTarget{Query: before.Code}); !errors.Is(err, ticket.ErrTicketCancelled) {
		t.Fatalf("code at the door: %v", err)
	}
	if _, err := svc.ResolveAndCheckIn(ctx, lead, e.sess.ID, ticket.DoorCheckInTarget{Query: "ada@example.com"}); !errors.Is(err, ticket.ErrTicketCancelled) {
		t.Fatalf("e-mail at the door: %v", err)
	}
	if found, err := svc.SearchDoorAttendees(ctx, lead, e.ev.ID, "ada"); err != nil || len(found) != 0 {
		t.Fatalf("door search %+v %v", found, err)
	}
	if _, err := e.store.AddCheckIn(ctx, ticket.CheckIn{TicketID: e.guest.ID, SessionID: e.sess.ID, EventDayID: e.sess.EventDayID}); !errors.Is(err, ticket.ErrTicketCancelled) {
		t.Fatalf("store check-in: %v", err)
	}
	counts, err := dashboard.NewPostgresStore(e.pool).TicketCounts(ctx, []uuid.UUID{e.ev.ID})
	if err != nil || counts[e.ev.ID].Applications != 0 {
		t.Fatalf("counts %+v %v", counts, err)
	}
	comp := competition.NewPostgresStore(e.pool)
	compID := uuid.New()
	if _, err := e.pool.Exec(ctx, `INSERT INTO competitions (id, event_id, name) VALUES ($1, $2, 'Yarışma')`, compID, e.ev.ID); err != nil {
		t.Fatal(err)
	}
	if candidates, err := comp.Candidates(ctx, compID, e.ev.ID); err != nil || len(candidates) != 0 {
		t.Fatalf("candidates %+v %v", candidates, err)
	}
	if refs, err := comp.Tickets(ctx, []uuid.UUID{e.guest.ID}); err != nil || len(refs) != 0 {
		t.Fatalf("entrant tickets %+v %v", refs, err)
	}
	// Again: the same answer, no second audit row.
	if again, err := svc.CancelTicket(as, lead, e.guest.ID, ""); err != nil || !again.CancelledAt.Equal(*cancelled.CancelledAt) {
		t.Fatalf("again %+v %v", again, err)
	}

	restored, err := svc.RestoreTicket(as, lead, e.guest.ID)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := e.store.Get(ctx, e.guest.ID)
	if restored.Status != ticket.StatusActive || after.CancelledAt != nil || after.CancelReason != "" ||
		after.AccessKey == before.AccessKey || after.QRVersion != before.QRVersion+1 {
		t.Fatalf("restored %+v", after)
	}
	if _, err := e.store.GetByAccessKey(ctx, before.AccessKey); !errors.Is(err, ticket.ErrNotFound) {
		t.Fatalf("old key: %v", err)
	}
	if n, ok := e.queued(t, e.guest.ID); !ok || n != 1 || e.resends(t, e.guest.ID) != 1 {
		t.Fatalf("restored mail queued %d %v", n, ok)
	}
	if candidates, err := comp.Candidates(ctx, compID, e.ev.ID); err != nil || len(candidates) != 1 {
		t.Fatalf("candidates after restore %+v %v", candidates, err)
	}

	var rows []string
	r, err := e.pool.Query(ctx, `SELECT action || ':' || outcome || ':' || actor_basis FROM team_audit_records ORDER BY occurred_at`)
	if err != nil {
		t.Fatal(err)
	}
	for r.Next() {
		var row string
		if err := r.Scan(&row); err != nil {
			t.Fatal(err)
		}
		rows = append(rows, row)
	}
	r.Close()
	if len(rows) != 2 || rows[0] != "ticket.cancel:ok:leader" || rows[1] != "ticket.restore:ok:leader" {
		t.Fatalf("audit rows %v", rows)
	}
}

// A Ticket with a check-in, or of a Competition's current Entrant, is not
// cancelled; a withdrawn Entrant's is. The refusal is a failed audit row.
func TestCancelRefusalsOnPostgres(t *testing.T) {
	e := newResendEnv(t)
	ctx := context.Background()
	compID, entrantID := uuid.New(), uuid.New()
	for _, step := range []struct {
		q    string
		args []any
	}{
		{`INSERT INTO competitions (id, event_id, name) VALUES ($1, $2, 'Yarışma')`, []any{compID, e.ev.ID}},
		{`INSERT INTO competition_entrants (id, competition_id) VALUES ($1, $2)`, []any{entrantID, compID}},
		{`INSERT INTO competition_entrant_members (entrant_id, competition_id, ticket_id) VALUES ($1, $2, $3)`, []any{entrantID, compID, e.guest.ID}},
	} {
		if _, err := e.pool.Exec(ctx, step.q, step.args...); err != nil {
			t.Fatalf("%s: %v", step.q, err)
		}
	}
	if _, err := e.svc.CancelTicket(ctx, weblabLead, e.guest.ID, ""); !errors.Is(err, ticket.ErrTicketInCompetition) {
		t.Fatalf("entrant: %v", err)
	}
	if _, err := e.pool.Exec(ctx, `UPDATE competition_entrants SET status = 'withdrawn' WHERE id = $1`, entrantID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.CheckIn(ctx, weblabLead, e.guest.ID, e.sess.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.CancelTicket(ctx, weblabLead, e.guest.ID, ""); !errors.Is(err, ticket.ErrTicketCheckedIn) {
		t.Fatalf("checked in: %v", err)
	}
	if err := e.svc.UndoCheckIn(ctx, weblabLead, e.guest.ID, e.sess.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := e.svc.CancelTicket(ctx, weblabLead, e.guest.ID, ""); err != nil || got.Status != ticket.StatusCancelled || got.CancelReason != "" {
		t.Fatalf("after the undo %+v %v", got, err)
	}
}

// Registering again on the database reopens the row (one Ticket per person
// and Event stays) and queues "Biletin hazır" under the next number; a
// publication tells only the active Tickets.
func TestRegisteringAgainAndPublishingOnPostgres(t *testing.T) {
	e := newResendEnv(t)
	ctx := context.Background()
	before, _ := e.store.Get(ctx, e.guest.ID)
	if _, err := e.svc.CancelTicket(ctx, weblabLead, e.guest.ID, ""); err != nil {
		t.Fatal(err)
	}
	applied, err := e.svc.ApplyGuest(ctx, anonymous, e.ev.ID, ticket.GuestInfo{FirstName: "Ada", LastName: "Lovelace", Email: "ada@example.com"})
	if err != nil || applied.Result != ticket.GuestCreated {
		t.Fatalf("again %+v %v", applied, err)
	}
	after, _ := e.store.Get(ctx, e.guest.ID)
	if after.Status != ticket.StatusActive || after.AccessKey == before.AccessKey || after.QRVersion != before.QRVersion+1 {
		t.Fatalf("reopened %+v", after)
	}
	if n, ok := e.queued(t, e.guest.ID); !ok || n != 1 {
		t.Fatalf("mail %d %v", n, ok)
	}

	start := time.Now().Add(72 * time.Hour)
	draft, err := e.events.Create(ctx, event.Event{Name: "Taslak", Location: "YTÜ", OwnerTeam: "WEBLAB", StartDate: &start, Status: event.StatusDraft})
	if err != nil {
		t.Fatal(err)
	}
	keep, err := e.store.Create(ctx, ticket.Ticket{EventID: draft.ID, TicketType: ticket.Guest, GuestFirstName: "A", GuestLastName: "B", GuestEmail: "a@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	gone, err := e.store.Create(ctx, ticket.Ticket{EventID: draft.ID, TicketType: ticket.Guest, GuestFirstName: "C", GuestLastName: "D", GuestEmail: "c@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.store.Cancel(ctx, gone.ID, ticket.CancelledByOperator, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `DELETE FROM ticket_notifications`); err != nil {
		t.Fatal(err)
	}
	e.events.NotifyTicketsOnPublish(true)
	if ok, err := e.events.Publish(ctx, draft.ID, nil, true); err != nil || !ok {
		t.Fatalf("publish %v %v", ok, err)
	}
	if _, ok := e.queued(t, keep.ID); !ok {
		t.Fatal("the active Ticket was not told")
	}
	if _, ok := e.queued(t, gone.ID); ok {
		t.Fatal("the cancelled Ticket was told")
	}
}
