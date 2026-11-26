package handlers

import (
	"errors"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/limiter"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/teamaudit"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
)

// Walk-in at the door ("Kapıda kayıt", docs/walk-in.md).

// WalkInPath is the route: POST /v1/events/{eventId}/entries/walk-in.
const WalkInPath = "/v1/events/:eventId/entries/walk-in"

// WalkInLimits are the route's budgets for each signed-in subject: wide
// enough for a busy door registering people by hand, narrow enough that
// one token cannot write guests without end. The daily one counts the
// answered requests only.
type WalkInLimits struct {
	PerSubject       int
	PerSubjectWindow time.Duration
	PerSubjectDay    int
}

// DefaultWalkInLimits are the production budgets: 60 in ten minutes, 300
// answered a day, as Guest apply's.
func DefaultWalkInLimits() WalkInLimits {
	return WalkInLimits{PerSubject: 60, PerSubjectWindow: 10 * time.Minute, PerSubjectDay: 300}
}

// Handlers are the route's limiters, in order: the ten-minute one, then the
// daily ceiling.
func (l WalkInLimits) Handlers() []any {
	subject := func(c fiber.Ctx) string {
		ident, _ := c.Locals(authn.LocalsIdentity).(authn.Identity)
		return ident.ID.String()
	}
	signedOut := func(c fiber.Ctx) bool {
		_, ok := c.Locals(authn.LocalsIdentity).(authn.Identity)
		return !ok
	}
	return []any{
		limiter.New(limiter.Config{
			Max: l.PerSubject, Expiration: l.PerSubjectWindow,
			Next: signedOut, KeyGenerator: subject, LimitReached: doorQRRateLimited("walk_in_rate_limited"),
		}),
		limiter.New(limiter.Config{
			Max: l.PerSubjectDay, Expiration: 24 * time.Hour, SkipFailedRequests: true,
			Next: signedOut, KeyGenerator: subject, LimitReached: doorQRRateLimited("walk_in_rate_limited"),
		}),
	}
}

// walkInBody is the door's form: the name and the e-mail, required; a
// phone number, a university, a department and a grade, optional.
type walkInBody struct {
	FirstName   string `json:"firstName"`
	LastName    string `json:"lastName"`
	Email       string `json:"email"`
	PhoneNumber string `json:"phoneNumber"`
	University  string `json:"university"`
	Department  string `json:"department"`
	Grade       string `json:"grade"`
}

// WalkIn is POST /v1/events/:eventId/entries/walk-in: the door registers a
// person with no Ticket; 201 with a new Ticket, 200 when their Ticket was
// already there. The caller rides in ctx for the Team audit row.
func (h *TicketHandler) WalkIn(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return ticketError(c, err)
	}
	eventID, err := uuid.Parse(c.Params("eventId"))
	if err != nil {
		return badID(c, "eventId")
	}
	var body walkInBody
	if err := c.Bind().Body(&body); err != nil {
		return problemCode(c, fiber.StatusBadRequest, "Bad Request", "invalid_body")
	}
	out, err := h.svc.WalkIn(auditContext(c, p), p, eventID, ticket.WalkIn{
		FirstName: body.FirstName, LastName: body.LastName, Email: body.Email,
		PhoneNumber: body.PhoneNumber, University: body.University, Department: body.Department, Grade: body.Grade,
	})
	if err != nil {
		if errors.Is(err, teamaudit.ErrUnavailable) {
			return problemCode(c, fiber.StatusServiceUnavailable, "Service Unavailable", auditUnavailableCode)
		}
		return ticketError(c, err)
	}
	c.Set(fiber.HeaderCacheControl, "no-store")
	status := fiber.StatusOK
	if out.Result == ticket.WalkInCreated {
		status = fiber.StatusCreated
	}
	return c.Status(status).JSON(out)
}
