package handlers

import (
	"context"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/feedback"
)

// FeedbackCounter reads an Event's feedback invite counts.
type FeedbackCounter interface {
	Counts(ctx context.Context, eventID uuid.UUID) (feedback.Counts, error)
}

// FeedbackHandler answers an Event's feedback counts (etkinlik geri bildirim
// formu, ticket 04).
type FeedbackHandler struct {
	events event.Service
	authz  authz.Authorizer
	counts FeedbackCounter
}

func NewFeedbackHandler(events event.Service, az authz.Authorizer, counts FeedbackCounter) *FeedbackHandler {
	return &FeedbackHandler{events: events, authz: az, counts: counts}
}

// Counts is GET /v1/events/{id}/feedback: how many invites the Event's form
// and each Session's form went out in, and how many were answered. Who may
// read the Event in full may (its editors, DK): the counts name nobody, but
// they are the organizers' figures.
func (h *FeedbackHandler) Counts(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return eventError(c, err)
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	ev, err := h.events.Get(c.Context(), &p, id)
	if err != nil {
		return eventError(c, err)
	}
	if !h.authz.Allow(p, authz.Resource{Type: authz.TypeEvent, OwnerTeam: ev.OwnerTeam}, authz.ReadFull) {
		return eventError(c, event.ErrForbidden)
	}
	counts, err := h.counts.Counts(c.Context(), ev.ID)
	if err != nil {
		return err
	}
	return c.JSON(counts)
}
