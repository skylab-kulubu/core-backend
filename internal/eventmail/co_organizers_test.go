package eventmail

import (
	"errors"
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// WEBLAB co-organizes YK's Event: WEBLAB's Leader opens its applicants'
// mail list as YK's Leader would; a WEBLAB member does not
// (docs/co-organizers.md).
func TestSyncByACoOrganizer(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	events := event.NewMemoryStore()
	ev, err := events.Create(ctx, event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "YK"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := events.SetCoOrganizers(ctx, event.CoOrganizersWrite{
		EventID: ev.ID, OwnerTeam: "YK",
		Teams: []event.CoOrganizerTeam{{Code: "WEBLAB", Management: event.ManagementLeaders, Visibility: event.VisibilityCoOrganizer}},
	}); err != nil {
		t.Fatal(err)
	}
	svc := New(events, ticket.NewMemoryStore(), user.NewMemoryStore(), newMemoryLists(), authz.NewAuthorizer(authz.DefaultPolicy()))
	if _, err := svc.Sync(ctx, mailLeader(), ev.ID, nil); err != nil {
		t.Fatalf("WEBLAB's Leader: %v", err)
	}
	if _, err := svc.Sync(ctx, authz.Principal{ID: "u1", Groups: []string{"/UYELER/ARGE/WEBLAB"}}, ev.ID, nil); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a WEBLAB member: %v", err)
	}
}
