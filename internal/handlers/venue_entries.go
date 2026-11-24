package handlers

import (
	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
)

// The Event's door without a Session: Venue entry (docs/venue-entry.md).
// The scan route is SkyPassHandler.EnterEvent (it reads SkyPass too).

// eventDoorTarget is the desk's body, as check-in/resolve's.
func eventDoorTarget(c fiber.Ctx) (ticket.DoorCheckInTarget, bool) {
	var body resolveCheckInBody
	if err := c.Bind().Body(&body); err != nil {
		return ticket.DoorCheckInTarget{}, false
	}
	target := ticket.DoorCheckInTarget{Query: body.Query}
	if body.PersonID != "" {
		personID, err := uuid.Parse(body.PersonID)
		if err != nil {
			return ticket.DoorCheckInTarget{}, false
		}
		target.PersonID = &personID
	}
	return target, true
}

// ResolveEntry is POST /v1/events/:eventId/entries/resolve: the desk finds
// the person (name, e-mail, person, Ticket code) and writes today's entry.
func (h *TicketHandler) ResolveEntry(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return ticketError(c, err)
	}
	eventID, err := uuid.Parse(c.Params("eventId"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	target, ok := eventDoorTarget(c)
	if !ok {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	entry, err := h.svc.ResolveEntry(c.Context(), p, eventID, target)
	if err != nil {
		return ticketError(c, err)
	}
	return c.Status(fiber.StatusCreated).JSON(entry)
}

// UndoEntry is DELETE /v1/events/:eventId/entries/:entryId: the desk takes
// back a wrong entry; the day's check-ins stay.
func (h *TicketHandler) UndoEntry(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return ticketError(c, err)
	}
	eventID, err := uuid.Parse(c.Params("eventId"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	entryID, err := uuid.Parse(c.Params("entryId"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	if err := h.svc.UndoEntry(c.Context(), p, eventID, entryID); err != nil {
		return ticketError(c, err)
	}
	return c.SendStatus(fiber.StatusNoContent)
}

// EntrySummary is GET /v1/events/:eventId/entries/summary.
func (h *TicketHandler) EntrySummary(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return ticketError(c, err)
	}
	eventID, err := uuid.Parse(c.Params("eventId"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	summary, err := h.svc.EntrySummary(c.Context(), p, eventID)
	if err != nil {
		return ticketError(c, err)
	}
	c.Set(fiber.HeaderCacheControl, "no-store")
	return c.JSON(summary)
}

// ListEntries is GET /v1/events/:eventId/entries?date=YYYY-MM-DD (today
// without one).
func (h *TicketHandler) ListEntries(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return ticketError(c, err)
	}
	eventID, err := uuid.Parse(c.Params("eventId"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	entries, err := h.svc.ListEntries(c.Context(), p, eventID, c.Query("date"))
	if err != nil {
		return ticketError(c, err)
	}
	c.Set(fiber.HeaderCacheControl, "no-store")
	return c.JSON(entries)
}
