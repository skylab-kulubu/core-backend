package formsfeed_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/events"
	"github.com/skylab-kulubu/core-backend/internal/formsfeed"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
)

// formsService is the forms service's account as the HTTP report's token
// reads.
var formsService = authz.Principal{ID: "forms-sa", ServiceAccount: true, Product: authz.ProductForms, Roles: []string{"ticket:forms"}}

type guestReader struct{ guest formsfeed.Guest }

func (g guestReader) Response(_ context.Context, formID, responseID uuid.UUID, _ ...string) (formsfeed.Response, error) {
	guest := g.guest
	return formsfeed.Response{FormID: formID, ResponseID: responseID, Status: formsfeed.StatusAccepted, HasGuest: true, Version: 1, Guest: &guest}, nil
}

type world struct {
	events event.Store
	svc    ticket.Service
	ev     event.Event
	formID uuid.UUID
}

func newWorld(t *testing.T, capacity int, waitlist bool, filled int) world {
	t.Helper()
	ctx := context.Background()
	events := event.NewMemoryStore()
	svc := ticket.NewService(ticket.NewMemoryStore(), events, authz.NewAuthorizer(authz.DefaultPolicy()))
	formID := uuid.New()
	ev, err := events.Create(ctx, event.Event{
		Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB", Capacity: capacity, WaitlistEnabled: &waitlist,
		FormURL: "https://forms.yildizskylab.com/" + formID.String(),
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < filled; i++ {
		if _, err := svc.RecordFormResponse(ctx, formsService, ticket.FormResponse{
			FormID: formID, ResponseID: uuid.New(), Status: ticket.FormResponseAccepted,
			Guest: &ticket.GuestInfo{FirstName: "İlk", LastName: "Kişi", Email: uuid.NewString() + "@example.com"},
		}); err != nil {
			t.Fatal(err)
		}
	}
	return world{events: events, svc: svc, ev: ev, formID: formID}
}

func (w world) statuses(t *testing.T) map[ticket.Status]int {
	t.Helper()
	listed, err := w.svc.ListByEventStatus(context.Background(), authz.Principal{ID: "yk", Groups: []string{"/UYELER/YK"}}, w.ev.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	out := map[ticket.Status]int{}
	for _, tk := range listed {
		out[tk.StatusOf()]++
	}
	return out
}

// An answer that comes as a core.forms event ends exactly as the same answer
// sent as the HTTP report: the one Ticket rule, with its capacity and
// waiting list (docs/event-capacity.md) and its handling of a declined
// answer.
func TestAnEventAndTheHTTPReportTakeTheSameTicketRule(t *testing.T) {
	cases := []struct {
		name     string
		capacity int
		waitlist bool
		filled   int
		status   string
		active   int
		waiting  int
	}{
		{name: "room", capacity: 0, status: formsfeed.StatusAccepted, active: 1},
		{name: "full with a waiting list", capacity: 1, waitlist: true, filled: 1, status: formsfeed.StatusAccepted, active: 1, waiting: 1},
		{name: "full without a waiting list", capacity: 1, waitlist: false, filled: 1, status: formsfeed.StatusAccepted, active: 1},
		{name: "pending", status: formsfeed.StatusPending},
		{name: "declined", status: formsfeed.StatusDeclined},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			guest := formsfeed.Guest{FirstName: "Ada", LastName: "Lovelace", Email: "ada@example.com"}
			responseID := uuid.New()

			viaHTTP := newWorld(t, tc.capacity, tc.waitlist, tc.filled)
			httpResult, err := viaHTTP.svc.RecordFormResponse(ctx, formsService, ticket.FormResponse{
				FormID: viaHTTP.formID, ResponseID: responseID, Status: ticket.FormResponseStatus(tc.status),
				Guest: &ticket.GuestInfo{FirstName: guest.FirstName, LastName: guest.LastName, Email: guest.Email},
			})
			if err != nil {
				t.Fatal(err)
			}

			viaEvent := newWorld(t, tc.capacity, tc.waitlist, tc.filled)
			rec := &formsfeed.Recorder{
				Events: viaEvent.events.(formsfeed.EventLister),
				Forms:  guestReader{guest: guest},
				Record: func(ctx context.Context, r ticket.FormResponse) (ticket.FormResponseResult, error) {
					return viaEvent.svc.RecordFormResponse(ctx, formsService, r)
				},
				Versions: alwaysNewer{},
			}
			data, _ := json.Marshal(map[string]any{
				"formId": viaEvent.formID, "responseId": responseID, "status": tc.status, "userId": nil,
				"hasGuest": true, "submittedAt": nil, "hiddenFields": map[string]any{}, "version": 1,
			})
			handle := formsfeed.Subscription(rec).Handle
			if err := handle(ctx, nil, events.Event{ID: uuid.NewString(), Source: formsfeed.Source, Type: formsfeed.TypeResponseCreated, Data: data}); err != nil {
				t.Fatal(err)
			}
			if got, want := viaEvent.statuses(t), viaHTTP.statuses(t); len(got) != len(want) || got[ticket.StatusActive] != want[ticket.StatusActive] || got[ticket.StatusWaitlisted] != want[ticket.StatusWaitlisted] {
				t.Fatalf("event %v, HTTP %v (HTTP outcome %s)", got, want, httpResult.Outcome)
			}
			if got := viaEvent.statuses(t); got[ticket.StatusActive] != tc.active || got[ticket.StatusWaitlisted] != tc.waiting {
				t.Fatalf("statuses %v, want %d active and %d waiting", got, tc.active, tc.waiting)
			}
		})
	}
}

type alwaysNewer struct{}

func (alwaysNewer) Advance(context.Context, pgx.Tx, string, uuid.UUID, int64, bool) (bool, error) {
	return true, nil
}
