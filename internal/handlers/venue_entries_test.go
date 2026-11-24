package handlers

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
)

// eventDoorApp serves the Session scan and the Event's door routes.
func (d *ticketQRDoor) eventDoorApp(ident authn.Identity) *fiber.App {
	pass := NewSkyPassHandler(d.pass, d.svc)
	tickets := NewTicketHandler(d.svc)
	app := fiber.New(fiber.Config{ErrorHandler: ErrorHandler})
	app.Use(func(c fiber.Ctx) error {
		c.Locals(authn.LocalsIdentity, ident)
		return c.Next()
	})
	app.Post("/v1/sessions/:sessionId/check-in/scan", pass.CheckInSession)
	app.Post("/v1/events/:eventId/entries/scan", pass.EnterEvent)
	app.Post("/v1/events/:eventId/entries/resolve", tickets.ResolveEntry)
	app.Get("/v1/events/:eventId/entries/summary", tickets.EntrySummary)
	app.Get("/v1/events/:eventId/entries", tickets.ListEntries)
	app.Delete("/v1/events/:eventId/entries/:entryId", tickets.UndoEntry)
	return app
}

func (d *ticketQRDoor) call(app *fiber.App, method, path, body string) scanAnswer {
	d.t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := app.Test(req)
	if err != nil {
		d.t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	var problem struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(raw, &problem)
	return scanAnswer{status: resp.StatusCode, code: problem.Code, body: string(raw)}
}

// The Event's door over HTTP: a Ticket QR and a SkyPass write today's
// entry (201, kind entry, the name, no Ticket id); a second the same day is
// 409 already_entered with the name; the desk, the day's list, the
// summary and the undo; another Event's Ticket QR is 404.
func TestEventDoorRoutes(t *testing.T) {
	t.Parallel()
	d := newTicketQRDoor(t)
	app := d.eventDoorApp(d.staff)
	base := "/v1/events/" + d.ev.ID.String() + "/entries"

	got := d.call(app, fiber.MethodPost, base+"/scan", `{"token":`+jsonString(d.qr(d.guest, d.ev))+`}`)
	if got.status != fiber.StatusCreated {
		t.Fatalf("scan: %+v", got)
	}
	var entry map[string]any
	if err := json.Unmarshal([]byte(got.body), &entry); err != nil {
		t.Fatal(err)
	}
	if entry["kind"] != "entry" || entry["name"] != "Elif Yılmaz" || entry["source"] != "scan" ||
		entry["entryDate"] != ticket.IstanbulDate(time.Now()) || entry["ticketId"] != nil {
		t.Fatalf("entry %s", got.body)
	}
	again := d.call(app, fiber.MethodPost, base+"/scan", `{"token":`+jsonString(d.qr(d.guest, d.ev))+`}`)
	if again.status != fiber.StatusConflict || again.code != "already_entered" || !strings.Contains(again.body, `"name":"Elif Yılmaz"`) {
		t.Fatalf("again: %+v", again)
	}
	pass, err := d.pass.Mint(t.Context(), authz.Principal{ID: d.memberID.String()})
	if err != nil {
		t.Fatal(err)
	}
	if got := d.call(app, fiber.MethodPost, base+"/scan", `{"token":`+jsonString(pass.Value)+`}`); got.status != fiber.StatusCreated ||
		!strings.Contains(got.body, `"name":"Grace Hopper"`) {
		t.Fatalf("SkyPass: %+v", got)
	}
	other, err := d.events.Create(t.Context(), event.Event{Name: "Başka", Location: "YTÜ", OwnerTeam: "AILAB", DoorStaffIDs: []uuid.UUID{d.staff.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if got := d.call(app, fiber.MethodPost, "/v1/events/"+other.ID.String()+"/entries/scan", `{"token":`+jsonString(d.qr(d.guest, d.ev))+`}`); got.status != fiber.StatusNotFound {
		t.Fatalf("another Event's code: %+v", got)
	}

	list := d.call(app, fiber.MethodGet, base, "")
	if list.status != fiber.StatusOK || strings.Count(list.body, `"kind":"entry"`) != 2 || strings.Contains(list.body, "@example.com") {
		t.Fatalf("list: %+v", list)
	}
	summary := d.call(app, fiber.MethodGet, base+"/summary", "")
	if summary.status != fiber.StatusOK || !strings.Contains(summary.body, `"total":2`) || !strings.Contains(summary.body, `"scan":2`) {
		t.Fatalf("summary: %+v", summary)
	}
	id, _ := entry["id"].(string)
	if got := d.call(app, fiber.MethodDelete, base+"/"+id, ""); got.status != fiber.StatusNoContent {
		t.Fatalf("undo: %+v", got)
	}
	if got := d.call(app, fiber.MethodDelete, base+"/"+id, ""); got.status != fiber.StatusNotFound {
		t.Fatalf("undo again: %+v", got)
	}
	// The desk by e-mail, now the entry is gone.
	if got := d.call(app, fiber.MethodPost, base+"/resolve", `{"query":"elif@example.com"}`); got.status != fiber.StatusCreated ||
		!strings.Contains(got.body, `"source":"desk"`) {
		t.Fatalf("desk: %+v", got)
	}
	if got := d.call(app, fiber.MethodGet, base+"?date=bugün", ""); got.status != fiber.StatusBadRequest {
		t.Fatalf("bad date: %+v", got)
	}
}

// Someone who may not use the door is 403 before the code is read; a
// draft is 403 event_closed with its reason.
func TestEventDoorRefusals(t *testing.T) {
	t.Parallel()
	d := newTicketQRDoor(t)
	base := "/v1/events/" + d.ev.ID.String() + "/entries"
	stranger := authn.Identity{ID: uuid.New(), Groups: []string{"/UYELER/ARGE/GAMELAB"}}
	if got := d.call(d.eventDoorApp(stranger), fiber.MethodPost, base+"/scan", `{"token":"junk"}`); got.status != fiber.StatusForbidden {
		t.Fatalf("stranger: %+v", got)
	}
	if got := d.call(d.eventDoorApp(stranger), fiber.MethodGet, base+"/summary", ""); got.status != fiber.StatusForbidden {
		t.Fatalf("stranger summary: %+v", got)
	}
	lead := authn.Identity{ID: uuid.New(), Groups: []string{"/UYELER/ARGE/AILAB/LIDERLER"}}
	draft, err := d.events.Create(t.Context(), event.Event{Name: "Taslak", Location: "YTÜ", OwnerTeam: "AILAB", Status: event.StatusDraft})
	if err != nil {
		t.Fatal(err)
	}
	got := d.call(d.eventDoorApp(lead), fiber.MethodPost, "/v1/events/"+draft.ID.String()+"/entries/scan", `{"token":"junk"}`)
	if got.status != fiber.StatusForbidden || got.code != "event_closed" || !strings.Contains(got.body, `"reason":"event_not_published"`) {
		t.Fatalf("draft: %+v", got)
	}
}

// A Session scan in the venue mode answers 201 kind entry with the scan
// answer's fields; a second that day is 409.
func TestSessionScanInVenueMode(t *testing.T) {
	t.Parallel()
	d := newTicketQRDoor(t)
	venue := event.EntryVenue
	ev := d.ev
	ev.EntryMode = &venue
	updated, err := d.events.Update(t.Context(), ev)
	if err != nil {
		t.Fatal(err)
	}
	app := d.eventDoorApp(d.staff)
	got := d.scan(app, "scan", d.sess.ID, d.qr(d.guest, updated))
	if got.status != fiber.StatusCreated {
		t.Fatalf("scan: %+v", got)
	}
	var ci ticket.CheckIn
	if err := json.Unmarshal([]byte(got.body), &ci); err != nil || ci.Kind != "entry" || ci.SessionID != d.sess.ID || ci.TicketID != d.guest.ID {
		t.Fatalf("answer %s", got.body)
	}
	if again := d.scan(app, "scan", d.second.ID, d.qr(d.guest, updated)); again.status != fiber.StatusConflict {
		t.Fatalf("again: %+v", again)
	}
}
