package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/commpref"
	"github.com/skylab-kulubu/core-backend/internal/guestlink"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
)

// The one-time command: the dry run prints the counts by category and
// writes nothing, -apply links and turns linking on, -undo takes it back.
// No line names a person or an address.
func TestGuestAccountLinkCommandRuns(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	ada, bob, eventID := uuid.New(), uuid.New(), uuid.New()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO users (id, email) VALUES ($1, 'ada@example.com'), ($2, 'bob@example.com')`, []any{ada, bob}},
		{`INSERT INTO events (id, name, location, owner_team) VALUES ($1, 'Hack', 'YTÜ', 'WEBLAB')`, []any{eventID}},
		{`INSERT INTO tickets (id, event_id, ticket_type, guest_first_name, guest_last_name, guest_email) VALUES
			($1, $4, 'GUEST', 'Ada', 'Konuk', 'ada.kisisel@example.org'),
			($2, $4, 'GUEST', 'Ortak', 'Adres', 'ortak@example.org'),
			($3, $4, 'GUEST', 'Kimse', 'Yok', 'kimse@example.org')`, []any{uuid.New(), uuid.New(), uuid.New(), eventID}},
	} {
		if _, err := pool.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	addrs := map[string]commpref.VerifiedAddress{
		"ada.kisisel@example.org": {Holders: []uuid.UUID{ada}, Kind: commpref.AddressPersonal, Enabled: true},
		"ortak@example.org":       {Holders: []uuid.UUID{ada, bob}, Kind: commpref.AddressPrimary, Enabled: true},
	}
	read := func(context.Context) (map[string]commpref.VerifiedAddress, error) { return addrs, nil }
	store := guestlink.NewPostgres(pool)
	run := func(args ...string) string {
		t.Helper()
		var out bytes.Buffer
		if code := guestAccountLinkCommand(ctx, args, &out, read, store); code != 0 {
			t.Fatalf("%v: exit %d\n%s", args, code, out.String())
		}
		for _, personal := range []string{"example.org", "Ada", "Konuk", ada.String(), bob.String()} {
			if strings.Contains(out.String(), personal) {
				t.Fatalf("%v printed %q:\n%s", args, personal, out.String())
			}
		}
		return out.String()
	}
	dry := run()
	for _, line := range []string{"linking: off", "tickets_link: 1", "tickets_link_personal: 1", "tickets_ambiguous: 1", "tickets_no_account: 1", "tickets_duplicate: 0"} {
		if !strings.Contains(dry, line+"\n") {
			t.Fatalf("dry run lacks %q:\n%s", line, dry)
		}
	}
	if on, _ := store.Enabled(ctx); on {
		t.Fatal("the dry run turned linking on")
	}
	var linked int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM tickets WHERE owner_id IS NOT NULL`).Scan(&linked); err != nil || linked != 0 {
		t.Fatalf("the dry run linked %d: %v", linked, err)
	}
	applied := run("-apply")
	if !strings.Contains(applied, "linking: on\n") || !strings.Contains(applied, "tickets_linked_so_far_bulk: 1\n") {
		t.Fatalf("apply:\n%s", applied)
	}
	again := run("-apply")
	if !strings.Contains(again, "tickets_link: 0\n") {
		t.Fatalf("apply again:\n%s", again)
	}
	undone := run("-undo")
	if !strings.Contains(undone, "linking: off\n") || !strings.Contains(undone, "tickets_unlinked: 1\n") {
		t.Fatalf("undo:\n%s", undone)
	}
	off := run("-off")
	if !strings.Contains(off, "linking: off\n") {
		t.Fatalf("off:\n%s", off)
	}
}
