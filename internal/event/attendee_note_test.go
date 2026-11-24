package event_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/optional"
)

// The attendee note (docs/attendance-mail.md): written trimmed with its
// line breaks, at most 1000 characters, no control characters; only who
// may edit the Event reads it; a write without it keeps it, null clears it;
// a copy does not take it.
func TestAttendeeNote(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := event.NewMemoryStore()
	svc := event.NewService(store, authz.NewAuthorizer(authz.DefaultPolicy()))
	note := "  Wi-Fi: SKYLAB\r\nParola: ornek-parola\n\tSalon D-001  "
	ev, err := svc.Create(ctx, copyLeader, event.Event{Name: "Buluşma", Location: "YTÜ", OwnerTeam: "WEBLAB", AttendeeNote: &note})
	if err != nil {
		t.Fatal(err)
	}
	stored, err := store.Get(ctx, ev.ID)
	if err != nil || stored.AttendeeNote == nil || *stored.AttendeeNote != "Wi-Fi: SKYLAB\nParola: ornek-parola\n\tSalon D-001" {
		t.Fatalf("stored %v %v", stored.AttendeeNote, err)
	}

	for name, p := range map[string]authz.Principal{"leader": copyLeader, "YK": copyYK, "DK": copyDK} {
		seen := svc.ProjectFor(&p, stored)
		if seen.AttendeeNoteView == nil || *seen.AttendeeNoteView != *stored.AttendeeNote {
			t.Fatalf("%s reads %v", name, seen.AttendeeNoteView)
		}
	}
	// DK reads it but cannot write it.
	if _, err := svc.Patch(ctx, copyDK, ev.ID, event.Patch{AttendeeNote: optional.Field[string]{Set: true, Value: "x"}}); !errors.Is(err, event.ErrForbidden) {
		t.Fatalf("DK wrote the note: %v", err)
	}
	for name, p := range map[string]*authz.Principal{"anonymous": nil, "member": &copyMember} {
		raw, _ := json.Marshal(svc.ProjectFor(p, stored))
		if strings.Contains(string(raw), "attendeeNote") || strings.Contains(string(raw), "ornek-parola") {
			t.Fatalf("%s reads the note: %s", name, raw)
		}
	}
	// The stored field itself never goes out.
	if raw, _ := json.Marshal(stored); strings.Contains(string(raw), "ornek-parola") {
		t.Fatalf("the Event JSON carries the note: %s", raw)
	}

	// A patch without it keeps it; null clears it.
	kept, err := svc.Patch(ctx, copyLeader, ev.ID, event.Patch{Name: optional.Field[string]{Set: true, Value: "Buluşma 2"}})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := store.Get(ctx, kept.ID); got.AttendeeNote == nil || *got.AttendeeNote == "" {
		t.Fatal("a patch without the note cleared it")
	}
	for name, bad := range map[string]string{
		"1001 characters": strings.Repeat("ş", 1001),
		"NUL":             "Wi-Fi\x00",
		"RLO":             "https://example.test/\u202Egnp.exe",
		"isolate":         "Salon \u2067D-001\u2069",
		"zero width":      "Wi\u200B-Fi",
		"BOM":             "\uFEFFWi-Fi",
	} {
		_, err := svc.Patch(ctx, copyLeader, ev.ID, event.Patch{AttendeeNote: optional.Field[string]{Set: true, Value: bad}})
		if !errors.Is(err, event.ErrAttendeeNoteInvalid) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if _, err := svc.Patch(ctx, copyLeader, ev.ID, event.Patch{AttendeeNote: optional.Field[string]{Set: true, Value: strings.Repeat("ş", 1000)}}); err != nil {
		t.Fatalf("1000 characters: %v", err)
	}
	if _, err := svc.Patch(ctx, copyLeader, ev.ID, event.Patch{AttendeeNote: optional.Field[string]{Null: true}}); err != nil {
		t.Fatal(err)
	}
	if got, _ := store.Get(ctx, ev.ID); got.AttendeeNote == nil || *got.AttendeeNote != "" {
		t.Fatalf("null did not clear: %v", got.AttendeeNote)
	}

	// A copy does not take the note (the Wi-Fi changes).
	again := "Wi-Fi: SKYLAB"
	if _, err := svc.Patch(ctx, copyLeader, ev.ID, event.Patch{AttendeeNote: optional.Field[string]{Set: true, Value: again}}); err != nil {
		t.Fatal(err)
	}
	copied, _, err := svc.Copy(ctx, copyLeader, ev.ID, copyIn("Kopya", "2031-03-14T10:00:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := store.Get(ctx, copied.Event.ID); got.AttendeeNote != nil && *got.AttendeeNote != "" {
		t.Fatalf("the copy took the note: %q", *got.AttendeeNote)
	}
}
