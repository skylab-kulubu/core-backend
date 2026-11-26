package dashboard_test

import (
	"slices"
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/event"
)

// The to-dos are decided per Event: a GECEKODU draft that WEBLAB
// co-organizes is WEBLAB's Leader's to publish (docs/co-organizers.md).
func TestHomeOfALeaderListsTheEventsTheyCoOrganize(t *testing.T) {
	t.Parallel()
	f := newHomeFixture(t)
	draft, err := f.events.Create(t.Context(), event.Event{Name: "Gece Fikir", OwnerTeam: "GECEKODU", Status: event.StatusDraft})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.events.SetCoOrganizers(t.Context(), event.CoOrganizersWrite{
		EventID: draft.ID, OwnerTeam: "GECEKODU",
		Teams: []event.CoOrganizerTeam{{Code: "WEBLAB", Management: event.ManagementLeaders, Visibility: event.VisibilityHelper}},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := f.svc.Summary(t.Context(), leader)
	if err != nil {
		t.Fatal(err)
	}
	if names := todoNames(got.Home.Todo.Drafts); !slices.Contains(names, "Gece Fikir") || !slices.Contains(names, "WebLab Fikir") {
		t.Fatalf("drafts %v", names)
	}
}
