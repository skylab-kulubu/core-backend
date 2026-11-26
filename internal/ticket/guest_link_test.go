package ticket_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// fakeGuestOwners is the directory of the guest-link tests: each address
// (trimmed, lower case) to its account. It answers whatever it holds; the
// rule of who may own a Ticket on the Event is guestlink's, and the
// database's when it is wrong.
type fakeGuestOwners struct {
	mu     sync.Mutex
	owners map[string]uuid.UUID
	asked  int
}

func (f *fakeGuestOwners) GuestOwner(_ context.Context, _ uuid.UUID, email string) (uuid.UUID, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked++
	id, ok := f.owners[strings.ToLower(strings.TrimSpace(email))]
	return id, ok
}

type guestLinkEnv struct {
	svc    ticket.Service
	events event.Store
	ev     event.Event
	sess   event.Session
	owners *fakeGuestOwners
	adaID  uuid.UUID
	ada    authz.Principal
}

// newGuestLinkEnv is a WEBLAB Event with a Session, the account Ada
// (ada@example.com in core) whose Personal e-mail ada@kisisel.org the
// directory names hers.
func newGuestLinkEnv(t *testing.T) guestLinkEnv {
	t.Helper()
	users := user.NewMemoryStore()
	e := guestLinkEnv{events: event.NewMemoryStore(), owners: &fakeGuestOwners{owners: map[string]uuid.UUID{}}, adaID: uuid.New()}
	e.svc = ticket.NewService(ticket.NewMemoryStore(), e.events, authz.NewAuthorizer(authz.DefaultPolicy()), users, ticket.GuestOwners(e.owners))
	e.ev = seedEvent(t, e.events, "WEBLAB")
	day := seedDay(t, e.events, e.ev.ID, "Gün 1")
	e.sess = seedSession(t, e.events, day.ID, "Açılış")
	seedUser(t, users, e.adaID)
	e.ada = authz.Principal{ID: e.adaID.String()}
	e.owners.owners["ada@kisisel.org"] = e.adaID
	return e
}

// A guest Ticket written for an address an account holds verified is
// written linked: still GUEST with the name and e-mail given, owned by the
// account, in the person's own list, and found at the door by the guest's
// e-mail and name as before, and by the person too.
func TestGuestTicketWrittenLinked(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newGuestLinkEnv(t)
	if _, err := e.svc.ApplyGuest(ctx, anonymous, e.ev.ID, ticket.GuestInfo{
		FirstName: "Ada", LastName: "Byron", Email: " Ada@Kisisel.ORG ", PhoneNumber: "555",
	}); err != nil {
		t.Fatal(err)
	}
	got := storedGuest(t, e.svc, e.ev, "ada@kisisel.org")
	if got.TicketType != ticket.Guest || got.OwnerID == nil || *got.OwnerID != e.adaID ||
		got.GuestFirstName != "Ada" || got.GuestLastName != "Byron" || got.GuestPhoneNumber != "555" {
		t.Fatalf("stored %+v", got)
	}
	mine, err := e.svc.Mine(ctx, e.ada)
	if err != nil || len(mine) != 1 || mine[0].ID != got.ID {
		t.Fatalf("Biletlerim: %+v %v", mine, err)
	}
	for _, q := range []string{"ada@kisisel.org", "byron", got.Code} {
		found, err := e.svc.SearchDoorAttendees(ctx, weblabLead, e.ev.ID, q)
		if err != nil || len(found) != 1 || found[0].Email != "ada@kisisel.org" || found[0].Name != "Ada Byron" ||
			found[0].PersonID == nil || *found[0].PersonID != e.adaID {
			t.Fatalf("door search %q: %+v %v", q, found, err)
		}
	}
	ci, err := e.svc.ResolveAndCheckIn(ctx, weblabLead, e.sess.ID, ticket.DoorCheckInTarget{Query: "ada@kisisel.org"})
	if err != nil || ci.TicketID != got.ID || ci.PersonName != "Ada Byron" {
		t.Fatalf("desk check-in by the guest's e-mail: %+v %v", ci, err)
	}
	if err := e.svc.UndoCheckIn(ctx, weblabLead, got.ID, e.sess.ID); err != nil {
		t.Fatal(err)
	}
	if ci, err := e.svc.ResolveAndCheckIn(ctx, weblabLead, e.sess.ID, ticket.DoorCheckInTarget{PersonID: &e.adaID}); err != nil || ci.TicketID != got.ID {
		t.Fatalf("desk check-in by the person: %+v %v", ci, err)
	}
	// The member registering on the Event is told they are: no second
	// Ticket.
	if _, err := e.svc.Apply(ctx, e.ada, e.ev.ID); !errors.Is(err, ticket.ErrConflict) {
		t.Fatalf("member apply after the linked Ticket: %v", err)
	}
	if listed, _ := e.svc.ListByEvent(ctx, yk, e.ev.ID); len(listed) != 1 {
		t.Fatalf("%d Tickets", len(listed))
	}
}

// An address the directory names nobody's is written as before: unlinked.
func TestGuestTicketOfAnUnknownAddressStaysUnlinked(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newGuestLinkEnv(t)
	if _, err := e.svc.ApplyGuest(ctx, weblabLead, e.ev.ID, ticket.GuestInfo{FirstName: "Grace", LastName: "Hopper", Email: "grace@example.com"}); err != nil {
		t.Fatal(err)
	}
	if got := storedGuest(t, e.svc, e.ev, "grace@example.com"); got.OwnerID != nil {
		t.Fatalf("linked: %+v", got)
	}
}

// The account already holds a Ticket on the Event (the directory's answer
// was stale): the guest Ticket is written unlinked, as before linking, and
// the write does not fail.
func TestGuestTicketOfAnAccountWithATicketIsWrittenUnlinked(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newGuestLinkEnv(t)
	member, err := e.svc.Apply(ctx, e.ada, e.ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	applied, err := e.svc.ApplyGuest(ctx, weblabLead, e.ev.ID, ticket.GuestInfo{FirstName: "Ada", LastName: "Byron", Email: "ada@kisisel.org"})
	if err != nil || applied.Result != ticket.GuestCreated {
		t.Fatalf("%+v %v", applied, err)
	}
	got := storedGuest(t, e.svc, e.ev, "ada@kisisel.org")
	if got.OwnerID != nil || got.ID == member.ID {
		t.Fatalf("stored %+v", got)
	}
	mine, _ := e.svc.Mine(ctx, e.ada)
	if len(mine) != 1 || mine[0].ID != member.ID {
		t.Fatalf("Biletlerim: %+v", mine)
	}
}

// A form answer's guest (the untrusted hop) and an import's guest row are
// linked the same way.
func TestFormAnswerAndImportGuestsAreLinked(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newGuestLinkEnv(t)
	grace := uuid.New()
	e.owners.owners["grace@okul.edu.tr"] = grace
	if _, err := e.svc.AdmitFormRespondent(ctx, e.ev.ID, ticket.FormRespondent{
		FormID: uuid.New(), ResponseID: uuid.New(),
		Guest: &ticket.GuestInfo{FirstName: "Ada", LastName: "Byron", Email: "ada@kisisel.org"},
	}); err != nil {
		t.Fatal(err)
	}
	if got := storedGuest(t, e.svc, e.ev, "ada@kisisel.org"); got.OwnerID == nil || *got.OwnerID != e.adaID {
		t.Fatalf("form answer's guest: %+v", got)
	}
	in := ticket.ImportInput{Text: "Ad,Soyad,E-posta\nGrace,Hopper,grace@okul.edu.tr\nAlan,Turing,alan@example.com\n", PastCapacity: true}
	if _, err := e.svc.ImportAttendees(importAs(importLead), importLead, e.ev.ID, in); err != nil {
		t.Fatal(err)
	}
	if got := storedGuest(t, e.svc, e.ev, "grace@okul.edu.tr"); got.OwnerID == nil || *got.OwnerID != grace {
		t.Fatalf("import's guest row: %+v", got)
	}
	if got := storedGuest(t, e.svc, e.ev, "alan@example.com"); got.OwnerID != nil {
		t.Fatalf("an unknown address linked: %+v", got)
	}
}
