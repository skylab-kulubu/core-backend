package event_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
)

// Who edits the Event as a Co-organizer person alone may not lengthen their
// own rights: clearing the dates their end is read from, or moving it later,
// is ErrCoOrganizerPersonDates; moving it earlier, or any other field, is
// fine. A Co-organizer team's Leader and the Owner team move it freely. A
// Co-organizer person does not change the door staff either.
func TestCoOrganizerPersonMayNotLengthenTheirRights(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, svc := organizerSetup(t)
	start := time.Date(2026, 12, 12, 10, 0, 0, 0, time.UTC)
	end := time.Date(2026, 12, 12, 18, 0, 0, 0, time.UTC)
	staff := uuid.New()
	created, err := svc.Create(ctx, ykMember, event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "YK", StartDate: &start, EndDate: &end, DoorStaffIDs: []uuid.UUID{staff}})
	if err != nil {
		t.Fatal(err)
	}
	personID := uuid.New()
	if _, err := svc.SetOrganizers(ctx, ykMember, created.ID, event.OrganizersInput{
		Teams: []event.OrganizerTeamInput{{Code: "WEBLAB"}}, People: []uuid.UUID{personID},
	}); err != nil {
		t.Fatal(err)
	}
	person := authz.Principal{ID: personID.String()}
	edit := func(p authz.Principal, start, end *time.Time) error {
		_, err := svc.Update(ctx, p, created.ID, event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "YK", StartDate: start, EndDate: end})
		return err
	}
	later := end.Add(48 * time.Hour)
	earlier := end.Add(-2 * time.Hour)
	for name, dates := range map[string][2]*time.Time{
		"clearing both dates":     {nil, nil},
		"clearing the end":        {&start, nil},
		"moving the end later":    {&start, &later},
		"moving the start beyond": {&later, nil},
	} {
		if err := edit(person, dates[0], dates[1]); !errors.Is(err, event.ErrCoOrganizerPersonDates) || !errors.Is(err, event.ErrForbidden) {
			t.Errorf("the person, %s: %v", name, err)
		}
	}
	if err := edit(person, &start, &earlier); err != nil {
		t.Fatalf("the person moves the end earlier: %v", err)
	}
	if err := edit(person, &start, &earlier); err != nil {
		t.Fatalf("the person saves the dates unchanged: %v", err)
	}
	// A plain member of a "leaders" Co-organizer team has no seat there:
	// the person is still bound.
	plain := authz.Principal{ID: personID.String(), Groups: []string{"/UYELER/ARGE/WEBLAB"}}
	if err := edit(plain, &start, &later); !errors.Is(err, event.ErrCoOrganizerPersonDates) {
		t.Fatalf("the person, also a plain WEBLAB member, moves the end later: %v", err)
	}
	// Door staff stay as they are: the person may not assign them.
	updated, err := svc.Update(ctx, person, created.ID, event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "YK", StartDate: &start, EndDate: &earlier, DoorStaffIDs: []uuid.UUID{uuid.New()}})
	if err != nil || !slices.Equal(updated.DoorStaffIDs, []uuid.UUID{staff}) {
		t.Fatalf("the person changed the door staff: %v %v", updated.DoorStaffIDs, err)
	}
	if err := edit(ownerLeader, &start, &later); err != nil {
		t.Fatalf("WEBLAB's Leader (a Co-organizer team) moves the end later: %v", err)
	}
	if err := edit(ykMember, nil, nil); err != nil {
		t.Fatalf("YK clears the dates: %v", err)
	}
	// No end to lengthen: the person edits the dates.
	if err := edit(person, &start, &end); err != nil {
		t.Fatalf("the person sets dates on an Event without them: %v", err)
	}
	// An Event with a start alone: the end is read from the start plus a
	// day, so an end after that lengthens it; one before it does not.
	if err := edit(ykMember, &start, nil); err != nil {
		t.Fatal(err)
	}
	if err := edit(person, &start, &later); !errors.Is(err, event.ErrCoOrganizerPersonDates) {
		t.Fatalf("the person adds an end after the start's day: %v", err)
	}
	if err := edit(person, &start, &end); err != nil {
		t.Fatalf("the person adds an end within the start's day: %v", err)
	}
	// Seated in a team too, the person is not bound.
	both := authz.Principal{ID: personID.String(), Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}
	if err := edit(both, &start, &later); err != nil {
		t.Fatalf("the person who also leads WEBLAB: %v", err)
	}
}
