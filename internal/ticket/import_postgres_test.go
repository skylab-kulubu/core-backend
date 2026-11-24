package ticket_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/teamaudit"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// The attendee import on the database (docs/attendee-import.md): the
// matches in one query, the mail queued or not as asked, the audit row of
// counts an Event's editor writes, and the same list again writing nothing.
func TestImportOnPostgres(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newCapacityPG(t)
	users := user.NewPostgresStore(e.pool)
	dir := identity.NewMemory()
	team := identity.Group{ID: uuid.NewString(), Name: "GECEKODU", Path: "/UYELER/ORGANIZASYON/GECEKODU"}
	dir.PutGroup(team)
	svc := ticket.NewService(e.store, e.events, authz.NewAuthorizer(authz.DefaultPolicy()), users, dir, teamaudit.NewPostgres(e.pool))

	ev := e.event(t, 3, true)
	if _, err := e.pool.Exec(ctx, `UPDATE events SET owner_team = 'GECEKODU' WHERE id = $1`, ev.ID); err != nil {
		t.Fatal(err)
	}
	adaID := uuid.New()
	if _, _, err := user.NewService(users).Ensure(ctx, adaID, user.Profile{Email: "Ada@Example.com", FirstName: "Ada", LastName: "Lovelace"}); err != nil {
		t.Fatal(err)
	}
	// A guest already there, in another case.
	if _, err := e.store.Create(ctx, ticket.Ticket{EventID: ev.ID, TicketType: ticket.Guest, GuestFirstName: "G", GuestLastName: "H", GuestEmail: "grace@example.com"}); err != nil {
		t.Fatal(err)
	}

	matches, err := e.store.ImportMatches(ctx, ev.ID, []string{"ada@example.com", "grace@example.com", "nobody@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if m := matches["ada@example.com"]; len(m.Accounts) != 1 || m.Accounts[0] != adaID || m.Ticket != "" {
		t.Fatalf("ada %+v", m)
	}
	if m := matches["grace@example.com"]; len(m.Accounts) != 0 || m.Ticket != ticket.StatusActive {
		t.Fatalf("grace %+v", m)
	}
	if m := matches["nobody@example.com"]; len(m.Accounts) != 0 || m.Ticket != "" {
		t.Fatalf("nobody %+v", m)
	}

	// A GECEKODU member edits the team's Events by the Event policy: an
	// operator, recorded as the Event's editor.
	editor := authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/ORGANIZASYON/GECEKODU"}}
	as := teamaudit.WithActor(ctx, teamaudit.Actor{ID: uuid.MustParse(editor.ID), Client: "core-frontend"})
	list := "Ad;Soyad;E-posta\nAda;Lovelace;ADA@example.com\nGrace;Hopper;grace@example.com\nAlan;Turing;alan@example.com\nEdsger;Dijkstra;edsger@example.com\n"

	quiet, err := svc.ImportAttendees(as, editor, ev.ID, ticket.ImportInput{Text: list, SendMail: false})
	if err != nil {
		t.Fatal(err)
	}
	// Three places, one taken: Ada and Alan get them, Edsger waits.
	if got := outcomes(quiet.Rows); got != "active,existing,active,waitlisted" || quiet.Mail != ticket.ImportMailOff {
		t.Fatalf("%s %s", got, quiet.Mail)
	}
	if kinds := e.mailKinds(t, ev.ID); kinds["created"] != 1 || kinds["waitlisted"] != 0 {
		// Only the guest written before the import (Create) was queued.
		t.Fatalf("mail %+v", kinds)
	}
	var owner *uuid.UUID
	if err := e.pool.QueryRow(ctx, `SELECT owner_id FROM tickets WHERE event_id = $1 AND ticket_type = 'REGISTERED'`, ev.ID).Scan(&owner); err != nil || owner == nil || *owner != adaID {
		t.Fatalf("ada's ticket %v %v", owner, err)
	}

	// The same list with the mail on: nothing new, so nothing queued.
	again, err := svc.ImportAttendees(as, editor, ev.ID, ticket.ImportInput{Text: list, SendMail: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := outcomes(again.Rows); got != "existing,existing,existing,existing" || again.Mail != ticket.ImportMailQueued {
		t.Fatalf("%s %s", got, again.Mail)
	}
	if counts := e.counts(t, ev.ID); counts[ticket.StatusActive] != 3 || counts[ticket.StatusWaitlisted] != 1 {
		t.Fatalf("counts %+v", counts)
	}

	// A new row with the mail on is queued.
	more, err := svc.ImportAttendees(as, editor, ev.ID, ticket.ImportInput{Text: list + "Barbara;Liskov;barbara@example.com\n", SendMail: true, PastCapacity: true})
	if err != nil {
		t.Fatal(err)
	}
	if more.Summary.Active != 1 || more.Summary.OverCapacity != 1 {
		t.Fatalf("more %+v", more.Summary)
	}
	if kinds := e.mailKinds(t, ev.ID); kinds["created"] != 2 {
		t.Fatalf("mail %+v", kinds)
	}

	rows, err := e.pool.Query(ctx, `SELECT actor_basis, actor_via_group_id, outcome, after FROM team_audit_records WHERE action = 'ticket.import' ORDER BY occurred_at`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var basis, outcome string
		var via *uuid.UUID
		var after []byte
		if err := rows.Scan(&basis, &via, &outcome, &after); err != nil {
			t.Fatal(err)
		}
		if basis != "event_editor" || via == nil || via.String() != team.ID || outcome != "ok" {
			t.Fatalf("row %s %v %s", basis, via, outcome)
		}
		var state teamaudit.State
		if err := json.Unmarshal(after, &state); err != nil || state.Import == nil || state.EventID != ev.ID.String() || state.TicketID != "" {
			t.Fatalf("after %s %v", after, err)
		}
		for _, personal := range []string{"@", "Ada", "Lovelace"} {
			if strings.Contains(string(after), personal) {
				t.Fatalf("the row keeps %q: %s", personal, after)
			}
		}
		if n == 0 && (state.Import.Rows != 4 || state.Import.Created != 2 || state.Import.Waitlisted != 1 || state.Import.Existing != 1 || state.Import.Members != 1 || state.Import.Mail) {
			t.Fatalf("first counts %+v", state.Import)
		}
		n++
	}
	if n != 3 {
		t.Fatalf("%d audit rows", n)
	}
}
