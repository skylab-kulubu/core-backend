package feedback_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/internal/feedback"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

type inviteDB struct {
	pool                 *pgxpool.Pool
	store                *feedback.PostgresStore
	event, session, form uuid.UUID
	member               uuid.UUID
	memberTicket, guest  uuid.UUID
}

func newInviteDB(t *testing.T) inviteDB {
	t.Helper()
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	d := inviteDB{pool: pool, store: feedback.NewPostgresStore(pool), event: uuid.New(), session: uuid.New(), form: uuid.New(),
		member: uuid.New(), memberTicket: uuid.New(), guest: uuid.New()}
	if _, _, err := user.NewService(user.NewPostgresStore(pool)).Ensure(ctx, d.member, user.Profile{Email: "uye@example.test"}); err != nil {
		t.Fatal(err)
	}
	day := uuid.New()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO events (id, name, location, owner_team) VALUES ($1, 'Hack', 'YTÜ', 'WEBLAB')`, []any{d.event}},
		{`INSERT INTO event_days (id, event_id) VALUES ($1, $2)`, []any{day, d.event}},
		{`INSERT INTO sessions (id, event_day_id, title, session_type) VALUES ($1, $2, 'Açılış', 'talk')`, []any{d.session, day}},
		{`INSERT INTO tickets (id, event_id, ticket_type, owner_id) VALUES ($1, $2, 'REGISTERED', $3)`, []any{d.memberTicket, d.event, d.member}},
		{`INSERT INTO tickets (id, event_id, ticket_type, guest_first_name, guest_last_name, guest_email) VALUES ($1, $2, 'GUEST', 'Ada', 'Lovelace', 'ada@example.test')`, []any{d.guest, d.event}},
	} {
		if _, err := pool.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	return d
}

func (d inviteDB) invite(t *testing.T, ticket uuid.UUID, session *uuid.UUID, form uuid.UUID) feedback.Invite {
	t.Helper()
	inv, err := d.store.Create(context.Background(), feedback.Invite{TicketID: ticket, EventID: d.event, SessionID: session, FormID: form})
	if err != nil {
		t.Fatal(err)
	}
	return inv
}

func (d inviteDB) answered(t *testing.T, id uuid.UUID) bool {
	t.Helper()
	inv, err := d.store.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return inv.AnsweredAt != nil
}

// One invite per Ticket and target (the Event, or a Session), each with its
// own code.
func TestPostgresInviteOnePerTicketAndTarget(t *testing.T) {
	d := newInviteDB(t)
	ctx := context.Background()
	inv := d.invite(t, d.guest, nil, d.form)
	if !feedback.ValidCode(inv.Code) || inv.SentAt.IsZero() || inv.AnsweredAt != nil {
		t.Fatalf("invite %+v", inv)
	}
	if _, err := d.store.Create(ctx, feedback.Invite{TicketID: d.guest, EventID: d.event, FormID: uuid.New()}); !errors.Is(err, feedback.ErrExists) {
		t.Fatalf("second Event invite: %v", err)
	}
	other := d.invite(t, d.guest, &d.session, uuid.New())
	if other.Code == inv.Code {
		t.Fatal("same code")
	}
	if _, err := d.store.Create(ctx, feedback.Invite{TicketID: d.guest, EventID: d.event, SessionID: &d.session, FormID: uuid.New()}); !errors.Is(err, feedback.ErrExists) {
		t.Fatalf("second Session invite: %v", err)
	}
	if _, err := d.store.Create(ctx, feedback.Invite{EventID: d.event, FormID: d.form}); !errors.Is(err, feedback.ErrInvalid) {
		t.Fatalf("no Ticket: %v", err)
	}
}

// The code marks its invite, only for its own form; a signed-in answer marks
// the person's Ticket's invite; marking twice changes nothing; the counts
// follow and stay once the invites are gone.
func TestPostgresMarkAnsweredAndCounts(t *testing.T) {
	d := newInviteDB(t)
	ctx := context.Background()
	sessionForm := uuid.New()
	guest := d.invite(t, d.guest, nil, d.form)
	member := d.invite(t, d.memberTicket, nil, d.form)
	guestSession := d.invite(t, d.guest, &d.session, sessionForm)

	// Another form's code, an unknown code, a malformed one: nothing.
	for _, code := range []string{guestSession.Code, "AAAAAAAAAAAAAAAAAAAAAA", "x' OR 1=1 --"} {
		if ok, err := d.store.MarkAnswered(ctx, d.form, code, nil); err != nil || ok {
			t.Fatalf("%q: %v %v", code, ok, err)
		}
	}
	if d.answered(t, guest.ID) || d.answered(t, guestSession.ID) {
		t.Fatal("marked by a wrong code")
	}
	if ok, err := d.store.MarkAnswered(ctx, d.form, guest.Code, nil); err != nil || !ok || !d.answered(t, guest.ID) || d.answered(t, member.ID) {
		t.Fatalf("by code: %v %v", ok, err)
	}
	first, _ := d.store.Get(ctx, guest.ID)
	if ok, err := d.store.MarkAnswered(ctx, d.form, guest.Code, nil); err != nil || !ok {
		t.Fatalf("again: %v %v", ok, err)
	}
	if again, _ := d.store.Get(ctx, guest.ID); !again.AnsweredAt.Equal(*first.AnsweredAt) {
		t.Fatal("answered again")
	}
	// No code: the signed-in answerer's Ticket.
	stranger := uuid.New()
	if ok, err := d.store.MarkAnswered(ctx, d.form, "", &stranger); err != nil || ok {
		t.Fatalf("stranger: %v %v", ok, err)
	}
	if ok, err := d.store.MarkAnswered(ctx, d.form, "", &d.member); err != nil || !ok || !d.answered(t, member.ID) {
		t.Fatalf("by user: %v %v", ok, err)
	}
	counts, err := d.store.Counts(ctx, d.event)
	if err != nil || counts.Event.Sent != 2 || counts.Event.Answered != 2 || len(counts.Sessions) != 1 ||
		*counts.Sessions[0].SessionID != d.session || counts.Sessions[0].Sent != 1 || counts.Sessions[0].Answered != 0 {
		t.Fatalf("counts %+v %v", counts, err)
	}
	if _, err := d.pool.Exec(ctx, `DELETE FROM feedback_invites`); err != nil {
		t.Fatal(err)
	}
	if after, err := d.store.Counts(ctx, d.event); err != nil || after.Event != counts.Event || len(after.Sessions) != 1 {
		t.Fatalf("counts after the invites went: %+v %v", after, err)
	}
	if none, err := d.store.Counts(ctx, uuid.New()); err != nil || none.Event.Sent != 0 || none.Sessions == nil {
		t.Fatalf("no counts %+v %v", none, err)
	}
}
