package event_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// The database's Co-organizers: the service writes and reads them back with
// the people's names, and their e-mails to who may change the list alone; a
// list and a read of several Events carry each Event's own; a new person who
// is no account is refused, naming them, and nothing is written; a write is
// the difference (a row that stays keeps who added it and when, a team's
// choices are updated in place, a person who stays is kept though their
// account is no longer active); an Event update keeps the list but drops
// the team that becomes the Owner team; a write whose Owner team moved
// meanwhile is a conflict.
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
	svc := event.NewServiceWithOptions(store, authz.NewAuthorizer(authz.DefaultPolicy()), event.ServiceOptions{Teams: organizerTeams, CoOrganizersWrite: true})
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
	// ARTLAB's Leader, a Co-organizer, edits the Event but may not change
	// the list: the person's name, not their e-mail; the Owner team's
	// Leader manages it and reads the e-mail.
	read, err := svc.Organizers(ctx, unrelatedLead, first.ID)
	if err != nil || read.CanManage || len(read.People) != 1 || read.People[0].Name != "Ada Lovelace" || read.People[0].Email != "" {
		t.Fatalf("the Co-organizer's Leader reads %+v %v", read.People, err)
	}
	if read, err = svc.Organizers(ctx, ownerLeader, first.ID); err != nil || !read.CanManage || read.People[0].Email != "ada@example.test" {
		t.Fatalf("the Owner team's Leader reads %+v %v", read.People, err)
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

	// The difference: ARTLAB and Ada stay as added (by, at), GECEKODU's
	// choices change in place, OYUNLAB comes in by another adder. Ada's
	// account is being erased meanwhile: she stays, being no new row.
	other := uuid.New()
	if _, _, err := users.Ensure(ctx, other, user.Profile{Email: "admin@example.test"}); err != nil {
		t.Fatal(err)
	}
	type row struct {
		by uuid.UUID
		at time.Time
	}
	rows := func() map[string]row {
		t.Helper()
		got, err := pool.Query(ctx, `SELECT COALESCE(team_code, user_id::text), added_by, added_at FROM event_co_organizers WHERE event_id = $1`, first.ID)
		if err != nil {
			t.Fatal(err)
		}
		defer got.Close()
		out := map[string]row{}
		for got.Next() {
			var key string
			var r row
			if err := got.Scan(&key, &r.by, &r.at); err != nil {
				t.Fatal(err)
			}
			out[key] = r
		}
		return out
	}
	before := rows()
	if _, err := pool.Exec(ctx, `UPDATE users SET account_state = 'deletion_pending' WHERE id = $1`, ada); err != nil {
		t.Fatal(err)
	}
	admin := authz.Principal{ID: other.String(), Groups: []string{"/UYELER/YK"}}
	got, err = svc.SetOrganizers(ctx, admin, first.ID, event.OrganizersInput{
		Teams:  []event.OrganizerTeamInput{{Code: "ARTLAB"}, {Code: "GECEKODU", Management: ptr(event.ManagementMembers), Visibility: ptr(event.VisibilityCoOrganizer)}, {Code: "OYUNLAB"}},
		People: []uuid.UUID{ada},
	})
	if err != nil {
		t.Fatalf("a write that keeps a person no longer active: %v", err)
	}
	after := rows()
	for _, kept := range []string{"ARTLAB", "GECEKODU", ada.String()} {
		if after[kept] != before[kept] {
			t.Errorf("%s: %+v, was %+v", kept, after[kept], before[kept])
		}
	}
	if after["OYUNLAB"].by != other || len(after) != 4 {
		t.Errorf("after %+v", after)
	}
	if len(got.Teams) != 3 || got.Teams[1].Code != "GECEKODU" || got.Teams[1].Management != event.ManagementMembers || got.Teams[1].Visibility != event.VisibilityCoOrganizer {
		t.Errorf("teams %+v", got.Teams)
	}
	if _, err := svc.SetOrganizers(ctx, actor, first.ID, event.OrganizersInput{Teams: []event.OrganizerTeamInput{{Code: "GECEKODU", Visibility: ptr(event.VisibilityHelper)}, {Code: "ARTLAB"}}, People: []uuid.UUID{ada}}); err != nil {
		t.Fatal(err)
	}
	if len(rows()) != 3 {
		t.Fatalf("OYUNLAB stayed: %+v", rows())
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

// HasCoOrganizerSeat: a person whose rights have not ended, and a member of
// a Co-organizer team; not a person 30 days after the Event, nor anyone
// else (authz.CoOrganizerSeats).
func TestHasCoOrganizerSeatPostgres(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	users := user.NewService(user.NewPostgresStore(pool))
	current, ended := uuid.New(), uuid.New()
	for _, id := range []uuid.UUID{current, ended} {
		if _, _, err := users.Ensure(ctx, id, user.Profile{Email: id.String() + "@example.test"}); err != nil {
			t.Fatal(err)
		}
	}
	store := event.NewPostgresStore(pool)
	soon, past := time.Now().Add(24*time.Hour), time.Now().Add(-40*24*time.Hour)
	for _, c := range []struct {
		end    time.Time
		people []uuid.UUID
		teams  []event.CoOrganizerTeam
	}{
		{soon, []uuid.UUID{current}, []event.CoOrganizerTeam{{Code: "WEBLAB", Management: event.ManagementLeaders, Visibility: event.VisibilityHelper}}},
		{past, []uuid.UUID{ended}, nil},
	} {
		e, err := store.Create(ctx, event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "YK", EndDate: &c.end})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.SetCoOrganizers(ctx, event.CoOrganizersWrite{EventID: e.ID, OwnerTeam: "YK", Teams: c.teams, People: c.people}); err != nil {
			t.Fatal(err)
		}
	}
	for name, c := range map[string]struct {
		p    authz.Principal
		want bool
	}{
		"the current person":       {authz.Principal{ID: current.String()}, true},
		"a WEBLAB member":          {authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/ARGE/WEBLAB"}}, true},
		"the person 30 days after": {authz.Principal{ID: ended.String()}, false},
		"another team's Leader":    {authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/SANAT/ARTLAB/LIDERLER"}}, false},
		"no id":                    {authz.Principal{ID: "x"}, false},
	} {
		if got := store.HasCoOrganizerSeat(c.p); got != c.want {
			t.Errorf("%s: %v, want %v", name, got, c.want)
		}
	}
}
