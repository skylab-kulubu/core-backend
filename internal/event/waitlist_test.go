package event_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
)

// A new Event has its waiting list on; PATCH turns it off and on, keeps it
// when the body leaves it out, and refuses null. A copy takes the source's.
func TestWaitlistSwitch(t *testing.T) {
	t.Parallel()
	svc := event.NewService(event.NewMemoryStore(), authz.NewAuthorizer(authz.DefaultPolicy()))
	ctx := context.Background()
	leader := authz.Principal{ID: "l", Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}
	created, err := svc.Create(ctx, leader, event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB", Capacity: 10})
	if err != nil {
		t.Fatal(err)
	}
	if !created.Waitlist() {
		t.Fatal("a new Event's waiting list is off")
	}
	off, err := svc.Patch(ctx, leader, created.ID, patchOf(t, `{"waitlistEnabled":false}`))
	if err != nil || off.Waitlist() {
		t.Fatalf("off: %+v %v", off.WaitlistEnabled, err)
	}
	kept, err := svc.Patch(ctx, leader, created.ID, patchOf(t, `{"capacity":20}`))
	if err != nil || kept.Waitlist() || kept.Capacity != 20 {
		t.Fatalf("kept: %v %d %v", kept.Waitlist(), kept.Capacity, err)
	}
	if _, err := svc.Patch(ctx, leader, created.ID, patchOf(t, `{"waitlistEnabled":null}`)); !errors.Is(err, event.ErrInvalid) {
		t.Fatalf("null: %v", err)
	}
	if on, err := svc.Patch(ctx, leader, created.ID, patchOf(t, `{"waitlistEnabled":true}`)); err != nil || !on.Waitlist() {
		t.Fatalf("on: %v", err)
	}
}

// Capacity: full once the active Tickets fill a bound; Free is -1 without
// one; promotable only while published, not archived and not started.
func TestCapacityOf(t *testing.T) {
	t.Parallel()
	now := time.Now()
	future, past := now.Add(time.Hour), now.Add(-time.Hour)
	ev := event.Event{Capacity: 2, Status: event.StatusPublished, StartDate: &future}
	if c := event.CapacityOf(ev, 1, now); c.Full() || c.Free() != 1 || !c.Promotable || !c.Waitlist {
		t.Fatalf("%+v", c)
	}
	if c := event.CapacityOf(ev, 3, now); !c.Full() || c.Free() != 0 {
		t.Fatalf("over: %+v", c)
	}
	ev.Capacity = 0
	if c := event.CapacityOf(ev, 100, now); c.Full() || c.Free() != -1 {
		t.Fatalf("unbounded: %+v", c)
	}
	ev.StartDate = &past
	if event.CapacityOf(ev, 0, now).Promotable {
		t.Fatal("started yet promotable")
	}
	ev.StartDate, ev.Status = nil, event.StatusDraft
	if event.CapacityOf(ev, 0, now).Promotable {
		t.Fatal("draft yet promotable")
	}
}
