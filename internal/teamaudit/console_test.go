package teamaudit_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/teamaudit"
)

// consoleRow is a row of a change made in Keycloak's console.
func consoleRow(action teamaudit.Action) teamaudit.Record {
	return teamaudit.Record{
		Actor:  teamaudit.Actor{ID: uuid.New(), Basis: teamaudit.BasisConsole, Client: "keycloak:master"},
		Action: action, Via: teamaudit.ViaConsole,
	}
}

type consoleWriter interface {
	teamaudit.ConsoleRecorder
	teamaudit.Reader
}

// A console row is written finished, with Keycloak's time, once per admin
// event; the basis and the way go together; keycloak.* rows carry no
// Group and no person, user.roles a person and no Group.
func testConsole(t *testing.T, w consoleWriter) {
	t.Helper()
	ctx := context.Background()
	at := time.Date(2026, 11, 25, 9, 30, 0, 0, time.UTC)

	member := consoleRow(teamaudit.ActionMemberAdd)
	member.GroupID, member.GroupPath, member.TargetUserID = uuid.NewString(), "/UYELER/YK", uuid.New()
	if wrote, err := w.RecordConsole(ctx, member, at, "ev-1"); err != nil || !wrote {
		t.Fatalf("member: %v %v", wrote, err)
	}
	if wrote, err := w.RecordConsole(ctx, member, at, "ev-1"); err != nil || wrote {
		t.Fatalf("the same event again: %v %v", wrote, err)
	}
	user := consoleRow(teamaudit.ActionUserRoles)
	user.TargetUserID = uuid.New()
	user.After = &teamaudit.State{Roles: []string{"forms:admin"}, Operation: "CREATE", ResourceType: "CLIENT_ROLE_MAPPING"}
	if wrote, err := w.RecordConsole(ctx, user, at, "ev-2"); err != nil || !wrote {
		t.Fatalf("user roles: %v %v", wrote, err)
	}
	client := consoleRow(teamaudit.ActionKeycloakClient)
	client.After = &teamaudit.State{ID: uuid.NewString(), Client: "forms", Operation: "UPDATE", ResourceType: "CLIENT"}
	if wrote, err := w.RecordConsole(ctx, client, at, "ev-3"); err != nil || !wrote {
		t.Fatalf("client: %v %v", wrote, err)
	}
	role := consoleRow(teamaudit.ActionKeycloakRole)
	role.After = &teamaudit.State{Client: "forms", Role: "form:manage", Composites: []string{":ADMIN", "core:audit:read", ":ADMIN"}}
	if wrote, err := w.RecordConsole(ctx, role, at, "ev-4"); err != nil || !wrote {
		t.Fatalf("role: %v %v", wrote, err)
	}

	refused := map[string]func() (teamaudit.Record, string){
		"a basis without the way": func() (teamaudit.Record, string) {
			r := consoleRow(teamaudit.ActionKeycloakClient)
			r.Via = ""
			return r, "ev-x1"
		},
		"the way with another basis": func() (teamaudit.Record, string) {
			r := consoleRow(teamaudit.ActionKeycloakClient)
			r.Actor.Basis = teamaudit.BasisPrivileged
			return r, "ev-x2"
		},
		"a configuration row with a Group": func() (teamaudit.Record, string) {
			r := consoleRow(teamaudit.ActionKeycloakRole)
			r.GroupID, r.GroupPath = uuid.NewString(), "/UYELER"
			return r, "ev-x3"
		},
		"a configuration row with a person": func() (teamaudit.Record, string) {
			r := consoleRow(teamaudit.ActionKeycloakClient)
			r.TargetUserID = uuid.New()
			return r, "ev-x4"
		},
		"a direct role without the person": func() (teamaudit.Record, string) {
			return consoleRow(teamaudit.ActionUserRoles), "ev-x5"
		},
		"no event id": func() (teamaudit.Record, string) {
			return consoleRow(teamaudit.ActionKeycloakClient), ""
		},
		"an event id that is no token": func() (teamaudit.Record, string) {
			return consoleRow(teamaudit.ActionKeycloakClient), "a b"
		},
	}
	for name, make := range refused {
		r, id := make()
		if _, err := w.RecordConsole(ctx, r, at, id); !errors.Is(err, teamaudit.ErrUnavailable) {
			t.Fatalf("%s: %v", name, err)
		}
	}

	rows, _, err := w.Records(ctx, teamaudit.Query{ActorID: role.Actor.ID, Limit: teamaudit.MaxPage})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows %d", len(rows))
	}
	got := rows[0]
	if got.Via != teamaudit.ViaConsole || got.Actor.Basis != teamaudit.BasisConsole || got.Outcome != teamaudit.OutcomeOK ||
		!got.OccurredAt.Equal(at) || got.After == nil || len(got.After.Composites) != 2 || got.After.Role != "form:manage" {
		t.Fatalf("row %+v %+v", got, got.After)
	}
}

func TestMemoryConsole(t *testing.T) {
	t.Parallel()
	testConsole(t, teamaudit.NewMemory())
}

func TestPostgresConsole(t *testing.T) {
	pool := migrated(t)
	testConsole(t, teamaudit.NewPostgres(pool))

	ctx := context.Background()
	// A console row never changes, and a row core writes carries no event id.
	var id uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM team_audit_records WHERE keycloak_event_id = 'ev-1'`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE team_audit_records SET keycloak_event_id = 'ev-9' WHERE id = $1`, id); err == nil {
		t.Fatal("a console row's event id changed")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO team_audit_records (id, occurred_at, actor_id, actor_basis, action, group_id, group_path, target_user_id, outcome, keycloak_event_id)
		VALUES (gen_random_uuid(), now(), gen_random_uuid(), 'privileged', 'member.add', gen_random_uuid(), '/UYELER', gen_random_uuid(), 'pending', 'ev-8')`); err == nil {
		t.Fatal("a row core writes took an event id")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO team_audit_records (id, occurred_at, actor_id, actor_basis, action, group_path, outcome, via, keycloak_event_id)
		VALUES (gen_random_uuid(), now(), gen_random_uuid(), 'console', 'keycloak.client', '', 'pending', 'console', 'ev-7')`); err == nil {
		t.Fatal("a console row was written pending")
	}
	// The cursor holds one row.
	if _, err := pool.Exec(ctx, `INSERT INTO console_audit_cursor (id, event_time) VALUES (2, 0)`); err == nil {
		t.Fatal("a second cursor")
	}
}
