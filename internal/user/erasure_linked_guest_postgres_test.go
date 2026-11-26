package user_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// A guest Ticket linked to the account (docs/guest-account-links.md) is the
// account's: anonymize_core clears its guest name, e-mail and phone and its
// link mark with its owner, whichever address linked it (here one the
// erasure is not told of), and the linked certificate loses its owner and
// address. The Ticket, its check-in and the certificate's record stay.
func TestPostgresAnonymizationClearsALinkedGuestTicket(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := user.NewPostgresStore(pool)
	subjectID := uuid.New()
	if _, _, err := user.NewService(store).Ensure(ctx, subjectID, user.Profile{Email: "ada@example.com", FirstName: "Ada", LastName: "Lovelace"}); err != nil {
		t.Fatal(err)
	}
	eventID, ticketID, certificateID := uuid.New(), uuid.New(), uuid.New()
	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO events (id, name, location, owner_team) VALUES ($1, 'Tarih', 'YTÜ', 'WEBLAB')`, []any{eventID}},
		{`INSERT INTO tickets (id, event_id, ticket_type, owner_id, guest_first_name, guest_last_name, guest_email, guest_phone_number, owner_link_source, owner_linked_at)
			VALUES ($1, $2, 'GUEST', $3, 'Ada', 'Byron', 'eski.adres@example.org', '+905551112233', 'sync', now())`, []any{ticketID, eventID, subjectID}},
		{`INSERT INTO certificates (id, event_id, ticket_id, owner_id, serial, recipient_name, recipient_email, event_name, owner_team, verify_url, owner_link_source, owner_linked_at)
			VALUES ($1, $2, $3, $4, 'LINKED-CERT', 'Ada Byron', 'eski.adres@example.org', 'Tarih', 'WEBLAB', 'https://example.test/c/LINKED-CERT', 'sync', now())`, []any{certificateID, eventID, ticketID, subjectID}},
	} {
		if _, err := pool.Exec(ctx, statement.sql, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.RequestDeletion(ctx, subjectID, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.AnonymizeAccount(ctx, subjectID, time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC), nil); err != nil {
		t.Fatal(err)
	}
	var owner *uuid.UUID
	var first, last, email, phone string
	var source *string
	if err := pool.QueryRow(ctx, `SELECT owner_id, guest_first_name, guest_last_name, guest_email, guest_phone_number, owner_link_source FROM tickets WHERE id = $1`,
		ticketID).Scan(&owner, &first, &last, &email, &phone, &source); err != nil {
		t.Fatal(err)
	}
	if owner != nil || first != "" || last != "" || email != "" || phone != "" || source != nil {
		t.Fatalf("linked guest Ticket after erasure: owner %v %q %q %q %q source %v", owner, first, last, email, phone, source)
	}
	var certOwner *uuid.UUID
	var recipientEmail, recipientName string
	var certSource *string
	if err := pool.QueryRow(ctx, `SELECT owner_id, recipient_email, recipient_name, owner_link_source FROM certificates WHERE id = $1`,
		certificateID).Scan(&certOwner, &recipientEmail, &recipientName, &certSource); err != nil {
		t.Fatal(err)
	}
	if certOwner != nil || recipientEmail != "" || certSource != nil || recipientName != "Ada Byron" {
		t.Fatalf("linked certificate after erasure: owner %v %q %q source %v", certOwner, recipientEmail, recipientName, certSource)
	}
}
