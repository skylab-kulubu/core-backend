package migrate_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
)

const (
	guestAccountLinksVersion = "20261127234500"
	guestAccountLinksDown    = guestAccountLinksVersion + "_guest_account_links.down.sql"
)

// The guest account links (docs/guest-account-links.md): up leaves the
// switch off and runs again over itself; the marks are checked; down turns
// the switch off and takes back exactly the links core made (a certificate
// issued from a linked Ticket included), every other owner staying.
func TestGuestAccountLinksMigration(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	var on bool
	if err := pool.QueryRow(ctx, `SELECT enabled_at IS NOT NULL FROM guest_account_link_settings`).Scan(&on); err != nil || on {
		t.Fatalf("switch after up: %t %v", on, err)
	}
	ada := uuid.New()
	eventID, linked, member, unlinked := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	linkedCert, issuedCert, memberCert := uuid.New(), uuid.New(), uuid.New()
	for _, sql := range []struct {
		q    string
		args []any
	}{
		{`INSERT INTO users (id, email, first_name, last_name) VALUES ($1, 'ada@example.com', 'Ada', 'L')`, []any{ada}},
		{`INSERT INTO events (id, name, location, owner_team) VALUES ($1, 'Hack', 'YTÜ', 'WEBLAB')`, []any{eventID}},
		{`INSERT INTO tickets (id, event_id, ticket_type, owner_id, guest_first_name, guest_last_name, guest_email, owner_link_source, owner_linked_at)
			VALUES ($1, $2, 'GUEST', $3, 'Ada', 'K', 'ada@kisisel.org', 'bulk', now())`, []any{linked, eventID, ada}},
		{`INSERT INTO events (id, name, location, owner_team) VALUES ($1, 'Başka', 'YTÜ', 'WEBLAB')`, []any{member}},
		{`INSERT INTO tickets (id, event_id, ticket_type, owner_id) VALUES ($1, $2, 'REGISTERED', $3)`, []any{unlinked, member, ada}},
		{`INSERT INTO certificates (id, event_id, ticket_id, owner_id, serial, recipient_name, recipient_email, event_name, owner_team, verify_url, owner_link_source, owner_linked_at, revoked_at)
			VALUES ($1, $2, $3, $4, 'C1', 'Ada K', 'ada@kisisel.org', 'Hack', 'WEBLAB', 'https://e/c/C1', 'bulk', now(), now())`, []any{linkedCert, eventID, linked, ada}},
		{`INSERT INTO certificates (id, event_id, ticket_id, owner_id, serial, recipient_name, recipient_email, event_name, owner_team, verify_url)
			VALUES ($1, $2, $3, $4, 'C2', 'Ada K', 'ada@kisisel.org', 'Hack', 'WEBLAB', 'https://e/c/C2')`, []any{issuedCert, eventID, linked, ada}},
		{`INSERT INTO certificates (id, event_id, ticket_id, owner_id, serial, recipient_name, recipient_email, event_name, owner_team, verify_url)
			VALUES ($1, $2, $3, $4, 'C3', 'Ada L', 'ada@example.com', 'Başka', 'WEBLAB', 'https://e/c/C3')`, []any{memberCert, member, unlinked, ada}},
		{`UPDATE guest_account_link_settings SET enabled_at = now()`, nil},
	} {
		if _, err := pool.Exec(ctx, sql.q, sql.args...); err != nil {
			t.Fatal(err)
		}
	}
	for what, q := range map[string]string{
		"a source without a time": `UPDATE tickets SET owner_linked_at = NULL WHERE id = '` + linked.String() + `'`,
		"an unknown source":       `UPDATE tickets SET owner_link_source = 'elle' WHERE id = '` + linked.String() + `'`,
		"a certificate written":   `UPDATE certificates SET owner_link_source = 'write' WHERE id = '` + linkedCert.String() + `'`,
		"a second switch":         `INSERT INTO guest_account_link_settings (id) VALUES (false)`,
	} {
		if _, err := pool.Exec(ctx, q); err == nil {
			t.Fatalf("%s was kept", what)
		}
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+guestAccountLinksVersion); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if !recorded(t, pool, guestAccountLinksVersion) {
		t.Fatal("not recorded")
	}
	if err := pool.QueryRow(ctx, `SELECT enabled_at IS NOT NULL FROM guest_account_link_settings`).Scan(&on); err != nil || !on {
		t.Fatalf("up again changed the switch: %t %v", on, err)
	}

	if _, err := pool.Exec(ctx, readDown(t, guestAccountLinksDown)); err != nil {
		t.Fatal(err)
	}
	owner := func(table string, id uuid.UUID) *uuid.UUID {
		t.Helper()
		var o *uuid.UUID
		if err := pool.QueryRow(ctx, `SELECT owner_id FROM `+table+` WHERE id = $1`, id).Scan(&o); err != nil {
			t.Fatal(err)
		}
		return o
	}
	if owner("tickets", linked) != nil || owner("certificates", linkedCert) != nil || owner("certificates", issuedCert) != nil {
		t.Fatal("down kept a link core made")
	}
	if o := owner("tickets", unlinked); o == nil || *o != ada {
		t.Fatal("down took a member's own Ticket")
	}
	if o := owner("certificates", memberCert); o == nil || *o != ada {
		t.Fatal("down took a member's own certificate")
	}
	if err := pool.QueryRow(ctx, `SELECT enabled_at IS NOT NULL FROM guest_account_link_settings`).Scan(&on); err != nil || on {
		t.Fatalf("switch after down: %t %v", on, err)
	}
	var email string
	if err := pool.QueryRow(ctx, `SELECT guest_email FROM tickets WHERE id = $1`, linked).Scan(&email); err != nil || email != "ada@kisisel.org" {
		t.Fatalf("down changed the guest Ticket: %q %v", email, err)
	}
}
