package event_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
)

// The ticket note lands in its own column next to the attendee note on
// create and on update, a write without it keeps it, and the database
// refuses one of 501 characters (docs/ticket-note.md).
func TestPostgresWritesTheTicketNote(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := event.NewPostgresStore(pool)
	svc := event.NewService(store, authz.NewAuthorizer(authz.DefaultPolicy()))
	yk := authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/YK"}}
	attendee, ticket := "Wi-Fi: SKYLAB", "https://chat.whatsapp.com/Ornek123"
	created, err := svc.Create(ctx, yk, event.Event{
		Name: "Gecekodu", Location: "YTÜ", OwnerTeam: "WEBLAB", StartDate: stamp("2031-03-14T10:00:00Z"),
		AttendeeNote: &attendee, TicketNote: &ticket,
	})
	if err != nil {
		t.Fatal(err)
	}
	check := func(when, attendee, ticket string) {
		t.Helper()
		got, err := store.Get(ctx, created.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.AttendeeNote == nil || *got.AttendeeNote != attendee || got.TicketNote == nil || *got.TicketNote != ticket {
			t.Fatalf("%s: attendee %v ticket %v", when, got.AttendeeNote, got.TicketNote)
		}
	}
	check("create", attendee, ticket)
	if _, err := svc.Patch(ctx, yk, created.ID, patchOf(t, `{"ticketNote":"Grup: https://chat.whatsapp.com/Yeni"}`)); err != nil {
		t.Fatal(err)
	}
	check("update", attendee, "Grup: https://chat.whatsapp.com/Yeni")
	if _, err := svc.Patch(ctx, yk, created.ID, patchOf(t, `{"name":"Gecekodu 2","attendeeNote":"Salon D-001"}`)); err != nil {
		t.Fatal(err)
	}
	check("update without it", "Salon D-001", "Grup: https://chat.whatsapp.com/Yeni")
	if _, err := svc.Patch(ctx, yk, created.ID, patchOf(t, `{"ticketNote":null}`)); err != nil {
		t.Fatal(err)
	}
	check("cleared", "Salon D-001", "")
	if _, err := pool.Exec(ctx, `UPDATE events SET ticket_note = repeat('a', 501) WHERE id = $1`, created.ID); err == nil {
		t.Error("a note of 501 characters was taken")
	}
}
