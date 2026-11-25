package migrate_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
)

const (
	coOrganizerTicketAuditVersion = "20261126190000"
	coOrganizerTicketAuditDown    = coOrganizerTicketAuditVersion + "_co_organizer_ticket_audit.down.sql"
)

// An event editor's resend and link renewal (an Event's Co-organizer's,
// docs/co-organizers.md): the basis check takes them, and still refuses an
// event editor's Team action; the up migration runs again over itself;
// down puts the narrower check back NOT VALID, so the rows written stay and
// a new one is refused; up again validates it.
func TestCoOrganizerTicketAuditMigration(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	row := `INSERT INTO team_audit_records (id, occurred_at, actor_id, actor_basis, actor_via_group_id, action, group_id, group_path, outcome)
		VALUES (gen_random_uuid(), now(), gen_random_uuid(), 'event_editor', $1, 'ticket.resend', gen_random_uuid(), '/UYELER/ARGE/WEBLAB', 'ok')`
	renew := strings.Replace(row, "'ticket.resend'", "'ticket.link.renew'", 1)
	for name, statement := range map[string]string{"resend": row, "link renewal": renew} {
		if _, err := pool.Exec(ctx, statement, uuid.New()); err != nil {
			t.Fatalf("an event editor's %s: %v", name, err)
		}
	}
	if _, err := pool.Exec(ctx, strings.Replace(row, "'ticket.resend'", "'member.add'", 1), uuid.New()); err == nil {
		t.Fatal("an event editor's member.add was taken")
	}

	// Again over itself: recorded, nothing changes.
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+coOrganizerTicketAuditVersion); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if !recorded(t, pool, coOrganizerTicketAuditVersion) {
		t.Fatal("not recorded")
	}

	if _, err := pool.Exec(ctx, readDown(t, coOrganizerTicketAuditDown)); err != nil {
		t.Fatal(err)
	}
	var kept int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM team_audit_records WHERE actor_basis = 'event_editor' AND action IN ('ticket.resend', 'ticket.link.renew')`).Scan(&kept); err != nil || kept != 2 {
		t.Fatalf("rows after down: %d %v", kept, err)
	}
	var validated bool
	if err := pool.QueryRow(ctx, `SELECT convalidated FROM pg_constraint WHERE conrelid = 'team_audit_records'::regclass AND conname = 'team_audit_records_actor_basis_check'`).Scan(&validated); err != nil || validated {
		t.Fatalf("after down the check is validated: %v %v", validated, err)
	}
	if _, err := pool.Exec(ctx, row, uuid.New()); err == nil {
		t.Fatal("after down an event editor's resend was taken")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+coOrganizerTicketAuditVersion); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if !recorded(t, pool, coOrganizerTicketAuditVersion) {
		t.Fatal("up again: not recorded")
	}
}
