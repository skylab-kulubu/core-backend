package user_test

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// Core's own guest data keyed by e-mail (spec §3.4): anonymize_core clears the
// guest tickets and ownerless certificates of every address the person held,
// the ones Keycloak alone knows included, and keeps the records.
func TestPostgresAnonymizationClearsTheGuestDataOfThePersonsAddresses(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := user.NewPostgresStore(pool)
	subjectID := uuid.New()
	if _, _, err := user.NewService(store).Ensure(ctx, subjectID, user.Profile{
		Email: "ada@example.com", SchoolEmail: "ada.lovelace@std.yildiz.edu.tr", FirstName: "Ada", LastName: "Lovelace",
	}); err != nil {
		t.Fatal(err)
	}

	eventID, dayID, sessionID := uuid.New(), uuid.New(), uuid.New()
	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO events (id, name, location, owner_team) VALUES ($1, 'Tarih', 'YTÜ', 'WEBLAB')`, []any{eventID}},
		{`INSERT INTO event_days (id, event_id, name) VALUES ($1, $2, 'Gün 1')`, []any{dayID, eventID}},
		{`INSERT INTO sessions (id, event_day_id, title, session_type) VALUES ($1, $2, 'Oturum', 'PRESENTATION')`, []any{sessionID, dayID}},
	} {
		if _, err := pool.Exec(ctx, statement.sql, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	type guest struct {
		ticket, checkIn, certificate uuid.UUID
		email, serial                string
		pdf                          []byte
	}
	newGuest := func(email, first, last, phone, serial string) guest {
		t.Helper()
		g := guest{ticket: uuid.New(), checkIn: uuid.New(), certificate: uuid.New(), email: email, serial: serial, pdf: []byte("%PDF-" + serial)}
		if _, err := pool.Exec(ctx, `
			INSERT INTO tickets (id, event_id, ticket_type, guest_first_name, guest_last_name, guest_email, guest_phone_number)
			VALUES ($1, $2, 'GUEST', $3, $4, $5, $6)
		`, g.ticket, eventID, first, last, email, phone); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO ticket_checkins (id, ticket_id, event_day_id, session_id) VALUES ($1, $2, $3, $4)`,
			g.checkIn, g.ticket, dayID, sessionID); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO certificates (id, event_id, ticket_id, serial, recipient_name, recipient_email, event_name, owner_team, verify_url, pdf)
			VALUES ($1, $2, $3, $4, $5, $6, 'Tarih', 'WEBLAB', 'https://example.test/c/' || $4, $7)
		`, g.certificate, eventID, g.ticket, serial, first+" "+last, email, g.pdf); err != nil {
			t.Fatal(err)
		}
		return g
	}
	// core's Primary e-mail, in another case; core's School e-mail; the
	// Personal e-mail only Keycloak knows; and someone else.
	primary := newGuest("ADA@Example.com", "Ada", "Lovelace", "+905551112233", "GUEST-PRIMARY")
	school := newGuest("ada.lovelace@std.yildiz.edu.tr", "Ada", "L.", "", "GUEST-SCHOOL")
	personal := newGuest("ada.personal@example.org", "Ada", "Byron", "+905554445566", "GUEST-PERSONAL")
	other := newGuest("grace@example.com", "Grace", "Hopper", "+905557778899", "GUEST-OTHER")

	if _, err := store.RequestDeletion(ctx, subjectID, nil); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	if err := store.AnonymizeAccount(ctx, subjectID, at, []string{" Ada.Personal@Example.ORG "}); err != nil {
		t.Fatal(err)
	}

	for name, g := range map[string]guest{"primary": primary, "school": school, "personal": personal} {
		var first, last, email, phone string
		if err := pool.QueryRow(ctx, `SELECT guest_first_name, guest_last_name, guest_email, guest_phone_number FROM tickets WHERE id = $1`,
			g.ticket).Scan(&first, &last, &email, &phone); err != nil {
			t.Fatalf("%s ticket: %v", name, err)
		}
		if first != "" || last != "" || email != "" || phone != "" {
			t.Fatalf("%s guest ticket kept %q %q %q %q", name, first, last, email, phone)
		}
		assertGuestRecordsKept(t, ctx, pool, name, g.checkIn, g.certificate, g.serial, g.pdf)
		var recipientEmail string
		if err := pool.QueryRow(ctx, `SELECT recipient_email FROM certificates WHERE id = $1`, g.certificate).Scan(&recipientEmail); err != nil {
			t.Fatal(err)
		}
		if recipientEmail != "" {
			t.Fatalf("%s certificate kept its recipient e-mail", name)
		}
	}

	var first, last, email, phone, recipientEmail string
	if err := pool.QueryRow(ctx, `
		SELECT t.guest_first_name, t.guest_last_name, t.guest_email, t.guest_phone_number, c.recipient_email
		FROM tickets t JOIN certificates c ON c.ticket_id = t.id WHERE t.id = $1
	`, other.ticket).Scan(&first, &last, &email, &phone, &recipientEmail); err != nil {
		t.Fatal(err)
	}
	if first != "Grace" || last != "Hopper" || email != "grace@example.com" || phone != "+905557778899" || recipientEmail != "grace@example.com" {
		t.Fatalf("another guest changed: %q %q %q %q %q", first, last, email, phone, recipientEmail)
	}
	assertGuestRecordsKept(t, ctx, pool, "other", other.checkIn, other.certificate, other.serial, other.pdf)

	// A retry after the checkpoint was lost changes nothing more.
	if err := store.AnonymizeAccount(ctx, subjectID, at.Add(time.Hour), nil); err != nil {
		t.Fatalf("retry: %v", err)
	}
}

// assertGuestRecordsKept checks that the ticket keeps its check-in and the
// certificate keeps its recipient name, serial and PDF.
func assertGuestRecordsKept(t *testing.T, ctx context.Context, pool *pgxpool.Pool, name string, checkIn, certificate uuid.UUID, serial string, pdf []byte) {
	t.Helper()
	var checkIns int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM ticket_checkins WHERE id = $1`, checkIn).Scan(&checkIns); err != nil {
		t.Fatal(err)
	}
	var gotSerial, recipientName string
	var gotPDF []byte
	if err := pool.QueryRow(ctx, `SELECT serial, recipient_name, pdf FROM certificates WHERE id = $1`, certificate).Scan(&gotSerial, &recipientName, &gotPDF); err != nil {
		t.Fatal(err)
	}
	if checkIns != 1 || gotSerial != serial || recipientName == "" || !bytes.Equal(gotPDF, pdf) {
		t.Fatalf("%s guest records not kept: check-ins=%d serial=%q name=%q pdf=%q", name, checkIns, gotSerial, recipientName, gotPDF)
	}
}
