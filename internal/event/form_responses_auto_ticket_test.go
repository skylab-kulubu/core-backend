package event_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
)

type linkedForms struct{ calls [][]uuid.UUID }

func (l *linkedForms) FormsLinked(_ context.Context, _ event.Event, formIDs []uuid.UUID) {
	l.calls = append(l.calls, formIDs)
}

// "Otomatik bilet gönder" is on for a new Event, a patch turns it off (null
// is refused), and the forms an Event newly lists are told to the hook
// (docs/form-respondents.md).
func TestFormResponsesAutoTicketAndLinkedForms(t *testing.T) {
	t.Parallel()
	linked := &linkedForms{}
	svc := event.NewServiceWithOptions(event.NewMemoryStore(), authz.NewAuthorizer(authz.DefaultPolicy()), event.ServiceOptions{FormsLinked: linked})
	ctx := context.Background()
	leader := authz.Principal{ID: "l", Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}
	first, extra := uuid.New(), uuid.New()
	created, err := svc.Create(ctx, leader, event.Event{
		Name: "Yaz Kampı", Location: "YTÜ", OwnerTeam: "WEBLAB", FormURL: "https://forms.yildizskylab.com/" + first.String(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.FormResponsesAutoTicket == nil || !*created.FormResponsesAutoTicket {
		t.Fatalf("auto ticket %v", created.FormResponsesAutoTicket)
	}
	if len(linked.calls) != 1 || !slices.Equal(linked.calls[0], []uuid.UUID{first}) {
		t.Fatalf("linked on create %v", linked.calls)
	}
	updated, err := svc.Patch(ctx, leader, created.ID, patchOf(t, `{"formResponsesAutoTicket": false, "extraFormUrls": [{"url": "https://forms.yildizskylab.com/`+extra.String()+`"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if updated.FormResponsesAutoTicket == nil || *updated.FormResponsesAutoTicket {
		t.Fatalf("auto ticket after patch %v", updated.FormResponsesAutoTicket)
	}
	if len(linked.calls) != 2 || !slices.Equal(linked.calls[1], []uuid.UUID{extra}) {
		t.Fatalf("linked on update %v", linked.calls)
	}
	// A patch that keeps the forms tells nothing.
	if _, err := svc.Patch(ctx, leader, created.ID, patchOf(t, `{"name": "Yaz Kampı 2"}`)); err != nil {
		t.Fatal(err)
	}
	if len(linked.calls) != 2 {
		t.Fatalf("linked again %v", linked.calls)
	}
	if _, err := svc.Patch(ctx, leader, created.ID, patchOf(t, `{"formResponsesAutoTicket": null}`)); !errors.Is(err, event.ErrInvalid) {
		t.Fatalf("null: %v", err)
	}
}
