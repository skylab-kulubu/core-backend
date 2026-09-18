package eventmail

import (
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

func TestRecipientsFromTicketsIncludesGuestAndMember(t *testing.T) {
	t.Parallel()
	owner := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	got := RecipientsFromTickets(
		[]ticket.Ticket{
			{
				TicketType:     ticket.Guest,
				GuestFirstName: "Ada",
				GuestLastName:  "Lovelace",
				GuestEmail:     "ada@example.com",
			},
			{
				TicketType: ticket.Registered,
				OwnerID:    &owner,
			},
		},
		map[uuid.UUID]user.User{
			owner: {ID: owner, Email: "grace@example.com", FirstName: "Grace", LastName: "Hopper"},
		},
	)
	if len(got) != 2 {
		t.Fatalf("len %d", len(got))
	}
	if got[0].Email != "ada@example.com" || got[0].FullName != "Ada Lovelace" {
		t.Fatalf("guest %+v", got[0])
	}
	if got[1].Email != "grace@example.com" || got[1].FullName != "Grace Hopper" {
		t.Fatalf("member %+v", got[1])
	}
}

func TestRecipientsFromTicketsSkipsBlankEmailAndDedupes(t *testing.T) {
	t.Parallel()
	owner := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	got := RecipientsFromTickets(
		[]ticket.Ticket{
			{TicketType: ticket.Guest, GuestEmail: "  ", GuestFirstName: "No"},
			{TicketType: ticket.Guest, GuestEmail: "Ada@example.com", GuestFirstName: "Ada"},
			{TicketType: ticket.Registered, OwnerID: &owner},
			{TicketType: ticket.Guest, GuestEmail: "ada@example.com", GuestFirstName: "Dup"},
		},
		map[uuid.UUID]user.User{
			owner: {Email: "ADA@example.com", FirstName: "Member"},
		},
	)
	if len(got) != 1 {
		t.Fatalf("len %d got %+v", len(got), got)
	}
	if got[0].Email != "ada@example.com" {
		t.Fatalf("email %q", got[0].Email)
	}
}

func TestListNameJoinsOwnerTeamAndEvent(t *testing.T) {
	t.Parallel()
	if got := ListName("GECEKODU", "SkyDays"); got != "GECEKODU SkyDays" {
		t.Fatalf("got %q", got)
	}
	if got := ListName("", "Hack"); got != "Hack" {
		t.Fatalf("got %q", got)
	}
}
