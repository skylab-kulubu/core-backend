package event_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// The database's Co-organizers: the service writes and reads them back with
// the people's names and e-mails; a list and a read of several Events carry
// each Event's own; a person who is no account is refused, naming them, and
// nothing is written; an Event update keeps the list but drops the team
// that becomes the Owner team; a write whose Owner team moved meanwhile is
// a conflict.
func TestCoOrganizersPostgres(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	users := user.NewService(user.NewPostgresStore(pool))
	ada, adder := uuid.New(), uuid.New()
	for id, profile := range map[uuid.UUID]user.Profile{
		ada:   {Email: "ada@example.test", FirstName: "Ada", LastName: "Lovelace"},
		adder: {Email: "yk@example.test"},
	} {
		if _, _, err := users.Ensure(ctx, id, profile); err != nil {
			t.Fatal(err)
		}
	}
	store := event.NewPostgresStore(pool)
	svc := event.NewServiceWithOptions(store, authz.NewAuthorizer(authz.DefaultPolicy()), event.ServiceOptions{Teams: organizerTeams})
	actor := authz.Principal{ID: adder.String(), Groups: []string{"/UYELER/YK"}}
	first := createEvent(t, svc, "WEBLAB", event.StatusPublished)
	second := createEvent(t, svc, "WEBLAB", event.StatusPublished)

	got, err := svc.SetOrganizers(ctx, actor, first.ID, event.OrganizersInput{
		Teams:  []event.OrganizerTeamInput{{Code: "gecekodu", Visibility: ptr(event.VisibilityHelper)}, {Code: "ARTLAB"}},
		People: []uuid.UUID{ada},
	})
	if err != nil {
		t.Fatal(err)
	}
	wantTeams := []event.CoOrganizerTeam{
		{Code: "ARTLAB", Management: event.ManagementLeaders, Visibility: event.VisibilityCoOrganizer},
		{Code: "GECEKODU", Management: event.ManagementLeaders, Visibility: event.VisibilityHelper},
	}
	if len(got.Teams) != 2 || got.Teams[0].Code != "ARTLAB" || got.Teams[1].Code != "GECEKODU" {
		t.Fatalf("teams %+v", got.Teams)
	}
	if len(got.People) != 1 || got.People[0].ID != ada || got.People[0].Name != "Ada Lovelace" || got.People[0].Email != "ada@example.test" || got.People[0].ExpiresAt == nil {
		t.Fatalf("people %+v", got.People)
	}
	var addedBy uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT added_by FROM event_co_organizers WHERE event_id = $1 AND user_id = $2`, first.ID, ada).Scan(&addedBy); err != nil || addedBy != adder {
		t.Fatalf("added_by %v %v", addedBy, err)
	}
	if _, err := svc.SetOrganizers(ctx, actor, second.ID, event.OrganizersInput{Teams: []event.OrganizerTeamInput{{Code: "OYUNLAB"}}}); err != nil {
		t.Fatal(err)
	}
	listed, err := store.List(ctx, "WEBLAB", false)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range listed {
		switch e.ID {
		case first.ID:
			if !slices.Equal(e.CoOrganizers, wantTeams) || !slices.Equal(e.CoOrganizerIDs, []uuid.UUID{ada}) {
				t.Errorf("first listed %+v %v", e.CoOrganizers, e.CoOrganizerIDs)
			}
		case second.ID:
			if len(e.CoOrganizers) != 1 || e.CoOrganizers[0].Code != "OYUNLAB" || e.CoOrganizerIDs != nil {
				t.Errorf("second listed %+v %v", e.CoOrganizers, e.CoOrganizerIDs)
			}
		}
	}

	// A person who is no account: refused, nothing written.
	stranger := uuid.New()
	_, err = svc.SetOrganizers(ctx, actor, first.ID, event.OrganizersInput{People: []uuid.UUID{ada, stranger}})
	var refused *event.OrganizersError
	if !errors.As(err, &refused) || refused.Field != "people" || refused.Index != 1 || refused.Reason != event.OrganizersInactivePerson {
		t.Fatalf("a stranger: %v", err)
	}
	if read, err := store.Get(ctx, first.ID); err != nil || !slices.Equal(read.CoOrganizers, wantTeams) {
		t.Fatalf("after the refusal %+v %v", read.CoOrganizers, err)
	}

	// An update keeps the list; making ARTLAB the Owner team drops it.
	updated, err := svc.Update(ctx, actor, first.ID, event.Event{Name: "Hack 2", Location: "YTÜ", OwnerTeam: "WEBLAB"})
	if err != nil || !slices.Equal(updated.CoOrganizers, wantTeams) || len(updated.CoOrganizerIDs) != 1 {
		t.Fatalf("update %+v %v", updated.CoOrganizers, err)
	}
	updated, err = svc.Update(ctx, actor, first.ID, event.Event{Name: "Hack 2", Location: "YTÜ", OwnerTeam: "ARTLAB"})
	if err != nil || !slices.Equal(updated.CoOrganizers, wantTeams[1:]) || len(updated.CoOrganizerIDs) != 1 {
		t.Fatalf("owner change %+v %v", updated.CoOrganizers, err)
	}

	// The Owner team moved after the request read it.
	if _, err := store.SetCoOrganizers(ctx, event.CoOrganizersWrite{EventID: first.ID, OwnerTeam: "WEBLAB"}); !errors.Is(err, event.ErrConflict) {
		t.Fatalf("a moved Owner team: %v", err)
	}
	if _, err := store.SetCoOrganizers(ctx, event.CoOrganizersWrite{EventID: uuid.New(), OwnerTeam: "WEBLAB"}); !errors.Is(err, event.ErrNotFound) {
		t.Fatalf("no Event: %v", err)
	}
}
