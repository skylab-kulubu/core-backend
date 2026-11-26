package handlers

import (
	"context"
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
	"github.com/skylab-kulubu/core-backend/internal/teamaudit"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
)

// walkInService answers WalkIn as told and keeps what it was given.
type walkInService struct {
	ticket.Service
	got   ticket.WalkIn
	actor teamaudit.Actor
	carry bool
	out   ticket.WalkInOutcome
	err   error
}

func (s *walkInService) WalkIn(ctx context.Context, _ authz.Principal, _ uuid.UUID, w ticket.WalkIn) (ticket.WalkInOutcome, error) {
	s.got = w
	s.actor, s.carry = teamaudit.ActorFrom(ctx)
	return s.out, s.err
}

func walkInApp(svc ticket.Service, ident authn.Identity, limits WalkInLimits) *fiber.App {
	app := fiber.New(fiber.Config{ErrorHandler: ErrorHandler})
	app.Use(func(c fiber.Ctx) error {
		c.Locals(authn.LocalsIdentity, ident)
		return c.Next()
	})
	h := limits.Handlers()
	app.Post(WalkInPath, h[0], append(h[1:], NewTicketHandler(svc).WalkIn)...)
	return app
}

func postWalkIn(t *testing.T, app *fiber.App, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(fiber.MethodPost, "/v1/events/"+uuid.NewString()+"/entries/walk-in", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	out := map[string]any{}
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

// The route hands the service every field and the caller (auditContext):
// 201 for a written Ticket, 200 for one already there; the answer names no
// e-mail or phone number.
func TestWalkInRoute(t *testing.T) {
	t.Parallel()
	ident := authn.Identity{ID: uuid.New(), Client: "core-frontend"}
	svc := &walkInService{out: ticket.WalkInOutcome{
		Result: ticket.WalkInCreated, Name: "Ayşe Yılmaz", TicketCode: "K7M2-QX9F", TicketMail: true,
		Entry: &ticket.DoorEntry{ID: uuid.New(), Kind: ticket.KindEntry, EntryDate: "2026-11-27", Source: ticket.EntryWalkIn, Name: "Ayşe Yılmaz", CreatedAt: time.Now()},
	}}
	app := walkInApp(svc, ident, DefaultWalkInLimits())
	status, body := postWalkIn(t, app, `{"firstName":"Ayşe","lastName":"Yılmaz","email":"ayse@example.com","phoneNumber":"05321234567","university":"YTÜ","department":"BM","grade":"2"}`)
	if status != fiber.StatusCreated || body["result"] != "created" || body["ticketCode"] != "K7M2-QX9F" || body["ticketMail"] != true {
		t.Fatalf("%d %v", status, body)
	}
	entry, _ := body["entry"].(map[string]any)
	if entry["source"] != "walk_in" || body["email"] != nil || body["phoneNumber"] != nil {
		t.Fatalf("answer %v", body)
	}
	want := ticket.WalkIn{FirstName: "Ayşe", LastName: "Yılmaz", Email: "ayse@example.com", PhoneNumber: "05321234567", University: "YTÜ", Department: "BM", Grade: "2"}
	if svc.got != want {
		t.Fatalf("service got %+v", svc.got)
	}
	if !svc.carry || svc.actor.ID != ident.ID {
		t.Fatalf("the caller was not handed over for the audit row: %+v %v", svc.actor, svc.carry)
	}

	svc.out = ticket.WalkInOutcome{Result: ticket.WalkInExisting, Name: "Ada Lovelace", TicketCode: "AAAA-BBBB"}
	status, body = postWalkIn(t, app, `{"firstName":"Ada","lastName":"L","email":"ada@example.com"}`)
	if status != fiber.StatusOK || body["result"] != "existing" || body["ticketMail"] != false || body["entry"] != nil {
		t.Fatalf("existing: %d %v", status, body)
	}
}

// The refusals: the audit row 503, the fields 400 with their code, the door
// 403, a full Event 409, a body that is not JSON 400.
func TestWalkInRouteRefusals(t *testing.T) {
	t.Parallel()
	ident := authn.Identity{ID: uuid.New()}
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{teamaudit.ErrUnavailable, fiber.StatusServiceUnavailable, auditUnavailableCode},
		{&ticket.InvalidError{Code: ticket.CodePhoneInvalid}, fiber.StatusBadRequest, "phone_invalid"},
		{ticket.ErrForbidden, fiber.StatusForbidden, ""},
		{&ticket.EventClosedError{Reason: "event_ended"}, fiber.StatusForbidden, "event_closed"},
		{&ticket.EventFullError{WaitlistEnabled: true}, fiber.StatusConflict, "event_full"},
		{&ticket.EntryConflictError{Name: "Ada"}, fiber.StatusConflict, "already_entered"},
		{ticket.ErrTicketWaitlisted, fiber.StatusConflict, "ticket_waitlisted"},
	} {
		app := walkInApp(&walkInService{err: tc.err}, ident, DefaultWalkInLimits())
		status, body := postWalkIn(t, app, `{"firstName":"A","lastName":"B","email":"a@b.co"}`)
		code, _ := body["code"].(string)
		if status != tc.status || code != tc.code {
			t.Errorf("%v: %d %q, want %d %q", tc.err, status, code, tc.status, tc.code)
		}
	}
	app := walkInApp(&walkInService{}, ident, DefaultWalkInLimits())
	if status, body := postWalkIn(t, app, `{"firstName":`); status != fiber.StatusBadRequest || body["code"] != "invalid_body" {
		t.Fatalf("bad body: %d %v", status, body)
	}
}

// Each subject's budget: past it 429 walk_in_rate_limited with the seconds
// to wait.
func TestWalkInRouteBudget(t *testing.T) {
	t.Parallel()
	svc := &walkInService{out: ticket.WalkInOutcome{Result: ticket.WalkInCreated}}
	app := walkInApp(svc, authn.Identity{ID: uuid.New()}, WalkInLimits{PerSubject: 2, PerSubjectWindow: time.Minute, PerSubjectDay: 10})
	for i := range 2 {
		if status, _ := postWalkIn(t, app, `{}`); status != fiber.StatusCreated {
			t.Fatalf("request %d: %d", i, status)
		}
	}
	status, body := postWalkIn(t, app, `{}`)
	if status != fiber.StatusTooManyRequests || body["code"] != "walk_in_rate_limited" || body["retryAfterSeconds"] == nil {
		t.Fatalf("past the budget: %d %v", status, body)
	}
}
