package handlers

import (
	"errors"

	"github.com/gofiber/fiber/v3"
	"github.com/skylab-kulubu/core-backend/internal/media"
)

// startUploadBody is the body of POST /v1/uploads.
type startUploadBody struct {
	Purpose string                `json:"purpose"`
	Name    string                `json:"name"`
	Size    int64                 `json:"size"`
	Limits  *media.NarrowedLimits `json:"limits"`
}

// completeUploadBody is the body of POST /v1/uploads/{id}/complete: every
// part, in order, with the ETag storage answered its PUT with.
type completeUploadBody struct {
	Parts []media.UploadedPart `json:"parts"`
}

// StartUpload starts a Direct upload: 201 with the upload and a presigned
// address for every part. The answer carries those addresses, so it is
// never stored by a cache, and nothing logs it.
func (h *MediaHandler) StartUpload(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return mediaError(c, err)
	}
	var body startUploadBody
	if err := c.Bind().JSON(&body); err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	upload, err := h.svc.StartDirectUpload(c.Context(), p, media.DirectUploadRequest{
		Purpose: body.Purpose, Name: body.Name, Size: body.Size, Limits: body.Limits,
	})
	if err != nil {
		return directUploadError(c, err)
	}
	c.Set(fiber.HeaderCacheControl, "no-store")
	return c.Status(fiber.StatusCreated).JSON(upload)
}

// UploadParts answers the uploader with the parts storage holds and new
// addresses for the others: an interrupted upload continues from there.
func (h *MediaHandler) UploadParts(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return mediaError(c, err)
	}
	upload, err := h.svc.DirectUploadParts(c.Context(), p, parsedID(c.Params("id")))
	if err != nil {
		return directUploadError(c, err)
	}
	c.Set(fiber.HeaderCacheControl, "no-store")
	return c.JSON(upload)
}

// CompleteUpload completes a Direct upload: 201 with the Media it created,
// as POST /v1/media answers. A completion retried after its answer was lost
// answers the same Media.
func (h *MediaHandler) CompleteUpload(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return mediaError(c, err)
	}
	var body completeUploadBody
	if err := c.Bind().JSON(&body); err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	created, err := h.svc.CompleteDirectUpload(c.Context(), p, parsedID(c.Params("id")), body.Parts)
	if err != nil {
		return directUploadError(c, err)
	}
	return c.Status(fiber.StatusCreated).JSON(created)
}

// directUploadError answers a refused Direct upload: the purpose's refusals
// as for POST /v1/media, the upload budget's 429 as for a single-step
// upload, and Direct upload's own.
func directUploadError(c fiber.Ctx, err error) error {
	var limit *media.UploadLimitRefusal
	if errors.As(err, &limit) {
		return uploadRateLimited(c, *limit)
	}
	var size *media.DirectUploadRefusal
	switch {
	case errors.As(err, &size):
		return problemWithFields(c, fiber.StatusUnprocessableEntity, "Unprocessable Content",
			"The stored file is not the size the upload declared. The upload is ended; start a new one.",
			"upload_size_mismatch", fiber.Map{"declaredSize": size.DeclaredSize, "size": size.Size})
	case errors.Is(err, media.ErrDirectUploadPartsMismatch):
		return problemWithFields(c, fiber.StatusBadRequest, "Bad Request",
			"The parts are not the parts storage holds for this upload: send every part, in order, with the ETag its upload answered. POST /v1/uploads/{id}/parts lists them.",
			"upload_parts_mismatch", nil)
	case errors.Is(err, media.ErrDirectUploadUnavailable):
		return problemWithFields(c, fiber.StatusServiceUnavailable, "Service Unavailable",
			"Direct upload is not available on this core.", "direct_upload_unavailable", nil)
	}
	return mediaError(c, err)
}
