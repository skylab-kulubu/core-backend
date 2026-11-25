package migrate_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
)

const (
	coOrganizerAuditMailVersion = "20261127110000"
	coOrganizerAuditMailDown    = coOrganizerAuditMailVersion + "_co_organizer_audit_and_mail.down.sql"
)

// The Co-organizer list in the Team audit record and its mails' queue
// (docs/co-organizers.md): a co_organizer.* row against a Group, or without
// one (an Event without an Owner team), is taken, any other action still
// needs its Group; the queue takes a team's or a person's row once per
// Event and kind. The up migration runs again over itself; down puts the
// narrower Group checks back NOT VALID, so the rows written stay and a new
// Group-less one is refused, and keeps the queue; up again validates them.
func TestCoOrganizerAuditMailMigration(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	withGroup := `INSERT INTO team_audit_records (id, occurred_at, actor_id, actor_basis, action, group_id, group_path, outcome)
		VALUES (gen_random_uuid(), now(), gen_random_uuid(), 'privileged', 'co_organizer.add', gen_random_uuid(), '/UYELER/YK', 'ok')`
	noGroup := `INSERT INTO team_audit_records (id, occurred_at, actor_id, actor_basis, action, group_id, group_path, outcome)
		VALUES (gen_random_uuid(), now(), gen_random_uuid(), 'privileged', 'co_organizer.remove', NULL, '', 'ok')`
	for name, statement := range map[string]string{"with a Group": withGroup, "without": noGroup,
		"a change": strings.Replace(withGroup, "'co_organizer.add'", "'co_organizer.change'", 1)} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if _, err := pool.Exec(ctx, strings.Replace(noGroup, "'co_organizer.remove'", "'team.profile'", 1)); err == nil {
		t.Fatal("a team.profile without a Group was taken")
	}
	if _, err := pool.Exec(ctx, strings.Replace(noGroup, "''", "'/UYELER/YK'", 1)); err == nil {
		t.Fatal("a path without a Group was taken")
	}

	eventID, person := uuid.New(), uuid.New()
	for _, statement := range []string{
		`INSERT INTO users (id, email) VALUES ('` + person.String() + `', 'co@example.test')`,
		`INSERT INTO events (id, name, location, owner_team) VALUES ('` + eventID.String() + `', 'Hack', 'YTÜ', 'YK')`,
		`INSERT INTO co_organizer_notifications (id, event_id, kind, team_code, next_attempt_at) VALUES (gen_random_uuid(), '` + eventID.String() + `', 'added', 'WEBLAB', now())`,
		`INSERT INTO co_organizer_notifications (id, event_id, kind, team_code, next_attempt_at) VALUES (gen_random_uuid(), '` + eventID.String() + `', 'removed', 'WEBLAB', now())`,
		`INSERT INTO co_organizer_notifications (id, event_id, kind, user_id, next_attempt_at) VALUES (gen_random_uuid(), '` + eventID.String() + `', 'added', '` + person.String() + `', now())`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	for name, statement := range map[string]string{
		"the same team and kind twice": `INSERT INTO co_organizer_notifications (id, event_id, kind, team_code, next_attempt_at) VALUES (gen_random_uuid(), '` + eventID.String() + `', 'added', 'WEBLAB', now())`,
		"a team and a person":          `INSERT INTO co_organizer_notifications (id, event_id, kind, team_code, user_id, next_attempt_at) VALUES (gen_random_uuid(), '` + eventID.String() + `', 'added', 'X', '` + person.String() + `', now())`,
		"another kind":                 `INSERT INTO co_organizer_notifications (id, event_id, kind, team_code, next_attempt_at) VALUES (gen_random_uuid(), '` + eventID.String() + `', 'changed', 'ARTLAB', now())`,
	} {
		if _, err := pool.Exec(ctx, statement); err == nil {
			t.Errorf("%s was taken", name)
		}
	}

	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+coOrganizerAuditMailVersion); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if !recorded(t, pool, coOrganizerAuditMailVersion) {
		t.Fatal("not recorded")
	}

	if _, err := pool.Exec(ctx, readDown(t, coOrganizerAuditMailDown)); err != nil {
		t.Fatal(err)
	}
	var kept, queued int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM team_audit_records WHERE action LIKE 'co_organizer.%'), (SELECT count(*) FROM co_organizer_notifications)`).Scan(&kept, &queued); err != nil || kept != 3 || queued != 3 {
		t.Fatalf("after down: %d rows, %d queued, %v", kept, queued, err)
	}
	if _, err := pool.Exec(ctx, noGroup); err == nil {
		t.Fatal("after down a Group-less row was taken")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+coOrganizerAuditMailVersion); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if !recorded(t, pool, coOrganizerAuditMailVersion) {
		t.Fatal("up again: not recorded")
	}
	if _, err := pool.Exec(ctx, noGroup); err != nil {
		t.Fatalf("up again: %v", err)
	}
}
