package ticket_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// An Event YK owns with WEBLAB ("leaders"), GECEKODU ("members", its Team
// door scan on) and a person as Co-organizers (docs/co-organizers.md):
// WEBLAB's Leader and the person read its Tickets, give one out and scan at
// its door as YK's Leader would; a GECEKODU member reads them and scans
// through GECEKODU's Team door scan but gives none out; a WEBLAB member
// does nothing, WEBLAB's Team door scan on or not.
func TestTicketDecisionsSeeTheCoOrganizers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	events := event.NewMemoryStore()
	users := user.NewMemoryStore()
	dir := identity.NewMemory()
	for _, g := range []identity.Group{
		{ID: "g-weblab", Name: "WEBLAB", Path: "/UYELER/ARGE/WEBLAB", Attributes: map[string]string{"team_door_scan": "true"}},
		{ID: "g-gecekodu", Name: "GECEKODU", Path: "/UYELER/ORGANIZASYON/GECEKODU", Attributes: map[string]string{"team_door_scan": "true"}},
		{ID: "g-yk", Name: "YK", Path: "/UYELER/YK"},
	} {
		dir.PutGroup(g)
	}
	svc := ticket.NewService(ticket.NewMemoryStore(), events, authz.NewAuthorizer(authz.DefaultPolicy()), dir, users)
	ev := seedEvent(t, events, "YK")
	personID := uuid.New()
	if _, err := events.SetCoOrganizers(ctx, event.CoOrganizersWrite{
		EventID: ev.ID, OwnerTeam: "YK",
		Teams: []event.CoOrganizerTeam{
			{Code: "WEBLAB", Management: event.ManagementLeaders, Visibility: event.VisibilityCoOrganizer},
			{Code: "GECEKODU", Management: event.ManagementMembers, Visibility: event.VisibilityHelper},
		},
		People: []uuid.UUID{personID},
	}); err != nil {
		t.Fatal(err)
	}
	day := seedDay(t, events, ev.ID, "Day 1")
	sess := seedSession(t, events, day.ID, "Opening")

	for _, c := range []struct {
		name               string
		p                  authz.Principal
		read, assign, scan bool
	}{
		{"WEBLAB's Leader", authz.Principal{ID: "wl", Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}, true, true, true},
		{"the person", authz.Principal{ID: personID.String()}, true, true, true},
		{"a GECEKODU member", authz.Principal{ID: "gm", Groups: []string{"/UYELER/ORGANIZASYON/GECEKODU"}}, true, false, true},
		{"a WEBLAB member", authz.Principal{ID: "wm", Groups: []string{"/UYELER/ARGE/WEBLAB"}}, false, false, false},
		{"another team's Leader", authz.Principal{ID: "al", Groups: []string{"/UYELER/SANAT/ARTLAB/LIDERLER"}}, false, false, false},
	} {
		_, err := svc.ListByEvent(ctx, c.p, ev.ID)
		if (err == nil) != c.read || (err != nil && !errors.Is(err, ticket.ErrForbidden)) {
			t.Errorf("%s reads the Tickets: %v, want %v", c.name, err, c.read)
		}
		target := uuid.New()
		if _, _, err := user.NewService(users).Ensure(ctx, target, user.Profile{Email: target.String() + "@example.com", FirstName: "Ada"}); err != nil {
			t.Fatal(err)
		}
		_, err = svc.ApplyForOther(ctx, c.p, ev.ID, target)
		if (err == nil) != c.assign {
			t.Errorf("%s gives a Ticket out: %v, want %v", c.name, err, c.assign)
		}
		holder := uuid.New()
		tk, err := svc.Apply(ctx, authz.Principal{ID: holder.String()}, ev.ID)
		if err != nil {
			t.Fatal(err)
		}
		_, err = svc.CheckIn(ctx, c.p, tk.ID, sess.ID)
		if (err == nil) != c.scan {
			t.Errorf("%s scans at the door: %v, want %v", c.name, err, c.scan)
		}
	}
}
