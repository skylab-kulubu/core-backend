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
)

// AuthzResource carries the Owner team, the Co-organizer teams with their
// management, the door staff, and the Co-organizer people until 30 days
// after the Event (none ends for an Event with no date).
func TestAuthzResource(t *testing.T) {
	t.Parallel()
	end := time.Date(2026, 12, 12, 18, 0, 0, 0, time.UTC)
	person, staff := uuid.New(), uuid.New()
	e := event.Event{
		OwnerTeam: "YK", EndDate: &end, DoorStaffIDs: []uuid.UUID{staff},
		CoOrganizers: []event.CoOrganizerTeam{
			{Code: "WEBLAB", Management: event.ManagementLeaders, Visibility: event.VisibilityCoOrganizer},
			{Code: "GECEKODU", Management: event.ManagementMembers, Visibility: event.VisibilityHelper},
		},
		CoOrganizerIDs: []uuid.UUID{person},
	}
	r := event.AuthzResource(e, authz.TypeTicket, end.Add(29*24*time.Hour))
	if r.Type != authz.TypeTicket || r.OwnerTeam != "YK" || !slices.Equal(r.DoorStaffIDs, []string{staff.String()}) ||
		!slices.Equal(r.CoOwnerTeams, []authz.CoTeam{{Code: "WEBLAB"}, {Code: "GECEKODU", MembersManage: true}}) ||
		!slices.Equal(r.CoOrganizerIDs, []string{person.String()}) {
		t.Fatalf("within 30 days: %+v", r)
	}
	if r := event.AuthzResource(e, authz.TypeEvent, end.Add(30*24*time.Hour)); r.CoOrganizerIDs != nil || len(r.CoOwnerTeams) != 2 {
		t.Fatalf("30 days after: %+v", r)
	}
	e.EndDate = nil
	if r := event.AuthzResource(e, authz.TypeEvent, end.Add(365*24*time.Hour)); len(r.CoOrganizerIDs) != 1 {
		t.Fatalf("no date: %+v", r)
	}
}

// On an Event YK owns with WEBLAB ("leaders"), GECEKODU ("members") and a
// person: WEBLAB's Leader, a GECEKODU member and the person see the draft,
// edit it, publish it and run its days; none deletes it or moves it to
// another Owner team; a WEBLAB member does nothing; 30 days after the Event
// the person does nothing either.
func TestCoOrganizersInTheEventDecisions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Date(2026, 12, 1, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	store := event.NewMemoryStore()
	svc := event.NewServiceWithOptions(store, authz.NewAuthorizer(authz.DefaultPolicy()), event.ServiceOptions{Teams: organizerTeams, CoOrganizersWrite: true, Now: clock})
	start, end := time.Date(2026, 12, 12, 10, 0, 0, 0, time.UTC), time.Date(2026, 12, 12, 18, 0, 0, 0, time.UTC)
	draft, err := svc.Create(ctx, ykMember, event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "YK", Status: event.StatusDraft, StartDate: &start, EndDate: &end})
	if err != nil {
		t.Fatal(err)
	}
	personID := uuid.New()
	if _, err := svc.SetOrganizers(ctx, ykMember, draft.ID, event.OrganizersInput{
		Teams:  []event.OrganizerTeamInput{{Code: "WEBLAB"}, {Code: "GECEKODU", Management: ptr(event.ManagementMembers)}},
		People: []uuid.UUID{personID},
	}); err != nil {
		t.Fatal(err)
	}
	coLeader := ownerLeader // WEBLAB's Leader
	coMember := authz.Principal{ID: "gm", Groups: []string{"/UYELER/ORGANIZASYON/GECEKODU"}}
	person := authz.Principal{ID: personID.String()}
	weblabMember := authz.Principal{ID: "wm", Groups: []string{"/UYELER/ARGE/WEBLAB"}}

	for name, p := range map[string]authz.Principal{"WEBLAB's Leader": coLeader, "a GECEKODU member": coMember, "the person": person} {
		if _, err := svc.Get(ctx, &p, draft.ID); err != nil {
			t.Fatalf("%s does not see the draft: %v", name, err)
		}
		if _, err := svc.Update(ctx, p, draft.ID, event.Event{Name: "Hack " + name, Location: "YTÜ", OwnerTeam: "YK", StartDate: &start, EndDate: &end}); err != nil {
			t.Errorf("%s does not edit: %v", name, err)
		}
		day, err := svc.CreateDay(ctx, p, event.Day{EventID: draft.ID, Name: name})
		if err != nil {
			t.Errorf("%s does not add a day: %v", name, err)
		}
		if name != "a GECEKODU member" {
			if err := svc.DeleteDay(ctx, p, day.ID); err != nil {
				t.Errorf("%s does not archive a day: %v", name, err)
			}
		}
		if err := svc.Delete(ctx, p, draft.ID); !errors.Is(err, event.ErrForbidden) {
			t.Errorf("%s deletes the Event: %v", name, err)
		}
		if _, err := svc.Update(ctx, p, draft.ID, event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB", StartDate: &start, EndDate: &end}); !errors.Is(err, event.ErrForbidden) {
			t.Errorf("%s moves the Event to WEBLAB: %v", name, err)
		}
		if _, err := svc.SetOrganizers(ctx, p, draft.ID, event.OrganizersInput{}); !errors.Is(err, event.ErrForbidden) {
			t.Errorf("%s changes the list: %v", name, err)
		}
	}
	if _, err := svc.Get(ctx, &weblabMember, draft.ID); !errors.Is(err, event.ErrNotFound) {
		t.Errorf("a member of a leaders team sees the draft: %v", err)
	}
	if _, err := svc.Update(ctx, weblabMember, draft.ID, event.Event{Name: "x", Location: "YTÜ", OwnerTeam: "YK"}); !errors.Is(err, event.ErrForbidden) {
		t.Errorf("a member of a leaders team edits: %v", err)
	}
	if _, err := svc.Publish(ctx, coLeader, draft.ID, false); err != nil {
		t.Errorf("WEBLAB's Leader does not publish: %v", err)
	}

	// The list of drafts and published Events is decided per Event.
	listed, err := svc.List(ctx, &coMember, "", false, true)
	if err != nil || len(listed) != 1 {
		t.Fatalf("listed %d %v", len(listed), err)
	}

	// 30 days after the Event the person's rights are gone.
	now = end.Add(30*24*time.Hour + time.Minute)
	if _, err := svc.Update(ctx, person, draft.ID, event.Event{Name: "late", Location: "YTÜ", OwnerTeam: "YK", StartDate: &start, EndDate: &end}); !errors.Is(err, event.ErrForbidden) {
		t.Errorf("the person edits after 30 days: %v", err)
	}
	if _, err := svc.Update(ctx, coLeader, draft.ID, event.Event{Name: "late", Location: "YTÜ", OwnerTeam: "YK", StartDate: &start, EndDate: &end}); err != nil {
		t.Errorf("a team's rights end: %v", err)
	}
}

// viewer.can answers, key by key, what the decision it names answers; via
// names the strongest seat; who is signed out gets none.
func TestViewerMatchesTheDecisions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	az := authz.NewAuthorizer(authz.DefaultPolicy())
	store := event.NewMemoryStore()
	svc := event.NewServiceWithOptions(store, az, event.ServiceOptions{Teams: organizerTeams, CoOrganizersWrite: true})
	personID := uuid.New()
	e := createEvent(t, svc, "YK", event.StatusPublished)
	if _, err := svc.SetOrganizers(ctx, ykMember, e.ID, event.OrganizersInput{
		Teams:  []event.OrganizerTeamInput{{Code: "WEBLAB"}, {Code: "GECEKODU", Management: ptr(event.ManagementMembers)}},
		People: []uuid.UUID{personID},
	}); err != nil {
		t.Fatal(err)
	}
	read, err := svc.Get(ctx, nil, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	actions := map[string]struct {
		typ authz.Type
		a   authz.Action
	}{
		"event.update": {authz.TypeEvent, authz.Update}, "event.delete": {authz.TypeEvent, authz.Delete},
		"event.publish": {authz.TypeEvent, authz.Update}, "event.manageOrganizers": {authz.TypeEvent, authz.ManageOrganizers},
		"event.assignDoorStaff": {authz.TypeEvent, authz.Assign}, "ticket.read": {authz.TypeTicket, authz.Read},
		"ticket.assign": {authz.TypeTicket, authz.Assign}, "certificate.issue": {authz.TypeCertificate, authz.Issue},
		"certificate.revoke": {authz.TypeCertificate, authz.Revoke}, "certificate.template.assign": {authz.TypeCertificateTemplate, authz.Assign},
		"competition.manage": {authz.TypeCompetition, authz.Update},
		"certificate.read":   {authz.TypeCertificate, authz.Read}, "door.checkIn": {authz.TypeTicket, authz.Validate},
	}
	if keys := event.ViewerAbilities(); len(keys) != len(actions) {
		t.Fatalf("keys %v", keys)
	}
	for _, c := range []struct {
		p   authz.Principal
		via authz.Via
	}{
		{ykMember, authz.ViaPrivileged},
		{dkMember, authz.ViaNone},
		{ownerLeader, authz.ViaCoTeam},
		{authz.Principal{ID: "gm", Groups: []string{"/UYELER/ORGANIZASYON/GECEKODU"}}, authz.ViaCoTeam},
		{authz.Principal{ID: personID.String()}, authz.ViaCoPerson},
		{authz.Principal{ID: "wm", Groups: []string{"/UYELER/ARGE/WEBLAB"}}, authz.ViaNone},
		{unrelatedLead, authz.ViaNone},
	} {
		got := svc.ProjectFor(&c.p, read).Viewer
		if got == nil || got.Via != c.via {
			t.Errorf("%s: viewer %+v, via want %s", c.p.ID, got, c.via)
			continue
		}
		for _, key := range event.ViewerAbilities() {
			want := az.Allow(c.p, event.AuthzResource(read, actions[key].typ, time.Now()), actions[key].a)
			switch key {
			case "event.delete":
				// The endpoint archives a published Event for event:manage alone.
				want = want && az.Permitted(c.p, authz.RoleEventManage)
			case "certificate.template.assign":
				// Who may edit the Event binds its template.
				want = want || az.Allow(c.p, event.AuthzResource(read, authz.TypeEvent, time.Now()), authz.Update)
			}
			if got.Can[key] != want {
				t.Errorf("%s: %s = %v, the decision says %v", c.p.ID, key, got.Can[key], want)
			}
		}
	}
	if svc.ProjectFor(nil, read).Viewer != nil {
		t.Error("a signed-out caller has a viewer")
	}
	// Off, nobody may manage the list, whatever the decision says.
	off := event.NewServiceWithOptions(store, az, event.ServiceOptions{Teams: organizerTeams})
	if off.ProjectFor(&ykMember, read).Viewer.Can["event.manageOrganizers"] {
		t.Error("manageOrganizers while the writes are off")
	}
}
