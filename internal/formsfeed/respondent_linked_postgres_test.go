package formsfeed_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/formsfeed"
)

// A guest Ticket linked to an account (docs/guest-account-links.md) is still
// the address's Ticket: an answer of the address is ticketed, not kept as
// an Unticketed respondent.
func TestALinkedGuestTicketIsTheAddresssTicket(t *testing.T) {
	pool := migrated(t)
	ctx := context.Background()
	eventID, owner := uuid.New(), uuid.New()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO users (id, email) VALUES ($1, 'ada@example.com')`, []any{owner}},
		{`INSERT INTO events (id, name, location, owner_team) VALUES ($1, 'Hack', 'YTÜ', 'WEBLAB')`, []any{eventID}},
		{`INSERT INTO tickets (id, event_id, ticket_type, owner_id, guest_first_name, guest_last_name, guest_email, owner_link_source, owner_linked_at)
			VALUES ($1, $2, 'GUEST', $3, 'Ada', 'Byron', 'ada@kisisel.org', 'sync', now())`, []any{uuid.New(), eventID, owner}},
	} {
		if _, err := pool.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	store := formsfeed.NewPostgresRespondents(pool)
	kept, err := store.Keep(ctx, formsfeed.RespondentRow{
		EventID: eventID, FormID: uuid.New(), ResponseID: uuid.New(),
		FirstName: "Ada", LastName: "Byron", Email: " ADA@kisisel.org ", FormsStatus: "accepted", Source: "new",
	})
	if err != nil || kept {
		t.Fatalf("kept %t %v", kept, err)
	}
	other, err := store.Keep(ctx, formsfeed.RespondentRow{
		EventID: eventID, FormID: uuid.New(), ResponseID: uuid.New(),
		FirstName: "Grace", LastName: "Hopper", Email: "grace@example.com", FormsStatus: "accepted", Source: "new",
	})
	if err != nil || !other {
		t.Fatalf("another address: kept %t %v", other, err)
	}
}
