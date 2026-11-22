package ticket_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/teamaudit"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
)

// Six times a day besides three an hour: the seventh in a day is refused.
func TestResendDailyLimit(t *testing.T) {
	e := newResendEnv(t)
	ctx := context.Background()
	for i := range 6 {
		if i > 0 && i%3 == 0 {
			// The hour's three are an hour old.
			if _, err := e.pool.Exec(ctx, `UPDATE ticket_resends SET created_at = created_at - interval '61 minutes'`); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := e.svc.ResendTicket(ctx, weblabLead, e.guest.ID, ""); err != nil {
			t.Fatalf("resend %d: %v", i+1, err)
		}
	}
	if _, err := e.pool.Exec(ctx, `UPDATE ticket_resends SET created_at = created_at - interval '61 minutes'`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.ResendTicket(ctx, weblabLead, e.guest.ID, ""); !errors.Is(err, ticket.ErrResendDailyLimited) {
		t.Fatalf("seventh in a day: %v", err)
	}
	if _, err := e.pool.Exec(ctx, `UPDATE ticket_resends SET created_at = created_at - interval '24 hours'`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.ResendTicket(ctx, weblabLead, e.guest.ID, ""); err != nil {
		t.Fatalf("a day on: %v", err)
	}
}

// Each resend and renewal is a Team audit row against the Event's team:
// who (a Leader, through their Team; or Privileged), the Ticket and the
// Event, the resend's number, how it ended. Without an actor the action is
// refused and nothing changes.
func TestResendAndRenewalAreAudited(t *testing.T) {
	e := newResendEnv(t)
	ctx := context.Background()
	dir := identity.NewMemory()
	arge := identity.Group{ID: uuid.NewString(), Name: "ARGE", Path: "/UYELER/ARGE"}
	weblab := identity.Group{ID: uuid.NewString(), Name: "WEBLAB", Path: "/UYELER/ARGE/WEBLAB"}
	dir.PutGroup(arge)
	dir.PutGroup(weblab)
	audit := teamaudit.NewMemory()
	svc := ticket.NewService(e.store, e.events, authz.NewAuthorizer(authz.DefaultPolicy()), e.users, e.signer, dir, audit)

	leadID, ykID := uuid.New(), uuid.New()
	lead := authz.Principal{ID: leadID.String(), Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}
	// A Leader of the team and of a Team above it: the nearest Team names
	// the basis.
	both := authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/ARGE/LIDERLER", "/UYELER/ARGE/WEBLAB/LIDERLER"}}
	ykP := authz.Principal{ID: ykID.String(), Groups: []string{"/UYELER/YK"}}
	as := func(p authz.Principal) context.Context {
		return teamaudit.WithActor(ctx, teamaudit.Actor{ID: uuid.MustParse(p.ID), Client: "core-frontend", CorrelationID: "req-1"})
	}

	sent, err := svc.ResendTicket(as(lead), lead, e.guest.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.RenewTicketLink(as(ykP), ykP, e.guest.ID, false, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ResendTicket(as(both), both, e.guest.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ResendTicket(as(lead), lead, e.guest.ID, ""); err != nil {
		t.Fatal(err)
	}
	// The fourth in the hour: refused, and recorded as such.
	if _, err := svc.ResendTicket(as(lead), lead, e.guest.ID, ""); !errors.Is(err, ticket.ErrResendLimited) {
		t.Fatalf("fourth: %v", err)
	}
	rows := audit.Rows()
	if len(rows) != 5 {
		t.Fatalf("%d rows", len(rows))
	}
	first := rows[0]
	if first.Action != teamaudit.ActionTicketResend || first.Outcome != teamaudit.OutcomeOK || first.GroupID != weblab.ID || first.GroupPath != weblab.Path ||
		first.Actor.ID != leadID || first.Actor.Basis != teamaudit.BasisLeader || first.Actor.ViaGroupID != weblab.ID || first.TargetUserID != uuid.Nil ||
		first.After == nil || first.After.TicketID != e.guest.ID.String() || first.After.EventID != e.ev.ID.String() || first.After.Resend != sent.N {
		t.Fatalf("resend row %+v after %+v", first, first.After)
	}
	if renew := rows[1]; renew.Action != teamaudit.ActionTicketLinkRenew || renew.Actor.Basis != teamaudit.BasisPrivileged || renew.Actor.ViaGroupID != "" ||
		renew.Outcome != teamaudit.OutcomeOK || renew.After.Resend != 0 {
		t.Fatalf("renewal row %+v after %+v", renew, renew.After)
	}
	if nearest := rows[2]; nearest.Actor.Basis != teamaudit.BasisLeader || nearest.Actor.ViaGroupID != weblab.ID {
		t.Fatalf("the nearest Team: %+v", nearest.Actor)
	}
	if refused := rows[4]; refused.Outcome != teamaudit.OutcomeFailed || refused.ErrorCode != "rate_limited" {
		t.Fatalf("refused row %+v", refused)
	}

	// No actor in the request: refused, nothing written or renewed.
	before, _ := e.store.Get(ctx, e.guest.ID)
	if _, _, err := svc.RenewTicketLink(ctx, ykP, e.guest.ID, false, ""); !errors.Is(err, teamaudit.ErrUnavailable) {
		t.Fatalf("no actor: %v", err)
	}
	if after, _ := e.store.Get(ctx, e.guest.ID); after.QRVersion != before.QRVersion || len(audit.Rows()) != 5 {
		t.Fatal("an unaudited renewal renewed")
	}
}
