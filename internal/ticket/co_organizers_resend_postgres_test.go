package ticket_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/teamaudit"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
)

// A Co-organizer person's resend and link renewal are written to the
// database's Team audit record as an event editor's through the Event's
// team, which its basis check takes (20261126190000, docs/co-organizers.md).
func TestCoOrganizerPersonResendIsAudited(t *testing.T) {
	e := newResendEnv(t)
	ctx := context.Background()
	dir := identity.NewMemory()
	weblab := identity.Group{ID: uuid.NewString(), Name: "WEBLAB", Path: "/UYELER/ARGE/WEBLAB"}
	dir.PutGroup(weblab)
	svc := ticket.NewService(e.store, e.events, authz.NewAuthorizer(authz.DefaultPolicy()), e.users, e.signer, dir, teamaudit.NewPostgres(e.pool))
	personID := uuid.New()
	if _, err := e.events.SetCoOrganizers(ctx, event.CoOrganizersWrite{EventID: e.ev.ID, OwnerTeam: "WEBLAB", People: []uuid.UUID{personID}}); err != nil {
		t.Fatal(err)
	}
	person := authz.Principal{ID: personID.String()}
	as := teamaudit.WithActor(ctx, teamaudit.Actor{ID: personID, Client: "core-frontend"})
	if _, err := svc.ResendTicket(as, person, e.guest.ID, ""); err != nil {
		t.Fatalf("the person resends: %v", err)
	}
	if _, _, err := svc.RenewTicketLink(as, person, e.guest.ID, false, ""); err != nil {
		t.Fatalf("the person renews the link: %v", err)
	}
	rows, err := e.pool.Query(ctx, `SELECT action, actor_id, actor_basis, actor_via_group_id, group_id, outcome FROM team_audit_records ORDER BY occurred_at`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var actions []string
	for rows.Next() {
		var action, basis, outcome string
		var actor, via, group uuid.UUID
		if err := rows.Scan(&action, &actor, &basis, &via, &group, &outcome); err != nil {
			t.Fatal(err)
		}
		if actor != personID || basis != string(teamaudit.BasisEventEditor) || via.String() != weblab.ID || group.String() != weblab.ID || outcome != string(teamaudit.OutcomeOK) {
			t.Errorf("%s row: actor %s basis %s via %s group %s outcome %s", action, actor, basis, via, group, outcome)
		}
		actions = append(actions, action)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(actions) != 2 || actions[0] != string(teamaudit.ActionTicketResend) || actions[1] != string(teamaudit.ActionTicketLinkRenew) {
		t.Fatalf("rows: %v", actions)
	}
}
