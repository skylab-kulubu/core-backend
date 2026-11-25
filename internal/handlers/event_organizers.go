package handlers

import (
	"bytes"
	"encoding/json"
	"errors"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/event"
)

// organizersBody is the body of PUT /v1/events/{id}/organizers: the whole
// lists (docs/co-organizers.md). A person is an id alone: a person has no
// choices, so an object in people is refused, as is any field not named
// here.
type organizersBody struct {
	Teams []struct {
		Code       string            `json:"code"`
		Management *event.Management `json:"management"`
		Visibility *event.Visibility `json:"visibility"`
	} `json:"teams"`
	People []uuid.UUID `json:"people"`
}

// Organizers is GET /v1/events/{id}/organizers: the Owner team, its rule
// and the Co-organizers, for who may edit the Event.
func (h *EventHandler) Organizers(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return eventError(c, err)
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	organizers, err := h.svc.Organizers(c.Context(), p, id)
	if err != nil {
		return organizersError(c, err)
	}
	return c.JSON(organizers)
}

// SetOrganizers is PUT /v1/events/{id}/organizers: it replaces the
// Co-organizer lists and answers them as GET does.
func (h *EventHandler) SetOrganizers(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return eventError(c, err)
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	var body organizersBody
	decoder := json.NewDecoder(bytes.NewReader(c.Body()))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	if decoder.More() {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	in := event.OrganizersInput{People: body.People}
	for _, team := range body.Teams {
		in.Teams = append(in.Teams, event.OrganizerTeamInput{Code: team.Code, Management: team.Management, Visibility: team.Visibility})
	}
	organizers, err := h.svc.SetOrganizers(c.Context(), p, id, in)
	if err != nil {
		return organizersError(c, err)
	}
	return c.JSON(organizers)
}

// organizersError answers a refused list with what to fix: the field, the
// item's index (none for the list itself) and why
// (event_organizers_invalid).
func organizersError(c fiber.Ctx, err error) error {
	var refused *event.OrganizersError
	if !errors.As(err, &refused) {
		return eventError(c, err)
	}
	fields := fiber.Map{"field": refused.Field, "reason": refused.Reason}
	if refused.Index >= 0 {
		fields["index"] = refused.Index
	}
	return problemWithFields(c, fiber.StatusBadRequest, "Bad Request", "Bad Request", "event_organizers_invalid", fields)
}
