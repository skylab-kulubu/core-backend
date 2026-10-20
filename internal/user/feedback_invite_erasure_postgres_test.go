package user_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/feedback"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// Account erasure deletes the person's feedback invites: their own
// Tickets' and their addresses' guest Tickets'. Other people's stay, and so
// do the counts (etkinlik geri bildirim formu, ticket 04).
func TestAnonymizationDeletesTheFeedbackInvites(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	users := user.NewPostgresStore(pool)
	person, other := uuid.New(), uuid.New()
	for id, email := range map[uuid.UUID]string{person: "kisi@example.test", other: "baska@example.test"} {
		if _, _, err := user.NewService(users).Ensure(ctx, id, user.Profile{Email: email}); err != nil {
			t.Fatal(err)
		}
	}
	eventID, own, asGuest, others := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO events (id, name, location, owner_team) VALUES ($1, 'Hack', 'YTÜ', 'WEBLAB')`, []any{eventID}},
		{`INSERT INTO tickets (id, event_id, ticket_type, owner_id) VALUES ($1, $2, 'REGISTERED', $3)`, []any{own, eventID, person}},
		{`INSERT INTO tickets (id, event_id, ticket_type, guest_first_name, guest_last_name, guest_email) VALUES ($1, $2, 'GUEST', 'Ada', 'Lovelace', 'kisi.misafir@example.test')`, []any{asGuest, eventID}},
		{`INSERT INTO tickets (id, event_id, ticket_type, owner_id) VALUES ($1, $2, 'REGISTERED', $3)`, []any{others, eventID, other}},
	} {
		if _, err := pool.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	invites := feedback.NewPostgresStore(pool)
	form := uuid.New()
	for _, ticketID := range []uuid.UUID{own, asGuest, others} {
		if _, err := invites.Create(ctx, feedback.Invite{TicketID: ticketID, EventID: eventID, FormID: form}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := users.RequestDeletion(ctx, person, nil); err != nil {
		t.Fatal(err)
	}
	if err := users.AnonymizeAccount(ctx, person, time.Now().UTC(), []string{"Kisi.Misafir@example.test"}); err != nil {
		t.Fatal(err)
	}
	var left []uuid.UUID
	rows, err := pool.Query(ctx, `SELECT ticket_id FROM feedback_invites`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		left = append(left, id)
	}
	if len(left) != 1 || left[0] != others {
		t.Fatalf("invites left %v", left)
	}
	if counts, err := invites.Counts(ctx, eventID); err != nil || counts.Event.Sent != 3 {
		t.Fatalf("counts %+v %v", counts, err)
	}
}
