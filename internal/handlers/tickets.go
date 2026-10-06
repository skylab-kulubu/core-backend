package handlers

import (
	"errors"
	"strings"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/qr"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
)

type TicketHandler struct {
	svc ticket.Service
}

func NewTicketHandler(svc ticket.Service) *TicketHandler {
	return &TicketHandler{svc: svc}
}

type guestBody struct {
	FirstName   string `json:"firstName"`
	LastName    string `json:"lastName"`
	Email       string `json:"email"`
	PhoneNumber string `json:"phoneNumber"`
}

func ticketError(c fiber.Ctx, err error) error {
	switch {
	case errors.Is(err, fiber.ErrUnauthorized):
		return problem(c, fiber.StatusUnauthorized, "Unauthorized")
	case errors.Is(err, ticket.ErrForbidden):
		return problem(c, fiber.StatusForbidden, "Forbidden")
	case errors.Is(err, ticket.ErrNotFound):
		return problem(c, fiber.StatusNotFound, "Not Found")
	case errors.Is(err, ticket.ErrConflict):
		return problem(c, fiber.StatusConflict, "Conflict")
	case errors.Is(err, ticket.ErrAmbiguous):
		return problem(c, fiber.StatusConflict, "Ambiguous Match")
	case errors.Is(err, ticket.ErrInvalid):
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	case errors.Is(err, ticket.ErrDoorQRRequired):
		return problemCode(c, fiber.StatusForbidden, "Door QR Required", "door_qr_required")
	case errors.Is(err, ticket.ErrDoorQRInvalid):
		return problemCode(c, fiber.StatusForbidden, "Door QR Invalid", "door_qr_invalid")
	case errors.Is(err, ticket.ErrDoorQRExpired):
		return problemCode(c, fiber.StatusForbidden, "Door QR Expired", "door_qr_expired")
	case errors.Is(err, ticket.ErrDoorQRUsedUp):
		return problemCode(c, fiber.StatusForbidden, "Door QR Used Up", "door_qr_used_up")
	case errors.Is(err, ticket.ErrSessionClosed):
		return problemCode(c, fiber.StatusForbidden, "Session Not Open", "session_closed")
	default:
		return err
	}
}

func (h *TicketHandler) Apply(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return ticketError(c, err)
	}
	eventID, err := uuid.Parse(c.Params("eventId"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	created, err := h.svc.Apply(c.Context(), p, eventID)
	if err != nil {
		return ticketError(c, err)
	}
	return c.Status(fiber.StatusCreated).JSON(created)
}

func (h *TicketHandler) ApplyForOther(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return ticketError(c, err)
	}
	eventID, err := uuid.Parse(c.Params("eventId"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	userID, err := uuid.Parse(c.Params("userId"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	created, err := h.svc.ApplyForOther(c.Context(), p, eventID, userID)
	if err != nil {
		return ticketError(c, err)
	}
	return c.Status(fiber.StatusCreated).JSON(created)
}

func (h *TicketHandler) ListAssignableUsers(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return ticketError(c, err)
	}
	eventID, err := uuid.Parse(c.Params("eventId"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	people, err := h.svc.ListAssignableUsers(c.Context(), p, eventID, c.Query("q"))
	if err != nil {
		return ticketError(c, err)
	}
	return c.JSON(people)
}

func (h *TicketHandler) ListDoorEvents(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return ticketError(c, err)
	}
	events, err := h.svc.ListDoorEvents(c.Context(), p)
	if err != nil {
		return ticketError(c, err)
	}
	return c.JSON(events)
}

func (h *TicketHandler) SearchDoorAttendees(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return ticketError(c, err)
	}
	eventID, err := uuid.Parse(c.Params("eventId"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	attendees, err := h.svc.SearchDoorAttendees(c.Context(), p, eventID, c.Query("q"))
	if err != nil {
		return ticketError(c, err)
	}
	return c.JSON(attendees)
}

// guestApplied is Guest apply's answer to a caller who may not see the
// Ticket. It is the same for a new guest and for one already registered, so
// the answer does not tell whether the e-mail had applied.
type guestApplied struct {
	Status string `json:"status"`
}

// ApplyGuest is Guest apply (docs/guest-apply.md). The route takes a token
// but does not require one: an operator of the Event and a product's service
// identity get the Ticket, as before; anybody else gets guestApplied.
func (h *TicketHandler) ApplyGuest(c fiber.Ctx) error {
	eventID, err := uuid.Parse(c.Params("eventId"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	var body guestBody
	if err := c.Bind().Body(&body); err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	applied, err := h.svc.ApplyGuest(c.Context(), optionalCaller(c), eventID, ticket.GuestInfo{
		FirstName: body.FirstName, LastName: body.LastName, Email: body.Email, PhoneNumber: body.PhoneNumber,
	})
	if err != nil {
		return ticketError(c, err)
	}
	c.Locals(localsGuestApplyResult, applied.Result)
	if !applied.Trusted {
		return c.Status(fiber.StatusCreated).JSON(guestApplied{Status: "applied"})
	}
	return c.Status(fiber.StatusCreated).JSON(applied.Ticket)
}

type formResponseBody struct {
	ResponseID uuid.UUID  `json:"responseId"`
	Status     string     `json:"status"`
	UserID     *uuid.UUID `json:"userId"`
	Guest      *guestBody `json:"guest"`
}

// RecordFormResponse takes the forms service's report of an answer to one of
// its forms (docs/form-response-tickets.md). The answer is 204 whether or not
// an Event lists the form: the forms service only needs to know it may stop
// sending the report.
func (h *TicketHandler) RecordFormResponse(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return ticketError(c, err)
	}
	formID, err := uuid.Parse(c.Params("formId"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	var body formResponseBody
	if err := c.Bind().Body(&body); err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	r := ticket.FormResponse{
		FormID:     formID,
		ResponseID: body.ResponseID,
		Status:     ticket.FormResponseStatus(body.Status),
		UserID:     body.UserID,
	}
	if body.Guest != nil {
		r.Guest = &ticket.GuestInfo{FirstName: body.Guest.FirstName, LastName: body.Guest.LastName, Email: body.Guest.Email}
	}
	if err := h.svc.RecordFormResponse(c.Context(), p, r); err != nil {
		return ticketError(c, err)
	}
	return c.SendStatus(fiber.StatusNoContent)
}

func (h *TicketHandler) Mine(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return ticketError(c, err)
	}
	tickets, err := h.svc.Mine(c.Context(), p)
	if err != nil {
		return ticketError(c, err)
	}
	return c.JSON(tickets)
}

func (h *TicketHandler) ListByEvent(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return ticketError(c, err)
	}
	eventID, err := uuid.Parse(c.Params("eventId"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	tickets, err := h.svc.ListByEvent(c.Context(), p, eventID)
	if err != nil {
		return ticketError(c, err)
	}
	return c.JSON(tickets)
}

func (h *TicketHandler) CheckIn(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return ticketError(c, err)
	}
	ticketID, err := uuid.Parse(c.Params("ticketId"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	sessionID, err := uuid.Parse(c.Params("sessionId"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	ci, err := h.svc.CheckIn(c.Context(), p, ticketID, sessionID)
	if err != nil {
		return ticketError(c, err)
	}
	return c.Status(fiber.StatusCreated).JSON(ci)
}

type guestCheckInBody struct {
	Email     string `json:"email"`
	DoorToken string `json:"doorToken"`
}

type resolveCheckInBody struct {
	PersonID string `json:"personId"`
	Query    string `json:"query"`
}

func (h *TicketHandler) CheckInMe(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return ticketError(c, err)
	}
	sessionID, err := uuid.Parse(c.Params("sessionId"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	ci, err := h.svc.CheckInMe(c.Context(), p, sessionID)
	if err != nil {
		return ticketError(c, err)
	}
	return c.Status(fiber.StatusCreated).JSON(ci)
}

func (h *TicketHandler) CheckInGuest(c fiber.Ctx) error {
	sessionID, err := uuid.Parse(c.Params("sessionId"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	var body guestCheckInBody
	if err := c.Bind().Body(&body); err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	ci, err := h.svc.CheckInGuest(c.Context(), sessionID, ticket.GuestCheckIn{Email: body.Email, DoorToken: body.DoorToken})
	if err != nil {
		return ticketError(c, err)
	}
	return c.Status(fiber.StatusCreated).JSON(ci)
}

// MintDoorQR answers the door screen: a fresh signed door QR for the Session
// (docs/guest-self-check-in.md).
func (h *TicketHandler) MintDoorQR(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return ticketError(c, err)
	}
	sessionID, err := uuid.Parse(c.Params("sessionId"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	pass, err := h.svc.MintDoorQR(c.Context(), p, sessionID)
	if err != nil {
		return ticketError(c, err)
	}
	c.Set(fiber.HeaderCacheControl, "no-store")
	if !queryFlag(c.Query("svg")) {
		return c.Status(fiber.StatusCreated).JSON(pass)
	}
	// ?svg=1: the QR itself, drawn from pass.URL, for a screen that has no QR
	// library. Square modules, no logo: it is redrawn every few seconds and
	// scanned from further away than a poster.
	svg, err := qr.PlainSVG(pass.URL)
	if err != nil {
		return err
	}
	return c.Status(fiber.StatusCreated).JSON(doorQRWithSVG{DoorQR: pass, SVG: string(svg)})
}

// queryFlag reads a yes/no query parameter: 1, true or yes is yes.
func queryFlag(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}

type doorQRWithSVG struct {
	ticket.DoorQR
	SVG string `json:"svg"`
}

func (h *TicketHandler) ResolveAndCheckIn(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return ticketError(c, err)
	}
	sessionID, err := uuid.Parse(c.Params("sessionId"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	var body resolveCheckInBody
	if err := c.Bind().Body(&body); err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	target := ticket.DoorCheckInTarget{Query: body.Query}
	if body.PersonID != "" {
		personID, err := uuid.Parse(body.PersonID)
		if err != nil {
			return problem(c, fiber.StatusBadRequest, "Bad Request")
		}
		target.PersonID = &personID
	}
	created, err := h.svc.ResolveAndCheckIn(c.Context(), p, sessionID, target)
	if err != nil {
		return ticketError(c, err)
	}
	return c.Status(fiber.StatusCreated).JSON(created)
}

func (h *TicketHandler) DoorActivity(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return ticketError(c, err)
	}
	sessionID, err := uuid.Parse(c.Params("sessionId"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	activity, err := h.svc.DoorActivity(c.Context(), p, sessionID)
	if err != nil {
		return ticketError(c, err)
	}
	return c.JSON(activity)
}

func (h *TicketHandler) Get(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return ticketError(c, err)
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	got, err := h.svc.Get(c.Context(), p, id)
	if err != nil {
		return ticketError(c, err)
	}
	return c.JSON(got)
}

func (h *TicketHandler) ByUserEvent(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return ticketError(c, err)
	}
	userID, err := uuid.Parse(c.Params("userId"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	eventID, err := uuid.Parse(c.Params("eventId"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	got, err := h.svc.GetByUserEvent(c.Context(), p, userID, eventID)
	if err != nil {
		return ticketError(c, err)
	}
	return c.JSON(got)
}

func (h *TicketHandler) List(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return ticketError(c, err)
	}
	email := c.Query("email")
	var userID *uuid.UUID
	if raw := c.Query("userId"); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			return problem(c, fiber.StatusBadRequest, "Bad Request")
		}
		userID = &id
	}
	tickets, err := h.svc.ListQuery(c.Context(), p, email, userID)
	if err != nil {
		return ticketError(c, err)
	}
	return c.JSON(tickets)
}
