package event_test

import (
	"context"
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

// The database's ?team= matches the Owner team and the Co-organizer teams
// shown outside the panel, never a helper team, drafts only when asked;
// ?managed= decides each Event the store reads; a copy writes the
// Co-organizer teams with their choices and, asked by who may change the
// list, the people whose accounts are active (one being erased stays out),
// who copies being who added them.
func TestCoOrganizerListsAndCopyPostgres(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	userStore := user.NewPostgresStore(pool)
	users := user.NewService(userStore)
	ada, grace, copier := uuid.New(), uuid.New(), uuid.New()
	for id, email := range map[uuid.UUID]string{ada: "ada@example.test", grace: "grace@example.test", copier: "al@example.test"} {
		if _, _, err := users.Ensure(ctx, id, user.Profile{Email: email}); err != nil {
			t.Fatal(err)
		}
	}
	store := event.NewPostgresStore(pool)
	svc := event.NewServiceWithOptions(store, authz.NewAuthorizer(authz.DefaultPolicy()), event.ServiceOptions{Teams: organizerTeams, CoOrganizersWrite: true, Now: listClock})
	start, end := time.Date(2026, 12, 12, 10, 0, 0, 0, time.UTC), time.Date(2026, 12, 12, 18, 0, 0, 0, time.UTC)
	create := func(owner string, status event.Status) event.Event {
		t.Helper()
		e, err := svc.Create(ctx, ykMember, event.Event{Name: "Hack " + owner, Location: "YTÜ", OwnerTeam: owner, Status: status, StartDate: &start, EndDate: &end})
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	published, draft, other := create("ARTLAB", event.StatusPublished), create("ARTLAB", event.StatusDraft), create("ALGOLAB", event.StatusPublished)
	if _, err := svc.SetOrganizers(ctx, ykMember, published.ID, event.OrganizersInput{
		Teams: []event.OrganizerTeamInput{
			{Code: "WEBLAB", Management: ptr(event.ManagementLeaders)},
			{Code: "GECEKODU", Management: ptr(event.ManagementMembers), Visibility: ptr(event.VisibilityHelper)},
		},
		People: []uuid.UUID{ada, grace},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetOrganizers(ctx, ykMember, draft.ID, event.OrganizersInput{Teams: []event.OrganizerTeamInput{{Code: "WEBLAB"}}}); err != nil {
		t.Fatal(err)
	}

	for name, c := range map[string]struct {
		team          string
		active, draft bool
		want          []event.Event
	}{
		"WEBLAB":             {"WEBLAB", false, false, []event.Event{published}},
		"WEBLAB with drafts": {"WEBLAB", false, true, []event.Event{published, draft}},
		"WEBLAB active":      {"WEBLAB", true, false, []event.Event{published}},
		"a helper team":      {"GECEKODU", false, true, nil},
		"the Owner team":     {"ARTLAB", false, true, []event.Event{published, draft}},
		"another team":       {"ALGOLAB", false, false, []event.Event{other}},
	} {
		got, err := store.ListTeam(ctx, c.team, c.active, c.draft)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		sameIDs(t, name, got, c.want...)
	}
	listed, err := store.ListTeam(ctx, "WEBLAB", false, false)
	if err != nil || len(listed) != 1 || len(listed[0].CoOrganizers) != 2 || len(listed[0].CoOrganizerIDs) != 2 {
		t.Fatalf("a listed Event carries its Co-organizers: %+v %v", listed, err)
	}
	managed, err := svc.ListManaged(ctx, gecekoduMember, "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	sameIDs(t, "managed by a member of a members helper team", managed, published)
	managed, _ = svc.ListManaged(ctx, authz.Principal{ID: ada.String()}, "", "", false)
	sameIDs(t, "managed by a person", managed, published)

	// Grace is being erased: her row stays on the source, not on the copy.
	if _, err := userStore.RequestDeletion(ctx, grace, nil); err != nil {
		t.Fatal(err)
	}
	owner := authz.Principal{ID: copier.String(), Groups: []string{"/UYELER/SANAT/ARTLAB/LIDERLER"}}
	next := time.Date(2027, 12, 12, 10, 0, 0, 0, time.UTC)
	copied, _, err := svc.Copy(ctx, owner, published.ID, event.CopyInput{ID: uuid.New(), Name: "Hack 2027", StartDate: &next, IncludeCoOrganizerPeople: true})
	if err != nil {
		t.Fatal(err)
	}
	wantTeams := []event.CoOrganizerTeam{
		{Code: "GECEKODU", Management: event.ManagementMembers, Visibility: event.VisibilityHelper},
		{Code: "WEBLAB", Management: event.ManagementLeaders, Visibility: event.VisibilityCoOrganizer},
	}
	if !slices.Equal(copied.Event.CoOrganizers, wantTeams) || !slices.Equal(copied.Event.CoOrganizerIDs, []uuid.UUID{ada}) || len(copied.Skipped) != 0 {
		t.Fatalf("copy: %+v %v %v", copied.Event.CoOrganizers, copied.Event.CoOrganizerIDs, copied.Skipped)
	}
	var adders int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM event_co_organizers WHERE event_id = $1 AND added_by = $2`, copied.Event.ID, copier).Scan(&adders); err != nil || adders != 3 {
		t.Fatalf("added by the copier: %d %v", adders, err)
	}

	// WEBLAB's Leader asks for the people too: the copy stays ARTLAB's, its
	// teams come along, the people are named skipped.
	co, _, err := svc.Copy(ctx, ownerLeader, published.ID, event.CopyInput{ID: uuid.New(), Name: "Hack co", StartDate: &next, IncludeCoOrganizerPeople: true})
	if err != nil {
		t.Fatal(err)
	}
	if co.Event.OwnerTeam != "ARTLAB" || !slices.Equal(co.Event.CoOrganizers, wantTeams) || co.Event.CoOrganizerIDs != nil ||
		!slices.Equal(co.Skipped, []string{event.SkippedCoOrganizerPeople}) {
		t.Fatalf("WEBLAB's Leader's copy: %s %+v %v %v", co.Event.OwnerTeam, co.Event.CoOrganizers, co.Event.CoOrganizerIDs, co.Skipped)
	}
}
