package handlers

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/requestid"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
)

// An operator cancelling a Ticket and taking it back
// (docs/ticket-cancellation.md).

// statusList reads a comma-separated list of the statuses other than
// active (GET /v1/tickets/me?include=waitlisted,cancelled): false for an
// unknown one.
func statusList(raw string) ([]ticket.Status, bool) {
	out := []ticket.Status{}
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		status := ticket.Status(part)
		if !ticket.ValidStatus(status) {
			return nil, false
		}
		out = append(out, status)
	}
	return out, true
}

// cancelError answers a refused cancellation or restoration.
func cancelError(c fiber.Ctx, err error) error {
	switch {
	case errors.Is(err, ticket.ErrTicketCheckedIn):
		return problemCode(c, fiber.StatusConflict, "Conflict", "ticket_checked_in")
	case errors.Is(err, ticket.ErrTicketInCompetition):
		return problemCode(c, fiber.StatusConflict, "Conflict", "ticket_in_competition")
	default:
		return resendError(c, err)
	}
}

type ticketCancelBody struct {
	Reason string `json:"reason"`
}

// CancelTicket is POST /v1/tickets/{id}/cancel {reason?}: an operator
// cancels the Ticket. A Ticket already cancelled answers 200 as it is.
func (h *TicketHandler) CancelTicket(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return ticketError(c, err)
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	var body ticketCancelBody
	if raw := c.Body(); len(strings.TrimSpace(string(raw))) > 0 {
		if err := json.Unmarshal(raw, &body); err != nil {
			return problem(c, fiber.StatusBadRequest, "Bad Request")
		}
	}
	cancelled, err := h.svc.CancelTicket(auditContext(c, p), p, id, body.Reason)
	if err != nil {
		return cancelError(c, err)
	}
	h.logStatus(c, "ticket_cancelled", cancelled)
	return c.JSON(cancelled)
}

// RestoreTicket is POST /v1/tickets/{id}/restore: an operator takes a
// cancellation back (a new link and QR, the mail again). An active Ticket
// answers 200 as it is.
func (h *TicketHandler) RestoreTicket(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return ticketError(c, err)
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	restored, err := h.svc.RestoreTicket(auditContext(c, p), p, id)
	if err != nil {
		return cancelError(c, err)
	}
	h.logStatus(c, "ticket_restored", restored)
	return c.JSON(restored)
}

// PromoteTicket is POST /v1/tickets/{id}/promote: an operator moves a
// waitlisted Ticket to active ("Öne al", docs/event-capacity.md), past the
// capacity if it must (overCapacity in the answer). An active Ticket
// answers 200 as it is; a cancelled one is 409 ticket_cancelled.
func (h *TicketHandler) PromoteTicket(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return ticketError(c, err)
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	promoted, err := h.svc.PromoteTicket(auditContext(c, p), p, id)
	if err != nil {
		return cancelError(c, err)
	}
	h.logStatus(c, "ticket_promoted", promoted)
	return c.JSON(promoted)
}

// logStatus writes one line per cancellation or restoration: the request,
// the Ticket and its Event, never who did it (the Team audit record keeps
// that), whose Ticket it is or the reason.
func (h *TicketHandler) logStatus(c fiber.Ctx, name string, t ticket.Ticket) {
	if h.logger == nil {
		return
	}
	if line, err := json.Marshal(struct {
		Event         string `json:"event"`
		CorrelationID string `json:"correlation_id"`
		TicketID      string `json:"ticket_id"`
		EventID       string `json:"event_id"`
	}{name, requestid.FromContext(c), t.ID.String(), t.EventID.String()}); err == nil {
		h.logger.Print(string(line))
	}
}
