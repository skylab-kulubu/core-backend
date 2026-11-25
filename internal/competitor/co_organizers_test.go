package competitor_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/competitor"
	"github.com/skylab-kulubu/core-backend/internal/event"
)

// WEBLAB co-organizes YK's Event: WEBLAB's Leader adds someone else's
// competitor and reads the Event's; YK's team list shows them only the
// Event they co-organize; another team's Leader does neither
// (docs/co-organizers.md).
func TestCoOrganizersManageTheEventsCompetitors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	events := event.NewMemoryStore()
	svc := competitor.NewService(competitor.NewMemoryStore(events), events, authz.NewAuthorizer(authz.DefaultPolicy()))
	ev := seedEvent(t, events, "YK", event.StatusPublished)
	other := seedEvent(t, events, "YK", event.StatusPublished)
	if _, err := events.SetCoOrganizers(ctx, event.CoOrganizersWrite{
		EventID: ev.ID, OwnerTeam: "YK",
		Teams: []event.CoOrganizerTeam{{Code: "WEBLAB", Management: event.ManagementLeaders, Visibility: event.VisibilityCoOrganizer}},
	}); err != nil {
		t.Fatal(err)
	}
	yk := authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/YK"}}
	if _, err := svc.Create(ctx, yk, competitor.CreateInput{UserID: uuid.New(), EventID: other.ID}); err != nil {
		t.Fatal(err)
	}
	leader := authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}
	added, err := svc.Create(ctx, leader, competitor.CreateInput{UserID: uuid.New(), EventID: ev.ID})
	if err != nil {
		t.Fatalf("WEBLAB's Leader adds: %v", err)
	}
	if _, err := svc.Create(ctx, leader, competitor.CreateInput{UserID: uuid.New(), EventID: other.ID}); !errors.Is(err, competitor.ErrForbidden) {
		t.Fatalf("WEBLAB's Leader adds to an Event they do not co-organize: %v", err)
	}
	listed, err := svc.ListByOwnerTeam(ctx, leader, "YK")
	if err != nil || len(listed) != 1 || listed[0].ID != added.ID {
		t.Fatalf("YK's list to WEBLAB's Leader: %+v %v", listed, err)
	}
	stranger := authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/SANAT/ARTLAB/LIDERLER"}}
	if _, err := svc.Create(ctx, stranger, competitor.CreateInput{UserID: uuid.New(), EventID: ev.ID}); !errors.Is(err, competitor.ErrForbidden) {
		t.Fatalf("another team's Leader adds: %v", err)
	}
	if _, err := svc.ListByOwnerTeam(ctx, stranger, "YK"); !errors.Is(err, competitor.ErrForbidden) {
		t.Fatalf("YK's list to another team's Leader: %v", err)
	}
}

// countingEvents counts the Event reads.
type countingEvents struct {
	event.Store
	gets atomic.Int64
}

func (c *countingEvents) Get(ctx context.Context, id uuid.UUID) (event.Event, error) {
	c.gets.Add(1)
	return c.Store.Get(ctx, id)
}

// YK's team list to whom is not of YK reads each of YK's Events once,
// however many competitors it has, and nothing for a caller who is no
// person (docs/co-organizers.md).
func TestCoOrganizerTeamListReadsEachEventOnce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := event.NewMemoryStore()
	events := &countingEvents{Store: store}
	svc := competitor.NewService(competitor.NewMemoryStore(store), events, authz.NewAuthorizer(authz.DefaultPolicy()))
	ev := seedEvent(t, store, "YK", event.StatusPublished)
	other := seedEvent(t, store, "YK", event.StatusPublished)
	yk := authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/YK"}}
	for _, id := range []uuid.UUID{ev.ID, ev.ID, ev.ID, other.ID, other.ID} {
		if _, err := svc.Create(ctx, yk, competitor.CreateInput{UserID: uuid.New(), EventID: id}); err != nil {
			t.Fatal(err)
		}
	}
	stranger := authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/SANAT/ARTLAB/LIDERLER"}}
	events.gets.Store(0)
	if _, err := svc.ListByOwnerTeam(ctx, stranger, "YK"); !errors.Is(err, competitor.ErrForbidden) {
		t.Fatalf("YK's list to another team's Leader: %v", err)
	}
	if got := events.gets.Load(); got != 2 {
		t.Fatalf("Event reads for 5 competitors on 2 Events: %d", got)
	}
	events.gets.Store(0)
	for name, p := range map[string]authz.Principal{
		"nobody":            {},
		"a service account": {ID: uuid.NewString(), ServiceAccount: true},
	} {
		if _, err := svc.ListByOwnerTeam(ctx, p, "YK"); !errors.Is(err, competitor.ErrForbidden) {
			t.Fatalf("YK's list to %s: %v", name, err)
		}
	}
	if got := events.gets.Load(); got != 0 {
		t.Fatalf("Event reads for no person: %d", got)
	}
}
