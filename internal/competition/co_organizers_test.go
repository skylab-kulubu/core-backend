package competition_test

import (
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/competition"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/lifecycle"
)

// WEBLAB co-organizes AGC's Event ("leaders"): WEBLAB's Leader and a
// Co-organizer person organize its Competitions as AGC's Leader does; a
// WEBLAB member does not (docs/co-organizers.md).
func TestCoOrganizersOrganizeTheEventsCompetitions(t *testing.T) {
	t.Parallel()
	f := newFixture(t, memoryStore)
	person := uuid.New()
	if _, err := f.events.SetCoOrganizers(t.Context(), event.CoOrganizersWrite{
		EventID: f.event.ID, OwnerTeam: "AGC",
		Teams:  []event.CoOrganizerTeam{{Code: "WEBLAB", Management: event.ManagementLeaders, Visibility: event.VisibilityCoOrganizer}},
		People: []uuid.UUID{person},
	}); err != nil {
		t.Fatal(err)
	}
	for name, p := range map[string]authz.Principal{
		"WEBLAB's Leader": {ID: uuid.NewString(), Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}},
		"the person":      {ID: person.String()},
	} {
		c, err := f.svc.Create(t.Context(), p, f.event.ID, competition.CreateInput{Name: name})
		if err != nil {
			t.Fatalf("%s creates: %v", name, err)
		}
		if _, err := f.svc.Get(t.Context(), p, c.ID); err != nil {
			t.Errorf("%s reads: %v", name, err)
		}
		if _, err := f.svc.ListByEvent(t.Context(), p, f.event.ID, lifecycle.CurrentOnly); err != nil {
			t.Errorf("%s lists: %v", name, err)
		}
	}
	member := authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/ARGE/WEBLAB"}}
	if _, err := f.svc.Create(t.Context(), member, f.event.ID, competition.CreateInput{Name: "x"}); !errors.Is(err, competition.ErrForbidden) {
		t.Errorf("a WEBLAB member creates: %v", err)
	}
}
