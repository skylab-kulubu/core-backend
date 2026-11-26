package handlers

import (
	"context"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/formsfeed"
	"github.com/skylab-kulubu/core-backend/internal/teamaudit"
)

type stubRespondents struct {
	err   error
	sel   formsfeed.Selection
	query formsfeed.RespondentQuery
	job   uuid.UUID
	// actor is the Team audit actor Send's ctx carried; actorOK whether it
	// carried one.
	actor   teamaudit.Actor
	actorOK bool
}

func (s *stubRespondents) List(_ context.Context, _ authz.Principal, _ uuid.UUID, q formsfeed.RespondentQuery) (formsfeed.RespondentPage, bool, error) {
	s.query = q
	return formsfeed.RespondentPage{Items: []formsfeed.RespondentRow{{ID: uuid.New(), FormsStatus: "pending", Source: "new", Email: "ada@example.com"}}, Total: 1,
		Incomplete: formsfeed.CollectCounts{MissingName: 2}}, true, s.err
}

func (s *stubRespondents) Preview(_ context.Context, _ authz.Principal, _ uuid.UUID, sel formsfeed.Selection) (formsfeed.Preview, error) {
	s.sel = sel
	return formsfeed.Preview{Tickets: 2, Waitlisted: 1}, s.err
}

func (s *stubRespondents) Send(ctx context.Context, _ authz.Principal, _ uuid.UUID, sel formsfeed.Selection) (formsfeed.SendResult, *uuid.UUID, error) {
	s.sel = sel
	s.actor, s.actorOK = teamaudit.ActorFrom(ctx)
	if sel.All {
		return formsfeed.SendResult{}, &s.job, s.err
	}
	return formsfeed.SendResult{Recorded: 1}, nil, s.err
}

func (s *stubRespondents) Job(_ context.Context, _ authz.Principal, _, jobID uuid.UUID) (formsfeed.TicketJob, error) {
	return formsfeed.TicketJob{ID: jobID, State: formsfeed.JobDone}, s.err
}

func respondentApp(stub *stubRespondents) *fiber.App {
	return respondentAppAs(stub, uuid.New())
}

func respondentAppAs(stub *stubRespondents, caller uuid.UUID) *fiber.App {
	app := fiber.New()
	app.Use(func(c fiber.Ctx) error {
		c.Locals(authn.LocalsIdentity, authn.Identity{ID: caller, Groups: []string{"/UYELER/YK"}})
		return c.Next()
	})
	h := NewFormRespondentHandler(stub)
	app.Get("/v1/events/:id/respondents", h.List)
	app.Post("/v1/events/:id/respondents/tickets/preview", h.Preview)
	app.Post("/v1/events/:id/respondents/tickets", h.Send)
	app.Get("/v1/events/:id/respondents/ticket-jobs/:jobId", h.Job)
	return app
}

func TestRespondentRoutesAnswerTheContract(t *testing.T) {
	stub := &stubRespondents{job: uuid.New()}
	app := respondentApp(stub)
	base := "/v1/events/" + uuid.NewString() + "/respondents"
	form := uuid.NewString()

	status, body := mappingCall(t, app, "GET", base+"?formId="+form+"&formsStatus=pending&limit=10", "")
	if status != fiber.StatusOK || body["total"] != float64(1) || body["collecting"] != true || body["nextCursor"] != nil ||
		body["incomplete"].(map[string]any)["missingName"] != float64(2) {
		t.Fatalf("list %d %v", status, body)
	}
	if stub.query.FormID == nil || stub.query.FormID.String() != form || stub.query.FormsStatus != "pending" || stub.query.Limit != 10 {
		t.Fatalf("query %+v", stub.query)
	}
	items := body["items"].([]any)
	if item := items[0].(map[string]any); item["formsStatus"] != "pending" || item["email"] != "ada@example.com" {
		t.Fatalf("item %v", item)
	}
	if status, body := mappingCall(t, app, "GET", base+"?limit=500", ""); status != fiber.StatusBadRequest || body["code"] != "respondents_invalid" {
		t.Fatalf("limit %d %v", status, body)
	}

	status, body = mappingCall(t, app, "POST", base+"/tickets/preview", `{"all":true,"formId":"`+form+`"}`)
	if status != fiber.StatusOK || body["tickets"] != float64(2) || body["waitlisted"] != float64(1) || body["pendingSkipped"] != float64(0) {
		t.Fatalf("preview %d %v", status, body)
	}
	if !stub.sel.All || stub.sel.FormID == nil {
		t.Fatalf("selection %+v", stub.sel)
	}

	id := uuid.NewString()
	status, body = mappingCall(t, app, "POST", base+"/tickets", `{"ids":["`+id+`"],"formId":"`+form+`"}`)
	if status != fiber.StatusOK || body["recorded"] != float64(1) || stub.sel.FormID != nil {
		t.Fatalf("send %d %v %+v", status, body, stub.sel)
	}
	status, body = mappingCall(t, app, "POST", base+"/tickets", `{"all":true}`)
	if status != fiber.StatusAccepted || body["jobId"] != stub.job.String() {
		t.Fatalf("send all %d %v", status, body)
	}
	status, body = mappingCall(t, app, "GET", base+"/ticket-jobs/"+stub.job.String(), "")
	if status != fiber.StatusOK || body["state"] != "done" || body["jobId"] != stub.job.String() {
		t.Fatalf("job %d %v", status, body)
	}
	if status, _ := mappingCall(t, app, "POST", base+"/tickets", `not json`); status != fiber.StatusBadRequest {
		t.Fatalf("bad body %d", status)
	}

	for err, want := range map[error][2]any{
		formsfeed.ErrRespondentsInvalid:        {fiber.StatusBadRequest, "respondents_invalid"},
		formsfeed.ErrRespondentsEventCancelled: {fiber.StatusConflict, "event_cancelled"},
		formsfeed.ErrRespondentsEventEnded:     {fiber.StatusConflict, "event_ended"},
		formsfeed.ErrTicketJobNotFound:         {fiber.StatusNotFound, "ticket_job_not_found"},
		formsfeed.ErrMappingForbidden:          {fiber.StatusForbidden, nil},
		formsfeed.ErrMappingEventNotFound:      {fiber.StatusNotFound, nil},
		teamaudit.ErrUnavailable:               {fiber.StatusServiceUnavailable, auditUnavailableCode},
	} {
		stub.err = err
		status, body := mappingCall(t, app, "POST", base+"/tickets", `{"ids":["`+id+`"]}`)
		if status != want[0] || body["code"] != want[1] {
			t.Errorf("%v: %d %v", err, status, body)
		}
	}
}

// The sending opens a Team audit row (ticket.import), whose actor is the
// caller: the route hands the service a ctx that carries them, as every
// audited write. Without one the service refuses it 503 audit_unavailable
// before anything is written.
func TestRespondentSendCarriesTheAuditActor(t *testing.T) {
	caller := uuid.New()
	base := "/v1/events/" + uuid.NewString() + "/respondents/tickets"
	for _, body := range []string{`{"ids":["` + uuid.NewString() + `"]}`, `{"all":true}`} {
		stub := &stubRespondents{job: uuid.New()}
		if status, got := mappingCall(t, respondentAppAs(stub, caller), "POST", base, body); status >= 300 {
			t.Fatalf("%s: %d %v", body, status, got)
		}
		if !stub.actorOK || stub.actor.ID != caller {
			t.Fatalf("%s: actor %+v (carried %v), want %s", body, stub.actor, stub.actorOK, caller)
		}
	}
}
