package event_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/teamaudit"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// In the database: each change of the list is a finished row of
// team_audit_records (an Event without an Owner team's without a Group);
// while the mail is on, each team and person added or removed is queued
// once (a choice changed is not), off nothing is; a write decided against a
// list that changed meanwhile is a conflict and writes nothing.
func TestCoOrganizerAuditAndMailPostgres(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	ada, ykID := uuid.New(), uuid.New()
	users := user.NewService(user.NewPostgresStore(pool))
	for id, email := range map[uuid.UUID]string{ada: "ada@example.test", ykID: "yk@example.test"} {
		if _, _, err := users.Ensure(ctx, id, user.Profile{Email: email}); err != nil {
			t.Fatal(err)
		}
	}
	dir := identity.NewMemory()
	dir.PutGroup(identity.Group{ID: uuid.NewString(), Name: "ARTLAB", Path: "/UYELER/SANAT/ARTLAB"})
	store := event.NewPostgresStore(pool)
	svc := event.NewServiceWithOptions(store, authz.NewAuthorizer(authz.DefaultPolicy()), event.ServiceOptions{
		Teams: organizerTeams, CoOrganizersWrite: true, Audit: teamaudit.NewPostgres(pool), Groups: dir,
	})
	yk := authz.Principal{ID: ykID.String(), Groups: []string{"/UYELER/YK"}}
	actx := teamaudit.WithActor(ctx, teamaudit.Actor{ID: ykID, Basis: teamaudit.BasisPrivileged})
	ev, err := svc.Create(ctx, yk, event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "ARTLAB", Status: event.StatusPublished})
	if err != nil {
		t.Fatal(err)
	}
	noTeam, err := svc.Create(ctx, yk, event.Event{Name: "Kulüp", Location: "YTÜ", Status: event.StatusPublished})
	if err != nil {
		t.Fatal(err)
	}
	count := func(sql string, args ...any) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	store.NotifyCoOrganizers(true)
	if _, err := svc.SetOrganizers(actx, yk, ev.ID, event.OrganizersInput{Teams: []event.OrganizerTeamInput{{Code: "WEBLAB"}}, People: []uuid.UUID{ada}}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetOrganizers(actx, yk, ev.ID, event.OrganizersInput{Teams: []event.OrganizerTeamInput{{Code: "WEBLAB", Visibility: ptr(event.VisibilityHelper)}}}); err != nil {
		t.Fatal(err)
	}
	if n := count(`SELECT count(*) FROM team_audit_records WHERE action LIKE 'co_organizer.%' AND outcome = 'ok' AND group_path = '/UYELER/SANAT/ARTLAB'`); n != 4 {
		t.Fatalf("audit rows %d", n)
	}
	if n := count(`SELECT count(*) FROM team_audit_records WHERE action = 'co_organizer.change' AND after->>'visibility' = 'helper' AND before->>'visibility' = 'co_organizer'`); n != 1 {
		t.Fatalf("change row %d", n)
	}
	queued := count(`SELECT count(*) FROM co_organizer_notifications WHERE event_id = $1`, ev.ID)
	if queued != 3 || count(`SELECT count(*) FROM co_organizer_notifications WHERE kind = 'removed' AND user_id = $1 AND actor_id = $2`, ada, ykID) != 1 {
		t.Fatalf("queued %d", queued)
	}

	store.NotifyCoOrganizers(false)
	if _, err := svc.SetOrganizers(actx, yk, noTeam.ID, event.OrganizersInput{Teams: []event.OrganizerTeamInput{{Code: "WEBLAB"}}}); err != nil {
		t.Fatal(err)
	}
	if n := count(`SELECT count(*) FROM team_audit_records WHERE action = 'co_organizer.add' AND group_id IS NULL AND group_path = '' AND after->>'eventId' = $1`, noTeam.ID.String()); n != 1 {
		t.Fatalf("no Owner team: %d", n)
	}
	if n := count(`SELECT count(*) FROM co_organizer_notifications WHERE event_id = $1`, noTeam.ID); n != 0 {
		t.Fatalf("queued while off: %d", n)
	}

	_, err = store.SetCoOrganizers(ctx, event.CoOrganizersWrite{EventID: ev.ID, OwnerTeam: "ARTLAB", Expect: true, WasTeams: nil})
	if !errors.Is(err, event.ErrConflict) {
		t.Fatalf("a list that changed meanwhile: %v", err)
	}
	if got, err := store.Get(ctx, ev.ID); err != nil || len(got.CoOrganizers) != 1 {
		t.Fatalf("the conflict wrote: %+v %v", got.CoOrganizers, err)
	}
}
