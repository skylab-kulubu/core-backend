package formsfeed_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/formsfeed"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
)

type mappingRows []formsfeed.Mapping

func (m mappingRows) ForForm(_ context.Context, formID uuid.UUID) ([]formsfeed.Mapping, error) {
	var out []formsfeed.Mapping
	for _, r := range m {
		if r.FormID == formID {
			out = append(out, r)
		}
	}
	return out, nil
}

// answersReader is Forms' single read of an anonymous answer: the answers
// of each response id.
type answersReader map[uuid.UUID]map[string]string

func (a answersReader) Response(_ context.Context, formID, responseID uuid.UUID, fields ...string) (formsfeed.Response, error) {
	asked := map[string]string{}
	for _, f := range fields {
		if v, ok := a[responseID][f]; ok {
			asked[f] = v
		}
	}
	return formsfeed.Response{FormID: formID, ResponseID: responseID, Status: formsfeed.StatusAccepted, Version: 1, Answers: asked}, nil
}

// The HTTP report of an anonymous answer, through the mapping, by the one
// Ticket rule: a guest Ticket; the same e-mail again writes no second one;
// a declined answer marks the Ticket it wrote (K6); a second Event listing the
// form without a mapping gets no guest.
func TestTheHTTPReportOfAnAnonymousAnswerThroughTheMapping(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t, 0, false, 0)
	other, err := w.events.Create(ctx, event.Event{Name: "Eski", Location: "YTÜ", OwnerTeam: "WEBLAB", FormURL: "https://forms.yildizskylab.com/" + w.formID.String()})
	if err != nil {
		t.Fatal(err)
	}
	first, second, third := uuid.New(), uuid.New(), uuid.New()
	reader := answersReader{
		first:  {"n": "Ada Lovelace", "e": "Ada@Example.com", "phone": "555"},
		second: {"n": "Ada King", "e": "ada@example.com"},
		third:  {"n": "Ada", "e": "solo@example.com"},
	}
	m := &formsfeed.MappedGuests{
		Store:  mappingRows{{EventID: w.ev.ID, FormID: w.formID, FullName: "n", Email: "e"}},
		Events: w.events.(formsfeed.EventLister), Forms: reader,
	}
	record := func(ctx context.Context, r ticket.FormResponse) (ticket.FormResponseResult, error) {
		return w.svc.RecordFormResponse(ctx, formsService, r)
	}
	report := func(id uuid.UUID, status ticket.FormResponseStatus) ticket.FormResponseResult {
		t.Helper()
		got, err := m.RecordReport(ctx, record, ticket.FormResponse{FormID: w.formID, ResponseID: id, Status: status})
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	yk := authz.Principal{ID: "yk", Groups: []string{"/UYELER/YK"}}
	tickets := func(eventID uuid.UUID) []ticket.Ticket {
		t.Helper()
		got, err := w.svc.ListByEventStatus(ctx, yk, eventID, nil)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	if got := report(first, ticket.FormResponseAccepted); got.Outcome != ticket.FormResponseMappedGuest || got.TicketsWritten != 1 || got.ListingEvents != 2 {
		t.Fatalf("first: %+v", got)
	}
	held := tickets(w.ev.ID)
	if len(held) != 1 || held[0].GuestEmail != "ada@example.com" || held[0].GuestFirstName != "Ada" || held[0].GuestLastName != "Lovelace" {
		t.Fatalf("tickets %+v", held)
	}
	if len(tickets(other.ID)) != 0 {
		t.Fatal("the Event without a mapping got the guest")
	}
	// The same person's second answer: no second Ticket, no rename.
	if got := report(second, ticket.FormResponseAccepted); got.TicketsWritten != 0 {
		t.Fatalf("second: %+v", got)
	}
	if held = tickets(w.ev.ID); len(held) != 1 || held[0].GuestLastName != "Lovelace" {
		t.Fatalf("after the second answer %+v", held)
	}
	if got := report(third, ticket.FormResponseAccepted); got.Outcome != ticket.FormResponseMappingIncomplete {
		t.Fatalf("one word: %+v", got)
	}
	// K6 (#460): the second accepted answer took the Ticket over, so the
	// first one's decline no longer reaches it; the second's marks it and
	// cancels nothing.
	if got := report(first, ticket.FormResponseDeclined); got.Outcome != ticket.FormResponseNotAccepted {
		t.Fatalf("the first answer declined: %+v", got)
	}
	if got := report(second, ticket.FormResponseDeclined); got.Outcome != ticket.FormResponseFormsRejected || got.TicketsMarked != 1 {
		t.Fatalf("declined: %+v", got)
	}
	if held = tickets(w.ev.ID); len(held) != 1 || held[0].StatusOf() == ticket.StatusCancelled || held[0].FormsRejectedAt == nil {
		t.Fatalf("after the decline %+v", held)
	}

	// Without a mapping the report is no_respondent, as before.
	none := &formsfeed.MappedGuests{Store: mappingRows{}, Events: w.events.(formsfeed.EventLister), Forms: reader}
	if got, err := none.RecordReport(ctx, record, ticket.FormResponse{FormID: w.formID, ResponseID: uuid.New(), Status: ticket.FormResponseAccepted}); err != nil || got.Outcome != ticket.FormResponseNoRespondent {
		t.Fatalf("no mapping: %+v %v", got, err)
	}
}
