package event_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/optional"
)

// The ticket note (docs/ticket-note.md): written trimmed with its line
// breaks, at most 500 characters, no control characters; it is read by who
// reads the attendee note and by no public answer; a write without it keeps
// it, null clears it; a copy does not take it.
func TestTicketNote(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := event.NewMemoryStore()
	svc := event.NewService(store, authz.NewAuthorizer(authz.DefaultPolicy()))
	note := "  WhatsApp grubu:\r\nhttps://chat.whatsapp.com/Ornek123  "
	ev, err := svc.Create(ctx, copyLeader, event.Event{Name: "Buluşma", Location: "YTÜ", OwnerTeam: "WEBLAB", TicketNote: &note})
	if err != nil {
		t.Fatal(err)
	}
	stored, err := store.Get(ctx, ev.ID)
	if err != nil || stored.TicketNote == nil || *stored.TicketNote != "WhatsApp grubu:\nhttps://chat.whatsapp.com/Ornek123" {
		t.Fatalf("stored %v %v", stored.TicketNote, err)
	}
	for name, p := range map[string]authz.Principal{"leader": copyLeader, "YK": copyYK, "DK": copyDK} {
		seen := svc.ProjectFor(&p, stored)
		if seen.TicketNoteView == nil || *seen.TicketNoteView != *stored.TicketNote {
			t.Fatalf("%s reads %v", name, seen.TicketNoteView)
		}
	}
	if _, err := svc.Patch(ctx, copyDK, ev.ID, event.Patch{TicketNote: optional.Field[string]{Set: true, Value: "x"}}); !errors.Is(err, event.ErrForbidden) {
		t.Fatalf("DK wrote the note: %v", err)
	}
	for name, p := range map[string]*authz.Principal{"anonymous": nil, "member": &copyMember} {
		raw, _ := json.Marshal(svc.ProjectFor(p, stored))
		if strings.Contains(string(raw), "ticketNote") || strings.Contains(string(raw), "Ornek123") {
			t.Fatalf("%s reads the note: %s", name, raw)
		}
	}
	if raw, _ := json.Marshal(stored); strings.Contains(string(raw), "Ornek123") {
		t.Fatalf("the Event JSON carries the note: %s", raw)
	}

	// A patch without it keeps it (and leaves the attendee note alone); null clears it.
	if _, err := svc.Patch(ctx, copyLeader, ev.ID, event.Patch{Name: optional.Field[string]{Set: true, Value: "Buluşma 2"}}); err != nil {
		t.Fatal(err)
	}
	if got, _ := store.Get(ctx, ev.ID); got.TicketNote == nil || *got.TicketNote == "" {
		t.Fatal("a patch without the note cleared it")
	}
	for name, bad := range map[string]string{
		"501 characters": strings.Repeat("ş", 501),
		"NUL":            "WhatsApp\x00",
		"RLO":            "https://example.test/‮gnp.exe",
		"zero width":     "Whats​App",
	} {
		_, err := svc.Patch(ctx, copyLeader, ev.ID, event.Patch{TicketNote: optional.Field[string]{Set: true, Value: bad}})
		if !errors.Is(err, event.ErrTicketNoteInvalid) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if _, err := svc.Patch(ctx, copyLeader, ev.ID, event.Patch{TicketNote: optional.Field[string]{Set: true, Value: strings.Repeat("ş", 500)}}); err != nil {
		t.Fatalf("500 characters: %v", err)
	}
	if _, err := svc.Patch(ctx, copyLeader, ev.ID, event.Patch{TicketNote: optional.Field[string]{Null: true}}); err != nil {
		t.Fatal(err)
	}
	if got, _ := store.Get(ctx, ev.ID); got.TicketNote == nil || *got.TicketNote != "" {
		t.Fatalf("null did not clear: %v", got.TicketNote)
	}

	// A copy does not take it (the next event has its own group).
	again := "https://chat.whatsapp.com/Ornek123"
	if _, err := svc.Patch(ctx, copyLeader, ev.ID, event.Patch{TicketNote: optional.Field[string]{Set: true, Value: again}}); err != nil {
		t.Fatal(err)
	}
	copied, _, err := svc.Copy(ctx, copyLeader, ev.ID, copyIn("Kopya", "2031-03-14T10:00:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := store.Get(ctx, copied.Event.ID); got.TicketNote == nil || *got.TicketNote != "" {
		t.Fatalf("the copy took the note: %v", got.TicketNote)
	}
}

// TicketNoteParts links https addresses alone, without their trailing
// punctuation; http and a bare scheme stay text.
func TestTicketNoteParts(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		note string
		want []event.NotePart
	}{
		"none":  {"", []event.NotePart{}},
		"text":  {"Salon D-001", []event.NotePart{{Text: "Salon D-001"}}},
		"https": {"Grup: https://chat.whatsapp.com/Ab1.", []event.NotePart{{Text: "Grup: "}, {Text: "https://chat.whatsapp.com/Ab1", URL: "https://chat.whatsapp.com/Ab1"}, {Text: "."}}},
		"two": {"https://a.example/x\n(https://b.example/y)", []event.NotePart{
			{Text: "https://a.example/x", URL: "https://a.example/x"}, {Text: "\n("}, {Text: "https://b.example/y", URL: "https://b.example/y"}, {Text: ")"},
		}},
		"http":              {"http://example.test/x", []event.NotePart{{Text: "http://example.test/x"}}},
		"bare scheme":       {"https://.", []event.NotePart{{Text: "https://"}, {Text: "."}}},
		"javascript":        {"javascript:alert(1)", []event.NotePart{{Text: "javascript:alert(1)"}}},
		"quote breaks":      {`https://x.example/"onmouseover=`, []event.NotePart{{Text: "https://x.example/", URL: "https://x.example/"}, {Text: `"onmouseover=`}}},
		"no-break space":    {"https://a.example/x\u00a0Grup", []event.NotePart{{Text: "https://a.example/x", URL: "https://a.example/x"}, {Text: "\u00a0Grup"}}},
		"ideographic space": {"https://a.example/x\u3000y", []event.NotePart{{Text: "https://a.example/x", URL: "https://a.example/x"}, {Text: "\u3000y"}}},
	} {
		note := tc.note
		got, parts := event.TicketNoteParts(event.Event{TicketNote: &note})
		if got != tc.note || parts == nil || !slices.Equal(parts, tc.want) {
			t.Errorf("%s: %q %#v", name, got, parts)
		}
	}
	if note, parts := event.TicketNoteParts(event.Event{}); note != "" || parts == nil || len(parts) != 0 {
		t.Errorf("nil note: %q %#v", note, parts)
	}
}
