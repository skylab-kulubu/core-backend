package handlers

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
)

func scanBody(token string, at time.Time) string {
	return `{"token":` + jsonString(token) + `,"scannedAt":` + jsonString(at.Format(time.RFC3339Nano)) + `}`
}

// The offline door over HTTP (docs/offline-door.md): a scan sent later
// with scannedAt is written (201) and answers it; the same scan sent again,
// or another door's of the same Ticket, is 200 duplicate, so a queue sent
// twice ends as one record; without scannedAt the second is 409 as before.
// A scannedAt that is not a time is 400, one ahead of the clock or too old
// 422.
func TestOfflineDoorRoutes(t *testing.T) {
	t.Parallel()
	d := newTicketQRDoor(t)
	app := d.eventDoorApp(d.staff)
	entries := "/v1/events/" + d.ev.ID.String() + "/entries/scan"
	at := time.Now().Add(-15 * time.Minute).UTC().Truncate(time.Second)
	token := d.qr(d.guest, d.ev)

	got := d.call(app, fiber.MethodPost, entries, scanBody(token, at))
	if got.status != fiber.StatusCreated || !strings.Contains(got.body, `"scannedAt":"`+at.Format(time.RFC3339)+`"`) {
		t.Fatalf("offline entry: %+v", got)
	}
	again := d.call(app, fiber.MethodPost, entries, scanBody(token, at.Add(time.Minute)))
	var dup map[string]any
	if err := json.Unmarshal([]byte(again.body), &dup); err != nil {
		t.Fatal(err)
	}
	if again.status != fiber.StatusOK || dup["duplicate"] != true || dup["kind"] != "entry" || dup["name"] != "Elif Yılmaz" {
		t.Fatalf("offline again: %+v", again)
	}
	if online := d.call(app, fiber.MethodPost, entries, `{"token":`+jsonString(token)+`}`); online.status != fiber.StatusConflict || online.code != "already_entered" {
		t.Fatalf("online again: %+v", online)
	}

	session := "/v1/sessions/" + d.second.ID.String() + "/check-in/scan"
	if got := d.call(app, fiber.MethodPost, session, scanBody(token, at)); got.status != fiber.StatusCreated || !strings.Contains(got.body, `"kind":"check_in"`) ||
		!strings.Contains(got.body, `"scannedAt"`) {
		t.Fatalf("offline check-in: %+v", got)
	}
	if got := d.call(app, fiber.MethodPost, session, scanBody(token, at)); got.status != fiber.StatusOK ||
		!strings.Contains(got.body, `"duplicate":true`) || !strings.Contains(got.body, `"kind":"check_in"`) {
		t.Fatalf("offline check-in again: %+v", got)
	}

	for body, want := range map[string]scanAnswer{
		`{"token":` + jsonString(token) + `,"scannedAt":"dün akşam"}`: {status: fiber.StatusBadRequest, code: "scanned_at_invalid"},
		scanBody(token, time.Now().Add(time.Hour)):                    {status: fiber.StatusUnprocessableEntity, code: "scanned_at_in_future"},
		scanBody(token, time.Now().Add(-73*time.Hour)):                {status: fiber.StatusUnprocessableEntity, code: "scanned_at_too_old"},
	} {
		if got := d.call(app, fiber.MethodPost, entries, body); got.status != want.status || got.code != want.code {
			t.Fatalf("%s: %+v, want %d %s", body, got, want.status, want.code)
		}
	}
}

// An Event that ended over a day ago: its door is closed to a scan now,
// and open to one taken while it ran, sent later; that entry's day is the
// Event's, not today. A scan taken after the window is refused as now.
func TestOfflineScanOfAnEndedEvent(t *testing.T) {
	t.Parallel()
	d := newTicketQRDoor(t)
	app := d.eventDoorApp(d.staff)
	start := time.Now().Add(-40 * time.Hour)
	end := time.Now().Add(-30 * time.Hour)
	ev, err := d.events.Create(t.Context(), event.Event{Name: "Gece", Location: "YTÜ", OwnerTeam: "AILAB", StartDate: &start, EndDate: &end,
		DoorStaffIDs: []uuid.UUID{d.staff.ID}})
	if err != nil {
		t.Fatal(err)
	}
	guest, err := d.tickets.Create(t.Context(), ticket.Ticket{EventID: ev.ID, TicketType: ticket.Guest, GuestFirstName: "Ada", GuestLastName: "Lovelace", GuestEmail: "ada@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	token := d.qr(guest, ev)
	path := "/v1/events/" + ev.ID.String() + "/entries/scan"

	if got := d.call(app, fiber.MethodPost, path, `{"token":`+jsonString(token)+`}`); got.status != fiber.StatusForbidden || got.code != "event_closed" {
		t.Fatalf("now: %+v", got)
	}
	taken := time.Now().Add(-31 * time.Hour)
	got := d.call(app, fiber.MethodPost, path, scanBody(token, taken))
	if got.status != fiber.StatusCreated || !strings.Contains(got.body, `"entryDate":"`+ticket.IstanbulDate(start)+`"`) {
		t.Fatalf("taken while it ran: %+v", got)
	}
	if got := d.call(app, fiber.MethodPost, path, scanBody(token, time.Now().Add(-2*time.Hour))); got.status != fiber.StatusForbidden || got.code != "event_closed" {
		t.Fatalf("taken after the window: %+v", got)
	}
}
