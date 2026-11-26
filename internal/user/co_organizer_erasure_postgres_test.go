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

// An Event's Co-organizer person goes with the account (docs/co-organizers.md);
// a row the erased account added stays, without who added it; so do their
// mails not sent yet and the actor of the others (docs/co-organizers.md#mails).
func TestAnonymizationRemovesTheCoOrganizerPerson(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	users := user.NewPostgresStore(pool)
	person, other := uuid.New(), uuid.New()
	for id, email := range map[uuid.UUID]string{person: "co@example.test", other: "other@example.test"} {
		if _, _, err := user.NewService(users).Ensure(ctx, id, user.Profile{Email: email}); err != nil {
			t.Fatal(err)
		}
	}
	eventID := uuid.New()
	for _, statement := range []string{
		`INSERT INTO events (id, name, location, owner_team) VALUES ('` + eventID.String() + `', 'Hack', 'YTÜ', 'YK')`,
		`INSERT INTO event_co_organizers (event_id, user_id, added_by) VALUES ('` + eventID.String() + `', '` + person.String() + `', '` + other.String() + `')`,
		`INSERT INTO event_co_organizers (event_id, user_id, added_by) VALUES ('` + eventID.String() + `', '` + other.String() + `', '` + person.String() + `')`,
		`INSERT INTO event_co_organizers (event_id, team_code, management, visibility, added_by) VALUES ('` + eventID.String() + `', 'WEBLAB', 'leaders', 'co_organizer', '` + person.String() + `')`,
		`INSERT INTO co_organizer_notifications (id, event_id, kind, user_id, actor_id, next_attempt_at) VALUES (gen_random_uuid(), '` + eventID.String() + `', 'added', '` + person.String() + `', '` + other.String() + `', now())`,
		`INSERT INTO co_organizer_notifications (id, event_id, kind, team_code, actor_id, next_attempt_at) VALUES (gen_random_uuid(), '` + eventID.String() + `', 'added', 'WEBLAB', '` + person.String() + `', now())`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	if _, err := users.RequestDeletion(ctx, person, nil); err != nil {
		t.Fatal(err)
	}
	if err := users.AnonymizeAccount(ctx, person, time.Now().UTC(), nil); err != nil {
		t.Fatal(err)
	}
	var rows, named, byPerson int
	if err := pool.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE user_id = $2), count(*) FILTER (WHERE added_by = $2)
		FROM event_co_organizers WHERE event_id = $1`, eventID, person).Scan(&rows, &named, &byPerson); err != nil {
		t.Fatal(err)
	}
	if rows != 2 || named != 0 || byPerson != 0 {
		t.Fatalf("rows %d, the person's %d, added by the person %d", rows, named, byPerson)
	}
	// Their mails not sent go; a mail of a change they made stays, without them.
	var mails, theirs, byThem int
	if err := pool.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE user_id = $1), count(*) FILTER (WHERE actor_id = $1)
		FROM co_organizer_notifications`, person).Scan(&mails, &theirs, &byThem); err != nil {
		t.Fatal(err)
	}
	if mails != 1 || theirs != 0 || byThem != 0 {
		t.Fatalf("mails %d, theirs %d, by them %d", mails, theirs, byThem)
	}
}
