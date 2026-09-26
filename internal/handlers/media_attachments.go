package handlers

import (
	"errors"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/media"
)

// attachBody is the body of POST /v1/media/{id}/attachments.
type attachBody struct {
	Owner struct {
		Service string `json:"service"`
		Type    string `json:"type"`
		ID      string `json:"id"`
	} `json:"owner"`
	Role string `json:"role"`
}

// Attach links a Media to a record of the calling product (the service
// attach API): 201 with the new Media attachment, 200 with the one already
// there for the same link.
func (h *MediaHandler) Attach(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return mediaError(c, err)
	}
	mediaID, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	var body attachBody
	if err := c.Bind().JSON(&body); err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	ownerID, err := uuid.Parse(body.Owner.ID)
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	owner := media.Owner{Service: body.Owner.Service, Type: body.Owner.Type, ID: ownerID}
	attachment, created, err := h.svc.Attach(c.Context(), p, mediaID, owner, media.Role(body.Role))
	if errors.Is(err, media.ErrRoleUnknown) {
		return problemWithFields(c, fiber.StatusBadRequest, "Bad Request",
			"The role is not one the calling product's records give a Media.", "media_role_unknown",
			fiber.Map{"role": body.Role})
	}
	if err != nil {
		return attachError(c, err)
	}
	status := fiber.StatusOK
	if created {
		status = fiber.StatusCreated
	}
	return c.Status(status).JSON(attachment)
}

// Detach removes a Media attachment of the calling product: 204, also when
// it is not there (any more).
func (h *MediaHandler) Detach(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return mediaError(c, err)
	}
	mediaID, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	attachmentID, err := uuid.Parse(c.Params("attachmentId"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	if err := h.svc.Detach(c.Context(), p, mediaID, attachmentID); err != nil {
		return attachError(c, err)
	}
	return c.SendStatus(fiber.StatusNoContent)
}

// attachError answers a refusal of the service attach API: problem+json with
// a stable code.
func attachError(c fiber.Ctx, err error) error {
	if handled, problemErr := linkProblem(c, err); handled {
		return problemErr
	}
	switch {
	case errors.Is(err, media.ErrAttachForbidden):
		return problemDetailCode(c, fiber.StatusForbidden, "Forbidden",
			"Only a product's service account with the media:attach role on the core client may manage Media attachments.",
			"media_attach_forbidden")
	case errors.Is(err, media.ErrAttachWrongService):
		return problemDetailCode(c, fiber.StatusForbidden, "Forbidden",
			"A product manages only the Media attachments of its own records.", "media_attach_wrong_service")
	default:
		return mediaError(c, err)
	}
}
