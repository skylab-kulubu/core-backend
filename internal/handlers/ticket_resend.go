package handlers

import (
	"encoding/json"
	"errors"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/requestid"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/teamaudit"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
)

// resendError answers a refused resend or renewal (docs/ticket-resend.md).
func resendError(c fiber.Ctx, err error) error {
	switch {
	case errors.Is(err, ticket.ErrResendLimited):
		c.Set(fiber.HeaderRetryAfter, "3600")
		return problemDetailCode(c, fiber.StatusTooManyRequests, "Too Many Requests",
			"Bu bilet son bir saatte 3 kez gönderildi.", "ticket_resend_rate_limited")
	case errors.Is(err, ticket.ErrResendDailyLimited):
		c.Set(fiber.HeaderRetryAfter, "3600")
		return problemDetailCode(c, fiber.StatusTooManyRequests, "Too Many Requests",
			"Bu bilet son 24 saatte 6 kez gönderildi.", "ticket_resend_rate_limited")
	case errors.Is(err, teamaudit.ErrUnavailable):
		return problemCode(c, fiber.StatusServiceUnavailable, "Service Unavailable", auditUnavailableCode)
	case errors.Is(err, ticket.ErrNotificationOff):
		return problemCode(c, fiber.StatusConflict, "Conflict", "ticket_notification_off")
	case errors.Is(err, ticket.ErrHolderErased):
		return problemCode(c, fiber.StatusConflict, "Conflict", "ticket_holder_erased")
	case errors.Is(err, ticket.ErrEventEnded):
		return problemCode(c, fiber.StatusConflict, "Conflict", "event_ended")
	default:
		return ticketError(c, err)
	}
}

// ResendTicket is POST /v1/tickets/{id}/notification: the Ticket's mail
// queued again. An Idempotency-Key header makes a repeated request answer
// as the first, with nothing written again.
func (h *TicketHandler) ResendTicket(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return ticketError(c, err)
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	sent, err := h.svc.ResendTicket(auditContext(c, p), p, id, c.Get("Idempotency-Key"))
	if err != nil {
		return resendError(c, err)
	}
	// One line per resend: the request, the Ticket and the resend's number,
	// never who did it (the Team audit record keeps that) or whose Ticket.
	if h.logger != nil && !sent.Replayed {
		if line, err := json.Marshal(struct {
			Event         string `json:"event"`
			CorrelationID string `json:"correlation_id"`
			TicketID      string `json:"ticket_id"`
			Resend        int    `json:"resend"`
		}{"ticket_resent", requestid.FromContext(c), id.String(), sent.N}); err == nil {
			h.logger.Print(string(line))
		}
	}
	return c.Status(fiber.StatusAccepted).JSON(sent)
}

// renewedTicket is the renewed Ticket (its code, never its key) and whether
// its mail was queued.
type renewedTicket struct {
	ticket.Ticket
	NotificationQueued bool `json:"notificationQueued"`
	Resend             int  `json:"resend,omitempty"`
}

// RenewTicketLink is POST /v1/tickets/{id}/access-key/rotate: a new link
// and QR, and the mail with them unless ?notify=false.
func (h *TicketHandler) RenewTicketLink(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return ticketError(c, err)
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	notify := true
	switch c.Query("notify") {
	case "", "true", "1":
	case "false", "0":
		notify = false
	default:
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	renewed, sent, err := h.svc.RenewTicketLink(auditContext(c, p), p, id, notify, c.Get("Idempotency-Key"))
	if err != nil {
		return resendError(c, err)
	}
	// One line per renewal: the request, the Ticket and its Event, never
	// who did it (the Team audit record keeps that) or whose Ticket it is.
	if h.logger != nil && !sent.Replayed {
		if line, err := json.Marshal(struct {
			Event         string `json:"event"`
			CorrelationID string `json:"correlation_id"`
			TicketID      string `json:"ticket_id"`
			EventID       string `json:"event_id"`
			Notified      bool   `json:"notified"`
		}{"ticket_link_renewed", requestid.FromContext(c), renewed.ID.String(), renewed.EventID.String(), sent.Queued}); err == nil {
			h.logger.Print(string(line))
		}
	}
	return c.JSON(renewedTicket{Ticket: renewed, NotificationQueued: sent.Queued, Resend: sent.N})
}
