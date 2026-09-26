package handlers

import (
	"errors"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/media"
)

// attachBody is the body of POST /v1/media/{id}/attachments.
type attachBody struct {
	Owner struct {
		Service string `json:"service"`
		Type    string `json:"type"`
		ID      string `json:"id"`
	} `json:"owner"`
	Role       string `json:"role"`
	OnBehalfOf string `json:"onBehalfOf"`
}

// Attach links a Media to a record of the calling product (the service
// attach API): 201 with the new Media attachment, 200 with the one already
// there for the same link.
//
// Nothing is refused here: an id or a body that cannot be read is passed on
// empty, so the service authorizes the caller before it finds the request
// malformed, and a person always gets 403.
func (h *MediaHandler) Attach(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return mediaError(c, err)
	}
	var body attachBody
	if err := c.Bind().JSON(&body); err != nil {
		body = attachBody{}
	}
	req := media.AttachRequest{
		Owner:      media.Owner{Service: authz.Product(body.Owner.Service), Type: body.Owner.Type, ID: body.Owner.ID},
		Role:       media.Role(body.Role),
		OnBehalfOf: parsedID(body.OnBehalfOf),
	}
	attachment, created, err := h.svc.Attach(c.Context(), p, parsedID(c.Params("id")), req)
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
	if err := h.svc.Detach(c.Context(), p, parsedID(c.Params("id")), parsedID(c.Params("attachmentId"))); err != nil {
		return attachError(c, err)
	}
	return c.SendStatus(fiber.StatusNoContent)
}

// parsedID is the UUID in raw, or uuid.Nil, which the media service refuses
// as invalid once it has authorized the caller.
func parsedID(raw string) uuid.UUID {
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil
	}
	return id
}

// attachError answers a refusal of the service attach API: problem+json with
// a stable code.
func attachError(c fiber.Ctx, err error) error {
	if handled, problemErr := linkProblem(c, err); handled {
		return problemErr
	}
	var role *media.RoleRefusal
	switch {
	case errors.Is(err, media.ErrAttachForbidden):
		return problemDetailCode(c, fiber.StatusForbidden, "Forbidden",
			"Only a product's service account with the media:attach role on the core client may manage Media attachments.",
			"media_attach_forbidden")
	case errors.Is(err, media.ErrAttachWrongService):
		return problemDetailCode(c, fiber.StatusForbidden, "Forbidden",
			"A product manages only the Media attachments of its own records.", "media_attach_wrong_service")
	case errors.As(err, &role):
		return problemWithFields(c, fiber.StatusBadRequest, "Bad Request",
			"The role is not one the calling product's records give a Media.", "media_role_unknown",
			fiber.Map{"role": string(role.Role)})
	default:
		return mediaError(c, err)
	}
}
