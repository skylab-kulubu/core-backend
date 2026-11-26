package handlers

import (
	"context"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/certificate"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/raffle"
	"github.com/skylab-kulubu/core-backend/internal/teamaudit"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
)

type raffleEvent struct{ ev event.Event }

func (r raffleEvent) Get(_ context.Context, id uuid.UUID) (event.Event, error) {
	if id != r.ev.ID {
		return event.Event{}, event.ErrNotFound
	}
	return r.ev, nil
}

type raffleAttendance struct{}

func (raffleAttendance) EventAttendance(context.Context, uuid.UUID) (certificate.Attendance, error) {
	return certificate.Attendance{}, nil
}

// raffleApp is the raffle routes with the Team audit record on, as in
// production, called by caller, who is also on the Event's door staff.
func raffleApp(t *testing.T, caller authn.Identity) (*fiber.App, *teamaudit.Memory, event.Event) {
	t.Helper()
	dir := identity.NewMemory()
	dir.PutGroup(identity.Group{ID: uuid.NewString(), Name: "WEBLAB", Path: "/UYELER/ARGE/WEBLAB"})
	audit := teamaudit.NewMemory()
	az := authz.NewAuthorizer(authz.DefaultPolicy())
	ev := event.Event{ID: uuid.New(), Name: "ARTLAB", OwnerTeam: "WEBLAB", Status: event.StatusPublished, DoorStaffIDs: []uuid.UUID{caller.ID}}
	store := raffle.NewMemory()
	var cands []raffle.Candidate
	for _, name := range []string{"Ada Lovelace", "Alan Turing", "Grace Hopper"} {
		owner := uuid.New()
		cands = append(cands, raffle.Candidate{TicketID: uuid.New(), TicketType: raffle.TypeRegistered, OwnerID: &owner, Name: name,
			Email: strings.ToLower(strings.Fields(name)[0]) + "@example.com"})
	}
	store.SetCandidates(ev.ID, cands)
	auditor, _ := ticket.NewService(ticket.NewMemoryStore(), event.NewMemoryStore(), az, dir, audit).(ticket.EventAuditor)
	svc := &raffle.Service{Store: store, Events: raffleEvent{ev}, Authz: az, Attendance: raffleAttendance{}, Audit: auditor}
	h := NewRaffleHandler(svc)
	app := fiber.New()
	app.Use(func(c fiber.Ctx) error {
		c.Locals(authn.LocalsIdentity, caller)
		return c.Next()
	})
	app.Post("/v1/events/:id/raffles/preview", h.Preview)
	app.Get("/v1/events/:id/raffles", h.List)
	app.Post("/v1/events/:id/raffles", h.Draw)
	app.Get("/v1/events/:id/raffles/:raffleId", h.Get)
	app.Get("/v1/events/:id/raffles/:raffleId/verification", h.Verification)
	app.Post("/v1/events/:id/raffles/:raffleId/forfeits", h.Forfeit)
	return app, audit, ev
}

// The draw and the forfeit open their Team audit rows under the caller:
// both routes hand the service a ctx that carries them (auditContext), so
// production's record answers 201/200, not 503 audit_unavailable.
func TestRaffleRoutesRecordTheCaller(t *testing.T) {
	lead := uuid.New()
	app, audit, ev := raffleApp(t, authn.Identity{ID: lead, Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}})
	base := "/v1/events/" + ev.ID.String() + "/raffles"

	status, body := mappingCall(t, app, "POST", base+"/preview", `{"criteria":{}}`)
	if status != 200 || body["eligible"] != float64(3) {
		t.Fatalf("preview: %d %v", status, body)
	}
	status, body = mappingCall(t, app, "POST", base, `{"title":"Kapanış","criteria":{},"winners":1,"alternates":1}`)
	if status != 201 || body["algorithm"] != raffle.Algorithm {
		t.Fatalf("draw: %d %v", status, body)
	}
	id, _ := body["id"].(string)
	entries, _ := body["entries"].([]any)
	if len(entries) != 2 {
		t.Fatalf("entries %v", body["entries"])
	}
	first, _ := entries[0].(map[string]any)
	if mail, _ := first["maskedEmail"].(string); !strings.Contains(mail, "***@e***.com") || first["email"] != nil {
		t.Fatalf("entry %v", first)
	}
	status, body = mappingCall(t, app, "GET", base+"/"+id+"/verification", "")
	if ids, _ := body["ticketIds"].([]any); status != 200 || len(ids) != 3 {
		t.Fatalf("verification: %d %v", status, body)
	}
	status, body = mappingCall(t, app, "POST", base+"/"+id+"/forfeits", `{"position":1,"reason":"Salonda değildi"}`)
	if status != 200 {
		t.Fatalf("forfeit: %d %v", status, body)
	}
	status, body = mappingCall(t, app, "POST", base+"/"+id+"/forfeits", `{"position":2,"reason":"Salonda değildi"}`)
	if status != 409 || body["code"] != "raffle_no_alternates" {
		t.Fatalf("no alternates: %d %v", status, body)
	}
	status, body = mappingCall(t, app, "GET", base, "")
	if items, _ := body["items"].([]any); status != 200 || len(items) != 1 {
		t.Fatalf("list: %d %v", status, body)
	}
	rows := audit.Rows()
	want := []teamaudit.Action{teamaudit.ActionRaffleDraw, teamaudit.ActionRaffleForfeit}
	if len(rows) != len(want) {
		t.Fatalf("%d audit rows", len(rows))
	}
	for i, r := range rows {
		if r.Action != want[i] || r.Actor.ID != lead {
			t.Fatalf("row %d: %s by %s", i, r.Action, r.Actor.ID)
		}
	}
}

func TestRaffleRoutesProblems(t *testing.T) {
	lead := uuid.New()
	app, _, ev := raffleApp(t, authn.Identity{ID: lead, Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}})
	base := "/v1/events/" + ev.ID.String() + "/raffles"
	for _, tc := range []struct {
		method, path, body string
		status             int
		code               string
	}{
		{"POST", base, `{"criteria":{},"winners":4}`, 409, "raffle_too_few_candidates"},
		{"POST", base, `{"criteria":{},"winners":0}`, 400, "raffle_invalid"},
		{"POST", base, `not json`, 400, "invalid_body"},
		{"GET", base + "/" + uuid.NewString(), "", 404, "raffle_not_found"},
		{"GET", base + "/nope", "", 400, "invalid_id"},
		{"GET", "/v1/events/" + uuid.NewString() + "/raffles", "", 404, ""},
	} {
		status, body := mappingCall(t, app, tc.method, tc.path, tc.body)
		if status != tc.status || (tc.code != "" && body["code"] != tc.code) {
			t.Errorf("%s %s %s: %d %v", tc.method, tc.path, tc.body, status, body)
		}
	}
	// The door staff take check-ins; they do not draw.
	door, _, ev2 := raffleApp(t, authn.Identity{ID: uuid.New(), Groups: []string{"/UYELER"}})
	status, body := mappingCall(t, door, "GET", "/v1/events/"+ev2.ID.String()+"/raffles", "")
	if status != 403 {
		t.Fatalf("door staff: %d %v", status, body)
	}
}
