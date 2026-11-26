package event_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
)

// scanTeams is a team tree whose Teams in scans have Team door scan on.
type scanTeams struct {
	fakeTeams
	scans map[string]bool
}

func (s scanTeams) TeamDoorScan(team string) bool { return s.scans[strings.ToUpper(team)] }

// viewer.can carries certificate.read and door.checkIn, each the decision
// its endpoint takes: a Co-organizer team's Leader and a person read the
// certificates and work the door; a member of a "members" team reads them
// only with certificate:issue or certificate:revoke, and works the door
// only while that team's Team door scan is on; a member of a "leaders" team
// neither; door staff work the door alone.
func TestViewerCertificateReadAndDoorCheckIn(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	teams := scanTeams{fakeTeams: organizerTeams, scans: map[string]bool{}}
	store := event.NewMemoryStore()
	svc := event.NewServiceWithOptions(store, authz.NewAuthorizer(authz.DefaultPolicy()), event.ServiceOptions{Teams: teams, CoOrganizersWrite: true, Now: listClock})
	staff := uuid.New()
	ev, err := svc.Create(ctx, ykMember, event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "ARTLAB", Status: event.StatusPublished, DoorStaffIDs: []uuid.UUID{staff}})
	if err != nil {
		t.Fatal(err)
	}
	person := uuid.New()
	if _, err := svc.SetOrganizers(ctx, ykMember, ev.ID, event.OrganizersInput{
		Teams:  []event.OrganizerTeamInput{{Code: "WEBLAB"}, {Code: "GECEKODU", Management: ptr(event.ManagementMembers)}},
		People: []uuid.UUID{person},
	}); err != nil {
		t.Fatal(err)
	}
	can := func(p authz.Principal) map[string]bool {
		t.Helper()
		got, err := svc.Get(ctx, &p, ev.ID)
		if err != nil {
			t.Fatal(err)
		}
		return svc.ProjectFor(&p, got).Viewer.Can
	}
	issuer := authz.Principal{ID: "gi", Groups: []string{"/UYELER/ORGANIZASYON/GECEKODU"}, Roles: []string{"certificate:issue"}}
	for name, c := range map[string]struct {
		p          authz.Principal
		read, door bool
	}{
		"WEBLAB's Leader":                {ownerLeader, true, true},
		"the person":                     {authz.Principal{ID: person.String()}, true, true},
		"a GECEKODU member":              {gecekoduMember, false, false},
		"a GECEKODU certificate manager": {issuer, true, false},
		"a WEBLAB member":                {weblabMember, false, false},
		"door staff":                     {authz.Principal{ID: staff.String()}, false, true},
	} {
		got := can(c.p)
		if got["certificate.read"] != c.read || got["door.checkIn"] != c.door {
			t.Errorf("%s: certificate.read %v door.checkIn %v, want %v %v", name, got["certificate.read"], got["door.checkIn"], c.read, c.door)
		}
	}
	// GECEKODU's Team door scan on: its members work the door; WEBLAB's
	// ("leaders") would not.
	teams.scans["GECEKODU"], teams.scans["WEBLAB"] = true, true
	if !can(gecekoduMember)["door.checkIn"] || can(weblabMember)["door.checkIn"] {
		t.Fatal("Team door scan: a members team's member, not a leaders team's")
	}
}
