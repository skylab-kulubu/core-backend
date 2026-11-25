package ticket_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/teamaudit"
)

// A Co-organizer's Ticket write is recorded too, never refused for want of
// a basis (docs/co-organizers.md): a Co-organizer team's Leader as a Leader
// through their Team, a Co-organizer person as an event editor through the
// Event's team.
func TestCoOrganizersTicketAuditBasis(t *testing.T) {
	ctx := context.Background()
	dir := identity.NewMemory()
	weblab := identity.Group{ID: uuid.NewString(), Name: "WEBLAB", Path: "/UYELER/ARGE/WEBLAB"}
	artlab := identity.Group{ID: uuid.NewString(), Name: "ARTLAB", Path: "/UYELER/SANAT/ARTLAB"}
	dir.PutGroup(weblab)
	dir.PutGroup(artlab)
	audit := teamaudit.NewMemory()
	e := newCancelEnv(t, dir, audit)
	personID := uuid.New()
	if _, err := e.events.SetCoOrganizers(ctx, event.CoOrganizersWrite{
		EventID: e.ev.ID, OwnerTeam: "WEBLAB",
		Teams:  []event.CoOrganizerTeam{{Code: "ARTLAB", Management: event.ManagementLeaders, Visibility: event.VisibilityCoOrganizer}},
		People: []uuid.UUID{personID},
	}); err != nil {
		t.Fatal(err)
	}
	as := func(p authz.Principal) context.Context {
		return teamaudit.WithActor(ctx, teamaudit.Actor{ID: uuid.MustParse(p.ID), Client: "core-frontend"})
	}
	artlabLead := authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/SANAT/ARTLAB/LIDERLER"}}
	person := authz.Principal{ID: personID.String()}
	if _, err := e.svc.CancelTicket(as(artlabLead), artlabLead, e.guest.ID, ""); err != nil {
		t.Fatalf("ARTLAB's Leader cancels: %v", err)
	}
	if _, err := e.svc.RestoreTicket(as(person), person, e.guest.ID); err != nil {
		t.Fatalf("the person restores: %v", err)
	}
	rows := audit.Rows()
	if len(rows) != 2 {
		t.Fatalf("%d rows", len(rows))
	}
	for i, want := range []struct {
		basis teamaudit.Basis
		via   string
	}{
		{teamaudit.BasisLeader, artlab.ID},
		{teamaudit.BasisEventEditor, weblab.ID},
	} {
		if r := rows[i]; r.Actor.Basis != want.basis || r.Actor.ViaGroupID != want.via || r.GroupID != weblab.ID || r.Outcome != teamaudit.OutcomeOK {
			t.Errorf("row %d: %+v, want %+v", i, r, want)
		}
	}
}
