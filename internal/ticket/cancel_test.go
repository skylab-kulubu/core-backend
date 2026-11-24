package ticket_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/doorqr"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// cancelEnv is an Event of WEBLAB with a day and a Session, a member's and
// a guest's Ticket, on the memory stores.
type cancelEnv struct {
	store    *ticket.MemoryStore
	events   event.Store
	users    user.Store
	svc      ticket.Service
	ev       event.Event
	sess     event.Session
	holder   authz.Principal
	holderID uuid.UUID
	member   ticket.Ticket
	guest    ticket.Ticket
}

func newCancelEnv(t *testing.T, extras ...any) *cancelEnv {
	t.Helper()
	ctx := context.Background()
	e := &cancelEnv{store: ticket.NewMemoryStore(), events: event.NewMemoryStore(), users: user.NewMemoryStore()}
	az := authz.NewAuthorizer(authz.DefaultPolicy())
	if len(extras) > 0 {
		if v, ok := extras[0].(authz.Authorizer); ok {
			az, extras = v, extras[1:]
		}
	}
	e.svc = ticket.NewService(e.store, e.events, az, append([]any{e.users}, extras...)...)
	e.ev = seedEvent(t, e.events, "WEBLAB")
	day := seedDay(t, e.events, e.ev.ID, "Gün 1")
	e.sess = seedSession(t, e.events, day.ID, "Açılış")
	e.holderID = uuid.New()
	seedUser(t, e.users, e.holderID)
	e.holder = authz.Principal{ID: e.holderID.String()}
	var err error
	if e.member, err = e.svc.Apply(ctx, e.holder, e.ev.ID); err != nil {
		t.Fatal(err)
	}
	applied, err := e.svc.ApplyGuest(ctx, weblabLead, e.ev.ID, ticket.GuestInfo{FirstName: "Grace", LastName: "Hopper", Email: "grace@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	e.guest = applied.Ticket
	return e
}

func (e *cancelEnv) cancel(t *testing.T, id uuid.UUID) ticket.Ticket {
	t.Helper()
	got, err := e.svc.CancelTicket(context.Background(), weblabLead, id, " Etkinliğe gelemeyeceğini yazdı. ")
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// Who may cancel and restore: the owning team's Leader and Privileged
// (YK; ticket:manage and event:manage in the roles mode). DK reads only;
// a member, door staff, the holder and another team's people may not.
func TestCancelAndRestoreRights(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		p    authz.Principal
		want error
	}{
		"Leader":       {weblabLead, nil},
		"YK":           {yk, nil},
		"DK":           {authz.Principal{ID: "dk", Groups: []string{"/UYELER/DK"}}, ticket.ErrForbidden},
		"member":       {authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/ARGE/WEBLAB"}}, ticket.ErrForbidden},
		"another team": {otherMember, ticket.ErrForbidden},
	} {
		e := newCancelEnv(t)
		e.holder.Groups = nil
		_, err := e.svc.CancelTicket(ctx, tc.p, e.guest.ID, "")
		if !errors.Is(err, tc.want) {
			t.Errorf("cancel by %s: %v, want %v", name, err, tc.want)
		}
		if tc.want != nil {
			e.cancel(t, e.guest.ID)
		}
		if _, err := e.svc.RestoreTicket(ctx, tc.p, e.guest.ID); !errors.Is(err, tc.want) {
			t.Errorf("restore by %s: %v, want %v", name, err, tc.want)
		}
		if _, err := e.svc.CancelTicket(ctx, e.holder, e.member.ID, ""); !errors.Is(err, ticket.ErrForbidden) {
			t.Errorf("the holder through the operator's route: %v", err)
		}
	}

	// A member who may edit their team's Events (GECEKODU's members) but
	// is no Leader has no basis in the Team audit record: refused
	// (docs/ticket-cancellation.md).
	g := newCancelEnv(t)
	gecekodu := seedEvent(t, g.events, "GECEKODU")
	applied, err := g.svc.ApplyGuest(ctx, yk, gecekodu.ID, ticket.GuestInfo{FirstName: "Ada", LastName: "L", Email: "ada@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	editor := authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/GECEKODU"}}
	if _, err := g.svc.CancelTicket(ctx, editor, applied.Ticket.ID, ""); !errors.Is(err, ticket.ErrForbidden) {
		t.Errorf("GECEKODU member: %v", err)
	}

	pol := authz.DefaultPolicy()
	pol.RoleMode = authz.RoleModeRoles
	for name, p := range map[string]authz.Principal{
		"ticket:manage": {ID: uuid.NewString(), Roles: []string{authz.RoleTicketManage}},
		"event:manage":  {ID: uuid.NewString(), Roles: []string{authz.RoleEventManage}},
	} {
		e := newCancelEnv(t, authz.NewAuthorizer(pol))
		if _, err := e.svc.CancelTicket(ctx, p, e.guest.ID, ""); err != nil {
			t.Errorf("cancel by %s: %v", name, err)
		}
		if _, err := e.svc.RestoreTicket(ctx, p, e.guest.ID); err != nil {
			t.Errorf("restore by %s: %v", name, err)
		}
	}
}

// A cancellation says when, by whom and why; a second one answers the
// same; a Ticket with a check-in and an Event that is over are refused; a
// reason over 500 characters is invalid.
func TestCancelRefusalsAndIdempotence(t *testing.T) {
	ctx := context.Background()
	e := newCancelEnv(t)
	got := e.cancel(t, e.guest.ID)
	if got.Status != ticket.StatusCancelled || got.CancelledBy != ticket.CancelledByOperator ||
		got.CancelReason != "Etkinliğe gelemeyeceğini yazdı." || got.CancelledAt == nil || got.StatusChangedAt == nil {
		t.Fatalf("cancelled %+v", got)
	}
	again, err := e.svc.CancelTicket(ctx, weblabLead, e.guest.ID, "başka")
	if err != nil || again.Status != ticket.StatusCancelled || !again.CancelledAt.Equal(*got.CancelledAt) || again.CancelReason != got.CancelReason {
		t.Fatalf("again %+v %v", again, err)
	}

	if _, err := e.svc.CheckIn(ctx, weblabLead, e.member.ID, e.sess.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.CancelTicket(ctx, weblabLead, e.member.ID, ""); !errors.Is(err, ticket.ErrTicketCheckedIn) {
		t.Fatalf("checked in: %v", err)
	}
	if _, err := e.svc.CancelTicket(ctx, weblabLead, e.member.ID, strings.Repeat("a", 501)); !errors.Is(err, ticket.ErrInvalid) {
		t.Fatalf("long reason: %v", err)
	}

	past := time.Now().Add(-48 * time.Hour)
	ended, err := e.events.Create(ctx, event.Event{Name: "Dün", Location: "YTÜ", OwnerTeam: "WEBLAB", StartDate: &past, EndDate: &past})
	if err != nil {
		t.Fatal(err)
	}
	old, err := e.store.Create(ctx, ticket.Ticket{EventID: ended.ID, TicketType: ticket.Guest, GuestFirstName: "Ada", GuestLastName: "L", GuestEmail: "ada@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.CancelTicket(ctx, weblabLead, old.ID, ""); !errors.Is(err, ticket.ErrEventEnded) {
		t.Fatalf("ended: %v", err)
	}
	if _, _, err := e.store.Cancel(ctx, old.ID, ticket.CancelledByOperator, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.RestoreTicket(ctx, weblabLead, old.ID); !errors.Is(err, ticket.ErrEventEnded) {
		t.Fatalf("restore ended: %v", err)
	}
}

// A cancelled Ticket leaves the panel's default list, the holder's own list
// and the lookup; the panel finds it under ?status=cancelled and all, the
// holder under include=cancelled.
func TestCancelledTicketLeavesTheLists(t *testing.T) {
	ctx := context.Background()
	e := newCancelEnv(t)
	e.cancel(t, e.member.ID)

	ids := func(tickets []ticket.Ticket) map[uuid.UUID]ticket.Status {
		out := map[uuid.UUID]ticket.Status{}
		for _, t := range tickets {
			out[t.ID] = t.Status
		}
		return out
	}
	active, err := e.svc.ListByEvent(ctx, weblabLead, e.ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(active); len(got) != 1 || got[e.guest.ID] != ticket.StatusActive {
		t.Fatalf("active list %v", got)
	}
	cancelled, err := e.svc.ListByEventStatus(ctx, weblabLead, e.ev.ID, []ticket.Status{ticket.StatusCancelled})
	if err != nil {
		t.Fatal(err)
	}
	if len(cancelled) != 1 || cancelled[0].ID != e.member.ID || cancelled[0].CancelledBy != ticket.CancelledByOperator {
		t.Fatalf("cancelled list %+v", cancelled)
	}
	all, err := e.svc.ListByEventStatus(ctx, weblabLead, e.ev.ID, nil)
	if err != nil || len(all) != 2 {
		t.Fatalf("all %d %v", len(all), err)
	}
	if _, err := e.svc.ListByEventStatus(ctx, otherMember, e.ev.ID, nil); !errors.Is(err, ticket.ErrForbidden) {
		t.Fatalf("another team: %v", err)
	}

	mine, err := e.svc.Mine(ctx, e.holder)
	if err != nil || len(mine) != 0 {
		t.Fatalf("mine %+v %v", mine, err)
	}
	mine, err = e.svc.MineIncluding(ctx, e.holder, []ticket.Status{ticket.StatusCancelled})
	if err != nil || len(mine) != 1 || mine[0].Status != ticket.StatusCancelled || mine[0].QRToken != "" {
		t.Fatalf("mine with cancelled %+v %v", mine, err)
	}
	found, err := e.svc.ListQuery(ctx, yk, "", &e.holderID)
	if err != nil || len(found) != 0 {
		t.Fatalf("lookup %+v %v", found, err)
	}
}

// Every way in at the door refuses a cancelled Ticket with
// ErrTicketCancelled (409 ticket_cancelled); the door's search lists it
// no more.
func TestDoorRefusesACancelledTicket(t *testing.T) {
	ctx := context.Background()
	e := newCancelEnv(t)
	e.cancel(t, e.member.ID)
	e.cancel(t, e.guest.ID)
	memberCode := e.member.Code

	if _, err := e.svc.CheckIn(ctx, weblabLead, e.member.ID, e.sess.ID); !errors.Is(err, ticket.ErrTicketCancelled) {
		t.Errorf("scan: %v", err)
	}
	if _, err := e.svc.CheckInUser(ctx, weblabLead, e.sess.ID, e.holderID); !errors.Is(err, ticket.ErrTicketCancelled) {
		t.Errorf("SkyPass: %v", err)
	}
	if _, err := e.svc.CheckInMe(ctx, e.holder, e.sess.ID); !errors.Is(err, ticket.ErrTicketCancelled) {
		t.Errorf("me: %v", err)
	}
	if _, err := e.svc.ResolveAndCheckIn(ctx, weblabLead, e.sess.ID, ticket.DoorCheckInTarget{PersonID: &e.holderID}); !errors.Is(err, ticket.ErrTicketCancelled) {
		t.Errorf("resolve by person: %v", err)
	}
	if _, err := e.svc.ResolveAndCheckIn(ctx, weblabLead, e.sess.ID, ticket.DoorCheckInTarget{Query: "grace@example.com"}); !errors.Is(err, ticket.ErrTicketCancelled) {
		t.Errorf("resolve by e-mail: %v", err)
	}
	if _, err := e.svc.ResolveAndCheckIn(ctx, weblabLead, e.sess.ID, ticket.DoorCheckInTarget{Query: memberCode}); !errors.Is(err, ticket.ErrTicketCancelled) {
		t.Errorf("resolve by code: %v", err)
	}
	for _, q := range []string{"grace", memberCode} {
		found, err := e.svc.SearchDoorAttendees(ctx, weblabLead, e.ev.ID, q)
		if err != nil || len(found) != 0 {
			t.Errorf("search %q: %+v %v", q, found, err)
		}
	}
	// The store itself takes no check-in of a cancelled Ticket.
	if _, err := e.store.AddCheckIn(ctx, ticket.CheckIn{TicketID: e.member.ID, SessionID: e.sess.ID}); !errors.Is(err, ticket.ErrTicketCancelled) {
		t.Errorf("store: %v", err)
	}
}

// A guest checking themselves in with a cancelled Ticket is refused too.
func TestGuestSelfCheckInRefusesACancelledTicket(t *testing.T) {
	ctx := context.Background()
	f := newDoorFixture(t, doorqr.ModeOpen)
	listed, err := f.svc.ListByEvent(ctx, weblabLead, f.ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	var ada ticket.Ticket
	for _, tk := range listed {
		if tk.GuestEmail == "ada@example.com" {
			ada = tk
		}
	}
	if _, err := f.svc.CancelTicket(ctx, weblabLead, ada.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.CheckInGuest(ctx, f.sess.ID, ticket.GuestCheckIn{Email: "ada@example.com"}); !errors.Is(err, ticket.ErrTicketCancelled) {
		t.Fatalf("guest: %v", err)
	}
}

// Restoring makes the Ticket active with a new link and the next QR
// version: the old key opens nothing. Restoring an active Ticket answers it
// as it is.
func TestRestoreRenewsTheLinkAndQR(t *testing.T) {
	ctx := context.Background()
	e := newCancelEnv(t)
	before, _ := e.store.Get(ctx, e.guest.ID)
	e.cancel(t, e.guest.ID)
	restored, err := e.svc.RestoreTicket(ctx, weblabLead, e.guest.ID)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := e.store.Get(ctx, e.guest.ID)
	if restored.Status != ticket.StatusActive || restored.CancelledAt != nil || restored.CancelReason != "" ||
		after.AccessKey == before.AccessKey || after.QRVersion != before.QRVersion+1 || after.Code != before.Code {
		t.Fatalf("restored %+v (before key v%d, after v%d)", restored, before.QRVersion, after.QRVersion)
	}
	if _, err := e.store.GetByAccessKey(ctx, before.AccessKey); !errors.Is(err, ticket.ErrNotFound) {
		t.Fatalf("old key: %v", err)
	}
	again, err := e.svc.RestoreTicket(ctx, weblabLead, e.guest.ID)
	if err != nil || again.QRVersion != after.QRVersion {
		t.Fatalf("restore an active Ticket: %+v %v", again, err)
	}
	if _, err := e.svc.CheckIn(ctx, weblabLead, e.guest.ID, e.sess.ID); err != nil {
		t.Fatalf("restored at the door: %v", err)
	}
}

// Registering again reopens a cancelled Ticket on all four ways in (Member
// apply, Apply-for-other, Guest apply, the forms report): active, a new
// key, the next QR version. Only an active Ticket is "already there".
func TestRegisteringAgainReopensACancelledTicket(t *testing.T) {
	ctx := context.Background()
	check := func(t *testing.T, e *cancelEnv, id uuid.UUID, before ticket.Ticket) {
		t.Helper()
		got, err := e.store.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != ticket.StatusActive || got.AccessKey == before.AccessKey || got.QRVersion != before.QRVersion+1 {
			t.Fatalf("reopened %+v", got)
		}
		if _, err := e.store.GetByAccessKey(ctx, before.AccessKey); !errors.Is(err, ticket.ErrNotFound) {
			t.Fatalf("old key: %v", err)
		}
	}
	t.Run("member apply", func(t *testing.T) {
		e := newCancelEnv(t)
		before, _ := e.store.Get(ctx, e.member.ID)
		e.cancel(t, e.member.ID)
		got, err := e.svc.Apply(ctx, e.holder, e.ev.ID)
		if err != nil || got.ID != e.member.ID {
			t.Fatalf("apply %+v %v", got, err)
		}
		check(t, e, e.member.ID, before)
		if _, err := e.svc.Apply(ctx, e.holder, e.ev.ID); !errors.Is(err, ticket.ErrConflict) {
			t.Fatalf("an active Ticket: %v", err)
		}
	})
	t.Run("apply for other", func(t *testing.T) {
		e := newCancelEnv(t)
		before, _ := e.store.Get(ctx, e.member.ID)
		e.cancel(t, e.member.ID)
		if _, err := e.svc.ApplyForOther(ctx, weblabLead, e.ev.ID, e.holderID); err != nil {
			t.Fatal(err)
		}
		check(t, e, e.member.ID, before)
	})
	t.Run("guest apply", func(t *testing.T) {
		e := newCancelEnv(t)
		before, _ := e.store.Get(ctx, e.guest.ID)
		e.cancel(t, e.guest.ID)
		applied, err := e.svc.ApplyGuest(ctx, anonymous, e.ev.ID, ticket.GuestInfo{FirstName: "Grace", LastName: "Hopper", Email: "grace@example.com"})
		if err != nil || applied.Result != ticket.GuestCreated {
			t.Fatalf("guest apply %+v %v", applied, err)
		}
		check(t, e, e.guest.ID, before)
	})
	t.Run("forms report", func(t *testing.T) {
		e := newCancelEnv(t)
		formID := uuid.New()
		ev := seedFormEvent(t, e.events, formAddress(formID))
		if _, err := e.svc.RecordFormResponse(ctx, formsService, acceptedGuest(formID, "Ada", "Lovelace", "ada@example.com")); err != nil {
			t.Fatal(err)
		}
		written := ticketsOf(t, e.svc, ev)
		if len(written) != 1 {
			t.Fatalf("%d tickets", len(written))
		}
		before, _ := e.store.Get(ctx, written[0].ID)
		e.cancel(t, before.ID)
		result, err := e.svc.RecordFormResponse(ctx, formsService, acceptedGuest(formID, "Ada", "Lovelace", "ada@example.com"))
		if err != nil || result.TicketsWritten != 1 {
			t.Fatalf("report again %+v %v", result, err)
		}
		check(t, e, before.ID, before)
	})
}
