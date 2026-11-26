package guestlink

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/commpref"
)

func one(id uuid.UUID, kind commpref.AddressKind) commpref.VerifiedAddress {
	return commpref.VerifiedAddress{Holders: []uuid.UUID{id}, Kind: kind, Enabled: true}
}

// The plan's every outcome: linked by kind, ambiguous, disabled, inactive,
// unmatched, and a duplicate both against a Ticket the account holds and
// between two guest Tickets of one person on one Event (the active one is
// linked, then the older).
func TestPlanTickets(t *testing.T) {
	t.Parallel()
	ada, bob, cem, erased, off := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	e1, e2 := uuid.New(), uuid.New()
	addrs := map[string]commpref.VerifiedAddress{
		"ada@example.test":      one(ada, commpref.AddressPrimary),
		"ada@std.yildiz.edu.tr": one(ada, commpref.AddressSchool),
		"bob@kisisel.org":       one(bob, commpref.AddressPersonal),
		"cem@example.test":      one(cem, commpref.AddressPrimary),
		"cem@kisisel.org":       one(cem, commpref.AddressPersonal),
		"ortak@example.test":    {Holders: []uuid.UUID{ada, bob}, Kind: commpref.AddressPrimary, Enabled: true},
		"kapali@example.test":   {Holders: []uuid.UUID{off}, Kind: commpref.AddressPrimary},
		"silinen@example.test":  one(erased, commpref.AddressPrimary),
	}
	accounts := Accounts{
		Active: map[uuid.UUID]bool{ada: true, bob: true, cem: true, off: true},
		Holds:  map[[2]uuid.UUID]bool{{bob, e1}: true},
	}
	at := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	row := func(event uuid.UUID, address, status string, minutes int) GuestTicket {
		return GuestTicket{ID: uuid.New(), EventID: event, Address: address, Status: status, CreatedAt: at.Add(time.Duration(minutes) * time.Minute)}
	}
	adaPrimary := row(e1, "ada@example.test", "active", 0)
	adaSchool := row(e2, "ada@std.yildiz.edu.tr", "waitlisted", 0)
	bobDup := row(e1, "bob@kisisel.org", "active", 0)
	cemCancelledOlder := row(e1, "cem@example.test", "cancelled", 0)
	cemActiveNewer := row(e1, "cem@kisisel.org", "active", 5)
	rows := []GuestTicket{
		adaPrimary, adaSchool, bobDup, cemCancelledOlder, cemActiveNewer,
		row(e1, "ortak@example.test", "active", 0),
		row(e1, "kapali@example.test", "active", 0),
		row(e1, "silinen@example.test", "active", 0),
		row(e1, "kimse@example.test", "active", 0),
	}
	links, c := PlanTickets(rows, addrs, accounts)
	want := map[uuid.UUID]uuid.UUID{adaPrimary.ID: ada, adaSchool.ID: ada, cemActiveNewer.ID: cem}
	if len(links) != len(want) {
		t.Fatalf("links %+v", links)
	}
	for _, l := range links {
		if want[l.ID] != l.Owner {
			t.Fatalf("link %+v", l)
		}
	}
	if c.Linked[KindPrimary] != 1 || c.Linked[KindSchool] != 1 || c.Linked[KindPersonal] != 1 ||
		c.Ambiguous != 1 || c.Disabled != 1 || c.Inactive != 1 || c.Unmatched != 1 || c.Duplicate != 2 {
		t.Fatalf("counts %+v", c)
	}
	if accounts.Holds[[2]uuid.UUID{ada, e1}] {
		t.Fatal("the plan changed the accounts it was given")
	}
}

// A certificate goes to its Ticket's owner, one linked in the same run
// included, else to its address's account.
func TestPlanCertificates(t *testing.T) {
	t.Parallel()
	ada, bob, member := uuid.New(), uuid.New(), uuid.New()
	linkedNow := uuid.New()
	addrs := map[string]commpref.VerifiedAddress{
		"bob@kisisel.org":    one(bob, commpref.AddressPersonal),
		"ortak@example.test": {Holders: []uuid.UUID{ada, bob}, Enabled: true},
	}
	accounts := Accounts{Active: map[uuid.UUID]bool{ada: true, bob: true, member: true}}
	rows := []OwnerlessCertificate{
		{ID: uuid.New(), TicketID: uuid.New(), Address: "member@example.test", TicketOwner: &member},
		{ID: uuid.New(), TicketID: linkedNow, Address: "ada@example.test"},
		{ID: uuid.New(), TicketID: uuid.New(), Address: "bob@kisisel.org"},
		{ID: uuid.New(), TicketID: uuid.New(), Address: "ortak@example.test"},
		{ID: uuid.New(), TicketID: uuid.New(), Address: "kimse@example.test"},
	}
	links, c := PlanCertificates(rows, addrs, accounts, map[uuid.UUID]uuid.UUID{linkedNow: ada})
	if len(links) != 3 || links[0].Owner != member || links[1].Owner != ada || links[2].Owner != bob {
		t.Fatalf("links %+v", links)
	}
	if c.Linked[KindTicket] != 2 || c.Linked[KindPersonal] != 1 || c.Ambiguous != 1 || c.Unmatched != 1 {
		t.Fatalf("counts %+v", c)
	}
}
