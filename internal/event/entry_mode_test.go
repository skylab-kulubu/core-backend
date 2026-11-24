package event_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
)

// The door's mode (docs/venue-entry.md): a new Event is automatic (null)
// and works as venue until it has a live Session, then per_session; a
// chosen mode holds while it has one; PATCH sets it, null gives it back to
// automatic, a body without it keeps it, and an unknown value is refused.
// A copy takes the source's.
func TestEntryMode(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := event.NewMemoryStore()
	svc := event.NewService(store, authz.NewAuthorizer(authz.DefaultPolicy()))
	leader := authz.Principal{ID: "l", Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}
	created, err := svc.Create(ctx, leader, event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB"})
	if err != nil {
		t.Fatal(err)
	}
	if created.EntryMode != nil || created.EffectiveEntryMode != event.EntryVenue {
		t.Fatalf("new: %v %q", created.EntryMode, created.EffectiveEntryMode)
	}
	raw, err := json.Marshal(created)
	if err != nil || !strings.Contains(string(raw), `"entryMode":null`) || !strings.Contains(string(raw), `"effectiveEntryMode":"venue"`) {
		t.Fatalf("json %s %v", raw, err)
	}
	if res := created.Resource(); res.EffectiveEntryMode != event.EntryVenue {
		t.Fatalf("resource %q", res.EffectiveEntryMode)
	}

	day, err := store.CreateDay(ctx, event.Day{EventID: created.ID, Name: "1. gün"})
	if err != nil {
		t.Fatal(err)
	}
	sess, err := store.CreateSession(ctx, event.Session{EventDayID: day.ID, Title: "Açılış", SpeakerName: "Ada", SessionType: "PRESENTATION"})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := svc.Get(ctx, &leader, created.ID); err != nil || got.EffectiveEntryMode != event.EntryPerSession {
		t.Fatalf("with a Session: %q %v", got.EffectiveEntryMode, err)
	}

	venue, err := svc.Patch(ctx, leader, created.ID, patchOf(t, `{"entryMode":"venue"}`))
	if err != nil || venue.EntryMode == nil || *venue.EntryMode != event.EntryVenue || venue.EffectiveEntryMode != event.EntryVenue {
		t.Fatalf("venue: %+v %v", venue.EntryMode, err)
	}
	kept, err := svc.Patch(ctx, leader, created.ID, patchOf(t, `{"capacity":20}`))
	if err != nil || kept.EntryMode == nil || *kept.EntryMode != event.EntryVenue {
		t.Fatalf("kept: %+v %v", kept.EntryMode, err)
	}
	if _, err := svc.Patch(ctx, leader, created.ID, patchOf(t, `{"entryMode":"door"}`)); !errors.Is(err, event.ErrInvalid) {
		t.Fatalf("unknown: %v", err)
	}
	if _, err := svc.Create(ctx, leader, event.Event{Name: "X", Location: "YTÜ", OwnerTeam: "WEBLAB", EntryMode: entryModeOf("door")}); !errors.Is(err, event.ErrInvalid) {
		t.Fatalf("unknown on create: %v", err)
	}

	copied, _, err := svc.Copy(ctx, leader, created.ID, copyIn("Hack 2", "2027-03-13T19:00:00+03:00"))
	if err != nil {
		t.Fatal(err)
	}
	if copied.Event.EntryMode == nil || *copied.Event.EntryMode != event.EntryVenue {
		t.Fatalf("copy: %+v", copied.Event.EntryMode)
	}

	auto, err := svc.Patch(ctx, leader, created.ID, patchOf(t, `{"entryMode":null}`))
	if err != nil || auto.EntryMode != nil || auto.EffectiveEntryMode != event.EntryPerSession {
		t.Fatalf("automatic: %+v %q %v", auto.EntryMode, auto.EffectiveEntryMode, err)
	}
	// A chosen per_session with no live Session left works as venue.
	if _, err := svc.Patch(ctx, leader, created.ID, patchOf(t, `{"entryMode":"per_session"}`)); err != nil {
		t.Fatal(err)
	}
	if err := store.ArchiveSession(ctx, sess.ID, nil); err != nil {
		t.Fatal(err)
	}
	if got, err := svc.Get(ctx, &leader, created.ID); err != nil || got.EffectiveEntryMode != event.EntryVenue {
		t.Fatalf("no live Session: %q %v", got.EffectiveEntryMode, err)
	}
}

func entryModeOf(m string) *event.EntryMode {
	mode := event.EntryMode(m)
	return &mode
}
