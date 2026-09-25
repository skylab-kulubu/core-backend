package handlers

import (
	"errors"

	"github.com/gofiber/fiber/v3"
	"github.com/skylab-kulubu/core-backend/internal/media"
)

// linkProblem answers a link refused for its Media: problem+json with a
// stable code, the Media and the role it was to play. handled is false for
// any other error.
func linkProblem(c fiber.Ctx, err error) (handled bool, _ error) {
	var refusal *media.LinkRefusal
	if !errors.As(err, &refusal) {
		return false, nil
	}
	fields := fiber.Map{"mediaId": refusal.MediaID.String(), "role": string(refusal.Role)}
	switch {
	case errors.Is(err, media.ErrPurposeMismatch):
		fields["purpose"] = refusal.Purpose
		return true, problemWithFields(c, fiber.StatusUnprocessableEntity, "Unprocessable Content",
			"The Media was uploaded for a purpose that does not fit this role.", "media_purpose_mismatch", fields)
	case errors.Is(err, media.ErrNotLinkable):
		return true, problemWithFields(c, fiber.StatusUnprocessableEntity, "Unprocessable Content",
			"The Media does not exist, is archived or purged, or expired before anything used it.", "media_not_linkable", fields)
	case errors.Is(err, media.ErrTeamMismatch):
		return true, problemWithFields(c, fiber.StatusForbidden, "Forbidden",
			"The Media is used on an Event of another Owner team.", "media_team_mismatch", fields)
	default:
		return false, nil
	}
}
