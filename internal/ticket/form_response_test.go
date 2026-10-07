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

// formsService is the forms service's account as core reads its token.
var formsService = authz.Principal{ID: "forms-sa", ServiceAccount: true, Product: authz.ProductForms, Roles: []string{"ticket:forms"}}

func formAddress(formID uuid.UUID) string {
	return "https://forms.yildizskylab.com/" + formID.String()
}

func seedFormEvent(t *testing.T, events event.Store, formURL string, extra ...event.EventFormLink) event.Event {
	t.Helper()
	ev, err := events.Create(context.Background(), event.Event{
		Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB", FormURL: formURL, ExtraFormURLs: extra,
	})
	if err != nil {
		t.Fatal(err)
	}
	return ev
}

func acceptedGuest(formID uuid.UUID, first, last, email string) ticket.FormResponse {
	return ticket.FormResponse{
		FormID: formID, ResponseID: uuid.New(), Status: ticket.FormResponseAccepted,
		Guest: &ticket.GuestInfo{FirstName: first, LastName: last, Email: email},
	}
}

func ticketsOf(t *testing.T, svc ticket.Service, ev event.Event) []ticket.Ticket {
	t.Helper()
	listed, err := svc.ListByEvent(context.Background(), yk, ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	return listed
}

func TestRecordFormResponseWritesTheGuestTicketOfAnAcceptedGuestAnswer(t *testing.T) {
	t.Parallel()
	events, svc := setup(t)
	formID := uuid.New()
	ev := seedFormEvent(t, events, formAddress(formID))

	if _, err := svc.RecordFormResponse(context.Background(), formsService, acceptedGuest(formID, " Ada ", "Lovelace", "ADA@Example.com")); err != nil {
		t.Fatal(err)
	}

	got := storedGuest(t, svc, ev, "ada@example.com")
	if got.TicketType != ticket.Guest || got.GuestFirstName != "Ada" || got.GuestLastName != "Lovelace" {
		t.Fatalf("ticket %+v", got)
	}
}

func TestRecordFormResponseRegistersAPersonWhoAnsweredSignedIn(t *testing.T) {
	t.Parallel()
	events, users, svc := setupApplyForOther(t)
	ctx := context.Background()
	formID := uuid.New()
	ev := seedFormEvent(t, events, formAddress(formID))
	person := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	seedUser(t, users, person)

	if _, err := svc.RecordFormResponse(ctx, formsService, ticket.FormResponse{
		FormID: formID, ResponseID: uuid.New(), Status: ticket.FormResponseAccepted, UserID: &person,
	}); err != nil {
		t.Fatal(err)
	}

	got, err := svc.GetByUserEvent(ctx, yk, person, ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.TicketType != ticket.Registered || got.GuestEmail != "" {
		t.Fatalf("ticket %+v", got)
	}
}

func TestRecordFormResponseWritesNothingForAPendingOrDeclinedAnswer(t *testing.T) {
	t.Parallel()
	events, svc := setup(t)
	formID := uuid.New()
	ev := seedFormEvent(t, events, formAddress(formID))

	for _, status := range []ticket.FormResponseStatus{ticket.FormResponsePending, ticket.FormResponseDeclined} {
		report := acceptedGuest(formID, "Ada", "Lovelace", "ada@example.com")
		report.Status = status
		if _, err := svc.RecordFormResponse(context.Background(), formsService, report); err != nil {
			t.Fatalf("%s: %v", status, err)
		}
	}

	if listed := ticketsOf(t, svc, ev); len(listed) != 0 {
		t.Fatalf("tickets %+v", listed)
	}
}

func TestRecordFormResponseTicketsEveryEventThatListsTheForm(t *testing.T) {
	t.Parallel()
	events, svc := setup(t)
	formID := uuid.New()
	apply := seedFormEvent(t, events, formAddress(formID))
	extra := seedFormEvent(t, events, "", event.EventFormLink{Label: "Başvuru", URL: formAddress(formID) + "?utm_source=site"})
	other := seedFormEvent(t, events, formAddress(uuid.New()))

	if _, err := svc.RecordFormResponse(context.Background(), formsService, acceptedGuest(formID, "Ada", "Lovelace", "ada@example.com")); err != nil {
		t.Fatal(err)
	}

	storedGuest(t, svc, apply, "ada@example.com")
	storedGuest(t, svc, extra, "ada@example.com")
	if listed := ticketsOf(t, svc, other); len(listed) != 0 {
		t.Fatalf("another form's Event got %+v", listed)
	}
}

func TestRecordFormResponseSentAgainFindsTheTicketsAlreadyThere(t *testing.T) {
	t.Parallel()
	events, users, svc := setupApplyForOther(t)
	ctx := context.Background()
	formID := uuid.New()
	ev := seedFormEvent(t, events, formAddress(formID))
	person := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	seedUser(t, users, person)
	member := ticket.FormResponse{FormID: formID, ResponseID: uuid.New(), Status: ticket.FormResponseAccepted, UserID: &person}
	guest := acceptedGuest(formID, "Ada", "Lovelace", "ada@example.com")

	for range 2 {
		for _, report := range []ticket.FormResponse{member, guest} {
			if _, err := svc.RecordFormResponse(ctx, formsService, report); err != nil {
				t.Fatal(err)
			}
		}
	}

	if listed := ticketsOf(t, svc, ev); len(listed) != 2 {
		t.Fatalf("tickets %+v", listed)
	}
}

func TestRecordFormResponseGivesNoTicketToAPersonCoreCannotFind(t *testing.T) {
	t.Parallel()
	events := event.NewMemoryStore()
	svc := ticket.NewService(ticket.NewMemoryStore(), events, authz.NewAuthorizer(authz.DefaultPolicy()), user.NewMemoryStore(), identity.NewMemory())
	formID := uuid.New()
	ev := seedFormEvent(t, events, formAddress(formID))
	unknown := uuid.New()

	if _, err := svc.RecordFormResponse(context.Background(), formsService, ticket.FormResponse{
		FormID: formID, ResponseID: uuid.New(), Status: ticket.FormResponseAccepted, UserID: &unknown,
	}); err != nil {
		t.Fatal(err)
	}

	if listed := ticketsOf(t, svc, ev); len(listed) != 0 {
		t.Fatalf("tickets %+v", listed)
	}
}

func TestRecordFormResponseRegistersAPersonCoreKnowsOnlyFromTheDirectory(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	events := event.NewMemoryStore()
	users := user.NewMemoryStore()
	dir := identity.NewMemory()
	svc := ticket.NewService(ticket.NewMemoryStore(), events, authz.NewAuthorizer(authz.DefaultPolicy()), users, dir)
	formID := uuid.New()
	ev := seedFormEvent(t, events, formAddress(formID))
	person := uuid.New()
	dir.PutUser(identity.Person{ID: person, Email: "ada@example.com", FirstName: "Ada", LastName: "Lovelace"})

	if _, err := svc.RecordFormResponse(ctx, formsService, ticket.FormResponse{
		FormID: formID, ResponseID: uuid.New(), Status: ticket.FormResponseAccepted, UserID: &person,
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := users.Get(ctx, person); err != nil {
		t.Fatalf("core row: %v", err)
	}
	if got, err := svc.GetByUserEvent(ctx, yk, person, ev.ID); err != nil || got.TicketType != ticket.Registered {
		t.Fatalf("ticket %+v %v", got, err)
	}
}

// The report carries what an anonymous form filler typed: it fills what a
// guest Ticket lacks and never renames the guest.
func TestRecordFormResponseNeverRenamesAGuest(t *testing.T) {
	t.Parallel()
	events, svc := setup(t)
	ctx := context.Background()
	formID := uuid.New()
	ev := seedFormEvent(t, events, formAddress(formID))
	if _, err := svc.ApplyGuest(ctx, yk, ev.ID, ticket.GuestInfo{FirstName: "Augusta", LastName: "Byron", Email: "ada@example.com"}); err != nil {
		t.Fatal(err)
	}

	if _, err := svc.RecordFormResponse(ctx, formsService, acceptedGuest(formID, "Mallory", "Renamed", "ada@example.com")); err != nil {
		t.Fatal(err)
	}

	got := storedGuest(t, svc, ev, "ada@example.com")
	if got.GuestFirstName != "Augusta" || got.GuestLastName != "Byron" {
		t.Fatalf("renamed %+v", got)
	}
}

func TestRecordFormResponseIgnoresAnArchivedEvent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	events := event.NewMemoryStore()
	tickets := ticket.NewMemoryStore()
	svc := ticket.NewService(tickets, events, authz.NewAuthorizer(authz.DefaultPolicy()))
	formID := uuid.New()
	ev := seedFormEvent(t, events, formAddress(formID))
	if err := events.Archive(ctx, ev.ID, nil); err != nil {
		t.Fatal(err)
	}

	if _, err := svc.RecordFormResponse(ctx, formsService, acceptedGuest(formID, "Ada", "Lovelace", "ada@example.com")); err != nil {
		t.Fatal(err)
	}

	if listed, err := tickets.ListByEvent(ctx, ev.ID); err != nil || len(listed) != 0 {
		t.Fatalf("tickets %+v %v", listed, err)
	}
}

func TestRecordFormResponseNeedsTheFormsServiceWithItsRole(t *testing.T) {
	t.Parallel()
	events, svc := setup(t)
	formID := uuid.New()
	seedFormEvent(t, events, formAddress(formID))
	report := acceptedGuest(formID, "Ada", "Lovelace", "ada@example.com")

	for name, p := range map[string]authz.Principal{
		"privileged person with the role": {ID: "yk", Groups: []string{"/UYELER/YK"}, Roles: []string{"ticket:forms"}},
		"forms service without the role":  {ID: "forms-sa", ServiceAccount: true, Product: authz.ProductForms},
		"another product with the role":   {ID: "cms-sa", ServiceAccount: true, Product: authz.ProductCMS, Roles: []string{"ticket:forms"}},
		"anonymous":                       anonymous,
	} {
		if _, err := svc.RecordFormResponse(context.Background(), p, report); !errors.Is(err, ticket.ErrForbidden) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestRecordFormResponseRefusesAMalformedReport(t *testing.T) {
	t.Parallel()
	events, svc := setup(t)
	formID := uuid.New()
	seedFormEvent(t, events, formAddress(formID))

	unknownStatus := acceptedGuest(formID, "Ada", "Lovelace", "ada@example.com")
	unknownStatus.Status = "approved"
	noForm := acceptedGuest(uuid.Nil, "Ada", "Lovelace", "ada@example.com")
	zero := uuid.Nil
	zeroUser := ticket.FormResponse{FormID: formID, ResponseID: uuid.New(), Status: ticket.FormResponseAccepted, UserID: &zero}

	for name, report := range map[string]ticket.FormResponse{
		"unknown status":       unknownStatus,
		"no form":              noForm,
		"zero userId":          zeroUser,
		"guest without e-mail": acceptedGuest(formID, "Ada", "Lovelace", " "),
		"guest without name":   acceptedGuest(formID, "", "Lovelace", "ada@example.com"),
	} {
		if _, err := svc.RecordFormResponse(context.Background(), formsService, report); !errors.Is(err, ticket.ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// The result names how a report ended and how many Tickets it wrote, for the
// report's log line.
func TestRecordFormResponseSaysHowTheReportEndedAndWhatItWrote(t *testing.T) {
	t.Parallel()
	events, users, svc := setupApplyForOther(t)
	ctx := context.Background()
	formID := uuid.New()
	seedFormEvent(t, events, formAddress(formID))
	seedFormEvent(t, events, "", event.EventFormLink{Label: "Başvuru", URL: formAddress(formID)})
	person := uuid.MustParse("cccccccc-cccc-cccc-cccc-cccccccccccc")
	seedUser(t, users, person)
	member := ticket.FormResponse{FormID: formID, ResponseID: uuid.New(), Status: ticket.FormResponseAccepted, UserID: &person}
	guest := acceptedGuest(formID, "Ada", "Lovelace", "ada@example.com")
	pending := acceptedGuest(formID, "Ada", "Lovelace", "ada@example.com")
	pending.Status = ticket.FormResponsePending
	unknown := uuid.New()
	stranger := ticket.FormResponse{FormID: formID, ResponseID: uuid.New(), Status: ticket.FormResponseAccepted, UserID: &unknown}

	for _, step := range []struct {
		name    string
		report  ticket.FormResponse
		outcome ticket.FormResponseOutcome
		written int
	}{
		{"member", member, ticket.FormResponseRecorded, 2},
		{"member again", member, ticket.FormResponseRecorded, 0},
		{"guest", guest, ticket.FormResponseRecorded, 2},
		{"guest again", guest, ticket.FormResponseRecorded, 0},
		{"pending", pending, ticket.FormResponseNotAccepted, 0},
		{"no respondent", ticket.FormResponse{FormID: formID, ResponseID: uuid.New(), Status: ticket.FormResponseAccepted}, ticket.FormResponseNoRespondent, 0},
		{"unlisted form", acceptedGuest(uuid.New(), "Ada", "Lovelace", "ada@example.com"), ticket.FormResponseNotListed, 0},
		{"person core cannot find", stranger, ticket.FormResponsePersonUnavailable, 0},
	} {
		got, err := svc.RecordFormResponse(ctx, formsService, step.report)
		if err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		if got.Outcome != step.outcome || got.TicketsWritten != step.written {
			t.Errorf("%s: %+v, want %s and %d written", step.name, got, step.outcome, step.written)
		}
	}
}
