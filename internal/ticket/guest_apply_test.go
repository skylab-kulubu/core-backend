package ticket_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
)

// The callers of Guest apply as the service tells them apart.
var (
	anonymous   = authz.Principal{}
	weblabLead  = authz.Principal{ID: "lead", Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}
	yk          = authz.Principal{ID: "yk", Groups: []string{"/UYELER/YK"}}
	otherMember = authz.Principal{ID: "member", Groups: []string{"/UYELER/ARGE/GAMELAB"}}
	formsHop    = authz.Principal{ID: "forms-sa", Product: authz.ProductForms}
)

func storedGuest(t *testing.T, svc ticket.Service, ev event.Event, email string) ticket.Ticket {
	t.Helper()
	listed, err := svc.ListByEvent(context.Background(), yk, ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, tk := range listed {
		if tk.GuestEmail == email {
			return tk
		}
	}
	t.Fatalf("no guest ticket for %s in %+v", email, listed)
	return ticket.Ticket{}
}

func TestApplyGuestAnonymousCreatesAndSeesNothing(t *testing.T) {
	t.Parallel()
	events, svc := setup(t)
	ev := seedEvent(t, events, "WEBLAB")

	applied, err := svc.ApplyGuest(context.Background(), anonymous, ev.ID, ticket.GuestInfo{
		FirstName: "Ada", LastName: "Lovelace", Email: "Ada@Example.com", PhoneNumber: "555",
	})
	if err != nil {
		t.Fatal(err)
	}
	if applied.Result != ticket.GuestCreated || applied.Trusted {
		t.Fatalf("applied %+v", applied)
	}
	if applied.Ticket.ID != uuid.Nil || applied.Ticket.GuestEmail != "" || applied.Ticket.GuestPhoneNumber != "" {
		t.Fatalf("an anonymous caller got the Ticket back: %+v", applied.Ticket)
	}
	got := storedGuest(t, svc, ev, "ada@example.com")
	if got.GuestFirstName != "Ada" || got.GuestLastName != "Lovelace" || got.GuestPhoneNumber != "555" {
		t.Fatalf("stored %+v", got)
	}
}

func TestApplyGuestAnonymousKeepsAnExistingGuestsDetails(t *testing.T) {
	t.Parallel()
	events, svc := setup(t)
	ctx := context.Background()
	ev := seedEvent(t, events, "WEBLAB")
	if _, err := svc.ApplyGuest(ctx, anonymous, ev.ID, ticket.GuestInfo{
		FirstName: "Ada", LastName: "Lovelace", Email: "ada@example.com", PhoneNumber: "555",
	}); err != nil {
		t.Fatal(err)
	}
	before := storedGuest(t, svc, ev, "ada@example.com")

	applied, err := svc.ApplyGuest(ctx, anonymous, ev.ID, ticket.GuestInfo{
		FirstName: "Mallory", LastName: "Renamed", Email: "ADA@example.com", PhoneNumber: "999",
	})
	if err != nil {
		t.Fatal(err)
	}
	if applied.Result != ticket.GuestKept || applied.Trusted || applied.Ticket.ID != uuid.Nil {
		t.Fatalf("applied %+v", applied)
	}
	after := storedGuest(t, svc, ev, "ada@example.com")
	if after.GuestFirstName != "Ada" || after.GuestLastName != "Lovelace" || after.GuestPhoneNumber != "555" {
		t.Fatalf("an anonymous caller changed the guest: %+v", after)
	}
	if !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatalf("the Ticket was written although nothing changed: %v -> %v", before.UpdatedAt, after.UpdatedAt)
	}
}

func TestApplyGuestAnonymousSameDetailsIsExisting(t *testing.T) {
	t.Parallel()
	events, svc := setup(t)
	ctx := context.Background()
	ev := seedEvent(t, events, "WEBLAB")
	guest := ticket.GuestInfo{FirstName: "Ada", LastName: "Lovelace", Email: "ada@example.com"}
	if _, err := svc.ApplyGuest(ctx, anonymous, ev.ID, guest); err != nil {
		t.Fatal(err)
	}
	// The forms hop sends no phone number: an unchanged resubmission.
	applied, err := svc.ApplyGuest(ctx, anonymous, ev.ID, guest)
	if err != nil {
		t.Fatal(err)
	}
	if applied.Result != ticket.GuestExisting || applied.Trusted {
		t.Fatalf("applied %+v", applied)
	}
}

func TestApplyGuestAnonymousFillsOnlyEmptyFields(t *testing.T) {
	t.Parallel()
	events, svc := setup(t)
	ctx := context.Background()
	ev := seedEvent(t, events, "WEBLAB")
	if _, err := svc.ApplyGuest(ctx, anonymous, ev.ID, ticket.GuestInfo{
		FirstName: "Ada", LastName: "Lovelace", Email: "ada@example.com",
	}); err != nil {
		t.Fatal(err)
	}

	applied, err := svc.ApplyGuest(ctx, anonymous, ev.ID, ticket.GuestInfo{
		FirstName: "Ada", LastName: "Lovelace", Email: "ada@example.com", PhoneNumber: "555",
	})
	if err != nil {
		t.Fatal(err)
	}
	if applied.Result != ticket.GuestExisting {
		t.Fatalf("applied %+v", applied)
	}
	if got := storedGuest(t, svc, ev, "ada@example.com"); got.GuestPhoneNumber != "555" {
		t.Fatalf("the empty phone number was not filled: %+v", got)
	}

	// A filled gap does not make a refused change go through.
	applied, err = svc.ApplyGuest(ctx, anonymous, ev.ID, ticket.GuestInfo{
		FirstName: "Mallory", LastName: "Lovelace", Email: "ada@example.com", PhoneNumber: "555",
	})
	if err != nil {
		t.Fatal(err)
	}
	if applied.Result != ticket.GuestKept {
		t.Fatalf("applied %+v", applied)
	}
	if got := storedGuest(t, svc, ev, "ada@example.com"); got.GuestFirstName != "Ada" {
		t.Fatalf("renamed: %+v", got)
	}
}

func TestApplyGuestOperatorSeesAndUpdatesTheTicket(t *testing.T) {
	t.Parallel()
	for name, operator := range map[string]authz.Principal{"owner team leader": weblabLead, "privileged": yk} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			events, svc := setup(t)
			ctx := context.Background()
			ev := seedEvent(t, events, "WEBLAB")
			created, err := svc.ApplyGuest(ctx, anonymous, ev.ID, ticket.GuestInfo{
				FirstName: "Ada", LastName: "Lovelace", Email: "ada@example.com", PhoneNumber: "555",
			})
			if err != nil {
				t.Fatal(err)
			}
			if created.Result != ticket.GuestCreated {
				t.Fatalf("created %+v", created)
			}

			applied, err := svc.ApplyGuest(ctx, operator, ev.ID, ticket.GuestInfo{
				FirstName: "Augusta", LastName: "Byron", Email: "ADA@example.com", PhoneNumber: "777",
			})
			if err != nil {
				t.Fatal(err)
			}
			if applied.Result != ticket.GuestExisting || !applied.Trusted {
				t.Fatalf("applied %+v", applied)
			}
			tk := applied.Ticket
			if tk.TicketType != ticket.Guest || tk.GuestFirstName != "Augusta" || tk.GuestLastName != "Byron" ||
				tk.GuestEmail != "ada@example.com" || tk.GuestPhoneNumber != "777" || tk.Event == nil {
				t.Fatalf("operator's Ticket %+v", tk)
			}
		})
	}
}

func TestApplyGuestPersonWithoutEventRightsIsTreatedLikeAnonymous(t *testing.T) {
	t.Parallel()
	events, svc := setup(t)
	ctx := context.Background()
	ev := seedEvent(t, events, "WEBLAB")
	if _, err := svc.ApplyGuest(ctx, weblabLead, ev.ID, ticket.GuestInfo{
		FirstName: "Ada", LastName: "Lovelace", Email: "ada@example.com", PhoneNumber: "555",
	}); err != nil {
		t.Fatal(err)
	}

	applied, err := svc.ApplyGuest(ctx, otherMember, ev.ID, ticket.GuestInfo{
		FirstName: "Mallory", LastName: "Renamed", Email: "ada@example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	if applied.Result != ticket.GuestKept || applied.Trusted || applied.Ticket.GuestPhoneNumber != "" {
		t.Fatalf("applied %+v", applied)
	}
	if got := storedGuest(t, svc, ev, "ada@example.com"); got.GuestFirstName != "Ada" {
		t.Fatalf("renamed: %+v", got)
	}
}

func TestApplyGuestProductServiceIdentityIsTrusted(t *testing.T) {
	t.Parallel()
	events, svc := setup(t)
	ctx := context.Background()
	ev := seedEvent(t, events, "WEBLAB")
	if _, err := svc.ApplyGuest(ctx, anonymous, ev.ID, ticket.GuestInfo{
		FirstName: "Ada", LastName: "Lovelace", Email: "ada@example.com",
	}); err != nil {
		t.Fatal(err)
	}

	applied, err := svc.ApplyGuest(ctx, formsHop, ev.ID, ticket.GuestInfo{
		FirstName: "Augusta", LastName: "Byron", Email: "ada@example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	if applied.Result != ticket.GuestExisting || !applied.Trusted || applied.Ticket.GuestFirstName != "Augusta" {
		t.Fatalf("applied %+v", applied)
	}
}
